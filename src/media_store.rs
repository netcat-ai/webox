use aes::cipher::{generic_array::GenericArray, BlockEncrypt, KeyInit};
use aes::Aes128;
use anyhow::{anyhow, bail, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use std::fmt::Write as _;
use std::fs;
use std::path::PathBuf;
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{SystemTime, UNIX_EPOCH};
use uuid::Uuid;

const MAX_MEDIA_PLAIN_BYTES: usize = 256 * 1024 * 1024;
const MEDIA_TTL_SECONDS: i64 = 24 * 60 * 60;
const MAX_MEDIA_STORE_BYTES: u64 = 1024 * 1024 * 1024;

#[derive(Clone, Debug)]
pub struct MediaStore {
    root: PathBuf,
    mutation_lock: Arc<Mutex<()>>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
pub struct StoredMedia {
    v: u8,
    token: String,
    rawsize: u64,
    filesize: u64,
    aeskey: String,
    content_sha256: String,
    created_at: i64,
}

#[derive(Clone, Debug)]
pub struct PublishedMedia {
    pub token: String,
    pub aes_key: String,
    pub encrypted_size: usize,
    pub filename: String,
    pub content_type: String,
}

impl MediaStore {
    pub fn new(root: PathBuf) -> Self {
        Self {
            root,
            mutation_lock: Arc::new(Mutex::new(())),
        }
    }

    pub fn publish_plain(&self, media: &crate::wechat_db::MediaFile) -> Result<PublishedMedia> {
        if media.data.is_empty() || media.data.len() > MAX_MEDIA_PLAIN_BYTES {
            bail!("local media is empty or too large");
        }
        let _guard = self.acquire_lock()?;
        self.ensure_dirs()?;
        self.cleanup()?;
        let content_sha256 = sha256_hex(&media.data);
        if let Some(published) = self.find_published(&content_sha256, media)? {
            return Ok(published);
        }

        let token = Uuid::new_v4().simple().to_string();
        let key = *Uuid::new_v4().as_bytes();
        let encrypted = aes_ecb_encrypt_pkcs7(&key, &media.data);
        self.ensure_capacity(encrypted.len() as u64)?;
        let stored = StoredMedia {
            v: 1,
            token: token.clone(),
            rawsize: media.data.len() as u64,
            filesize: encrypted.len() as u64,
            aeskey: STANDARD.encode(key),
            content_sha256,
            created_at: now(),
        };
        let bin_path = self.object_bin_path(&token);
        let bin_tmp = bin_path.with_extension("bin.tmp");
        fs::write(&bin_tmp, &encrypted)?;
        fs::rename(bin_tmp, &bin_path)?;
        write_json(&self.object_meta_path(&token), &stored)?;
        Ok(published_view(stored, media))
    }

    pub fn read_encrypted(&self, token: &str) -> Result<Vec<u8>> {
        let token = safe_token(token)?;
        let _guard = self.acquire_lock()?;
        let meta: StoredMedia = read_json(&self.object_meta_path(&token))?;
        if meta.token != token {
            bail!("media token mismatch");
        }
        if now().saturating_sub(meta.created_at) > MEDIA_TTL_SECONDS {
            self.remove_object(&token);
            bail!("media object has expired");
        }
        let data = fs::read(self.object_bin_path(&token))?;
        if data.len() as u64 != meta.filesize {
            self.remove_object(&token);
            bail!("encrypted media size mismatch");
        }
        Ok(data)
    }

    fn acquire_lock(&self) -> Result<MutexGuard<'_, ()>> {
        self.mutation_lock
            .lock()
            .map_err(|_| anyhow!("media store lock is poisoned"))
    }

    fn ensure_dirs(&self) -> Result<()> {
        fs::create_dir_all(self.object_dir())?;
        let legacy_pending = self.root.join("pending");
        if legacy_pending.is_dir() {
            fs::remove_dir_all(legacy_pending)?;
        }
        Ok(())
    }

    fn find_published(
        &self,
        content_sha256: &str,
        media: &crate::wechat_db::MediaFile,
    ) -> Result<Option<PublishedMedia>> {
        for entry in fs::read_dir(self.object_dir())?.flatten() {
            let path = entry.path();
            if path.extension().and_then(|value| value.to_str()) != Some("json") {
                continue;
            }
            let Ok(stored) = read_json::<StoredMedia>(&path) else {
                continue;
            };
            if stored.content_sha256 != content_sha256
                || stored.rawsize != media.data.len() as u64
                || path.file_stem().and_then(|value| value.to_str()) != Some(&stored.token)
            {
                continue;
            }
            let Ok(metadata) = fs::metadata(self.object_bin_path(&stored.token)) else {
                continue;
            };
            if metadata.len() != stored.filesize || stored.aeskey.is_empty() {
                continue;
            }
            return Ok(Some(published_view(stored, media)));
        }
        Ok(None)
    }

    fn cleanup(&self) -> Result<()> {
        let object_dir = self.object_dir();
        cleanup_json_dir(&object_dir, |token| {
            let _ = fs::remove_file(object_dir.join(format!("{token}.bin")));
        })?;
        for entry in fs::read_dir(&object_dir)?.flatten() {
            let path = entry.path();
            let extension = path.extension().and_then(|value| value.to_str());
            if (extension == Some("bin") && !path.with_extension("json").is_file())
                || extension == Some("tmp")
            {
                let _ = fs::remove_file(path);
            }
        }
        Ok(())
    }

    fn ensure_capacity(&self, incoming: u64) -> Result<()> {
        let used = fs::read_dir(self.object_dir())?
            .flatten()
            .filter(|entry| {
                entry.path().extension().and_then(|value| value.to_str()) == Some("bin")
            })
            .filter_map(|entry| entry.metadata().ok().map(|metadata| metadata.len()))
            .sum::<u64>();
        if used.saturating_add(incoming) > MAX_MEDIA_STORE_BYTES {
            bail!("media store capacity exceeded");
        }
        Ok(())
    }

    fn object_dir(&self) -> PathBuf {
        self.root.join("objects")
    }

    fn object_meta_path(&self, token: &str) -> PathBuf {
        self.object_dir().join(format!("{token}.json"))
    }

    fn object_bin_path(&self, token: &str) -> PathBuf {
        self.object_dir().join(format!("{token}.bin"))
    }

    fn remove_object(&self, token: &str) {
        let _ = fs::remove_file(self.object_bin_path(token));
        let _ = fs::remove_file(self.object_meta_path(token));
    }
}

fn published_view(stored: StoredMedia, media: &crate::wechat_db::MediaFile) -> PublishedMedia {
    PublishedMedia {
        token: stored.token,
        aes_key: stored.aeskey,
        encrypted_size: stored.filesize as usize,
        filename: media.filename.clone(),
        content_type: media.content_type.clone(),
    }
}

fn cleanup_json_dir(dir: &PathBuf, mut remove_related: impl FnMut(&str)) -> Result<()> {
    for entry in fs::read_dir(dir)?.flatten() {
        let path = entry.path();
        if path.extension().and_then(|value| value.to_str()) == Some("tmp") {
            let _ = fs::remove_file(path);
            continue;
        }
        if path.extension().and_then(|value| value.to_str()) != Some("json") {
            continue;
        }
        let expired = read_json::<Value>(&path)
            .ok()
            .and_then(|value| value.get("created_at").and_then(Value::as_i64))
            .map(|created_at| now().saturating_sub(created_at) > MEDIA_TTL_SECONDS)
            .unwrap_or(true);
        if expired {
            if let Some(token) = path.file_stem().and_then(|value| value.to_str()) {
                remove_related(token);
            }
            let _ = fs::remove_file(path);
        }
    }
    Ok(())
}

fn aes_ecb_encrypt_pkcs7(key: &[u8; 16], data: &[u8]) -> Vec<u8> {
    let pad = 16 - (data.len() % 16);
    let mut plain = data.to_vec();
    plain.extend(std::iter::repeat_n(pad as u8, pad));
    let aes = Aes128::new(GenericArray::from_slice(key));
    let mut out = Vec::with_capacity(plain.len());
    for chunk in plain.chunks_exact(16) {
        let mut block = GenericArray::clone_from_slice(chunk);
        aes.encrypt_block(&mut block);
        out.extend_from_slice(&block);
    }
    out
}

fn sha256_hex(data: &[u8]) -> String {
    Sha256::digest(data)
        .iter()
        .fold(String::with_capacity(64), |mut value, byte| {
            write!(&mut value, "{byte:02x}").expect("writing to a string cannot fail");
            value
        })
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
    Ok(serde_json::from_str(&fs::read_to_string(path)?)?)
}

fn write_json<T: Serialize>(path: &PathBuf, value: &T) -> Result<()> {
    let tmp = path.with_extension("tmp");
    fs::write(&tmp, serde_json::to_vec_pretty(value)?)?;
    fs::rename(tmp, path)?;
    Ok(())
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

    #[test]
    fn published_media_is_reused_and_downloadable() {
        let root = test_root();
        let store = MediaStore::new(root.clone());
        let source = source_media();

        let first = store.publish_plain(&source).unwrap();
        let second = store.publish_plain(&source).unwrap();
        let encrypted = store.read_encrypted(&first.token).unwrap();

        assert_eq!(first.token, second.token);
        assert_eq!(encrypted.len(), first.encrypted_size);
        assert_ne!(encrypted, source.data);
        fs::remove_dir_all(root).ok();
    }

    #[test]
    fn concurrent_publish_reuses_one_object() {
        let root = test_root();
        let store = MediaStore::new(root.clone());
        let source = source_media();
        let handles = (0..8)
            .map(|_| {
                let store = store.clone();
                let source = source.clone();
                std::thread::spawn(move || store.publish_plain(&source).unwrap().token)
            })
            .collect::<Vec<_>>();
        let tokens = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect::<Vec<_>>();

        assert!(tokens.iter().all(|token| token == &tokens[0]));
        assert_eq!(fs::read_dir(root.join("objects")).unwrap().count(), 2);
        fs::remove_dir_all(root).ok();
    }

    #[test]
    fn truncated_object_is_rejected_and_removed() {
        let root = test_root();
        let store = MediaStore::new(root.clone());
        let published = store.publish_plain(&source_media()).unwrap();
        fs::write(store.object_bin_path(&published.token), b"broken").unwrap();

        assert!(store.read_encrypted(&published.token).is_err());
        assert!(!store.object_meta_path(&published.token).exists());
        fs::remove_dir_all(root).ok();
    }

    fn source_media() -> crate::wechat_db::MediaFile {
        crate::wechat_db::MediaFile {
            data: b"local wechat media".to_vec(),
            content_type: "application/octet-stream".to_string(),
            filename: "wechat.bin".to_string(),
        }
    }

    fn test_root() -> PathBuf {
        std::env::temp_dir().join(format!("webox-media-test-{}", Uuid::new_v4()))
    }
}
