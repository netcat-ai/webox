use anyhow::{Context, Result};
use quircs::{Code, Quirc};
use std::path::{Path, PathBuf};

const XWD_FILE_VERSION: u32 = 7;
const XWD_Z_PIXMAP: u32 = 2;
const XWD_MSB_FIRST: u32 = 0;
const XWD_LSB_FIRST: u32 = 1;
const MIN_SCREEN_QR_BLUE_PIXELS: usize = 1_500;
const MIN_SCREEN_QR_SIDE: i32 = 80;

#[derive(Clone)]
pub struct QrSource {
    screenshot_path: Option<PathBuf>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct LoginQrCode {
    pub id: String,
    pub login_url: String,
}

impl QrSource {
    pub fn new(screenshot_path: Option<PathBuf>) -> Self {
        Self { screenshot_path }
    }

    pub async fn latest(&self) -> Result<Option<LoginQrCode>> {
        let Some(path) = self.screenshot_path.clone() else {
            return Ok(None);
        };
        tokio::task::spawn_blocking(move || latest_from_path(&path))
            .await
            .context("join qrcode capture task")?
    }
}

fn latest_from_path(path: &Path) -> Result<Option<LoginQrCode>> {
    let Some(screen) = read_xwd_screen(path)? else {
        return Ok(None);
    };
    if !looks_like_wechat_login_qr_screen(&screen) {
        return Ok(None);
    }
    let Some(qr) = extract_screen_qr(&screen) else {
        return Ok(None);
    };
    let digest = md5::compute(qr.payload.as_bytes());
    Ok(Some(LoginQrCode {
        id: format!("xvfb-qr-{digest:x}"),
        login_url: qr.payload,
    }))
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

#[derive(Debug, PartialEq)]
struct ScreenQr {
    width: u32,
    height: u32,
    rgb: Vec<u8>,
    payload: String,
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
    screen
        .rgb
        .chunks_exact(3)
        .filter(|pixel| is_wechat_qr_blue(pixel[0], pixel[1], pixel[2]))
        .count()
        >= MIN_SCREEN_QR_BLUE_PIXELS
}

fn is_wechat_qr_blue(red: u8, green: u8, blue: u8) -> bool {
    blue > 145
        && red < 130
        && green < 150
        && blue.saturating_sub(red) > 45
        && blue.saturating_sub(green) > 35
}

fn extract_screen_qr(screen: &RgbScreen) -> Option<ScreenQr> {
    let grayscale = screen
        .rgb
        .chunks_exact(3)
        .map(|pixel| {
            ((u32::from(pixel[0]) * 299
                + u32::from(pixel[1]) * 587
                + u32::from(pixel[2]) * 114
                + 500)
                / 1000) as u8
        })
        .collect::<Vec<_>>();
    let mut decoder = Quirc::default();
    let (code, payload) = decoder
        .identify(screen.width as usize, screen.height as usize, &grayscale)
        .filter_map(Result::ok)
        .filter_map(|code| {
            let bounds = qr_bounds(&code)?;
            let payload = code.decode().ok()?;
            let payload = String::from_utf8(payload.payload).ok()?;
            let area = (bounds.2 - bounds.0) * (bounds.3 - bounds.1);
            Some((code, payload, area))
        })
        .max_by_key(|(_, _, area)| *area)
        .map(|(code, payload, _)| (code, payload))?;
    let (left, top, right, bottom) = qr_bounds(&code)?;
    let module = ((right - left).max(bottom - top) / code.size.max(1)).max(1);
    let margin = module * 4;
    let left = (left - margin).max(0) as u32;
    let top = (top - margin).max(0) as u32;
    let right = (right + margin).min(screen.width as i32 - 1) as u32;
    let bottom = (bottom + margin).min(screen.height as i32 - 1) as u32;
    let width = right - left + 1;
    let height = bottom - top + 1;
    let mut rgb = Vec::with_capacity(width as usize * height as usize * 3);
    for y in top..=bottom {
        let row_start = ((y * screen.width + left) * 3) as usize;
        let row_end = row_start + width as usize * 3;
        rgb.extend_from_slice(&screen.rgb[row_start..row_end]);
    }
    Some(ScreenQr {
        width,
        height,
        rgb,
        payload,
    })
}

fn qr_bounds(code: &Code) -> Option<(i32, i32, i32, i32)> {
    let left = code.corners.iter().map(|point| point.x).min()?;
    let top = code.corners.iter().map(|point| point.y).min()?;
    let right = code.corners.iter().map(|point| point.x).max()?;
    let bottom = code.corners.iter().map(|point| point.y).max()?;
    ((right - left) >= MIN_SCREEN_QR_SIDE && (bottom - top) >= MIN_SCREEN_QR_SIDE)
        .then_some((left, top, right, bottom))
}

#[cfg(test)]
mod tests {
    use super::*;
    use qrcode::types::Color;
    use qrcode::QrCode;
    use std::fs;

    #[tokio::test]
    async fn reads_and_decodes_wechat_qr_from_xvfb() {
        let screen_path =
            std::env::temp_dir().join(format!("webox-qr-screen-{}.xwd", uuid::Uuid::new_v4()));
        fs::write(&screen_path, xwd_fixture_with_blue_qr()).unwrap();

        let qrcode = QrSource::new(Some(screen_path.clone()))
            .latest()
            .await
            .unwrap()
            .expect("qr code");

        assert!(qrcode.id.starts_with("xvfb-qr-"));
        assert_eq!(
            qrcode.login_url,
            "https://login.weixin.qq.com/l/screen-test"
        );
        fs::remove_file(screen_path).unwrap();
    }

    #[test]
    fn screen_without_wechat_qr_is_ignored() {
        let screen = parse_xwd_screen(&xwd_fixture(80, 80, |_, _| [255, 255, 255])).unwrap();
        assert!(!looks_like_wechat_login_qr_screen(&screen));
        assert!(extract_screen_qr(&screen).is_none());
    }

    #[tokio::test]
    async fn missing_framebuffer_is_not_an_error() {
        let path = std::env::temp_dir().join(format!("missing-{}.xwd", uuid::Uuid::new_v4()));
        assert!(QrSource::new(Some(path)).latest().await.unwrap().is_none());
    }

    fn xwd_fixture_with_blue_qr() -> Vec<u8> {
        let code = QrCode::new(b"https://login.weixin.qq.com/l/screen-test").unwrap();
        let scale = 5;
        let offset_x = 80;
        let offset_y = 40;
        xwd_fixture(320, 240, |x, y| {
            let qr_x = x.checked_sub(offset_x).map(|value| value / scale);
            let qr_y = y.checked_sub(offset_y).map(|value| value / scale);
            match (qr_x, qr_y) {
                (Some(qr_x), Some(qr_y))
                    if qr_x < code.width() as u32
                        && qr_y < code.width() as u32
                        && code[(qr_x as usize, qr_y as usize)] == Color::Dark =>
                {
                    [45, 65, 255]
                }
                _ => [255, 255, 255],
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
}
