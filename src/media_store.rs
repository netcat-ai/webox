use aes::cipher::{generic_array::GenericArray, BlockDecrypt, KeyInit};
use aes::Aes128;
use anyhow::{anyhow, bail, Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::fs;
use std::path::PathBuf;
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;

pub const MAX_MEDIA_UPLOAD_BYTES: usize = 256 * 1024 * 1024;

#[derive(Clone, Debug)]
pub struct MediaStore {
    root: PathBuf,
}

#[derive(Debug, Deserialize)]
pub struct GetUploadUrlRequest {
    pub filekey: String,
    pub media_type: i64,
    pub to_user_id: String,
    pub rawsize: u64,
    pub rawfilemd5: String,
    pub filesize: u64,
    #[serde(default)]
    pub thumb_rawsize: Option<u64>,
    #[serde(default)]
    pub thumb_rawfilemd5: Option<String>,
    #[serde(default)]
    pub thumb_filesize: Option<u64>,
    #[serde(default)]
    pub no_need_thumb: Option<bool>,
    #[serde(default)]
    pub aeskey: Option<String>,
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Serialize, Deserialize)]
struct PendingUpload {
    v: u8,
    filekey: String,
    media_type: i64,
    to_user_id: String,
    rawsize: u64,
    rawfilemd5: String,
    filesize: u64,
    aeskey: Option<String>,
    created_at: i64,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct StoredMedia {
    v: u8,
    pub token: String,
    pub filekey: String,
    pub media_type: i64,
    pub to_user_id: String,
    pub rawsize: u64,
    pub rawfilemd5: String,
    pub filesize: u64,
    pub aeskey: Option<String>,
    pub created_at: i64,
}

#[derive(Clone, Debug)]
pub struct PreparedUpload {
    pub upload_param: String,
    pub filekey: String,
}

#[derive(Clone, Debug)]
pub enum MediaKind {
    Image,
    Voice,
    File,
    Video,
}

#[derive(Clone, Debug)]
pub struct PlainMedia {
    pub filename: String,
    pub data: Vec<u8>,
}

impl MediaStore {
    pub fn new(root: PathBuf) -> Self {
        Self { root }
    }

    pub fn prepare_upload(&self, request: &GetUploadUrlRequest) -> Result<PreparedUpload> {
        validate_upload_request(request)?;
        self.ensure_dirs()?;
        let upload_param = Uuid::new_v4().simple().to_string();
        let pending = PendingUpload {
            v: 1,
            filekey: request.filekey.trim().to_string(),
            media_type: request.media_type,
            to_user_id: request.to_user_id.trim().to_string(),
            rawsize: request.rawsize,
            rawfilemd5: request.rawfilemd5.trim().to_string(),
            filesize: request.filesize,
            aeskey: request
                .aeskey
                .as_ref()
                .map(|value| value.trim().to_string()),
            created_at: now(),
        };
        write_json(&self.pending_path(&upload_param), &pending)?;
        Ok(PreparedUpload {
            upload_param,
            filekey: pending.filekey,
        })
    }

    pub fn store_upload(
        &self,
        upload_param: &str,
        filekey: &str,
        encrypted: &[u8],
    ) -> Result<StoredMedia> {
        if encrypted.is_empty() || encrypted.len() > MAX_MEDIA_UPLOAD_BYTES {
            bail!("uploaded media is empty or too large");
        }
        let upload_param = safe_token(upload_param)?;
        let pending_path = self.pending_path(&upload_param);
        let pending: PendingUpload = read_json(&pending_path)
            .with_context(|| format!("read pending upload {}", pending_path.display()))?;
        if pending.filekey != filekey.trim() {
            bail!("filekey mismatch");
        }
        if pending.filesize != encrypted.len() as u64 {
            bail!("encrypted filesize mismatch");
        }
        let token = Uuid::new_v4().simple().to_string();
        let stored = StoredMedia {
            v: 1,
            token: token.clone(),
            filekey: pending.filekey,
            media_type: pending.media_type,
            to_user_id: pending.to_user_id,
            rawsize: pending.rawsize,
            rawfilemd5: pending.rawfilemd5,
            filesize: pending.filesize,
            aeskey: pending.aeskey,
            created_at: now(),
        };
        fs::write(self.object_bin_path(&token), encrypted)?;
        write_json(&self.object_meta_path(&token), &stored)?;
        let _ = fs::remove_file(pending_path);
        Ok(stored)
    }

