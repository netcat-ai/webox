use crate::error::ApiError;
use crate::media_store::MediaStore;
use crate::qr_source::QrSource;
use crate::signed_payload;
use crate::ui_sender::UiSender;
use crate::wechat_state::{LoginStatusKind, WechatState};
use axum::extract::{Query, State};
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use sha2::{Digest, Sha256};
use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::{Arc, Mutex};
use tokio::time::{sleep, Duration, Instant};

const TEXT_ITEM_TYPE: i64 = 1;
const GET_UPDATES_TIMEOUT: Duration = Duration::from_secs(35);
const GET_UPDATES_POLL_INTERVAL: Duration = Duration::from_secs(1);
const GET_UPDATES_TIMEOUT_MS: i64 = 35_000;
#[cfg(not(test))]
const QR_ACQUIRE_TIMEOUT: Duration = Duration::from_secs(20);
#[cfg(test)]
const QR_ACQUIRE_TIMEOUT: Duration = Duration::from_millis(10);
const QR_SESSION_TTL: Duration = Duration::from_secs(5 * 60);
const MAX_SEND_RECEIPTS: usize = 1024;
const REMINDER_RETRY_INTERVAL: Duration = Duration::from_secs(5);
#[derive(Clone)]
pub struct AppState {
    pub api_token: String,
    pub provider_account_id: String,
    pub wechat: WechatState,
    pub sender: Arc<tokio::sync::Mutex<UiSender>>,
    pub qr_source: QrSource,
    pub media_store: MediaStore,
    pub login_session: Arc<Mutex<LoginSession>>,
    pub send_receipts: Arc<Mutex<SendReceiptCache>>,
    pub remark_reminders: Arc<Mutex<HashSet<String>>>,
}

#[derive(Clone, Debug)]
struct CachedSend {
    fingerprint: String,
    client_msg_id: String,
}

#[derive(Debug, Default)]
pub struct SendReceiptCache {
    entries: HashMap<String, CachedSend>,
    order: VecDeque<String>,
}

