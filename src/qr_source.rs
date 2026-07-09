use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use png::{BitDepth, ColorType, Encoder};
use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fs::File;
use std::io::{Read, Seek, SeekFrom};
use std::path::{Path, PathBuf};
use std::time::UNIX_EPOCH;

const MAX_ACCESS_LOG_SCAN_BYTES: u64 = 4 * 1024 * 1024;
const XWD_FILE_VERSION: u32 = 7;
const XWD_Z_PIXMAP: u32 = 2;
const XWD_MSB_FIRST: u32 = 0;
const XWD_LSB_FIRST: u32 = 1;
const MIN_SCREEN_QR_BLUE_PIXELS: usize = 1_500;

#[derive(Clone)]
pub struct QrSource {
    base_url: String,
    client: reqwest::Client,
    match_terms: Vec<String>,
    log_path: Option<PathBuf>,
    screenshot_path: Option<PathBuf>,
}

#[derive(Clone, Debug, Serialize, PartialEq)]
#[serde(rename_all = "snake_case")]
pub struct QrEvent {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub captured_at: String,
    pub method: String,
    pub url: String,
    pub host: String,
    pub path: String,
    pub request_body_base64: Option<String>,
    pub response_status: Option<i64>,
    pub response_body_base64: Option<String>,
    pub response_text: Option<String>,
    pub matched_by: Vec<String>,
}