    pub fn read_encrypted(&self, token: &str) -> Result<(StoredMedia, Vec<u8>)> {
        let token = safe_token(token)?;
        let meta: StoredMedia = read_json(&self.object_meta_path(&token))?;
        let data = fs::read(self.object_bin_path(&token))?;
        Ok((meta, data))
    }

    pub fn read_plain_media(
        &self,
        media: &Value,
        kind: MediaKind,
        filename_hint: Option<&str>,
    ) -> Result<PlainMedia> {
        let token = media
            .get("encrypt_query_param")
            .and_then(Value::as_str)
            .ok_or_else(|| anyhow!("media.encrypt_query_param is required"))?;
        let key_source = media
            .get("aes_key")
            .and_then(Value::as_str)
            .or_else(|| media.get("aeskey").and_then(Value::as_str))
            .ok_or_else(|| anyhow!("media.aes_key is required"))?;
        let (stored, encrypted) = self.read_encrypted(token)?;
        let key = decode_aes_key(key_source)?;
        let data = aes_ecb_decrypt_pkcs7(&key, &encrypted)?;
        if stored.rawsize != 0 && stored.rawsize != data.len() as u64 {
            bail!("raw media size mismatch");
        }
        validate_plain_md5(&stored.rawfilemd5, &data)?;
        Ok(PlainMedia {
            filename: media_filename(filename_hint, &data, &kind, token),
            data,
        })
    }

    fn ensure_dirs(&self) -> Result<()> {
        fs::create_dir_all(self.pending_dir())?;
        fs::create_dir_all(self.object_dir())?;
        Ok(())
    }

    fn pending_dir(&self) -> PathBuf {
        self.root.join("pending")
    }

    fn object_dir(&self) -> PathBuf {
        self.root.join("objects")
    }

    fn pending_path(&self, token: &str) -> PathBuf {
        self.pending_dir().join(format!("{token}.json"))
    }

    fn object_meta_path(&self, token: &str) -> PathBuf {
        self.object_dir().join(format!("{token}.json"))
    }

    fn object_bin_path(&self, token: &str) -> PathBuf {
        self.object_dir().join(format!("{token}.bin"))
    }
}

fn validate_upload_request(request: &GetUploadUrlRequest) -> Result<()> {
    if request.filekey.trim().is_empty() {
        bail!("filekey is required");
    }
    if !(1..=4).contains(&request.media_type) {
        bail!("media_type must be 1, 2, 3, or 4");
    }
    if request.to_user_id.trim().is_empty() {
        bail!("to_user_id is required");
    }
    if request.rawsize == 0 || request.filesize == 0 {
        bail!("rawsize and filesize must be positive");
    }
    if request.filesize as usize > MAX_MEDIA_UPLOAD_BYTES {
        bail!("filesize exceeds upload limit");
    }
    if request.rawfilemd5.trim().is_empty() {
        bail!("rawfilemd5 is required");
    }
    if let Some(thumb_filesize) = request.thumb_filesize {
        if thumb_filesize as usize > MAX_MEDIA_UPLOAD_BYTES {
            bail!("thumb_filesize exceeds upload limit");
        }
    }
    if request.thumb_rawsize.is_some() || request.thumb_filesize.is_some() {
        let Some(md5) = request.thumb_rawfilemd5.as_deref() else {
            bail!("thumb_rawfilemd5 is required when thumb sizes are provided");
        };
        if md5.trim().is_empty() {
            bail!("thumb_rawfilemd5 is required when thumb sizes are provided");
        }
    }
    let _no_need_thumb = request.no_need_thumb.unwrap_or(false);
    Ok(())
}

fn safe_token(raw: &str) -> Result<String> {
    let value = raw.trim();
    if value.is_empty()
        || value.len() > 128
        || !value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-' || byte == b'_')
    {
        bail!("invalid media token");
    }
    Ok(value.to_string())
}

fn read_json<T: for<'de> Deserialize<'de>>(path: &PathBuf) -> Result<T> {
    let data = fs::read_to_string(path)?;
    Ok(serde_json::from_str(&data)?)
}

fn write_json<T: Serialize>(path: &PathBuf, value: &T) -> Result<()> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent)?;
    }
    let tmp = path.with_extension("tmp");
    fs::write(&tmp, serde_json::to_vec_pretty(value)?)?;
    fs::rename(tmp, path)?;
    Ok(())
}

