use crate::signed_payload;
use crate::wechat_db;
use anyhow::{anyhow, bail, Context, Result};
use serde::{Deserialize, Serialize};
use serde_json::Value;
use std::collections::HashMap;
use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::{Arc, Mutex, MutexGuard};
use std::time::{SystemTime, UNIX_EPOCH};

const MAX_POLL_LIMIT: usize = 500;
const DB_KEY_VALIDATION_INTERVAL_SECS: u64 = 30;

#[derive(Clone, Debug)]
pub struct WechatState {
    state_dir: PathBuf,
    key_file: PathBuf,
    cursor_key: Arc<str>,
    runtime: Arc<WechatRuntime>,
}

#[derive(Debug, Default)]
struct WechatRuntime {
    initialized: AtomicBool,
    had_ready_session: AtomicBool,
    last_key_validation_at: AtomicU64,
    db_io: Mutex<()>,
    last_error: Mutex<Option<String>>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct KeyFile {
    wxid: String,
    #[serde(rename = "dbDir")]
    db_dir: String,
    keys: HashMap<String, String>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct DbCursor {
    started_at: i64,
    positions: wechat_db::MessagePositions,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct PollResult {
    pub cursor: String,
    pub messages: Vec<Value>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "snake_case")]
pub struct LoginStatus {
    pub status: LoginStatusKind,
    pub has_key: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub detail: Option<String>,
}

#[derive(Clone, Copy, Debug, Serialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum LoginStatusKind {
    LoggedIn,
    WaitingForLogin,
    WaitingForKey,
    KeyUnavailable,
}

#[derive(Debug, PartialEq, Eq)]
pub enum InitializationState {
    WaitingForLogin,
    Ready,
}

impl WechatState {
    pub fn new(state_dir: PathBuf, cursor_key: impl Into<Arc<str>>) -> Self {
        Self {
            key_file: state_dir.join("wechat.key"),
            state_dir,
            cursor_key: cursor_key.into(),
            runtime: Arc::new(WechatRuntime::default()),
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

    fn has_key(&self) -> bool {
        self.key_file.exists()
    }

    pub fn is_initialized(&self) -> bool {
        self.runtime.initialized.load(Ordering::Acquire)
    }

    pub fn had_ready_session(&self) -> bool {
        self.runtime.had_ready_session.load(Ordering::Acquire)
    }

    pub fn login_status(&self) -> LoginStatus {
        let has_key = self.has_key();
        match self.read_key() {
            Ok(key) => {
                let db_dir = PathBuf::from(&key.db_dir);
                let has_db_storage = db_dir.is_dir();
                let ui_ready = wechat_main_window_ready();
                let has_material = has_db_storage && !key.keys.is_empty();
                let can_read_messages = has_material && self.is_initialized();
                let logged_in = can_read_messages && ui_ready == Some(true);
                LoginStatus {
                    status: if logged_in {
                        LoginStatusKind::LoggedIn
                    } else if ui_ready == Some(false) {
                        LoginStatusKind::WaitingForLogin
                    } else {
                        LoginStatusKind::KeyUnavailable
                    },
                    has_key: true,
                    detail: self.last_init_error().or_else(|| {
                        (!has_db_storage)
                            .then(|| "wechat db_storage directory is missing".to_string())
                    }),
                }
            }
            Err(_) => {
                let detected_db = wechat_db::detect_db_storage();
                let has_db_storage = detected_db.as_ref().is_some_and(|path| path.is_dir());
                LoginStatus {
                    status: if has_key {
                        LoginStatusKind::KeyUnavailable
                    } else if has_db_storage {
                        LoginStatusKind::WaitingForKey
                    } else {
                        LoginStatusKind::WaitingForLogin
                    },
                    has_key,
                    detail: self.last_init_error(),
                }
            }
        }
    }

    pub fn initialize_if_ready(&self) -> Result<InitializationState> {
        if wechat_main_window_ready() != Some(true) {
            self.runtime.initialized.store(false, Ordering::Release);
            return Ok(InitializationState::WaitingForLogin);
        }
        let _db_guard = self.acquire_db_lock()?;
        let active_db_dir = wechat_db::detect_db_storage()
            .ok_or_else(|| anyhow!("wechat db_storage directory not found"))?;
        let active_wxid = wechat_db::account_id_from_db_dir(&active_db_dir)
            .ok_or_else(|| anyhow!("cannot identify active WeChat account"))?;
        let material = self.read_key().and_then(|key| {
            let material = Self::db_material_from(&key)?;
            if key.wxid != active_wxid
                || wechat_db::account_id_from_db_dir(&material.0).as_deref()
                    != Some(active_wxid.as_str())
            {
                bail!("stored WeChat database key belongs to another account");
            }
            Ok(material)
        });
        if self.is_initialized()
            && material.is_ok()
            && !validation_due(
                self.runtime.last_key_validation_at.load(Ordering::Acquire),
                now_secs(),
            )
        {
            return Ok(InitializationState::Ready);
        }
        self.runtime.initialized.store(false, Ordering::Release);
        let material = match material.and_then(|material| self.validate_db_material(material)) {
            Ok(material) => material,
            Err(_) => {
                let init = wechat_db::init_from_memory()
                    .context("extract wechat message keys during automatic initialization")?;
                self.write_wechat_db_key(init)?;
                self.validate_db_material(self.db_material()?)?
            }
        };
        drop(material);
        self.runtime.initialized.store(true, Ordering::Release);
        self.runtime
            .had_ready_session
            .store(true, Ordering::Release);
        self.runtime
            .last_key_validation_at
            .store(now_secs(), Ordering::Release);
        self.set_init_error(None);
        Ok(InitializationState::Ready)
    }

    pub fn record_init_error(&self, error: String) {
        self.runtime.initialized.store(false, Ordering::Release);
        self.set_init_error(Some(error));
    }

    pub fn click_saved_account_login(&self) -> Result<bool> {
        let Some(window) = wechat_login_window() else {
            return Ok(false);
        };
        let status = Command::new("xdotool")
            .args(["mousemove", "--window", &window, "140", "290", "click", "1"])
            .status()
            .context("click saved-account login button")?;
        if !status.success() {
            bail!("xdotool could not click saved-account login button");
        }
        Ok(true)
    }

    pub fn dismiss_post_login_overlay(&self) -> Result<bool> {
        let Some(window) = wechat_main_window() else {
            return Ok(false);
        };
        let status = Command::new("xdotool")
            .args([
                "windowactivate",
                "--sync",
                &window,
                "key",
                "--clearmodifiers",
                "Escape",
            ])
            .status()
            .context("run xdotool for post-login overlay")?;
        if !status.success() {
            bail!("xdotool could not dismiss post-login overlay");
        }
        Ok(true)
    }

    pub fn refresh_login_qrcode(&self) -> Result<bool> {
        let Some(window) = wechat_login_window() else {
            return Ok(false);
        };
        let status = Command::new("xdotool")
            .args(["mousemove", "--window", &window, "140", "130", "click", "1"])
            .status()
            .context("click expired QR refresh area")?;
        if !status.success() {
            bail!("xdotool could not click expired QR refresh area");
        }
        Ok(true)
    }

    pub fn poll_messages(&self, cursor: Option<&str>, limit: usize) -> Result<PollResult> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        let limit = limit.clamp(1, MAX_POLL_LIMIT);
        let cursor = cursor
            .and_then(non_empty_cursor)
            .map(|cursor| self.decode_db_cursor(cursor))
            .transpose()?;
        let cursor = match cursor {
            Some(cursor) => cursor,
            None => {
                let started_at = now();
                let positions = self.track_db_result(
                    wechat_db::baseline_message_positions(
                        db_dir,
                        keys,
                        self.cache_dir(),
                        started_at,
                    ),
                    "baseline WeChat messages",
                )?;
                let cursor = DbCursor {
                    started_at,
                    positions,
                };
                return Ok(PollResult {
                    cursor: self.encode_db_cursor(&cursor)?,
                    messages: Vec::new(),
                });
            }
        };
        let mut data = self
            .track_db_result(
                wechat_db::poll_new_messages(
                    db_dir,
                    keys,
                    cursor.positions,
                    cursor.started_at,
                    limit,
                    self.cache_dir(),
                ),
                "poll WeChat messages",
            )
            .context("poll wechat local db")?;
        data.messages.sort_by_key(message_order_key);
        let cursor = DbCursor {
            started_at: cursor.started_at,
            positions: data.new_state,
        };
        Ok(PollResult {
            cursor: self.encode_db_cursor(&cursor)?,
            messages: data.messages,
        })
    }

    pub fn validate_poll_cursor(&self, cursor: &str) -> Result<()> {
        if let Some(cursor) = non_empty_cursor(cursor) {
            self.decode_db_cursor(cursor)?;
        }
        Ok(())
    }

    pub fn current_user_id(&self) -> Result<String> {
        Ok(self.read_key()?.wxid)
    }

    pub fn resolve_recipient(&self, username: &str) -> Result<wechat_db::Recipient> {
        let _db_guard = self.acquire_db_lock()?;
        if !self.is_initialized() {
            bail!("wechat automatic initialization is not complete");
        }
        let key = self.read_key()?;
        let current_user_id = key.wxid.clone();
        let (db_dir, keys) = Self::db_material_from(&key)?;
        self.track_db_result(
            wechat_db::resolve_recipient_by_username(
                db_dir,
                keys,
                self.cache_dir(),
                username,
                &current_user_id,
            ),
            "resolve WeChat recipient",
        )
        .with_context(|| format!("resolve wechat recipient {username}"))?
        .ok_or_else(|| anyhow!("recipient not found: target must be a WeChat internal id"))
    }

    pub fn contact_identity(&self, username: &str) -> Result<Option<wechat_db::ContactIdentity>> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        self.track_db_result(
            wechat_db::contact_identity(db_dir, keys, self.cache_dir(), username),
            "read WeChat contact identity",
        )
    }