#[derive(Clone, Debug, Serialize, PartialEq)]
#[serde(rename_all = "snake_case")]
pub struct LoginQrCode {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub status: String,
    pub captured_at: String,
    pub source_url: String,
    pub response_status: Option<i64>,
    pub login_url: Option<String>,
    pub image_content_type: Option<String>,
    pub image_base64: Option<String>,
    pub image_data_uri: Option<String>,
    pub response_text: Option<String>,
    pub request_body_base64: Option<String>,
    pub response_body_base64: Option<String>,
    pub matched_by: Vec<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct SearchLogsRequest {
    limit: i64,
    include_attributes: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SearchLogsResponse {
    logs: Vec<LogEntry>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LogEntry {
    id: String,
    completed_at: String,
    http_status: Option<i64>,
    attributes: Option<Value>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct GetLogRequest<'a> {
    id: &'a str,
    include_payload: bool,
}

#[derive(Debug, Deserialize)]
struct GetLogResponse {
    log: Option<LogEntry>,
}

impl QrSource {
    pub fn new(base_url: String, match_terms: Vec<String>) -> Self {
        Self {
            base_url,
            client: reqwest::Client::new(),
            match_terms,
            log_path: None,
            screenshot_path: None,
        }
    }

    pub fn with_log_path(mut self, path: Option<PathBuf>) -> Self {
        self.log_path = path;
        self
    }

    pub fn with_screenshot_path(mut self, path: Option<PathBuf>) -> Self {
        self.screenshot_path = path;
        self
    }

    pub async fn latest(&self) -> Result<Option<QrEvent>> {
        Ok(self.search_newest(1).await?.into_iter().next())
    }

    async fn search_newest(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let wanted = limit.clamp(1, 100);
        let mut api_error = None;
        let mut out = match self.search_api_newest(wanted).await {
            Ok(events) => events,
            Err(api_err) => {
                api_error = Some(api_err);
                Vec::new()
            }
        };
        if out.len() >= wanted {
            out.truncate(wanted);
            return Ok(out);
        }
        let access_log_events = self.search_access_log_newest(wanted - out.len())?;
        for event in access_log_events {
            if !out.iter().any(|existing| existing.id == event.id) {
                out.push(event);
            }
        }
        if out.len() < wanted {
            if let Some(event) = self.search_screenshot_newest()? {
                if !out.iter().any(|existing| existing.id == event.id) {
                    out.push(event);
                }
            }
        }
        if out.is_empty() {
            if let Some(api_err) = api_error {
                return Err(api_err);
            }
        }
        out.sort_by(|left, right| right.captured_at.cmp(&left.captured_at));
        out.truncate(wanted);
        Ok(out)
    }

    async fn search_api_newest(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let wanted = limit.clamp(1, 100);
        let scan_limit = (wanted * 20).clamp(100, 500) as i64;
        let search = self
            .client
            .post(format!("{}/api/logs/search", self.base_url))
            .json(&SearchLogsRequest {
                limit: scan_limit,
                include_attributes: true,
            })
            .send()
            .await
            .context("query agentgateway logs search api")?;
        ensure_success(search.status())?;
        let response: SearchLogsResponse = search
            .json()
            .await
            .context("decode agentgateway logs search response")?;

        let mut out = Vec::with_capacity(wanted);
        for entry in response.logs {
            let event = if let Some(candidate) = self.entry_to_event(&entry) {
                match self.get_log(&entry.id).await {
                    Ok(Some(full)) => self.entry_to_event(&full).unwrap_or(candidate),
                    Ok(None) | Err(_) => candidate,
                }
            } else {
                let Some(full) = self.get_log(&entry.id).await.ok().flatten() else {
                    continue;
                };
                let Some(event) = self.entry_to_event(&full) else {
                    continue;
                };
                event
            };
            out.push(event);
            if out.len() >= wanted {
                break;
            }
        }
        Ok(out)
    }

    fn search_access_log_newest(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let Some(path) = &self.log_path else {
            return Ok(Vec::new());
        };
        let Some(contents) = read_recent_access_log(path)? else {
            return Ok(Vec::new());
        };
        let mut out = Vec::with_capacity(limit);
        for (index, line) in contents.lines().rev().enumerate() {
            let Some(entry) = access_log_line_to_entry(line, index) else {
                continue;
            };
            let Some(event) = self.entry_to_event(&entry) else {
                continue;
            };
            out.push(event);
            if out.len() >= limit {
                break;
            }
        }
        Ok(out)
    }

    fn search_screenshot_newest(&self) -> Result<Option<QrEvent>> {
        let Some(path) = &self.screenshot_path else {
            return Ok(None);
        };
        let Some(screen) = read_xwd_screen(path)? else {
            return Ok(None);
        };
        if !looks_like_wechat_login_qr_screen(&screen) {
            return Ok(None);
        }
        let png = encode_rgb_png(screen.width, screen.height, &screen.rgb)?;
        let digest = md5::compute(&png);
        let metadata = std::fs::metadata(path).ok();
        let captured_at = metadata
            .and_then(|value| value.modified().ok())
            .and_then(|time| time.duration_since(UNIX_EPOCH).ok())
            .map(|duration| format!("unix:{}", duration.as_secs()))
            .unwrap_or_else(|| "unix:0".to_string());
        Ok(Some(QrEvent {
            id: format!("xvfb-screen-{digest:x}"),
            kind: "wechat.login_qrcode".to_string(),
            captured_at,
            method: "SCREEN".to_string(),
            url: "x11://wechat/login-screen".to_string(),
            host: "wechat-ui".to_string(),
            path: "/login-screen".to_string(),
            request_body_base64: None,
            response_status: None,
            response_body_base64: Some(STANDARD.encode(png)),
            response_text: None,
            matched_by: vec!["screen".to_string(), "qrcode".to_string()],
        }))
    }

    async fn get_log(&self, id: &str) -> Result<Option<LogEntry>> {
        let response = self
            .client
            .post(format!("{}/api/logs/get", self.base_url))
            .json(&GetLogRequest {
                id,
                include_payload: true,
            })
            .send()
            .await
            .with_context(|| format!("query agentgateway log {id}"))?;
        ensure_success(response.status())?;
        Ok(response
            .json::<GetLogResponse>()
            .await
            .context("decode agentgateway log get response")?
            .log)
    }

    fn entry_to_event(&self, entry: &LogEntry) -> Option<QrEvent> {
        let attributes = entry.attributes.as_ref()?;
        let method = attr_string(attributes, "http.method").unwrap_or_default();
        let host = attr_string(attributes, "http.host").unwrap_or_default();
        let path = attr_string(attributes, "http.path").unwrap_or_default();
        let request_body = attr_string(attributes, "request.body");
        let response_body = attr_string(attributes, "response.body");
        let request_payload = request_body.as_deref().map(body_payload_from_attribute);
        let response_payload = response_body.as_deref().map(body_payload_from_attribute);
        let request_text = request_payload
            .as_ref()
            .and_then(|body| body.text.as_deref())
            .or(request_body.as_deref())
            .unwrap_or_default();
        let response_text = response_payload
            .as_ref()
            .and_then(|body| body.text.as_deref())
            .or(response_body.as_deref())
            .unwrap_or_default();
        let response_status = entry
            .http_status
            .or_else(|| attr_i64(attributes, "http.status"));
        let url = build_url(&host, &path);
        if !is_wechat_login_qr_candidate(&host, &path, request_text, response_text) {
            return None;
        }
        let haystack = [
            method.as_str(),
            host.as_str(),
            path.as_str(),
            url.as_str(),
            request_text,
            response_text,
        ]
        .join("\n")
        .to_ascii_lowercase();
        let matched_by = self
            .match_terms
            .iter()
            .filter(|term| haystack.contains(term.as_str()))
            .cloned()
            .collect::<Vec<_>>();
        if matched_by.is_empty() {
            return None;
        }
        Some(QrEvent {
            id: entry.id.clone(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: entry.completed_at.clone(),
            method,
            url,
            host,
            path,
            request_body_base64: request_payload
                .as_ref()
                .map(|body| STANDARD.encode(&body.bytes)),
            response_status,
            response_body_base64: response_payload
                .as_ref()
                .map(|body| STANDARD.encode(&body.bytes)),
            response_text: response_payload.and_then(|body| body.text),
            matched_by,
        })
    }
}

fn read_recent_access_log(path: &Path) -> Result<Option<String>> {
    let mut file = match File::open(path) {
        Ok(file) => file,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err).with_context(|| format!("open {}", path.display())),
    };
    let len = file
        .metadata()
        .with_context(|| format!("stat {}", path.display()))?
        .len();
    let offset = len.saturating_sub(MAX_ACCESS_LOG_SCAN_BYTES);
    file.seek(SeekFrom::Start(offset))
        .with_context(|| format!("seek {}", path.display()))?;
    let mut contents = String::new();
    file.read_to_string(&mut contents)
        .with_context(|| format!("read {}", path.display()))?;
    if offset > 0 {
        if let Some((_, rest)) = contents.split_once('\n') {
            contents = rest.to_string();
        }
    }
    Ok(Some(contents))
}

fn access_log_line_to_entry(line: &str, index_from_tail: usize) -> Option<LogEntry> {
    let value: Value = serde_json::from_str(line).ok()?;
    if attr_string(&value, "scope").as_deref() != Some("request") {
        return None;
    }
    let completed_at = attr_string(&value, "time").unwrap_or_default();
    let id = attr_string(&value, "id").unwrap_or_else(|| {
        let digest = md5::compute(format!("{completed_at}:{index_from_tail}:{line}").as_bytes());
        format!("access-log-{digest:x}")
    });
    Some(LogEntry {
        id,
        completed_at,
        http_status: attr_i64(&value, "http.status"),
        attributes: Some(value),
    })
}

#[derive(Clone, Debug, PartialEq)]
struct RgbScreen {
    width: u32,
    height: u32,
    rgb: Vec<u8>,
}

#[derive(Clone, Debug, PartialEq)]
struct XwdHeader {
    header_size: usize,
    file_version: u32,
    pixmap_format: u32,
    width: u32,
    height: u32,
    byte_order: u32,
    bits_per_pixel: u32,
    bytes_per_line: usize,
    red_mask: u32,
    green_mask: u32,
    blue_mask: u32,
    ncolors: usize,
}

fn read_xwd_screen(path: &Path) -> Result<Option<RgbScreen>> {
    let bytes = match std::fs::read(path) {
        Ok(bytes) => bytes,
        Err(err) if err.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(err) => return Err(err).with_context(|| format!("read {}", path.display())),
    };
    parse_xwd_screen(&bytes)
        .map(Some)
        .with_context(|| format!("parse {}", path.display()))
}

fn parse_xwd_screen(bytes: &[u8]) -> Result<RgbScreen> {
    let header = parse_xwd_header(bytes)?;
    if header.file_version != XWD_FILE_VERSION || header.pixmap_format != XWD_Z_PIXMAP {
        anyhow::bail!("unsupported xwd header");
    }
    if header.width == 0 || header.height == 0 {
        anyhow::bail!("empty xwd screen");
    }
    if header.bits_per_pixel != 24 && header.bits_per_pixel != 32 {
        anyhow::bail!("unsupported xwd bits_per_pixel {}", header.bits_per_pixel);
    }
    if !matches!(header.byte_order, XWD_MSB_FIRST | XWD_LSB_FIRST) {
        anyhow::bail!("unsupported xwd byte_order {}", header.byte_order);
    }
    let bytes_per_pixel = (header.bits_per_pixel / 8) as usize;
    let color_table_len = header.ncolors.saturating_mul(12);
    let offset = header
        .header_size
        .checked_add(color_table_len)
        .context("xwd color table offset overflow")?;
    let required = header
        .bytes_per_line
        .checked_mul(header.height as usize)
        .and_then(|len| offset.checked_add(len))
        .context("xwd image length overflow")?;
    if bytes.len() < required {
        anyhow::bail!("truncated xwd image");
    }
    let mut rgb = Vec::with_capacity(header.width as usize * header.height as usize * 3);
    let image = &bytes[offset..required];
    for y in 0..header.height as usize {
        let row = &image[y * header.bytes_per_line..(y + 1) * header.bytes_per_line];
        for x in 0..header.width as usize {
            let start = x * bytes_per_pixel;
            let pixel = read_xwd_pixel(
                &row[start..start + bytes_per_pixel],
                header.byte_order,
                header.bits_per_pixel,
            );
            rgb.push(masked_channel(pixel, header.red_mask));
            rgb.push(masked_channel(pixel, header.green_mask));
            rgb.push(masked_channel(pixel, header.blue_mask));
        }
    }
    Ok(RgbScreen {
        width: header.width,
        height: header.height,
        rgb,
    })
}

fn parse_xwd_header(bytes: &[u8]) -> Result<XwdHeader> {
    if bytes.len() < 100 {
        anyhow::bail!("truncated xwd header");
    }
    parse_xwd_header_with_endian(bytes, true)
        .or_else(|_| parse_xwd_header_with_endian(bytes, false))
}

fn parse_xwd_header_with_endian(bytes: &[u8], big_endian: bool) -> Result<XwdHeader> {
    let field = |index: usize| {
        let start = index * 4;
        if big_endian {
            u32::from_be_bytes(bytes[start..start + 4].try_into().unwrap())
        } else {
            u32::from_le_bytes(bytes[start..start + 4].try_into().unwrap())
        }
    };
    let header = XwdHeader {
        header_size: field(0) as usize,
        file_version: field(1),
        pixmap_format: field(2),
        width: field(4),
        height: field(5),
        byte_order: field(7),
        bits_per_pixel: field(11),
        bytes_per_line: field(12) as usize,
        red_mask: field(14),
        green_mask: field(15),
        blue_mask: field(16),
        ncolors: field(19) as usize,
    };
    if header.header_size < 100 || header.header_size > bytes.len() {
        anyhow::bail!("invalid xwd header size");
    }
    if header.file_version != XWD_FILE_VERSION {
        anyhow::bail!("invalid xwd version");
    }
    if header.width > 16_384 || header.height > 16_384 {
        anyhow::bail!("unreasonable xwd dimensions");
    }
    Ok(header)
}

fn read_xwd_pixel(bytes: &[u8], byte_order: u32, bits_per_pixel: u32) -> u32 {
    if bits_per_pixel == 32 {
        return if byte_order == XWD_MSB_FIRST {
            u32::from_be_bytes(bytes.try_into().unwrap())
        } else {
            u32::from_le_bytes(bytes.try_into().unwrap())
        };
    }
    if byte_order == XWD_MSB_FIRST {
        bytes
            .iter()
            .fold(0u32, |pixel, byte| (pixel << 8) | *byte as u32)
    } else {
        bytes
            .iter()
            .rev()
            .fold(0u32, |pixel, byte| (pixel << 8) | *byte as u32)
    }
}

fn masked_channel(pixel: u32, mask: u32) -> u8 {
    if mask == 0 {
        return 0;
    }
    let shift = mask.trailing_zeros();
    let max = mask >> shift;
    let value = (pixel & mask) >> shift;
    ((value * 255 + max / 2) / max) as u8
}

fn looks_like_wechat_login_qr_screen(screen: &RgbScreen) -> bool {
    let blue_pixels = screen
        .rgb
        .chunks_exact(3)
        .filter(|pixel| is_wechat_qr_blue(pixel[0], pixel[1], pixel[2]))
        .count();
    blue_pixels >= MIN_SCREEN_QR_BLUE_PIXELS
}

fn is_wechat_qr_blue(red: u8, green: u8, blue: u8) -> bool {
    blue > 145
        && red < 130
        && green < 150
        && blue.saturating_sub(red) > 45
        && blue.saturating_sub(green) > 35
}

fn encode_rgb_png(width: u32, height: u32, rgb: &[u8]) -> Result<Vec<u8>> {
    let expected = width as usize * height as usize * 3;
    if rgb.len() != expected {
        anyhow::bail!("rgb length does not match dimensions");
    }
    let mut out = Vec::new();
    {
        let mut encoder = Encoder::new(&mut out, width, height);
        encoder.set_color(ColorType::Rgb);
        encoder.set_depth(BitDepth::Eight);
        let mut writer = encoder.write_header().context("write png header")?;
        writer.write_image_data(rgb).context("write png pixels")?;
    }
    Ok(out)
}

impl QrEvent {
    pub fn to_login_qrcode(&self) -> LoginQrCode {
        let image = self
            .response_text
            .as_deref()
            .and_then(image_from_body_text)
            .or_else(|| {
                self.response_body_base64
                    .as_deref()
                    .and_then(image_from_base64_body)
            });
        LoginQrCode {
            id: self.id.clone(),
            kind: "wechat.login_qrcode".to_string(),
            status: "captured".to_string(),
            captured_at: self.captured_at.clone(),
            source_url: self.url.clone(),
            response_status: self.response_status,
            login_url: self.response_text.as_deref().and_then(login_url_from_text),
            image_content_type: image.as_ref().map(|value| value.content_type.clone()),
            image_base64: image.as_ref().map(|value| value.base64.clone()),
            image_data_uri: image.map(|value| value.data_uri),
            response_text: self.response_text.as_deref().and_then(printable_text),
            request_body_base64: self.request_body_base64.clone(),
            response_body_base64: self.response_body_base64.clone(),
            matched_by: self.matched_by.clone(),
        }
    }
}

#[derive(Clone, Debug, PartialEq)]
struct ImageBody {
    content_type: String,
    base64: String,
    data_uri: String,
}

#[derive(Clone, Debug, PartialEq)]
struct BodyPayload {
    bytes: Vec<u8>,
    text: Option<String>,
}

fn ensure_success(status: StatusCode) -> Result<()> {
    if status.is_success() {
        return Ok(());
    }
    anyhow::bail!("agentgateway api returned {status}")
}

fn body_payload_from_attribute(value: &str) -> BodyPayload {
    if let Some(bytes) = decode_logged_base64_body(value) {
        return body_payload_from_bytes(bytes);
    }
    body_payload_from_bytes(value.as_bytes().to_vec())
}

fn decode_logged_base64_body(value: &str) -> Option<Vec<u8>> {
    let trimmed = value.trim();
    if !looks_like_standard_base64(trimmed) {
        return None;
    }
    let bytes = STANDARD.decode(trimmed).ok()?;
    if bytes.is_empty() {
        return None;
    }
    if image_content_type(&bytes).is_some() || printable_text_from_bytes(&bytes).is_some() {
        return Some(bytes);
    }
    None
}

fn looks_like_standard_base64(value: &str) -> bool {
    if value.is_empty() || !value.len().is_multiple_of(4) {
        return false;
    }
    let mut padding = false;
    for byte in value.bytes() {
        match byte {
            b'A'..=b'Z' | b'a'..=b'z' | b'0'..=b'9' | b'+' | b'/' if !padding => {}
            b'=' => padding = true,
            _ => return false,
        }
    }
    true
}

fn body_payload_from_bytes(bytes: Vec<u8>) -> BodyPayload {
    let text = printable_text_from_bytes(&bytes);
    BodyPayload { bytes, text }
}

fn printable_text_from_bytes(bytes: &[u8]) -> Option<String> {
    std::str::from_utf8(bytes).ok().and_then(printable_text)
}

fn image_from_body_text(body: &str) -> Option<ImageBody> {
    image_from_data_uri(body).or_else(|| image_from_bytes(body.as_bytes()))
}

fn image_from_base64_body(body: &str) -> Option<ImageBody> {
    let bytes = STANDARD.decode(body).ok()?;
    image_from_bytes(&bytes)
}

fn image_from_data_uri(body: &str) -> Option<ImageBody> {
    let trimmed = body.trim();
    let lower = trimmed.to_ascii_lowercase();
    if !lower.starts_with("data:image/") {
        return None;
    }
    let (meta, data) = trimmed.split_once(',')?;
    if !meta.to_ascii_lowercase().ends_with(";base64") {
        return None;
    }
    let content_type = meta.strip_prefix("data:")?.strip_suffix(";base64")?;
    if STANDARD.decode(data).is_err() {
        return None;
    }
    Some(ImageBody {
        content_type: content_type.to_string(),
        base64: data.to_string(),
        data_uri: trimmed.to_string(),
    })
}

fn image_from_bytes(bytes: &[u8]) -> Option<ImageBody> {
    let content_type = image_content_type(bytes)?;
    let base64 = STANDARD.encode(bytes);
    Some(ImageBody {
        data_uri: format!("data:{content_type};base64,{base64}"),
        content_type: content_type.to_string(),
        base64,
    })
}

fn image_content_type(bytes: &[u8]) -> Option<&'static str> {
    if bytes.starts_with(b"\x89PNG\r\n\x1a\n") {
        return Some("image/png");
    }
    if bytes.starts_with(b"\xff\xd8\xff") {
        return Some("image/jpeg");
    }
    if bytes.starts_with(b"GIF87a") || bytes.starts_with(b"GIF89a") {
        return Some("image/gif");
    }
    if bytes.len() >= 12 && bytes.starts_with(b"RIFF") && &bytes[8..12] == b"WEBP" {
        return Some("image/webp");
    }
    let trimmed = trim_ascii_start(bytes);
    if starts_with_ascii_case_insensitive(trimmed, b"<svg") {
        return Some("image/svg+xml");
    }
    None
}

fn printable_text(body: &str) -> Option<String> {
    let value = body.trim();
    if value.is_empty() {
        return None;
    }
    let total = value.chars().count();
    if total == 0 {
        return None;
    }
    let control = value
        .chars()
        .filter(|ch| ch.is_control() && !matches!(ch, '\n' | '\r' | '\t'))
        .count();
    if control * 10 > total {
        return None;
    }
    Some(value.to_string())
}

fn is_wechat_login_qr_candidate(
    host: &str,
    path: &str,
    request_text: &str,
    response_text: &str,
) -> bool {
    if contains_wechat_login_url(request_text) || contains_wechat_login_url(response_text) {
        return true;
    }
    if !is_wechat_host(host) {
        return false;
    }
    let value = [path, request_text, response_text]
        .join("\n")
        .to_ascii_lowercase();
    [
        "getloginqrcode",
        "checkloginqrcode",
        "loginqrcode",
        "qrcode",
        "qr_code",
        "qrlogin",
        "uuid",
    ]
    .iter()
    .any(|term| value.contains(term))
}

fn contains_wechat_login_url(value: &str) -> bool {
    let lower = value.to_ascii_lowercase();
    lower.contains("login.weixin.qq.com/l/") || lower.contains("weixin.qq.com/x/")
}

fn is_wechat_host(host: &str) -> bool {
    let lower = host
        .split(':')
        .next()
        .unwrap_or(host)
        .trim_end_matches('.')
        .to_ascii_lowercase();
    lower == "weixin.qq.com"
        || lower.ends_with(".weixin.qq.com")
        || lower == "wechat.com"
        || lower.ends_with(".wechat.com")
}

fn login_url_from_text(body: &str) -> Option<String> {
    let urls = urls_in_text(body);
    urls.iter()
        .find(|url| is_wechat_login_url(url))
        .cloned()
        .or_else(|| urls.into_iter().next())
}

fn is_wechat_login_url(url: &str) -> bool {
    let value = url.to_ascii_lowercase();
    value.contains("login.weixin.qq.com/l/")
}

fn urls_in_text(body: &str) -> Vec<String> {
    let mut urls = Vec::new();
    if let Ok(value) = serde_json::from_str::<Value>(body) {
        collect_urls_from_json_value(&value, &mut urls);
    }
    collect_urls_from_plain_text(body, &mut urls);
    urls
}

fn collect_urls_from_json_value(value: &Value, urls: &mut Vec<String>) {
    match value {
        Value::String(value) => {
            collect_urls_from_plain_text(value, urls);
            let trimmed = value.trim();
            if matches!(trimmed.as_bytes().first(), Some(b'{') | Some(b'[')) {
                if let Ok(nested) = serde_json::from_str::<Value>(trimmed) {
                    collect_urls_from_json_value(&nested, urls);
                }
            }
        }
        Value::Array(values) => {
            for value in values {
                collect_urls_from_json_value(value, urls);
            }
        }
        Value::Object(values) => {
            for value in values.values() {
                collect_urls_from_json_value(value, urls);
            }
        }
        Value::Null | Value::Bool(_) | Value::Number(_) => {}
    }
}

fn collect_urls_from_plain_text(body: &str, urls: &mut Vec<String>) {
    let mut offset = 0;
    while offset < body.len() {
        let rest = &body[offset..];
        let Some(start) = next_url_start(rest) else {
            break;
        };
        let absolute_start = offset + start;
        let url_rest = &body[absolute_start..];
        let end = url_rest
            .find(|ch: char| {
                ch.is_whitespace() || matches!(ch, '"' | '\'' | '<' | '>' | ')' | ']' | '}' | '\\')
            })
            .unwrap_or(url_rest.len());
        if let Some(url) = clean_url(&url_rest[..end]) {
            push_url(urls, url);
        }
        offset = absolute_start + end.max(1);
    }
}

fn push_url(urls: &mut Vec<String>, url: String) {
    if !urls.iter().any(|existing| existing == &url) {
        urls.push(url);
    }
}

fn next_url_start(body: &str) -> Option<usize> {
    let http = body.find("http://");
    let https = body.find("https://");
    let start = match (http, https) {
        (Some(http), Some(https)) => http.min(https),
        (Some(http), None) => http,
        (None, Some(https)) => https,
        (None, None) => return None,
    };
    Some(start)
}

fn clean_url(url: &str) -> Option<String> {
    let value = url.trim_end_matches([',', '.', ';', ':']);
    if value == "http://" || value == "https://" {
        return None;
    }
    Some(value.to_string())
}

fn trim_ascii_start(mut bytes: &[u8]) -> &[u8] {
    while let Some((first, rest)) = bytes.split_first() {
        if first.is_ascii_whitespace() {
            bytes = rest;
        } else {
            break;
        }
    }
    bytes
}

fn starts_with_ascii_case_insensitive(value: &[u8], prefix: &[u8]) -> bool {
    value.len() >= prefix.len()
        && value[..prefix.len()]
            .iter()
            .zip(prefix)
            .all(|(left, right)| left.eq_ignore_ascii_case(right))
}

fn attr_string(attributes: &Value, key: &str) -> Option<String> {
    attr_value(attributes, key).and_then(|value| match value {
        Value::String(value) => Some(value.clone()),
        Value::Number(value) => Some(value.to_string()),
        Value::Bool(value) => Some(value.to_string()),
        _ => None,
    })
}

fn attr_i64(attributes: &Value, key: &str) -> Option<i64> {
    attr_value(attributes, key).and_then(|value| match value {
        Value::Number(value) => value.as_i64(),
        Value::String(value) => value.parse().ok(),
        _ => None,
    })
}

fn attr_value<'a>(attributes: &'a Value, key: &str) -> Option<&'a Value> {
    if let Some(value) = attributes.get(key) {
        return Some(value);
    }
    let mut current = attributes;
    for part in key.split('.') {
        current = current.get(part)?;
    }
    Some(current)
}