fn decode_aes_key(raw: &str) -> Result<[u8; 16]> {
    let value = raw.trim();
    if value.len() == 32 && value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return decode_hex_16(value);
    }
    let decoded = STANDARD
        .decode(value)
        .map_err(|err| anyhow!("decode aes_key base64: {err}"))?;
    if decoded.len() == 16 {
        let mut out = [0_u8; 16];
        out.copy_from_slice(&decoded);
        return Ok(out);
    }
    if decoded.len() == 32 && decoded.iter().all(|byte| byte.is_ascii_hexdigit()) {
        let hex = std::str::from_utf8(&decoded)?;
        return decode_hex_16(hex);
    }
    bail!("decoded aes_key has unexpected length");
}

fn decode_hex_16(value: &str) -> Result<[u8; 16]> {
    let mut out = [0_u8; 16];
    for (idx, byte) in out.iter_mut().enumerate() {
        let hi = hex_nibble(value.as_bytes()[idx * 2])?;
        let lo = hex_nibble(value.as_bytes()[idx * 2 + 1])?;
        *byte = (hi << 4) | lo;
    }
    Ok(out)
}

fn hex_nibble(byte: u8) -> Result<u8> {
    match byte {
        b'0'..=b'9' => Ok(byte - b'0'),
        b'a'..=b'f' => Ok(byte - b'a' + 10),
        b'A'..=b'F' => Ok(byte - b'A' + 10),
        _ => bail!("invalid hex character"),
    }
}

fn aes_ecb_decrypt_pkcs7(key: &[u8; 16], cipher: &[u8]) -> Result<Vec<u8>> {
    if cipher.is_empty() || !cipher.len().is_multiple_of(16) {
        bail!("invalid AES-ECB ciphertext length");
    }
    let aes = Aes128::new(GenericArray::from_slice(key));
    let mut out = Vec::with_capacity(cipher.len());
    for chunk in cipher.chunks_exact(16) {
        let mut block = GenericArray::clone_from_slice(chunk);
        aes.decrypt_block(&mut block);
        out.extend_from_slice(&block);
    }
    let pad = *out.last().ok_or_else(|| anyhow!("empty AES plaintext"))? as usize;
    if pad == 0 || pad > 16 || pad > out.len() {
        bail!("invalid AES PKCS7 padding length");
    }
    if out[out.len() - pad..]
        .iter()
        .any(|byte| *byte as usize != pad)
    {
        bail!("invalid AES PKCS7 padding bytes");
    }
    out.truncate(out.len() - pad);
    Ok(out)
}

fn validate_plain_md5(expected: &str, data: &[u8]) -> Result<()> {
    let expected = expected.trim();
    if expected.is_empty() {
        bail!("rawfilemd5 is required");
    }
    let actual = format!("{:x}", md5::compute(data));
    if !actual.eq_ignore_ascii_case(expected) {
        bail!("raw media md5 mismatch");
    }
    Ok(())
}

fn media_filename(
    filename_hint: Option<&str>,
    data: &[u8],
    kind: &MediaKind,
    fallback_token: &str,
) -> String {
    if let Some(name) = filename_hint
        .and_then(non_empty)
        .map(|value| safe_media_file_name(&value))
        .filter(|value| !value.is_empty())
    {
        return name;
    }
    let ext = match detect_content_type(data).as_str() {
        "image/jpeg" => "jpg",
        "image/png" => "png",
        "image/gif" => "gif",
        "image/webp" => "webp",
        "video/mp4" => "mp4",
        _ => match kind {
            MediaKind::Image => "jpg",
            MediaKind::Voice => "silk",
            MediaKind::File => "bin",
            MediaKind::Video => "mp4",
        },
    };
    format!("webox-{fallback_token}.{ext}")
}

fn safe_media_file_name(raw: &str) -> String {
    raw.rsplit(['/', '\\'])
        .next()
        .unwrap_or(raw)
        .split(['?', '#'])
        .next()
        .unwrap_or("")
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '.' | '-' | '_') {
                ch
            } else {
                '_'
            }
        })
        .take(180)
        .collect::<String>()
        .trim_matches('.')
        .to_string()
}

fn detect_content_type(data: &[u8]) -> String {
    if data.starts_with(&[0xFF, 0xD8, 0xFF]) {
        return "image/jpeg".to_string();
    }
    if data.starts_with(b"\x89PNG\r\n\x1A\n") {
        return "image/png".to_string();
    }
    if data.starts_with(b"GIF87a") || data.starts_with(b"GIF89a") {
        return "image/gif".to_string();
    }
    if data.len() >= 12 && &data[0..4] == b"RIFF" && &data[8..12] == b"WEBP" {
        return "image/webp".to_string();
    }
    if data.len() >= 12 && &data[4..8] == b"ftyp" {
        return "video/mp4".to_string();
    }
    "application/octet-stream".to_string()
}