    pub fn outgoing_text_contains(&self, target: &str, needle: &str) -> Result<bool> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        self.track_db_result(
            wechat_db::outgoing_text_contains(db_dir, keys, self.cache_dir(), target, needle),
            "search outgoing WeChat text",
        )
    }

    pub fn room_message_positions(&self, target: &str) -> Result<wechat_db::RoomMessagePositions> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        self.track_db_result(
            wechat_db::room_message_positions(db_dir, keys, self.cache_dir(), target),
            "read WeChat message positions",
        )
    }

    pub fn has_text_message_after(
        &self,
        positions: &wechat_db::RoomMessagePositions,
        target: &str,
        text: &str,
    ) -> Result<bool> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        self.track_db_result(
            wechat_db::has_outgoing_text(db_dir, keys, self.cache_dir(), target, positions, text),
            "verify outgoing WeChat text",
        )
    }

    pub fn read_media(&self, roomid: &str, msgid: &str) -> Result<Option<wechat_db::MediaFile>> {
        let _db_guard = self.acquire_db_lock()?;
        let (db_dir, keys) = self.ready_db_material()?;
        self.track_db_result(
            wechat_db::read_media(db_dir, keys, self.cache_dir(), roomid, msgid),
            "read WeChat media",
        )
    }

    fn ready_db_material(&self) -> Result<(PathBuf, HashMap<String, String>)> {
        if !self.is_initialized() {
            bail!("wechat automatic initialization is not complete");
        }
        self.db_material().map_err(|error| {
            self.record_init_error(format!("load WeChat database keys: {error:#}"));
            error
        })
    }

    fn track_db_result<T>(&self, result: Result<T>, operation: &str) -> Result<T> {
        result.map_err(|error| {
            self.record_init_error(format!("{operation}: {error:#}"));
            error
        })
    }

    fn acquire_db_lock(&self) -> Result<MutexGuard<'_, ()>> {
        self.runtime
            .db_io
            .lock()
            .map_err(|_| anyhow!("WeChat database lock is poisoned"))
    }

    fn validate_db_material(
        &self,
        material: (PathBuf, HashMap<String, String>),
    ) -> Result<(PathBuf, HashMap<String, String>)> {
        wechat_db::current_session_state(material.0.clone(), material.1.clone(), self.cache_dir())
            .context("validate wechat database keys")?;
        Ok(material)
    }

    fn db_material(&self) -> Result<(PathBuf, HashMap<String, String>)> {
        let key = self.read_key()?;
        Self::db_material_from(&key)
    }

    fn db_material_from(key: &KeyFile) -> Result<(PathBuf, HashMap<String, String>)> {
        if key.db_dir.trim().is_empty() || key.keys.is_empty() {
            bail!("wechat db key material not found");
        }
        Ok((PathBuf::from(&key.db_dir), key.keys.clone()))
    }

    fn read_key(&self) -> Result<KeyFile> {
        let data = fs::read_to_string(&self.key_file)
            .with_context(|| format!("read key file {}", self.key_file.display()))?;
        let key: KeyFile = serde_json::from_str(&data)?;
        if key.wxid.trim().is_empty() || key.db_dir.trim().is_empty() || key.keys.is_empty() {
            bail!("wechat key file has no database keys");
        }
        Ok(key)
    }

    fn write_wechat_db_key(&self, init: wechat_db::InitData) -> Result<KeyFile> {
        self.ensure_state_dir()?;
        let doc = KeyFile {
            wxid: init.wxid,
            db_dir: init.db_dir.to_string_lossy().into_owned(),
            keys: init.keys,
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

    fn set_init_error(&self, error: Option<String>) {
        if let Ok(mut value) = self.runtime.last_error.lock() {
            *value = error;
        }
    }

    fn last_init_error(&self) -> Option<String> {
        self.runtime
            .last_error
            .lock()
            .ok()
            .and_then(|value| value.clone())
    }

    fn encode_db_cursor(&self, cursor: &DbCursor) -> Result<String> {
        signed_payload::encode(&self.cursor_key, cursor)
    }

    fn decode_db_cursor(&self, raw: &str) -> Result<DbCursor> {
        let cursor: DbCursor =
            signed_payload::decode(&self.cursor_key, raw).context("decode get_updates_buf")?;
        if cursor.started_at <= 0 {
            bail!("unsupported get_updates_buf");
        }
        Ok(cursor)
    }
}

fn now() -> i64 {
    now_secs() as i64
}

fn now_secs() -> u64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
}