fn build_url(host: &str, path: &str) -> String {
    if path.starts_with("http://") || path.starts_with("https://") {
        return path.to_string();
    }
    if host.is_empty() {
        return path.to_string();
    }
    if path.starts_with('/') {
        format!("https://{host}{path}")
    } else if path.is_empty() {
        format!("https://{host}")
    } else {
        format!("https://{host}/{path}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::routing::post;
    use axum::{Json, Router};
    use serde_json::json;
    use std::fs;

    #[tokio::test]
    async fn latest_prefers_agentgateway_api_over_access_log_fallback() {
        let access_log_path =
            std::env::temp_dir().join(format!("webox-qr-access-{}.log", uuid::Uuid::new_v4()));
        write_qr_access_log(&access_log_path, "access-log", "access");

        let app = Router::new()
            .route("/api/logs/search", post(search_logs_fixture))
            .route("/api/logs/get", post(get_log_fixture));
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });

        let source = QrSource::new(format!("http://{addr}"), vec!["qrcode".into()])
            .with_log_path(Some(access_log_path.clone()));
        let event = source.latest().await.unwrap().expect("qr event");

        assert_eq!(event.id, "api-log");
        assert_eq!(
            event.response_text.as_deref(),
            Some("{\"qr\":\"https://login.weixin.qq.com/l/api\"}")
        );
        let _ = fs::remove_file(access_log_path);
    }

    #[tokio::test]
    async fn latest_falls_back_to_access_log_when_api_is_unavailable() {
        let access_log_path = std::env::temp_dir().join(format!(
            "webox-qr-access-fallback-{}.log",
            uuid::Uuid::new_v4()
        ));
        write_qr_access_log(&access_log_path, "access-log", "access");

        let source = QrSource::new("http://127.0.0.1:9".to_string(), vec!["qrcode".into()])
            .with_log_path(Some(access_log_path.clone()));
        let event = source.latest().await.unwrap().expect("qr event");

        assert_eq!(event.id, "access-log");
        assert_eq!(
            event.response_text.as_deref(),
            Some("{\"qr\":\"https://login.weixin.qq.com/l/access\"}")
        );
        let _ = fs::remove_file(access_log_path);
    }

    #[tokio::test]
    async fn latest_falls_back_to_xvfb_screen_when_capture_logs_are_empty() {
        let screen_path =
            std::env::temp_dir().join(format!("webox-qr-screen-{}.xwd", uuid::Uuid::new_v4()));
        fs::write(&screen_path, xwd_fixture_with_blue_qr()).unwrap();

        let source = QrSource::new("http://127.0.0.1:9".to_string(), vec!["qrcode".into()])
            .with_screenshot_path(Some(screen_path.clone()));
        let event = source.latest().await.unwrap().expect("qr event");
        let qrcode = event.to_login_qrcode();

        assert!(event.id.starts_with("xvfb-screen-"));
        assert_eq!(event.method, "SCREEN");
        assert_eq!(qrcode.image_content_type, Some("image/png".to_string()));
        assert!(qrcode
            .image_data_uri
            .as_deref()
            .unwrap()
            .starts_with("data:image/png;base64,"));
        let _ = fs::remove_file(screen_path);
    }

    #[test]
    fn xwd_screen_without_qr_blue_is_ignored() {
        let screen = parse_xwd_screen(&xwd_fixture(80, 80, |_, _| [255, 255, 255])).unwrap();
        assert!(!looks_like_wechat_login_qr_screen(&screen));
    }

    fn write_qr_access_log(path: &Path, id: &str, code: &str) {
        fs::write(
            path,
            json!({
                "scope": "request",
                "time": "2026-07-09T00:00:00Z",
                "id": id,
                "http.method": "POST",
                "http.host": "login.weixin.qq.com",
                "http.path": "/cgi-bin/micromsg-bin/getloginqrcode",
                "response.body": format!("{{\"qr\":\"https://login.weixin.qq.com/l/{code}\"}}")
            })
            .to_string()
                + "\n",
        )
        .unwrap();
    }

    fn xwd_fixture_with_blue_qr() -> Vec<u8> {
        xwd_fixture(96, 96, |x, y| {
            if (16..80).contains(&x) && (16..80).contains(&y) {
                [45, 65, 255]
            } else {
                [255, 255, 255]
            }
        })
    }

    fn xwd_fixture(width: u32, height: u32, pixel: impl Fn(u32, u32) -> [u8; 3]) -> Vec<u8> {
        let header_size = 104u32;
        let bits_per_pixel = 32u32;
        let bytes_per_line = width * 4;
        let fields = [
            header_size,
            XWD_FILE_VERSION,
            XWD_Z_PIXMAP,
            24,
            width,
            height,
            0,
            XWD_MSB_FIRST,
            32,
            XWD_MSB_FIRST,
            32,
            bits_per_pixel,
            bytes_per_line,
            4,
            0x00ff0000,
            0x0000ff00,
            0x000000ff,
            8,
            256,
            0,
            width,
            height,
            0,
            0,
            0,
        ];
        let mut out = Vec::new();
        for field in fields {
            out.extend_from_slice(&field.to_be_bytes());
        }
        out.extend_from_slice(b"X\0\0\0");
        for y in 0..height {
            for x in 0..width {
                let [red, green, blue] = pixel(x, y);
                let value = ((red as u32) << 16) | ((green as u32) << 8) | blue as u32;
                out.extend_from_slice(&value.to_be_bytes());
            }
        }
        out
    }

    async fn search_logs_fixture() -> Json<Value> {
        Json(json!({
            "logs": [api_log_fixture()],
            "nextCursor": null
        }))
    }

    async fn get_log_fixture() -> Json<Value> {
        Json(json!({
            "log": api_log_fixture()
        }))
    }

    fn api_log_fixture() -> Value {
        json!({
            "id": "api-log",
            "startedAt": "2026-07-09T00:00:00Z",
            "completedAt": "2026-07-09T00:00:01Z",
            "durationMs": 1000,
            "traceId": null,
            "spanId": null,
            "httpStatus": 200,
            "error": null,
            "genAi": {},
            "usage": {},
            "cost": null,
            "hasPayload": false,
            "attributes": {
                "http.method": "POST",
                "http.host": "login.weixin.qq.com",
                "http.path": "/cgi-bin/micromsg-bin/getloginqrcode",
                "response.body": "{\"qr\":\"https://login.weixin.qq.com/l/api\"}"
            }
        })
    }

    #[test]
    fn event_matches_flat_agentgateway_attributes() {
        let source = QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]);
        let entry = LogEntry {
            id: "log-1".to_string(),
            completed_at: "2026-07-09T00:00:00Z".to_string(),
            http_status: Some(200),
            attributes: Some(json!({
                "http.method": "POST",
                "http.host": "login.weixin.qq.com",
                "http.path": "/cgi-bin/micromsg-bin/getloginqrcode",
                "request.body": "device=linux",
                "response.body": "qrcode-bytes"
            })),
        };
        let event = source.entry_to_event(&entry).expect("qr event");
        assert_eq!(
            event.url,
            "https://login.weixin.qq.com/cgi-bin/micromsg-bin/getloginqrcode"
        );
        assert_eq!(
            event.response_body_base64,
            Some(STANDARD.encode("qrcode-bytes"))
        );
    }

    #[test]
    fn event_matches_nested_attributes() {
        let source = QrSource::new("http://127.0.0.1:15000".to_string(), vec!["uuid".into()]);
        let entry = LogEntry {
            id: "log-2".to_string(),
            completed_at: "2026-07-09T00:00:00Z".to_string(),
            http_status: None,
            attributes: Some(json!({
                "http": { "method": "GET", "host": "weixin.qq.com", "path": "/login" },
                "response": { "body": "uuid=abc" }
            })),
        };
        let event = source.entry_to_event(&entry).expect("qr event");
        assert_eq!(event.response_status, None);
        assert_eq!(event.matched_by, vec!["uuid"]);
    }

    #[test]
    fn probe_qrcode_request_without_wechat_signal_is_ignored() {
        let source = QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]);
        let entry = LogEntry {
            id: "log-probe".to_string(),
            completed_at: "2026-07-09T00:00:00Z".to_string(),
            http_status: Some(200),
            attributes: Some(json!({
                "http.method": "POST",
                "http.host": "httpbingo.org",
                "http.path": "/anything/proxychains-qrcode",
                "request.body": "{\"probe\":\"qrcode\"}",
                "response.body": "{\"url\":\"https://httpbingo.org/anything/proxychains-qrcode\"}"
            })),
        };

        assert!(source.entry_to_event(&entry).is_none());
    }

    #[test]
    fn login_qrcode_projects_data_uri_image() {
        let image_base64 = STANDARD.encode(b"<svg xmlns=\"http://www.w3.org/2000/svg\"/>");
        let event = QrEvent {
            id: "log-3".to_string(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: "2026-07-09T00:00:00Z".to_string(),
            method: "GET".to_string(),
            url: "https://login.weixin.qq.com/qrcode".to_string(),
            host: "login.weixin.qq.com".to_string(),
            path: "/qrcode".to_string(),
            request_body_base64: None,
            response_status: Some(200),
            response_body_base64: None,
            response_text: Some(format!("data:image/svg+xml;base64,{image_base64}")),
            matched_by: vec!["qrcode".to_string()],
        };

        let qrcode = event.to_login_qrcode();

        assert_eq!(qrcode.image_content_type, Some("image/svg+xml".to_string()));
        assert_eq!(qrcode.image_base64, Some(image_base64));
        assert_eq!(qrcode.status, "captured");
    }

    #[test]
    fn login_qrcode_projects_binary_image_from_base64_body() {
        let png = b"\x89PNG\r\n\x1a\nminimal";
        let event = QrEvent {
            id: "log-4".to_string(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: "2026-07-09T00:00:00Z".to_string(),
            method: "GET".to_string(),
            url: "https://login.weixin.qq.com/qrcode".to_string(),
            host: "login.weixin.qq.com".to_string(),
            path: "/qrcode".to_string(),
            request_body_base64: None,
            response_status: Some(200),
            response_body_base64: Some(STANDARD.encode(png)),
            response_text: None,
            matched_by: vec!["qrcode".to_string()],
        };

        let qrcode = event.to_login_qrcode();

        assert_eq!(qrcode.image_content_type, Some("image/png".to_string()));
        assert_eq!(qrcode.image_base64, Some(STANDARD.encode(png)));
        assert_eq!(
            qrcode.image_data_uri,
            Some(format!("data:image/png;base64,{}", STANDARD.encode(png)))
        );
    }

    #[test]
    fn login_qrcode_extracts_login_url_from_text_body() {
        let event = QrEvent {
            id: "log-5".to_string(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: "2026-07-09T00:00:00Z".to_string(),
            method: "POST".to_string(),
            url: "https://login.weixin.qq.com/cgi-bin/micromsg-bin/getloginqrcode".to_string(),
            host: "login.weixin.qq.com".to_string(),
            path: "/cgi-bin/micromsg-bin/getloginqrcode".to_string(),
            request_body_base64: None,
            response_status: Some(200),
            response_body_base64: None,
            response_text: Some("{\"qr\":\"https://login.weixin.qq.com/l/abc123\"}".to_string()),
            matched_by: vec!["qrcode".to_string()],
        };

        let qrcode = event.to_login_qrcode();

        assert_eq!(
            qrcode.login_url,
            Some("https://login.weixin.qq.com/l/abc123".to_string())
        );
        assert_eq!(qrcode.response_text, event.response_text);
    }

    #[test]
    fn login_qrcode_prefers_wechat_login_url() {
        let event = QrEvent {
            id: "log-6".to_string(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: "2026-07-09T00:00:00Z".to_string(),
            method: "POST".to_string(),
            url: "https://httpbingo.org/anything/getloginqrcode".to_string(),
            host: "httpbingo.org".to_string(),
            path: "/anything/getloginqrcode".to_string(),
            request_body_base64: None,
            response_status: Some(200),
            response_body_base64: None,
            response_text: Some(
                json!({
                    "url": "https://httpbingo.org/anything/getloginqrcode",
                    "data": "{\"qr\":\"https://login.weixin.qq.com/l/webox-smoke\"}",
                    "json": {
                        "qr": "https://login.weixin.qq.com/l/webox-smoke"
                    }
                })
                .to_string(),
            ),
            matched_by: vec!["qrcode".to_string()],
        };

        assert_eq!(
            event.to_login_qrcode().login_url,
            Some("https://login.weixin.qq.com/l/webox-smoke".to_string())
        );
    }

    #[test]
    fn access_log_source_reads_agentgateway_json_lines() {
        let path = std::env::temp_dir().join(format!(
            "webox-agentgateway-log-{}.log",
            uuid::Uuid::new_v4()
        ));
        let request_body = "device=linux";
        let response_body = "{\"qr\":\"https://login.weixin.qq.com/l/from-log\"}";
        fs::write(
            &path,
            format!(
                "{}\n{}\n",
                json!({
                    "level": "info",
                    "time": "2026-07-09T00:00:00Z",
                    "scope": "agentgateway::app",
                    "message": "not a request"
                }),
                json!({
                    "level": "info",
                    "time": "2026-07-09T00:00:01Z",
                    "scope": "request",
                    "http.method": "POST",
                    "http.host": "login.weixin.qq.com",
                    "http.path": "/cgi-bin/micromsg-bin/getloginqrcode",
                    "http.status": 200,
                    "request.body": STANDARD.encode(request_body),
                    "response.body": STANDARD.encode(response_body)
                })
            ),
        )
        .unwrap();

        let source = QrSource::new(
            "http://127.0.0.1:15000".to_string(),
            vec!["getloginqrcode".into()],
        )
        .with_log_path(Some(path.clone()));
        let events = source.search_access_log_newest(5).unwrap();

        fs::remove_file(path).unwrap();
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].method, "POST");
        assert_eq!(events[0].response_status, Some(200));
        assert_eq!(
            events[0].request_body_base64,
            Some(STANDARD.encode(request_body))
        );
        assert_eq!(events[0].response_text.as_deref(), Some(response_body));
        assert_eq!(
            events[0].to_login_qrcode().login_url,
            Some("https://login.weixin.qq.com/l/from-log".to_string())
        );
    }
}
