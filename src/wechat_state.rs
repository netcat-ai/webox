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
const UPDATE_ID_SCALE: i64 = 1_000_000;

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

    pub fn poll_messages_after_id(&self, after_id: i64, limit: usize) -> Result<PollResult> {
        let (db_dir, keys) = self.ensure_db_material()?;
        let limit = limit.clamp(1, MAX_POLL_LIMIT);
        let cursor_state = if after_id > 0 {
            let since_ts = update_id_seconds(after_id).saturating_sub(1);
            Some(
                wechat_db::current_session_state(db_dir.clone(), keys.clone(), self.cache_dir())?
                    .into_keys()
                    .map(|room| (room, since_ts))
                    .collect(),
            )
        } else {
            None
        };
        let internal_limit = if after_id > 0 {
            limit.saturating_mul(20).clamp(limit, MAX_POLL_LIMIT)
        } else {
            limit
        };
        let mut data = wechat_db::poll_new_messages(
            db_dir,
            keys,
            cursor_state,
            internal_limit,
            self.cache_dir(),
        )
        .context("poll wechat local db")?;
        if after_id > 0 {
            data.messages
                .retain(|message| message_update_id(message) > after_id);
        }
        data.messages.sort_by_key(message_update_id);
        data.messages.truncate(limit);
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

pub fn message_update_id(message: &Value) -> i64 {
    let ts_ms = message
        .get("msgtime")
        .and_then(Value::as_i64)
        .or_else(|| {
            message
                .get("timestamp")
                .and_then(Value::as_i64)
                .map(|ts| ts.saturating_mul(1000))
        })
        .unwrap_or_default();
    let ts = ts_ms.saturating_div(1000).max(0);
    ts.saturating_mul(UPDATE_ID_SCALE)
        .saturating_add(stable_sequence(message) % UPDATE_ID_SCALE)
}

fn update_id_seconds(update_id: i64) -> i64 {
    update_id.saturating_div(UPDATE_ID_SCALE).max(0)
}

fn stable_sequence(message: &Value) -> i64 {
    if let Some(local_id) = message.get("local_id").and_then(Value::as_i64) {
        return (local_id.unsigned_abs() % UPDATE_ID_SCALE as u64) as i64;
    }
    let seed = message
        .get("msgid")
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .or_else(|| message.get("server_id").and_then(Value::as_str))
        .unwrap_or_else(|| {
            message
                .get("from")
                .and_then(Value::as_str)
                .unwrap_or_default()
        });
    let digest = md5::compute(seed.as_bytes());
    let mut bytes = [0_u8; 8];
    bytes.copy_from_slice(&digest.0[..8]);
    i64::from_be_bytes(bytes) & i64::MAX
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn message_update_id_keeps_timestamp_prefix() {
        let message = json!({
            "msgtime": 1781703356000_i64,
            "local_id": 42,
            "msgid": "7318462845630259071",
        });

        assert_eq!(message_update_id(&message) / UPDATE_ID_SCALE, 1781703356);
        assert_eq!(message_update_id(&message) % UPDATE_ID_SCALE, 42);
    }
}