fn validation_due(last_validation_at: u64, current_time: u64) -> bool {
    last_validation_at == 0
        || current_time.saturating_sub(last_validation_at) >= DB_KEY_VALIDATION_INTERVAL_SECS
}

fn wechat_main_window_ready() -> Option<bool> {
    let geometry = visible_wechat_window_geometry()?;
    Some(window_geometry_has_main_window(&geometry))
}

fn wechat_main_window() -> Option<String> {
    main_window_from_geometry(&visible_wechat_window_geometry()?)
}

fn wechat_login_window() -> Option<String> {
    login_window_from_geometry(&visible_wechat_window_geometry()?)
}

fn visible_wechat_window_geometry() -> Option<String> {
    if std::env::var("DISPLAY").ok()?.trim().is_empty() {
        return None;
    }
    let output = Command::new("xdotool")
        .args([
            "search",
            "--onlyvisible",
            "--class",
            "wechat",
            "getwindowgeometry",
            "--shell",
            "%@",
        ])
        .output()
        .ok()?;
    if !output.status.success() {
        return Some(String::new());
    }
    Some(String::from_utf8_lossy(&output.stdout).into_owned())
}

fn login_window_from_geometry(output: &str) -> Option<String> {
    let mut window = None;
    let mut width = None;
    for line in output.lines() {
        if let Some(value) = line.strip_prefix("WINDOW=") {
            window = Some(value.to_string());
            width = None;
        } else if let Some(value) = line.strip_prefix("WIDTH=") {
            width = value.parse::<u32>().ok();
        } else if let Some(value) = line.strip_prefix("HEIGHT=") {
            let height = value.parse::<u32>().ok();
            if width.is_some_and(|width| width <= 400) && height.is_some_and(|height| height <= 500)
            {
                return window;
            }
        }
    }
    None
}

