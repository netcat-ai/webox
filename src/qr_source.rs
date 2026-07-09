use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Clone)]
pub struct QrSource {
    base_url: String,
    client: reqwest::Client,
    match_terms: Vec<String>,
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
        }
    }

    pub async fn latest(&self) -> Result<Option<QrEvent>> {
        Ok(self.search_newest(1).await?.into_iter().next())
    }

    pub async fn recent(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let mut events = self.search_newest(limit).await?;
        events.reverse();
        Ok(events)
    }

    async fn search_newest(&self, limit: usize) -> Result<Vec<QrEvent>> {
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
            let Some(candidate) = self.entry_to_event(&entry) else {
                continue;
            };
            let event = match self.get_log(&entry.id).await {
                Ok(Some(full)) => self.entry_to_event(&full).unwrap_or(candidate),
                Ok(None) | Err(_) => candidate,
            };
            out.push(event);
            if out.len() >= wanted {
                break;
            }
        }
        Ok(out)
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
        let response_status = entry
            .http_status
            .or_else(|| attr_i64(attributes, "http.status"));
        let url = build_url(&host, &path);
        let haystack = [
            method.as_str(),
            host.as_str(),
            path.as_str(),
            url.as_str(),
            request_body.as_deref().unwrap_or_default(),
            response_body.as_deref().unwrap_or_default(),
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
            request_body_base64: request_body.map(|body| STANDARD.encode(body.as_bytes())),
            response_status,
            response_body_base64: response_body
                .as_ref()
                .map(|body| STANDARD.encode(body.as_bytes())),
            response_text: response_body,
            matched_by,
        })
    }
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
            login_url: self.response_text.as_deref().and_then(first_url),
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

fn ensure_success(status: StatusCode) -> Result<()> {
    if status.is_success() {
        return Ok(());
    }
    anyhow::bail!("agentgateway api returned {status}")
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

fn first_url(body: &str) -> Option<String> {
    let http = body.find("http://");
    let https = body.find("https://");
    let start = match (http, https) {
        (Some(http), Some(https)) => http.min(https),
        (Some(http), None) => http,
        (None, Some(https)) => https,
        (None, None) => return None,
    };
    let rest = &body[start..];
    let end = rest
        .find(|ch: char| {
            ch.is_whitespace() || matches!(ch, '"' | '\'' | '<' | '>' | ')' | ']' | '}')
        })
        .unwrap_or(rest.len());
    let value = rest[..end].trim_end_matches([',', '.', ';', ':']);
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
    use serde_json::json;

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
}
