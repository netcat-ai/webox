use crate::wechat_db;
use anyhow::{anyhow, bail, Context, Result};
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;
use std::time::{SystemTime, UNIX_EPOCH};

const MAX_POLL_LIMIT: usize = 500;

#[derive(Clone, Debug)]
pub struct WechatState {
    state_dir: PathBuf,
    key_file: PathBuf,
}

#[derive(Debug, Serialize, Deserialize)]
struct KeyFile {
    version: u8,
    wxid: String,
    key: String,
    #[serde(rename = "source")]
    source: Option<String>,
    #[serde(rename = "keysFile")]
    keys_file: Option<String>,
    #[serde(rename = "dbDir", skip_serializing_if = "Option::is_none")]
    db_dir: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    keys: Option<HashMap<String, String>>,
    #[serde(rename = "createdAt")]
    created_at: i64,
    #[serde(rename = "updatedAt")]
    updated_at: i64,
}

#[derive(Debug, Serialize, Deserialize)]
struct DbCursor {
    v: u8,
    source: String,
    sessions: HashMap<String, i64>,
}

#[derive(Debug, Serialize, Deserialize)]
struct LegacySpoolCursor {
    v: u8,
    pos: usize,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PollResult {
    pub cursor: String,
    pub messages: Vec<Value>,
    pub meta: Value,
}

impl WechatState {
    pub fn new(state_dir: PathBuf) -> Self {
        Self {
            key_file: state_dir.join("wechat.key"),
            state_dir,
        }
    }

    pub fn ensure_state_dir(&self) -> Result<()> {
        fs::create_dir_all(&self.state_dir)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&self.state_dir, fs::Permissions::from_mode(0o700));
        }
        Ok(())
    }

    pub fn has_key(&self) -> bool {
        self.key_file.exists()
    }

    pub fn current_cursor(&self) -> String {
        let Ok((db_dir, keys)) = self.db_material() else {
            return encode_db_cursor(&HashMap::new());
        };
        match wechat_db::current_session_state(db_dir, keys, self.cache_dir()) {
            Ok(state) => encode_db_cursor(&state),
            Err(_) => encode_db_cursor(&HashMap::new()),
        }
    }

    pub fn poll_messages(&self, cursor: Option<&str>, limit: usize) -> Result<PollResult> {
        let (db_dir, keys) = self.ensure_db_material()?;
        let cursor_state = decode_db_cursor(cursor)?;
        let limit = limit.clamp(1, MAX_POLL_LIMIT);
        let data =
            wechat_db::poll_new_messages(db_dir, keys, cursor_state, limit, self.cache_dir())
                .context("poll wechat local db")?;
        let cursor = encode_db_cursor(&data.new_state);
        Ok(PollResult {
            cursor,
            messages: data.messages,
            meta: json!({
                "source": "wechat_db",
                "newState": data.new_state,
                "raw": data.meta,
            }),
        })
    }

    pub fn resolve_recipient(&self, username: &str) -> Result<wechat_db::Recipient> {
        let (db_dir, keys) = self.ensure_db_material()?;
        wechat_db::resolve_recipient_by_username(db_dir, keys, self.cache_dir(), username)
            .with_context(|| format!("resolve wechat recipient {username}"))?
            .ok_or_else(|| anyhow!("recipient not found: target must be a WeChat internal id"))
    }

    fn ensure_db_material(&self) -> Result<(PathBuf, HashMap<String, String>)> {
        if let Ok(material) = self.db_material() {
            return Ok(material);
        }
        let init = wechat_db::init_from_memory().context("extract wechat message keys")?;
        self.write_wechat_db_key(init, None)?;
        self.db_material()
    }

    fn db_material(&self) -> Result<(PathBuf, HashMap<String, String>)> {
        let key = self.read_key()?;
        if let (Some(db_dir), Some(keys)) = (&key.db_dir, &key.keys) {
            if !keys.is_empty() {
                return Ok((PathBuf::from(db_dir), keys.clone()));
            }
        }
        if let Some(keys_file) = key
            .keys_file
            .as_ref()
            .filter(|path| !path.trim().is_empty())
        {
            let keys = wechat_db::read_keys_file(&PathBuf::from(keys_file))?;
            let db_dir = key
                .db_dir
                .as_ref()
                .map(PathBuf::from)
                .or_else(wechat_db::detect_db_storage)
                .ok_or_else(|| anyhow!("wechat db_storage directory not found"))?;
            return Ok((db_dir, keys));
        }
        bail!("wechat db key material not found")
    }

    fn read_key(&self) -> Result<KeyFile> {
        let data = fs::read_to_string(&self.key_file)
            .with_context(|| format!("read key file {}", self.key_file.display()))?;
        let key: KeyFile = serde_json::from_str(&data)?;
        let has_key_map = key
            .keys
            .as_ref()
            .map(|keys| !keys.is_empty())
            .unwrap_or(false);
        let has_keys_file = key
            .keys_file
            .as_ref()
            .map(|path| !path.trim().is_empty())
            .unwrap_or(false);
        if !has_key_map && !has_keys_file {
            bail!("wechat key file has no database keys");
        }
        Ok(key)
    }

    fn write_wechat_db_key(
        &self,
        init: wechat_db::InitData,
        wxid: Option<String>,
    ) -> Result<KeyFile> {
        self.ensure_state_dir()?;
        let previous = self.read_key().ok();
        let doc = KeyFile {
            version: 1,
            wxid: wxid
                .or_else(|| previous.as_ref().map(|p| p.wxid.clone()))
                .unwrap_or_default(),
            key: "webox-weagent".to_string(),
            source: Some("memory".to_string()),
            keys_file: None,
            db_dir: Some(init.db_dir.to_string_lossy().into_owned()),
            keys: Some(init.keys),
            created_at: previous.as_ref().map(|p| p.created_at).unwrap_or_else(now),
            updated_at: now(),
        };
        let tmp = self.key_file.with_extension("tmp");
        fs::write(&tmp, serde_json::to_vec_pretty(&doc)?)?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            let _ = fs::set_permissions(&tmp, fs::Permissions::from_mode(0o600));
        }
        fs::rename(&tmp, &self.key_file)?;
        Ok(doc)
    }

    fn cache_dir(&self) -> PathBuf {
        self.state_dir.join("cache")
    }
}