fn window_geometry_has_main_window(output: &str) -> bool {
    main_window_from_geometry(output).is_some()
}

fn main_window_from_geometry(output: &str) -> Option<String> {
    let mut window = None;
    let mut width = None;
    for line in output.lines() {
        if let Some(value) = line.strip_prefix("WINDOW=") {
            window = Some(value.to_string());
            width = None;
        } else if let Some(value) = line.strip_prefix("WIDTH=") {
            width = value.parse::<u32>().ok();
        } else if let Some(value) = line.strip_prefix("HEIGHT=") {
            let height = value.parse::<u32>().ok();
            if width.is_some_and(|width| width >= 700) && height.is_some_and(|height| height >= 500)
            {
                return window;
            }
            width = None;
        }
    }
    None
}

fn non_empty_cursor(value: &str) -> Option<&str> {
    let value = value.trim();
    (!value.is_empty()).then_some(value)
}

fn message_order_key(message: &Value) -> (i64, i64, String) {
    (
        message
            .get("msgtime")
            .and_then(Value::as_i64)
            .unwrap_or_default(),
        message
            .get("local_id")
            .and_then(Value::as_i64)
            .unwrap_or_default(),
        message
            .get("roomid")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string(),
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::collections::HashMap;
    use uuid::Uuid;

    #[test]
    fn db_cursor_round_trips_exact_stream_positions() {
        let cursor = DbCursor {
            started_at: 100,
            positions: std::collections::BTreeMap::from([(
                "room".to_string(),
                std::collections::BTreeMap::from([(
                    "message/msg_0.db".to_string(),
                    wechat_db::MessagePosition {
                        create_time: 101,
                        local_id: 42,
                    },
                )]),
            )]),
        };

        let state = WechatState::new(PathBuf::from("/tmp/test"), "test-token");
        let encoded = state.encode_db_cursor(&cursor).unwrap();
        let decoded = state.decode_db_cursor(&encoded).unwrap();

        assert_eq!(decoded.positions["room"]["message/msg_0.db"].local_id, 42);
    }

    #[test]
    fn db_cursor_rejects_tampering() {
        let state = WechatState::new(PathBuf::from("/tmp/test"), "test-token");
        let cursor = DbCursor {
            started_at: 100,
            positions: Default::default(),
        };
        let mut encoded = state.encode_db_cursor(&cursor).unwrap().into_bytes();
        encoded[0] = if encoded[0] == b'A' { b'B' } else { b'A' };

        assert!(state
            .decode_db_cursor(std::str::from_utf8(&encoded).unwrap())
            .is_err());
    }

    #[test]
    fn db_cursor_rejects_legacy_fields() {
        let state = WechatState::new(PathBuf::from("/tmp/test"), "test-token");
        let encoded = signed_payload::encode(
            &state.cursor_key,
            &serde_json::json!({
                "v": 3,
                "source": "db",
                "started_at": 100,
                "positions": {},
            }),
        )
        .unwrap();

        assert!(state.decode_db_cursor(&encoded).is_err());
    }

    #[test]
    fn key_file_rejects_legacy_fields() {
        let legacy = serde_json::json!({
            "version": 1,
            "wxid": "wxid_test",
            "dbDir": "/tmp/db",
            "keys": {},
        });

        assert!(serde_json::from_value::<KeyFile>(legacy).is_err());
    }

    #[test]
    fn key_validation_is_periodic() {
        assert!(validation_due(0, 100));
        assert!(!validation_due(100, 129));
        assert!(validation_due(100, 130));
        assert!(!validation_due(100, 90));
    }

    #[test]
    fn missing_key_material_invalidates_a_ready_session() {
        let state_dir = std::env::temp_dir().join(format!("webox-key-loss-{}", Uuid::new_v4()));
        let state = WechatState::new(state_dir.clone(), "test-token");
        state.runtime.initialized.store(true, Ordering::Release);

        assert!(state.ready_db_material().is_err());
        assert!(!state.is_initialized());

        fs::remove_dir_all(state_dir).ok();
    }

    #[test]
    fn cloned_state_serializes_database_access() {
        let state = WechatState::new(PathBuf::from("/tmp/test"), "test-token");
        let cloned = state.clone();
        let _guard = state.acquire_db_lock().unwrap();

        assert!(cloned.runtime.db_io.try_lock().is_err());
    }

    #[test]
    fn main_window_geometry_rejects_login_window() {
        assert!(!window_geometry_has_main_window(
            "WINDOW=1\nWIDTH=280\nHEIGHT=380\n"
        ));
        let geometry = "WINDOW=1\nWIDTH=280\nHEIGHT=380\nWINDOW=2\nWIDTH=980\nHEIGHT=710\n";
        assert!(window_geometry_has_main_window(geometry));
        assert_eq!(main_window_from_geometry(geometry).as_deref(), Some("2"));
    }

    #[test]
    fn login_window_geometry_returns_small_wechat_window() {
        assert_eq!(
            login_window_from_geometry(
                "WINDOW=1\nWIDTH=980\nHEIGHT=710\nWINDOW=2\nWIDTH=280\nHEIGHT=380\n"
            )
            .as_deref(),
            Some("2")
        );
        assert!(login_window_from_geometry("WINDOW=1\nWIDTH=980\nHEIGHT=710\n").is_none());
    }

    #[test]
    fn login_status_does_not_create_key_material() {
        let state_dir = std::env::temp_dir().join(format!("webox-login-{}", Uuid::new_v4()));
        let state = WechatState::new(state_dir.clone(), "test-token");

        let _ = state.login_status();

        assert!(!state_dir.join("wechat.key").exists());
        fs::remove_dir_all(state_dir).ok();
    }

    #[test]
    fn login_status_waits_for_login_without_key_or_db() {
        let state_dir = std::env::temp_dir().join(format!("webox-login-{}", Uuid::new_v4()));
        let state = WechatState::new(state_dir.clone(), "test-token");

        let status = state.login_status();

        fs::remove_dir_all(state_dir).ok();
        assert_eq!(status.status, LoginStatusKind::WaitingForLogin);
        assert!(!status.has_key);
        assert!(status.detail.is_none());
    }

    #[test]
    fn login_status_does_not_assume_login_from_key_material_alone() {
        let state_dir = std::env::temp_dir().join(format!("webox-login-{}", Uuid::new_v4()));
        let db_dir = state_dir.join("db_storage");
        fs::create_dir_all(&db_dir).unwrap();
        let state = WechatState::new(state_dir.clone(), "test-token");
        state.ensure_state_dir().unwrap();
        let mut keys = HashMap::new();
        keys.insert("message/msg_0.db".to_string(), "00".repeat(32));
        fs::write(
            state_dir.join("wechat.key"),
            serde_json::to_vec(&KeyFile {
                wxid: "wxid_test".to_string(),
                db_dir: db_dir.to_string_lossy().into_owned(),
                keys,
            })
            .unwrap(),
        )
        .unwrap();

        let status = state.login_status();

        fs::remove_dir_all(state_dir).ok();
        assert_eq!(status.status, LoginStatusKind::KeyUnavailable);
        assert!(status.has_key);
        assert!(status.detail.is_none());
    }
}