fn non_empty(value: &str) -> Option<String> {
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_string())
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|value| value.as_secs() as i64)
        .unwrap_or_default()
}

#[cfg(test)]
mod tests {
    use super::*;
    use aes::cipher::BlockEncrypt;
    use serde_json::json;

    #[test]
    fn upload_round_trip_keeps_encrypted_bytes() {
        let root = std::env::temp_dir().join(format!("webox-media-{}", Uuid::new_v4()));
        let store = MediaStore::new(root.clone());
        let request = upload_request(16);
        let prepared = store.prepare_upload(&request).unwrap();
        let encrypted = vec![7_u8; 16];

        let stored = store
            .store_upload(&prepared.upload_param, &prepared.filekey, &encrypted)
            .unwrap();
        let (meta, data) = store.read_encrypted(&stored.token).unwrap();

        assert_eq!(meta.filekey, request.filekey);
        assert_eq!(data, encrypted);
        fs::remove_dir_all(root).ok();
    }

    #[test]
    fn read_plain_media_decrypts_sdk_media_reference() {
        let root = std::env::temp_dir().join(format!("webox-media-{}", Uuid::new_v4()));
        let store = MediaStore::new(root.clone());
        let key = [0x11_u8; 16];
        let plaintext = b"hello image".to_vec();
        let encrypted = encrypt_aes_ecb_pkcs7(&key, &plaintext);
        let mut request = upload_request(encrypted.len() as u64);
        request.rawsize = plaintext.len() as u64;
        request.rawfilemd5 = format!("{:x}", md5::compute(&plaintext));
        let prepared = store.prepare_upload(&request).unwrap();
        let stored = store
            .store_upload(&prepared.upload_param, &prepared.filekey, &encrypted)
            .unwrap();
        let media = json!({
            "encrypt_query_param": stored.token,
            "aes_key": STANDARD.encode("11111111111111111111111111111111"),
        });

        let plain = store
            .read_plain_media(&media, MediaKind::Image, Some("a/b/c.png"))
            .unwrap();

        assert_eq!(plain.data, plaintext);
        assert_eq!(plain.filename, "c.png");
        fs::remove_dir_all(root).ok();
    }

    #[test]
    fn read_plain_media_rejects_md5_mismatch() {
        let root = std::env::temp_dir().join(format!("webox-media-{}", Uuid::new_v4()));
        let store = MediaStore::new(root.clone());
        let key = [0x11_u8; 16];
        let plaintext = b"hello image".to_vec();
        let encrypted = encrypt_aes_ecb_pkcs7(&key, &plaintext);
        let mut request = upload_request(encrypted.len() as u64);
        request.rawsize = plaintext.len() as u64;
        request.rawfilemd5 = "00000000000000000000000000000000".to_string();
        let prepared = store.prepare_upload(&request).unwrap();
        let stored = store
            .store_upload(&prepared.upload_param, &prepared.filekey, &encrypted)
            .unwrap();
        let media = json!({
            "encrypt_query_param": stored.token,
            "aes_key": STANDARD.encode("11111111111111111111111111111111"),
        });

        let err = store
            .read_plain_media(&media, MediaKind::Image, None)
            .unwrap_err()
            .to_string();

        assert!(err.contains("raw media md5 mismatch"));
        fs::remove_dir_all(root).ok();
    }

    fn upload_request(filesize: u64) -> GetUploadUrlRequest {
        GetUploadUrlRequest {
            filekey: "filekey123".to_string(),
            media_type: 1,
            to_user_id: "alice".to_string(),
            rawsize: 11,
            rawfilemd5: "5eb63bbbe01eeed093cb22bb8f5acdc3".to_string(),
            filesize,
            thumb_rawsize: None,
            thumb_rawfilemd5: None,
            thumb_filesize: None,
            no_need_thumb: Some(true),
            aeskey: Some("11111111111111111111111111111111".to_string()),
            base_info: None,
        }
    }

    fn encrypt_aes_ecb_pkcs7(key: &[u8; 16], data: &[u8]) -> Vec<u8> {
        let pad = 16 - (data.len() % 16);
        let mut plain = data.to_vec();
        plain.extend(std::iter::repeat_n(pad as u8, pad));
        let aes = Aes128::new(GenericArray::from_slice(key));
        let mut out = Vec::new();
        for chunk in plain.chunks_exact(16) {
            let mut block = GenericArray::clone_from_slice(chunk);
            aes.encrypt_block(&mut block);
            out.extend_from_slice(&block);
        }
        out
    }
}