#[derive(Debug, Default)]
pub struct LoginSession {
    active_qrcode: Option<String>,
    active_issued_at: Option<Instant>,
    confirmed_qrcode: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct BotQrcodeQuery {
    #[serde(default)]
    pub bot_type: Option<String>,
}

#[derive(Debug, Default, Deserialize)]
pub struct BotQrcodeRequest {
    #[serde(default)]
    pub local_token_list: Vec<String>,
}

#[derive(Debug, Deserialize)]
pub struct QrcodeStatusQuery {
    pub qrcode: String,
}

#[derive(Debug, Deserialize)]
pub struct GetUpdatesRequest {
    #[serde(default)]
    pub get_updates_buf: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SendMessageRequest {
    pub msg: OutboundMessage,
}

#[derive(Debug, Deserialize)]
pub struct GetConfigRequest {
    #[serde(default)]
    pub ilink_user_id: Option<String>,
    #[serde(default)]
    pub context_token: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct SendTypingRequest {
    #[serde(default)]
    pub ilink_user_id: Option<String>,
    #[serde(default)]
    pub typing_ticket: Option<String>,
    #[serde(default)]
    pub status: Option<i64>,
}

#[derive(Debug, Deserialize)]
pub struct LifecycleRequest {}

#[derive(Debug, Deserialize)]
pub struct CdnDownloadQuery {
    pub encrypted_query_param: String,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct OutboundMessage {
    #[serde(default)]
    pub client_id: Option<String>,
    #[serde(default)]
    pub context_token: Option<String>,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub item_list: Vec<Value>,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct TypingTicket {
    ilink_user_id: String,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ContextToken {
    target: String,
}

pub async fn health(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    Json(json!({
        "ok": true,
        "ready": state.wechat.is_initialized(),
    }))
}

pub async fn get_bot_qrcode(
    State(state): State<Arc<AppState>>,
    Query(query): Query<BotQrcodeQuery>,
    Json(request): Json<BotQrcodeRequest>,
) -> Result<impl IntoResponse, ApiError> {
    issue_bot_qrcode(state, query, request.local_token_list).await
}

pub async fn get_bot_qrcode_without_tokens(
    State(state): State<Arc<AppState>>,
    Query(query): Query<BotQrcodeQuery>,
) -> Result<impl IntoResponse, ApiError> {
    issue_bot_qrcode(state, query, Vec::new()).await
}

async fn issue_bot_qrcode(
    state: Arc<AppState>,
    query: BotQrcodeQuery,
    local_token_list: Vec<String>,
) -> Result<Json<Value>, ApiError> {
    if query.bot_type.as_deref().unwrap_or("3") != "3" {
        return Err(ApiError::bad_request("unsupported bot_type"));
    }
    let has_local_token = has_matching_local_token(&local_token_list, &state.api_token);
    let has_expired_qrcode = state
        .login_session
        .lock()
        .map_err(|_| ApiError::Internal("login session lock is poisoned".to_string()))?
        .expired_qrcode()
        .is_some();
    if has_expired_qrcode && state.wechat.login_status().status != LoginStatusKind::LoggedIn {
        let wechat = state.wechat.clone();
        match tokio::task::spawn_blocking(move || wechat.refresh_login_qrcode()).await {
            Ok(Ok(true)) => sleep(Duration::from_millis(750)).await,
            Ok(Ok(false)) => {}
            Ok(Err(err)) => {
                tracing::warn!(error = %err, "could not refresh expired WeChat QR code")
            }
            Err(err) => tracing::warn!(error = %err, "WeChat QR refresh task failed"),
        }
    }
    let deadline = Instant::now() + QR_ACQUIRE_TIMEOUT;
    let (qrcode, login) = loop {
        let login = state.wechat.login_status();
        if login.status == LoginStatusKind::LoggedIn {
            if has_local_token {
                break (None, login);
            }
            return Err(ApiError::Unauthorized(
                "WeChat is already logged in; a matching local token is required".to_string(),
            ));
        }
        let qrcode = match state.qr_source.latest().await {
            Ok(qrcode) => qrcode,
            Err(err) => {
                tracing::warn!(error = %err, "qrcode capture source unavailable");
                None
            }
        };
        if qrcode.is_some() {
            break (qrcode, login);
        }
        if Instant::now() >= deadline {
            return Err(ApiError::Unavailable(
                "WeChat login QR code is not ready".to_string(),
            ));
        }
        sleep(Duration::from_millis(500)).await;
    };
    let qrcode_id = {
        let mut session = state
            .login_session
            .lock()
            .map_err(|_| ApiError::Internal("login session lock is poisoned".to_string()))?;
        match qrcode.as_ref() {
            Some(qrcode) => session.register_qrcode(&qrcode.id),
            None if login.status == LoginStatusKind::LoggedIn && has_local_token => {
                session.register_resume()
            }
            None => unreachable!("QR acquisition only completes with a QR code or logged-in state"),
        }
    };
    Ok(Json(json!({
        "qrcode": qrcode_id,
        "qrcode_img_content": qrcode.map(|qrcode| qrcode.login_url).unwrap_or_default(),
    })))
}

pub async fn get_qrcode_status(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Query(query): Query<QrcodeStatusQuery>,
) -> Result<impl IntoResponse, ApiError> {
    if query.qrcode.trim().is_empty() {
        return Err(ApiError::bad_request("qrcode is required"));
    }
    let current_qrcode = state.qr_source.latest().await.ok().flatten();
    let login = state.wechat.login_status();
    if let Some(detail) = login.detail.as_deref() {
        tracing::warn!(status = ?login.status, detail, "wechat login state is not ready");
    }
    let mut response = serde_json::Map::new();
    let status = state
        .login_session
        .lock()
        .map_err(|_| ApiError::Internal("login session lock is poisoned".to_string()))?
        .status(
            &query.qrcode,
            current_qrcode.as_ref().map(|qrcode| qrcode.id.as_str()),
            login.status,
            login.has_key,
        );
    response.insert("status".to_string(), json!(status));
    if status == "confirmed" {
        response.insert("bot_token".to_string(), json!(state.api_token));
        response.insert("ilink_bot_id".to_string(), json!(state.provider_account_id));
        response.insert(
            "ilink_user_id".to_string(),
            json!(state.provider_account_id),
        );
        response.insert("baseurl".to_string(), json!(ilink_base_url(&headers)));
    }
    Ok(Json(Value::Object(response)))
}

impl LoginSession {
    pub(crate) fn register_qrcode(&mut self, qrcode: &str) -> String {
        if self.active_qrcode.as_deref() != Some(qrcode) {
            self.active_qrcode = Some(qrcode.to_string());
            self.active_issued_at = Some(Instant::now());
            self.confirmed_qrcode = None;
        }
        qrcode.to_string()
    }

    fn register_resume(&mut self) -> String {
        if self.active_qrcode.is_none() {
            self.active_qrcode = Some(format!("resume-{}", uuid::Uuid::new_v4().simple()));
            self.active_issued_at = Some(Instant::now());
            self.confirmed_qrcode = None;
        }
        self.active_qrcode.clone().unwrap_or_default()
    }

    fn expired_qrcode(&self) -> Option<&str> {
        self.active_issued_at
            .filter(|issued_at| issued_at.elapsed() >= QR_SESSION_TTL)
            .and(self.active_qrcode.as_deref())
    }

    fn status(
        &mut self,
        requested_qrcode: &str,
        current_qrcode: Option<&str>,
        login_status: LoginStatusKind,
        has_key: bool,
    ) -> &'static str {
        let known = self.active_qrcode.as_deref() == Some(requested_qrcode)
            || self.confirmed_qrcode.as_deref() == Some(requested_qrcode);
        if !known {
            return "expired";
        }
        if login_status == LoginStatusKind::LoggedIn {
            self.confirmed_qrcode = Some(requested_qrcode.to_string());
            self.active_qrcode = None;
            self.active_issued_at = None;
            return "confirmed";
        }
        if self.active_qrcode.as_deref() == Some(requested_qrcode)
            && self.expired_qrcode().is_some()
        {
            return "expired";
        }
        if self.confirmed_qrcode.as_deref() == Some(requested_qrcode) {
            return "expired";
        }
        if let Some(current_qrcode) = current_qrcode {
            return if current_qrcode == requested_qrcode {
                "wait"
            } else {
                "expired"
            };
        }
        match login_status {
            LoginStatusKind::WaitingForKey | LoginStatusKind::KeyUnavailable => "scaned",
            LoginStatusKind::WaitingForLogin if has_key => "scaned",
            LoginStatusKind::WaitingForLogin => "wait",
            LoginStatusKind::LoggedIn => unreachable!(),
        }
    }
}

pub async fn get_updates(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetUpdatesRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let mut cursor = request.get_updates_buf.unwrap_or_default();
    state
        .wechat
        .validate_poll_cursor(&cursor)
        .map_err(|err| ApiError::bad_request(format!("invalid get_updates_buf: {err}")))?;
    let deadline = Instant::now() + GET_UPDATES_TIMEOUT;
    let result = loop {
        if !state.wechat.is_initialized() {
            if state.wechat.had_ready_session() {
                return Ok(Json(session_expired_response(&cursor)));
            }
            if Instant::now() >= deadline {
                return Ok(Json(session_expired_response(&cursor)));
            }
            sleep(GET_UPDATES_POLL_INTERVAL).await;
            continue;
        }
        let wechat = state.wechat.clone();
        let poll_cursor = cursor.clone();
        let poll =
            tokio::task::spawn_blocking(move || wechat.poll_messages(Some(&poll_cursor), 100))
                .await
                .map_err(|err| ApiError::Internal(format!("join message polling task: {err}")))?;
        match poll {
            Ok(result) if !result.messages.is_empty() => break Ok(result),
            Ok(result) if Instant::now() >= deadline => break Ok(result),
            Ok(result) => {
                cursor = result.cursor;
                sleep(GET_UPDATES_POLL_INTERVAL).await;
            }
            Err(err) => {
                tracing::warn!(error = %err, "wechat message polling failed");
                break Err(err);
            }
        }
    };
    let result = match result {
        Ok(result) => result,
        Err(_err) => {
            return Ok(Json(json!({
                "ret": -14,
                "errcode": -14,
                "errmsg": "wechat session is unavailable",
                "msgs": [],
                "get_updates_buf": cursor,
            })));
        }
    };
    let baseurl = ilink_base_url(&headers);
    let reminder_targets = result
        .messages
        .iter()
        .filter_map(remark_reminder_target)
        .collect::<HashSet<_>>();
    let view_state = state.clone();
    let messages = result.messages;
    let msgs = tokio::task::spawn_blocking(move || {
        messages
            .iter()
            .map(|message| standard_message_view(&view_state, message, &baseurl))
            .collect::<Vec<_>>()
    })
    .await
    .map_err(|err| ApiError::Internal(format!("join message mapping task: {err}")))?;
    schedule_remark_reminders(state, reminder_targets);
    Ok(Json(json!({
        "ret": 0,
        "msgs": msgs,
        "get_updates_buf": result.cursor,
        "longpolling_timeout_ms": GET_UPDATES_TIMEOUT_MS,
    })))
}

fn schedule_remark_reminders(state: Arc<AppState>, targets: HashSet<String>) {
    for target_id in targets {
        let scheduled = state
            .remark_reminders
            .lock()
            .is_ok_and(|mut pending| pending.insert(target_id.clone()));
        if !scheduled {
            continue;
        }
        let task_state = state.clone();
        tokio::spawn(async move {
            loop {
                match remind_missing_remark(&task_state, &target_id).await {
                    Ok(()) => break,
                    Err(err) => {
                        tracing::warn!(error = %err, target_id, "could not deliver remark reminder");
                        sleep(REMINDER_RETRY_INTERVAL).await;
                    }
                }
            }
            if let Ok(mut pending) = task_state.remark_reminders.lock() {
                pending.remove(&target_id);
            }
        });
    }
}

async fn remind_missing_remark(state: &AppState, target_id: &str) -> anyhow::Result<()> {
    let wechat = state.wechat.clone();
    let lookup_id = target_id.to_string();
    let identity =
        tokio::task::spawn_blocking(move || wechat.contact_identity(&lookup_id)).await??;
    let Some(identity) = identity.filter(|identity| !identity.has_remark) else {
        return Ok(());
    };
    let current_user_id = state.wechat.current_user_id()?;
    let marker = remark_reminder_marker(target_id);
    let wechat = state.wechat.clone();
    let history_target = current_user_id.clone();
    let history_marker = marker.clone();
    let already_reminded = tokio::task::spawn_blocking(move || {
        wechat.outgoing_text_contains(&history_target, &history_marker)
    })
    .await??;
    if already_reminded {
        return Ok(());
    }
    state
        .sender
        .lock()
        .await
        .send_text(
            current_user_id,
            remark_reminder_text(&marker, &identity.display),
        )
        .await?;
    Ok(())
}

fn remark_reminder_marker(target_id: &str) -> String {
    format!("wb-{target_id}")
}

fn remark_reminder_text(marker: &str, display: &str) -> String {
    format!("{marker} {display}")
}

fn remark_reminder_target(message: &Value) -> Option<String> {
    let room_id = value_str(message, "roomid");
    let target = if room_id.ends_with("@chatroom") {
        room_id
    } else {
        value_str(message, "from")
    };
    (!target.is_empty() && target != "filehelper").then(|| target.to_string())
}

fn session_expired_response(cursor: &str) -> Value {
    let mut response = session_unavailable_response();
    let object = response
        .as_object_mut()
        .expect("session response is always an object");
    object.insert("msgs".to_string(), json!([]));
    object.insert("get_updates_buf".to_string(), json!(cursor));
    response
}

fn session_unavailable_response() -> Value {
    json!({
        "ret": -14,
        "errcode": -14,
        "errmsg": "wechat session is unavailable",
    })
}

pub async fn send_message(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendMessageRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    reject_outbound_media(&request.msg)?;
    if !state.wechat.is_initialized() {
        return Ok(Json(session_unavailable_response()));
    }
    let request_client_id = normalized_client_id(request.msg.client_id.as_deref())?;
    let fingerprint = outbound_fingerprint(&request.msg)?;
    if let Some(client_id) = request_client_id.as_deref() {
        if let Some(receipt) = cached_send(&state, client_id, &fingerprint)? {
            return Ok(Json(send_success_response(&receipt)));
        }
    }
    let target = outbound_target(&state, &request.msg)?;
    let text = outbound_text(&request.msg)
        .ok_or_else(|| ApiError::bad_request("msg.text or text item is required"))?;
    let sender = state.sender.lock().await;
    if let Some(client_id) = request_client_id.as_deref() {
        if let Some(receipt) = cached_send(&state, client_id, &fingerprint)? {
            return Ok(Json(send_success_response(&receipt)));
        }
    }
    let receipt = sender
        .send_text(target, text)
        .await
        .map_err(|err| ApiError::Internal(err.to_string()))?;
    let client_msg_id = request_client_id.clone().unwrap_or(receipt.client_msg_id);
    if let Some(client_id) = request_client_id {
        remember_send(&state, client_id, fingerprint, client_msg_id.clone())?;
    }
    Ok(Json(send_success_response(&client_msg_id)))
}

fn normalized_client_id(raw: Option<&str>) -> Result<Option<String>, ApiError> {
    let Some(value) = raw.and_then(non_empty) else {
        return Ok(None);
    };
    if value.len() > 128 {
        return Err(ApiError::bad_request("msg.client_id is too long"));
    }
    Ok(Some(value))
}

fn outbound_fingerprint(message: &OutboundMessage) -> Result<String, ApiError> {
    let data = serde_json::to_vec(message)
        .map_err(|err| ApiError::Internal(format!("serialize outbound message: {err}")))?;
    Ok(format!("{:x}", Sha256::digest(data)))
}

fn cached_send(
    state: &AppState,
    client_id: &str,
    fingerprint: &str,
) -> Result<Option<String>, ApiError> {
    let cache = state
        .send_receipts
        .lock()
        .map_err(|_| ApiError::Internal("send receipt lock is poisoned".to_string()))?;
    let Some(cached) = cache.entries.get(client_id) else {
        return Ok(None);
    };
    if cached.fingerprint != fingerprint {
        return Err(ApiError::bad_request(
            "msg.client_id was already used for different content",
        ));
    }
    Ok(Some(cached.client_msg_id.clone()))
}

fn remember_send(
    state: &AppState,
    client_id: String,
    fingerprint: String,
    client_msg_id: String,
) -> Result<(), ApiError> {
    let mut cache = state
        .send_receipts
        .lock()
        .map_err(|_| ApiError::Internal("send receipt lock is poisoned".to_string()))?;
    if !cache.entries.contains_key(&client_id) {
        while cache.entries.len() >= MAX_SEND_RECEIPTS {
            let Some(candidate) = cache.order.pop_front() else {
                break;
            };
            cache.entries.remove(&candidate);
        }
        cache.order.push_back(client_id.clone());
    }
    cache.entries.insert(
        client_id,
        CachedSend {
            fingerprint,
            client_msg_id,
        },
    );
    Ok(())
}

fn send_success_response(client_msg_id: &str) -> Value {
    json!({
        "ret": 0,
        "client_msg_id": client_msg_id,
    })
}

pub async fn get_config(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetConfigRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let context_token = request.context_token.as_deref().and_then(non_empty);
    let context = context_token
        .as_deref()
        .map(|token| decode_context_token(&state, token))
        .transpose()
        .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
    let ilink_user_id = request
        .ilink_user_id
        .as_deref()
        .and_then(non_empty)
        .or_else(|| context.as_ref().and_then(|value| non_empty(&value.target)))
        .ok_or_else(|| ApiError::bad_request("ilink_user_id or context_token is required"))?;
    Ok(Json(json!({
        "ret": 0,
        "typing_ticket": typing_ticket_for(&state, &ilink_user_id),
    })))
}

pub async fn send_typing(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendTypingRequest>,
) -> Result<Json<Value>, ApiError> {
    authenticate(&state, &headers)?;
    let status = request
        .status
        .ok_or_else(|| ApiError::bad_request("status is required"))?;
    if status != 1 && status != 2 {
        return Err(ApiError::bad_request("status must be 1 or 2"));
    }
    let ticket = request
        .typing_ticket
        .as_deref()
        .and_then(non_empty)
        .ok_or_else(|| ApiError::bad_request("typing_ticket is required"))?;
    let ticket = decode_typing_ticket(&state, &ticket)
        .map_err(|err| ApiError::bad_request(format!("invalid typing_ticket: {err}")))?;
    if let Some(ilink_user_id) = request.ilink_user_id.as_deref().and_then(non_empty) {
        if ilink_user_id != ticket.ilink_user_id {
            return Err(ApiError::bad_request("typing_ticket user mismatch"));
        }
    }
    Err(ApiError::Unsupported(
        "WeChat Linux UI does not expose a reliable typing indicator action".to_string(),
    ))
}

pub async fn notify_connection(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(_request): Json<LifecycleRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    Ok(Json(json!({ "ret": 0 })))
}

pub async fn cdn_download(
    State(state): State<Arc<AppState>>,
    Query(query): Query<CdnDownloadQuery>,
) -> Result<impl IntoResponse, ApiError> {
    let data = state
        .media_store
        .read_encrypted(&query.encrypted_query_param)
        .map_err(|err| ApiError::bad_request(err.to_string()))?;
    let mut headers = HeaderMap::new();
    headers.insert(
        header::CONTENT_TYPE,
        HeaderValue::from_static("application/octet-stream"),
    );
    Ok((headers, data))
}

pub async fn not_found() -> impl IntoResponse {
    (
        StatusCode::NOT_FOUND,
        Json(json!({ "error": "not_found", "detail": "route not found" })),
    )
}

fn authenticate(state: &AppState, headers: &HeaderMap) -> Result<(), ApiError> {
    let auth_type = headers
        .get("authorizationtype")
        .and_then(|value| value.to_str().ok())
        .map(str::trim);
    if !auth_type.is_some_and(|value| value.eq_ignore_ascii_case("ilink_bot_token")) {
        return Err(ApiError::Unauthorized(
            "missing or invalid AuthorizationType".to_string(),
        ));
    }
    let uin = headers
        .get("x-wechat-uin")
        .and_then(|value| value.to_str().ok())
        .and_then(non_empty);
    if uin.is_none() {
        return Err(ApiError::Unauthorized("missing X-WECHAT-UIN".to_string()));
    }
    let token = headers
        .get(axum::http::header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.trim().strip_prefix("Bearer "))
        .map(str::trim);
    if token == Some(state.api_token.as_str()) {
        return Ok(());
    }
    Err(ApiError::Unauthorized(
        "missing or invalid bearer token".to_string(),
    ))
}

fn has_matching_local_token(tokens: &[String], expected: &str) -> bool {
    tokens.iter().any(|token| token.trim() == expected)
}

fn ilink_base_url(headers: &HeaderMap) -> String {
    let host = header_string(headers, "host").unwrap_or_else(|| "127.0.0.1:8080".to_string());
    format!("http://{host}")
}

fn normalize_base_url(value: &str) -> String {
    value.trim().trim_end_matches('/').to_string()
}

fn header_string(headers: &HeaderMap, name: &str) -> Option<String> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .and_then(non_empty)
}

fn typing_ticket_for(state: &AppState, ilink_user_id: &str) -> String {
    signed_payload::encode(
        &state.api_token,
        &TypingTicket {
            ilink_user_id: ilink_user_id.to_string(),
        },
    )
    .expect("typing ticket serializes")
}

fn decode_typing_ticket(state: &AppState, ticket: &str) -> anyhow::Result<TypingTicket> {
    signed_payload::decode(&state.api_token, ticket)
}

fn standard_message_view(state: &AppState, message: &Value, baseurl: &str) -> Value {
    let text = message_display_text(message);
    let created_time_ms = message_time_millis(message);
    let external_id = external_message_id(message);
    let message_id = external_id
        .parse::<i64>()
        .unwrap_or_else(|_| stable_positive_id(&external_id));
    let roomid = value_str(message, "roomid");
    let mut view = json!({
        "seq": message.get("local_id").and_then(Value::as_i64).unwrap_or(message_id),
        "message_id": message_id,
        "msgid": external_id,
        "client_id": external_id,
        "from_user_id": sender_id(message),
        "to_user_id": state.provider_account_id,
        "ilink_user_id": sender_id(message),
        "create_time_ms": created_time_ms,
        "update_time_ms": created_time_ms,
        "session_id": roomid,
        "message_type": 1,
        "message_state": 2,
        "context_token": context_token_for_target(state, roomid),
        "text": text,
        "item_list": standard_item_list(state, message, baseurl, &text),
        "wechat_msgtype": message_type(message),
    });
    if roomid.ends_with("@chatroom") {
        view["group_id"] = json!(roomid);
    }
    view
}

fn standard_item_list(state: &AppState, message: &Value, baseurl: &str, text: &str) -> Vec<Value> {
    let msg_type = message_type(message);
    if matches!(msg_type.as_str(), "image" | "video" | "emotion" | "file") {
        let roomid = value_str(message, "roomid");
        let msgid = value_str(message, "msgid");
        match state.wechat.read_media(roomid, msgid) {
            Ok(Some(media)) => match state.media_store.publish_plain(&media) {
                Ok(published) => {
                    let media_ref = json!({
                        "encrypt_query_param": published.token,
                        "aes_key": published.aes_key,
                        "encrypt_type": 1,
                        "full_url": format!(
                            "{}/c2c/download?encrypted_query_param={}",
                            normalize_base_url(baseurl),
                            published.token
                        ),
                    });
                    if published.content_type.starts_with("video/") || msg_type == "video" {
                        return vec![json!({
                            "type": 5,
                            "create_time_ms": message_time_millis(message),
                            "is_completed": true,
                            "msg_id": msgid,
                            "video_item": {
                                "media": media_ref,
                                "video_size": published.encrypted_size,
                            },
                        })];
                    }
                    if msg_type == "file" {
                        return vec![json!({
                            "type": 4,
                            "create_time_ms": message_time_millis(message),
                            "is_completed": true,
                            "msg_id": msgid,
                            "file_item": {
                                "media": media_ref,
                                "file_name": published.filename,
                                "len": published.encrypted_size,
                            },
                        })];
                    }
                    return vec![json!({
                        "type": 2,
                        "create_time_ms": message_time_millis(message),
                        "is_completed": true,
                        "msg_id": msgid,
                        "image_item": {
                            "media": media_ref,
                            "mid_size": published.encrypted_size,
                            "file_name": published.filename,
                        },
                    })];
                }
                Err(err) => tracing::warn!(error = %err, msgid, "could not publish local media"),
            },
            Ok(None) => {}
            Err(err) => tracing::warn!(error = %err, msgid, "could not read local media"),
        }
    }
    vec![json!({
        "type": TEXT_ITEM_TYPE,
        "create_time_ms": message_time_millis(message),
        "is_completed": true,
        "msg_id": external_message_id(message),
        "text_item": { "text": text },
    })]
}

fn outbound_target(state: &AppState, message: &OutboundMessage) -> Result<String, ApiError> {
    let token = message
        .context_token
        .as_deref()
        .and_then(non_empty)
        .ok_or_else(|| ApiError::bad_request("msg.context_token is required"))?;
    let context = decode_context_token(state, &token)
        .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
    non_empty(&context.target)
        .ok_or_else(|| ApiError::bad_request("msg.context_token has no outbound target"))
}

fn outbound_text(message: &OutboundMessage) -> Option<String> {
    message.text.as_deref().and_then(non_empty).or_else(|| {
        message
            .item_list
            .iter()
            .find_map(|item| item_text(item).and_then(|value| non_empty(&value)))
    })
}

fn reject_outbound_media(message: &OutboundMessage) -> Result<(), ApiError> {
    for item in &message.item_list {
        if ["image_item", "voice_item", "file_item", "video_item"]
            .iter()
            .any(|key| item.get(key).is_some())
        {
            return Err(ApiError::Unsupported(
                "binary media sending is not supported; send an external URL as text".to_string(),
            ));
        }
    }
    Ok(())
}

fn item_text(item: &Value) -> Option<String> {
    item.get("text")
        .and_then(Value::as_str)
        .or_else(|| {
            item.get("text_item")
                .and_then(|value| value.get("text"))
                .and_then(Value::as_str)
        })
        .map(ToString::to_string)
}

fn context_token_for_target(state: &AppState, target: &str) -> String {
    signed_payload::encode(
        &state.api_token,
        &ContextToken {
            target: target.to_string(),
        },
    )
    .expect("context token serializes")
}

fn decode_context_token(state: &AppState, token: &str) -> anyhow::Result<ContextToken> {
    signed_payload::decode(&state.api_token, token)
}

fn message_body(message: &Value) -> Value {
    let msg_type = message_type(message);
    if msg_type == "text" {
        if let Some(text) = message
            .get("text")
            .and_then(|value| value.get("content"))
            .and_then(Value::as_str)
        {
            return json!({ "text": text });
        }
    }
    message
        .get(&msg_type)
        .map(|body| json!({ msg_type: body }))
        .unwrap_or_else(|| json!({ "raw": message }))
}

fn message_display_text(message: &Value) -> String {
    let msg_type = message_type(message);
    let body = message_body(message);
    if let Some(text) = text_body(&body) {
        return text;
    }
    if let Some(text) = typed_body_text(message, &msg_type) {
        return text;
    }
    match msg_type.as_str() {
        "image" => "[图片]".to_string(),
        "voice" => "[语音]".to_string(),
        "video" => "[视频]".to_string(),
        "emotion" => "[表情]".to_string(),
        "location" => "[位置]".to_string(),
        "voip" => "[通话]".to_string(),
        "system" => "[系统消息]".to_string(),
        "revoke" => "[撤回了一条消息]".to_string(),
        "link" => link_display_text(message),
        "sphfeed" => sphfeed_display_text(message),
        "unknown" => "[消息]".to_string(),
        _ => format!("[{msg_type}]"),
    }
}

fn typed_body_text(message: &Value, msg_type: &str) -> Option<String> {
    message
        .get(msg_type)
        .and_then(|value| value.get("content"))
        .and_then(Value::as_str)
        .and_then(non_empty)
}

fn text_body(body: &Value) -> Option<String> {
    match body {
        Value::String(value) => non_empty(value),
        Value::Object(map) => map
            .get("text")
            .and_then(Value::as_str)
            .and_then(non_empty)
            .or_else(|| {
                map.get("content")
                    .and_then(Value::as_str)
                    .and_then(non_empty)
            }),
        _ => None,
    }
}

fn link_display_text(message: &Value) -> String {
    let Some(link) = message.get("link").and_then(Value::as_object) else {
        return "[链接]".to_string();
    };
    let title = link
        .get("title")
        .and_then(Value::as_str)
        .and_then(non_empty);
    let description = link
        .get("description")
        .and_then(Value::as_str)
        .and_then(non_empty);
    let url = link
        .get("link_url")
        .or_else(|| link.get("url"))
        .and_then(Value::as_str)
        .and_then(non_empty);
    display_join(
        "[链接]",
        [title.as_deref(), description.as_deref(), url.as_deref()],
    )
}

fn sphfeed_display_text(message: &Value) -> String {
    let Some(feed) = message.get("sphfeed").and_then(Value::as_object) else {
        return "[视频号]".to_string();
    };
    let name = feed
        .get("sph_name")
        .and_then(Value::as_str)
        .and_then(non_empty);
    let desc = feed
        .get("feed_desc")
        .and_then(Value::as_str)
        .and_then(non_empty);
    let url = feed.get("url").and_then(Value::as_str).and_then(non_empty);
    display_join(
        "[视频号]",
        [name.as_deref(), desc.as_deref(), url.as_deref()],
    )
}

fn display_join<const N: usize>(fallback: &str, parts: [Option<&str>; N]) -> String {
    let values = parts.into_iter().flatten().collect::<Vec<_>>();
    if values.is_empty() {
        fallback.to_string()
    } else {
        format!("{} {}", fallback, values.join("\n"))
    }
}

fn message_type(message: &Value) -> String {
    message
        .get("msgtype")
        .and_then(Value::as_str)
        .unwrap_or("text")
        .to_string()
}

fn message_time_millis(message: &Value) -> i64 {
    message
        .get("msgtime")
        .and_then(Value::as_i64)
        .unwrap_or_default()
}

fn external_message_id(message: &Value) -> String {
    let value = value_str(message, "msgid");
    if value.is_empty() {
        stable_positive_id(&format!(
            "{}:{}:{}",
            value_str(message, "roomid"),
            message_time_millis(message),
            message
                .get("local_id")
                .and_then(Value::as_i64)
                .unwrap_or_default()
        ))
        .to_string()
    } else {
        value.to_string()
    }
}

fn sender_id(message: &Value) -> String {
    value_str(message, "from").to_string()
}

fn value_str<'a>(value: &'a Value, key: &str) -> &'a str {
    value.get(key).and_then(Value::as_str).unwrap_or_default()
}

fn stable_positive_id(value: &str) -> i64 {
    let digest = md5::compute(value.as_bytes());
    let mut bytes = [0_u8; 8];
    bytes.copy_from_slice(&digest.0[..8]);
    i64::from_be_bytes(bytes) & i64::MAX
}

fn non_empty(value: &str) -> Option<String> {
    let value = value.trim();
    (!value.is_empty()).then(|| value.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn qrcode_status_expires_replaced_qrcode() {
        let mut session = LoginSession::default();
        session.register_qrcode("xvfb-qr-current");
        assert_eq!(
            session.status(
                "xvfb-qr-old",
                Some("xvfb-qr-current"),
                LoginStatusKind::WaitingForLogin,
                false,
            ),
            "expired"
        );
        assert_eq!(
            session.status(
                "xvfb-qr-current",
                Some("xvfb-qr-current"),
                LoginStatusKind::WaitingForLogin,
                false,
            ),
            "wait"
        );
    }

    #[test]
    fn qrcode_status_does_not_confirm_an_unknown_id() {
        let mut session = LoginSession::default();
        session.register_qrcode("known");

        assert_eq!(
            session.status("unknown", None, LoginStatusKind::LoggedIn, true),
            "expired"
        );
    }

    #[test]
    fn qrcode_status_confirms_the_exact_issued_id_after_login() {
        let mut session = LoginSession::default();
        session.register_qrcode("issued");

        assert_eq!(
            session.status("issued", None, LoginStatusKind::LoggedIn, true),
            "confirmed"
        );
        assert_eq!(
            session.status("other", None, LoginStatusKind::LoggedIn, true),
            "expired"
        );
    }

    #[test]
    fn qrcode_status_expires_an_old_unconfirmed_session() {
        let mut session = LoginSession::default();
        session.register_qrcode("issued");
        session.active_issued_at = Some(Instant::now() - QR_SESSION_TTL);

        assert_eq!(
            session.status(
                "issued",
                Some("issued"),
                LoginStatusKind::WaitingForLogin,
                false,
            ),
            "expired"
        );
    }

    #[test]
    fn resume_requires_the_current_local_token() {
        assert!(has_matching_local_token(
            &[" old ".to_string(), " current ".to_string()],
            "current"
        ));
        assert!(!has_matching_local_token(&["old".to_string()], "current"));
    }

    #[test]
    fn outbound_text_accepts_standard_text_item() {
        let message = OutboundMessage {
            client_id: None,
            context_token: None,
            text: None,
            item_list: vec![json!({ "type": 1, "text_item": { "text": "hello" } })],
        };

        assert_eq!(outbound_text(&message).as_deref(), Some("hello"));
    }

    #[test]
    fn remark_reminder_targets_private_sender() {
        let message = json!({ "from": "wxid_alice", "roomid": "wxid_alice" });

        assert_eq!(
            remark_reminder_target(&message).as_deref(),
            Some("wxid_alice")
        );
    }

    #[test]
    fn remark_reminder_targets_group_instead_of_member() {
        let message = json!({ "from": "wxid_alice", "roomid": "group@chatroom" });

        assert_eq!(
            remark_reminder_target(&message).as_deref(),
            Some("group@chatroom")
        );
    }

    #[test]
    fn remark_reminder_uses_stable_target_marker_and_original_name() {
        assert_eq!(remark_reminder_marker("wxid_alice"), "wb-wxid_alice");
        assert_eq!(
            remark_reminder_text("wb-wxid_alice", "Alice"),
            "wb-wxid_alice Alice"
        );
    }

    #[test]
    fn context_token_round_trips_room_target() {
        let state = test_state();
        let token = context_token_for_target(&state, "alice");
        let message = OutboundMessage {
            client_id: None,
            context_token: Some(token),
            text: Some("hello".to_string()),
            item_list: Vec::new(),
        };

        assert_eq!(outbound_target(&state, &message).unwrap(), "alice");
    }

    #[test]
    fn send_receipt_cache_replays_the_same_client_request() {
        let state = test_state();
        let message = OutboundMessage {
            client_id: Some("request-1".to_string()),
            context_token: Some("context".to_string()),
            text: Some("hello".to_string()),
            item_list: Vec::new(),
        };
        let fingerprint = outbound_fingerprint(&message).unwrap();
        remember_send(
            &state,
            "request-1".to_string(),
            fingerprint.clone(),
            "request-1".to_string(),
        )
        .unwrap();

        assert_eq!(
            cached_send(&state, "request-1", &fingerprint)
                .unwrap()
                .as_deref(),
            Some("request-1")
        );
    }

    #[test]
    fn send_receipt_cache_rejects_client_id_reuse_with_different_content() {
        let state = test_state();
        remember_send(
            &state,
            "request-1".to_string(),
            "first".to_string(),
            "request-1".to_string(),
        )
        .unwrap();

        assert!(matches!(
            cached_send(&state, "request-1", "second"),
            Err(ApiError::BadRequest(_))
        ));
    }

    #[test]
    fn send_receipt_cache_evicts_the_oldest_entry_at_capacity() {
        let state = test_state();
        for index in 0..=MAX_SEND_RECEIPTS {
            let id = format!("request-{index}");
            remember_send(&state, id.clone(), id.clone(), id).unwrap();
        }

        assert!(cached_send(&state, "request-0", "request-0")
            .unwrap()
            .is_none());
        assert!(cached_send(&state, "request-1", "request-1")
            .unwrap()
            .is_some());
    }

    #[test]
    fn context_token_rejects_tampering() {
        let state = test_state();
        let token = context_token_for_target(&state, "alice");
        let (payload, signature) = token.split_once('.').unwrap();
        let mut payload = payload.as_bytes().to_vec();
        payload[0] ^= 1;
        let tampered = format!("{}.{}", String::from_utf8(payload).unwrap(), signature);

        assert!(decode_context_token(&state, &tampered).is_err());
    }

    #[test]
    fn typing_ticket_round_trips_user() {
        let state = test_state();
        let ticket = typing_ticket_for(&state, "alice");
        let decoded = decode_typing_ticket(&state, &ticket).unwrap();

        assert_eq!(decoded.ilink_user_id, "alice");
    }

    #[test]
    fn context_token_rejects_legacy_fields() {
        let state = test_state();
        let token = signed_payload::encode(
            &state.api_token,
            &json!({
                "target": "alice",
                "room_type": "direct",
            }),
        )
        .unwrap();

        assert!(decode_context_token(&state, &token).is_err());
    }

    #[test]
    fn maps_wechat_message_to_standard_ilink_message() {
        let state = test_state();
        let message = json!({
            "msgid": "m1",
            "from": "wxid_a",
            "roomid": "alice",
            "msgtime": 1781703356000_i64,
            "msgtype": "text",
            "text": { "content": "hello" },
        });

        let view = standard_message_view(&state, &message, "http://127.0.0.1:8080");

        assert_eq!(view["client_id"], "m1");
        assert_eq!(view["from_user_id"], "wxid_a");
        assert_eq!(view["to_user_id"], "wx");
        assert_eq!(view["create_time_ms"], 1781703356000_i64);
        assert_eq!(view["text"], "hello");
        assert_eq!(view["item_list"][0]["text_item"]["text"], "hello");
    }

    #[test]
    fn maps_non_text_wechat_message_to_readable_ilink_text() {
        let state = test_state();
        let message = json!({
            "msgid": "m2",
            "from": "wxid_a",
            "roomid": "alice",
            "msgtime": 1781703356000_i64,
            "msgtype": "image",
            "image": { "content": "[图片]" },
        });

        let view = standard_message_view(&state, &message, "http://127.0.0.1:8080");

        assert_eq!(view["text"], "[图片]");
        assert_eq!(view["item_list"][0]["text_item"]["text"], "[图片]");
        assert_eq!(view["wechat_msgtype"], "image");
    }

    #[test]
    fn maps_link_wechat_message_to_readable_ilink_text() {
        let state = test_state();
        let message = json!({
            "msgid": "m3",
            "from": "wxid_a",
            "roomid": "alice",
            "msgtime": 1781703356000_i64,
            "msgtype": "link",
            "link": {
                "title": "Protocol",
                "description": "iLink docs",
                "link_url": "https://www.wechatbot.dev/zh/protocol"
            },
        });

        let view = standard_message_view(&state, &message, "http://127.0.0.1:8080");

        assert_eq!(
            view["text"],
            "[链接] Protocol\niLink docs\nhttps://www.wechatbot.dev/zh/protocol"
        );
        assert_eq!(
            view["item_list"][0]["text_item"]["text"],
            "[链接] Protocol\niLink docs\nhttps://www.wechatbot.dev/zh/protocol"
        );
    }

    fn test_state() -> AppState {
        let wechat = WechatState::new(std::env::temp_dir().join("webox-ilink-test"), "test-token");
        AppState {
            api_token: "token".to_string(),
            provider_account_id: "wx".to_string(),
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new(None),
            media_store: MediaStore::new(
                std::env::temp_dir().join(format!("webox-ilink-media-{}", uuid::Uuid::new_v4())),
            ),
            login_session: Arc::new(Mutex::new(LoginSession::default())),
            send_receipts: Arc::new(Mutex::new(Default::default())),
            remark_reminders: Arc::new(Mutex::new(Default::default())),
            wechat,
        }
    }
}