fn now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

fn encode_db_cursor(sessions: &HashMap<String, i64>) -> String {
    URL_SAFE_NO_PAD.encode(
        serde_json::to_vec(&DbCursor {
            v: 2,
            source: "db".to_string(),
            sessions: sessions.clone(),
        })
        .expect("cursor serializes"),
    )
}

fn decode_db_cursor(raw: Option<&str>) -> Result<Option<HashMap<String, i64>>> {
    let Some(raw) = raw.filter(|value| !value.is_empty()) else {
        return Ok(None);
    };
    let bytes = URL_SAFE_NO_PAD
        .decode(raw)
        .context("cursor is not base64url")?;
    let value: Value = serde_json::from_slice(&bytes).context("cursor is not json")?;
    match value.get("v").and_then(Value::as_u64) {
        Some(2) => {
            let cursor: DbCursor = serde_json::from_value(value).context("cursor is invalid")?;
            Ok(Some(cursor.sessions))
        }
        Some(1) => {
            let _: LegacySpoolCursor =
                serde_json::from_value(value).context("legacy cursor is invalid")?;
            Ok(None)
        }
        _ => bail!("cursor version is unsupported"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn db_cursor_round_trips_session_state() {
        let sessions = HashMap::from([("wxid_test".to_string(), 123_i64)]);
        let encoded = encode_db_cursor(&sessions);
        assert_eq!(decode_db_cursor(Some(&encoded)).unwrap().unwrap(), sessions);
        assert!(decode_db_cursor(None).unwrap().is_none());
    }

    #[test]
    fn legacy_cursor_is_treated_as_empty_db_state() {
        let encoded = URL_SAFE_NO_PAD
            .encode(serde_json::to_vec(&LegacySpoolCursor { v: 1, pos: 42 }).unwrap());
        assert!(decode_db_cursor(Some(&encoded)).unwrap().is_none());
    }
}
