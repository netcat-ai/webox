use crate::error::ApiError;
use crate::media_store::{GetUploadUrlRequest, MediaKind, MediaStore, PlainMedia};
use crate::qr_source::{LoginQrCode, QrSource};
use crate::ui_sender::UiSender;
use crate::wechat_state::{message_update_id, LoginStatusKind, WechatState};
use axum::body::Bytes;
use axum::extract::{Query, State};
use axum::http::{header, HeaderMap, HeaderValue, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::Arc;
use tokio::time::{sleep, Duration, Instant};

const ILINK_BASE_PATH: &str = "/ilink/bot";
const TEXT_ITEM_TYPE: i64 = 1;
const GET_UPDATES_TIMEOUT: Duration = Duration::from_secs(35);
const GET_UPDATES_POLL_INTERVAL: Duration = Duration::from_secs(1);
const GET_UPDATES_TIMEOUT_MS: i64 = 35_000;

#[derive(Clone)]
pub struct AppState {
    pub api_token: String,
    pub tenant_id: String,
    pub provider_account_id: String,
    pub public_base_url: Option<String>,
    pub wechat: WechatState,
    pub sender: Arc<tokio::sync::Mutex<UiSender>>,
    pub qr_source: QrSource,
    pub media_store: MediaStore,
}

#[derive(Debug, Deserialize)]
pub struct BotQrcodeQuery {
    #[serde(default)]
    pub bot_type: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct QrcodeStatusQuery {
    pub qrcode: String,
    #[serde(default)]
    pub verify_code: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct GetUpdatesRequest {
    #[serde(default)]
    pub get_updates_buf: Option<String>,
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Deserialize)]
pub struct SendMessageRequest {
    pub msg: OutboundMessage,
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Deserialize)]
pub struct GetConfigRequest {
    #[serde(default)]
    pub ilink_user_id: Option<String>,
    #[serde(default)]
    pub context_token: Option<String>,
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Deserialize)]
pub struct SendTypingRequest {
    #[serde(default)]
    pub ilink_user_id: Option<String>,
    #[serde(default)]
    pub typing_ticket: Option<String>,
    #[serde(default)]
    pub status: Option<i64>,
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Deserialize)]
pub struct LifecycleRequest {
    #[serde(default)]
    pub base_info: Option<Value>,
}

#[derive(Debug, Deserialize)]
pub struct CdnUploadQuery {
    pub encrypted_query_param: String,
    pub filekey: String,
}

#[derive(Debug, Deserialize)]
pub struct CdnDownloadQuery {
    pub encrypted_query_param: String,
}

#[derive(Debug, Deserialize)]
pub struct OutboundMessage {
    #[serde(default)]
    pub context_token: Option<String>,
    #[serde(default)]
    pub text: Option<String>,
    #[serde(default)]
    pub item_list: Vec<Value>,
}

#[derive(Debug, Serialize, Deserialize)]
struct UpdatesCursor {
    v: u8,
    last_update_id: i64,
}

#[derive(Debug, Serialize, Deserialize)]
struct TypingTicket {
    v: u8,
    tenant_id: String,
    provider_account_id: String,
    ilink_user_id: String,
    context_token: String,
}

#[derive(Debug, Serialize, Deserialize)]
struct ContextToken {
    v: u8,
    tenant_id: String,
    channel: String,
    provider_account_id: String,
    external_room_id: String,
    room_type: String,
    display_name: String,
    outbound_target: String,
}

pub async fn health(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    Json(json!({
        "ok": true,
        "hasWechatKey": state.wechat.has_key(),
    }))
}

pub async fn get_bot_qrcode(
    State(state): State<Arc<AppState>>,
    Query(query): Query<BotQrcodeQuery>,
) -> Result<impl IntoResponse, ApiError> {
    let _bot_type = query.bot_type.as_deref().unwrap_or("3");
    let event = match state.qr_source.latest().await {
        Ok(event) => event,
        Err(err) => {
            tracing::warn!(error = %err, "qrcode capture source unavailable");
            None
        }
    };
    let qrcode = event.map(|event| event.to_login_qrcode());
    Ok(Json(json!({
        "qrcode": qrcode.as_ref().map(|value| value.id.as_str()).unwrap_or_default(),
        "qrcode_img_content": qrcode.as_ref().and_then(qrcode_content).unwrap_or_default(),
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
    let _verify_code = query.verify_code.as_deref();
    let login = state.wechat.login_status(true);
    let mut response = serde_json::Map::new();
    let status = match login.status {
        LoginStatusKind::LoggedIn => "confirmed",
        LoginStatusKind::WaitingForKey | LoginStatusKind::KeyUnavailable => "scaned",
        LoginStatusKind::WaitingForLogin => "wait",
    };
    response.insert("status".to_string(), json!(status));
    if status == "confirmed" {
        response.insert("bot_token".to_string(), json!(state.api_token));
        response.insert("ilink_bot_id".to_string(), json!(state.provider_account_id));
        response.insert(
            "ilink_user_id".to_string(),
            json!(state.provider_account_id),
        );
        response.insert(
            "baseurl".to_string(),
            json!(ilink_base_url(&state, &headers)),
        );
    }
    Ok(Json(Value::Object(response)))
}

pub async fn get_updates(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetUpdatesRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    let after_id = decode_updates_buf(request.get_updates_buf.as_deref())?;
    let deadline = Instant::now() + GET_UPDATES_TIMEOUT;
    let result = loop {
        match state.wechat.poll_messages_after_id(after_id, 100) {
            Ok(result) if !result.messages.is_empty() => break Ok(result),
            Ok(result) if Instant::now() >= deadline => break Ok(result),
            Ok(_) => sleep(GET_UPDATES_POLL_INTERVAL).await,
            Err(err) => break Err(err),
        }
    };
    let result = match result {
        Ok(result) => result,
        Err(err) => {
            return Ok(Json(json!({
                "ret": -14,
                "errcode": -14,
                "errmsg": err.to_string(),
                "msgs": [],
                "get_updates_buf": request.get_updates_buf.unwrap_or_default(),
            })));
        }
    };
    let mut last_update_id = after_id;
    let msgs = result
        .messages
        .iter()
        .map(|message| {
            let update_id = message_update_id(message);
            last_update_id = last_update_id.max(update_id);
            standard_message_view(&state, message)
        })
        .collect::<Vec<_>>();
    Ok(Json(json!({
        "ret": 0,
        "msgs": msgs,
        "get_updates_buf": encode_updates_buf(last_update_id),
        "longpolling_timeout_ms": GET_UPDATES_TIMEOUT_MS,
    })))
}

pub async fn send_message(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendMessageRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    let target = outbound_target(&request.msg)?;
    let text = outbound_text(&request.msg);
    let media_items = outbound_media_items(&state.media_store, &request.msg)?;
    if text.is_none() && media_items.is_empty() {
        return Err(ApiError::bad_request(
            "msg.text, text item, or media item is required",
        ));
    }
    let mut client_msg_id = None;
    if let Some(text) = text {
        let receipt = state
            .sender
            .lock()
            .await
            .send_text(target.clone(), text)
            .await
            .map_err(|err| ApiError::Internal(err.to_string()))?;
        client_msg_id = Some(receipt.client_msg_id);
    }
    for media in media_items {
        let receipt = state
            .sender
            .lock()
            .await
            .send_file(target.clone(), media.filename, media.data)
            .await
            .map_err(|err| ApiError::Internal(err.to_string()))?;
        client_msg_id = Some(receipt.client_msg_id);
    }
    Ok(Json(json!({
        "ret": 0,
        "client_msg_id": client_msg_id.unwrap_or_default(),
    })))
}

pub async fn get_config(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetConfigRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    let context_token = request.context_token.as_deref().and_then(non_empty);
    if let Some(token) = context_token.as_deref() {
        decode_context_token(token)
            .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
    }
    let ilink_user_id = request
        .ilink_user_id
        .as_deref()
        .and_then(non_empty)
        .or_else(|| {
            context_token
                .as_deref()
                .and_then(|token| decode_context_token(token).ok())
                .and_then(|context| non_empty(&context.external_room_id))
        })
        .ok_or_else(|| ApiError::bad_request("ilink_user_id or context_token is required"))?;
    Ok(Json(json!({
        "ret": 0,
        "typing_ticket": typing_ticket_for(&state, &ilink_user_id, context_token.as_deref()),
    })))
}

pub async fn send_typing(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendTypingRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
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
    let ticket = decode_typing_ticket(&ticket)
        .map_err(|err| ApiError::bad_request(format!("invalid typing_ticket: {err}")))?;
    if ticket.provider_account_id != state.provider_account_id {
        return Err(ApiError::bad_request("typing_ticket account mismatch"));
    }
    if let Some(ilink_user_id) = request.ilink_user_id.as_deref().and_then(non_empty) {
        if ilink_user_id != ticket.ilink_user_id {
            return Err(ApiError::bad_request("typing_ticket user mismatch"));
        }
    }
    Ok(Json(json!({ "ret": 0 })))
}

pub async fn notify_start(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<LifecycleRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    Ok(Json(json!({ "ret": 0 })))
}

pub async fn notify_stop(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<LifecycleRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    Ok(Json(json!({ "ret": 0 })))
}

pub async fn get_upload_url(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetUploadUrlRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    let upload = state
        .media_store
        .prepare_upload(&request)
        .map_err(|err| ApiError::bad_request(err.to_string()))?;
    let baseurl = ilink_base_url(&state, &headers);
    Ok(Json(json!({
        "ret": 0,
        "upload_param": upload.upload_param,
        "upload_full_url": format!(
            "{}/c2c/upload?encrypted_query_param={}&filekey={}",
            baseurl, upload.upload_param, upload.filekey
        ),
    })))
}

pub async fn cdn_upload(
    State(state): State<Arc<AppState>>,
    Query(query): Query<CdnUploadQuery>,
    body: Bytes,
) -> Result<impl IntoResponse, ApiError> {
    let stored = state
        .media_store
        .store_upload(&query.encrypted_query_param, &query.filekey, &body)
        .map_err(|err| ApiError::bad_request(err.to_string()))?;
    let mut headers = HeaderMap::new();
    headers.insert(
        "x-encrypted-param",
        HeaderValue::from_str(&stored.token).map_err(|err| ApiError::Internal(err.to_string()))?,
    );
    Ok((headers, Json(json!({ "ret": 0 }))))
}

pub async fn cdn_download(
    State(state): State<Arc<AppState>>,
    Query(query): Query<CdnDownloadQuery>,
) -> Result<impl IntoResponse, ApiError> {
    let (_meta, data) = state
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

fn qrcode_content(qrcode: &LoginQrCode) -> Option<String> {
    qrcode
        .image_data_uri
        .as_ref()
        .or(qrcode.login_url.as_ref())
        .or(Some(&qrcode.source_url))
        .and_then(|value| non_empty(value))
}

fn ilink_base_url(state: &AppState, headers: &HeaderMap) -> String {
    if let Some(value) = state.public_base_url.as_deref().and_then(non_empty) {
        return normalize_base_url(&value);
    }
    let proto = header_string(headers, "x-forwarded-proto").unwrap_or_else(|| "http".to_string());
    let host = header_string(headers, "x-forwarded-host")
        .or_else(|| header_string(headers, "host"))
        .unwrap_or_else(|| "127.0.0.1:8080".to_string());
    format!("{}://{}", proto, host)
}

fn normalize_base_url(value: &str) -> String {
    let value = value.trim().trim_end_matches('/');
    value
        .strip_suffix(ILINK_BASE_PATH)
        .unwrap_or(value)
        .trim_end_matches('/')
        .to_string()
}

fn header_string(headers: &HeaderMap, name: &str) -> Option<String> {
    headers
        .get(name)
        .and_then(|value| value.to_str().ok())
        .and_then(non_empty)
}

fn decode_updates_buf(raw: Option<&str>) -> Result<i64, ApiError> {
    let Some(raw) = raw.and_then(non_empty) else {
        return Ok(0);
    };
    if let Ok(value) = raw.parse::<i64>() {
        return Ok(value.max(0));
    }
    let bytes = URL_SAFE_NO_PAD
        .decode(raw)
        .map_err(|err| ApiError::bad_request(format!("invalid get_updates_buf: {err}")))?;
    let cursor: UpdatesCursor = serde_json::from_slice(&bytes)
        .map_err(|err| ApiError::bad_request(format!("invalid get_updates_buf: {err}")))?;
    if cursor.v != 1 {
        return Err(ApiError::bad_request("unsupported get_updates_buf version"));
    }
    Ok(cursor.last_update_id.max(0))
}

fn encode_updates_buf(last_update_id: i64) -> String {
    URL_SAFE_NO_PAD.encode(
        serde_json::to_vec(&UpdatesCursor {
            v: 1,
            last_update_id,
        })
        .expect("updates cursor serializes"),
    )
}

fn typing_ticket_for(state: &AppState, ilink_user_id: &str, context_token: Option<&str>) -> String {
    URL_SAFE_NO_PAD.encode(
        serde_json::to_vec(&TypingTicket {
            v: 1,
            tenant_id: state.tenant_id.clone(),
            provider_account_id: state.provider_account_id.clone(),
            ilink_user_id: ilink_user_id.to_string(),
            context_token: context_token.unwrap_or_default().to_string(),
        })
        .expect("typing ticket serializes"),
    )
}

fn decode_typing_ticket(ticket: &str) -> anyhow::Result<TypingTicket> {
    let bytes = URL_SAFE_NO_PAD.decode(ticket.trim())?;
    let ticket: TypingTicket = serde_json::from_slice(&bytes)?;
    if ticket.v != 1 {
        anyhow::bail!("unsupported version");
    }
    Ok(ticket)
}

fn standard_message_view(state: &AppState, message: &Value) -> Value {
    let room = room_view(state, message);
    let text = message_display_text(message);
    let created_time_ms = message_time_millis(message);
    let external_id = external_message_id(message);
    json!({
        "msgid": external_id,
        "client_id": external_id,
        "from_user_id": sender_id(message),
        "to_user_id": state.provider_account_id,
        "ilink_user_id": sender_id(message),
        "create_time": created_time_ms / 1000,
        "create_time_ms": created_time_ms,
        "message_type": 1,
        "message_state": 2,
        "context_token": context_token_for_room(&room),
        "text": text,
        "item_list": [{
            "type": TEXT_ITEM_TYPE,
            "text_item": { "text": text },
        }],
        "wechat_msgtype": message_type(message),
    })
}

fn outbound_target(message: &OutboundMessage) -> Result<String, ApiError> {
    let token = message
        .context_token
        .as_deref()
        .and_then(non_empty)
        .ok_or_else(|| ApiError::bad_request("msg.context_token is required"))?;
    let context = decode_context_token(&token)
        .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
    non_empty(&context.outbound_target)
        .or_else(|| non_empty(&context.external_room_id))
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

fn outbound_media_items(
    media_store: &MediaStore,
    message: &OutboundMessage,
) -> Result<Vec<PlainMedia>, ApiError> {
    let mut out = Vec::new();
    for item in &message.item_list {
        if let Some(media) = item_media(item, "image_item", "aeskey")? {
            out.push(
                media_store
                    .read_plain_media(&media, MediaKind::Image, None)
                    .map_err(|err| ApiError::bad_request(format!("invalid image media: {err}")))?,
            );
        }
        if let Some(media) = item_media(item, "voice_item", "aeskey")? {
            out.push(
                media_store
                    .read_plain_media(&media, MediaKind::Voice, None)
                    .map_err(|err| ApiError::bad_request(format!("invalid voice media: {err}")))?,
            );
        }
        if let Some((media, filename)) = item_file_media(item)? {
            out.push(
                media_store
                    .read_plain_media(&media, MediaKind::File, filename.as_deref())
                    .map_err(|err| ApiError::bad_request(format!("invalid file media: {err}")))?,
            );
        }
        if let Some(media) = item_media(item, "video_item", "aeskey")? {
            out.push(
                media_store
                    .read_plain_media(&media, MediaKind::Video, None)
                    .map_err(|err| ApiError::bad_request(format!("invalid video media: {err}")))?,
            );
        }
    }
    Ok(out)
}

fn item_media(item: &Value, item_key: &str, key_override: &str) -> Result<Option<Value>, ApiError> {
    let Some(body) = item.get(item_key) else {
        return Ok(None);
    };
    let Some(media) = body.get("media") else {
        return Err(ApiError::bad_request(format!(
            "{item_key}.media is required"
        )));
    };
    let mut media = media.clone();
    if media.get("aes_key").is_none() {
        if let Some(aeskey) = body.get(key_override) {
            media["aes_key"] = aeskey.clone();
        }
    }
    Ok(Some(media))
}

fn item_file_media(item: &Value) -> Result<Option<(Value, Option<String>)>, ApiError> {
    let Some(body) = item.get("file_item") else {
        return Ok(None);
    };
    let Some(media) = body.get("media") else {
        return Err(ApiError::bad_request("file_item.media is required"));
    };
    let filename = body
        .get("file_name")
        .and_then(Value::as_str)
        .and_then(non_empty);
    Ok(Some((media.clone(), filename)))
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

fn room_view(state: &AppState, message: &Value) -> Value {
    let external_room_id = message
        .get("roomid")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    room_view_from_target(
        state,
        &external_room_id,
        None,
        external_room_id.ends_with("@chatroom"),
    )
}

fn room_view_from_target(
    state: &AppState,
    external_room_id: &str,
    display_name: Option<&str>,
    is_group: bool,
) -> Value {
    let room_type = if is_group { "group" } else { "direct" };
    json!({
        "id": stable_positive_id(external_room_id),
        "tenant_id": state.tenant_id,
        "channel": "wechat",
        "provider_account_id": state.provider_account_id,
        "external_room_id": external_room_id,
        "room_type": room_type,
        "display_name": display_name.unwrap_or(external_room_id),
        "outbound_target": external_room_id,
    })
}

fn context_token_for_room(room: &Value) -> String {
    let context = ContextToken {
        v: 1,
        tenant_id: value_str(room, "tenant_id").to_string(),
        channel: value_str(room, "channel").to_string(),
        provider_account_id: value_str(room, "provider_account_id").to_string(),
        external_room_id: value_str(room, "external_room_id").to_string(),
        room_type: value_str(room, "room_type").to_string(),
        display_name: value_str(room, "display_name").to_string(),
        outbound_target: value_str(room, "outbound_target").to_string(),
    };
    URL_SAFE_NO_PAD.encode(serde_json::to_vec(&context).expect("context token serializes"))
}

fn decode_context_token(token: &str) -> anyhow::Result<ContextToken> {
    let bytes = URL_SAFE_NO_PAD.decode(token.trim())?;
    let context: ContextToken = serde_json::from_slice(&bytes)?;
    if context.v != 1 {
        anyhow::bail!("unsupported version");
    }
    Ok(context)
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
        message_update_id(message).to_string()
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
    fn updates_cursor_round_trips() {
        let encoded = encode_updates_buf(42);

        assert_eq!(decode_updates_buf(Some(&encoded)).unwrap(), 42);
        assert_eq!(decode_updates_buf(Some("")).unwrap(), 0);
    }

    #[test]
    fn outbound_text_accepts_standard_text_item() {
        let message = OutboundMessage {
            context_token: None,
            text: None,
            item_list: vec![json!({ "type": 1, "text_item": { "text": "hello" } })],
        };

        assert_eq!(outbound_text(&message).as_deref(), Some("hello"));
    }

    #[test]
    fn context_token_round_trips_room_target() {
        let room = json!({
            "tenant_id": "default",
            "channel": "wechat",
            "provider_account_id": "wx",
            "external_room_id": "alice",
            "room_type": "direct",
            "display_name": "Alice",
            "outbound_target": "alice",
        });
        let token = context_token_for_room(&room);
        let message = OutboundMessage {
            context_token: Some(token),
            text: Some("hello".to_string()),
            item_list: Vec::new(),
        };

        assert_eq!(outbound_target(&message).unwrap(), "alice");
    }

    #[test]
    fn typing_ticket_round_trips_user_and_context() {
        let state = test_state();
        let ticket = typing_ticket_for(&state, "alice", Some("ctx"));
        let decoded = decode_typing_ticket(&ticket).unwrap();

        assert_eq!(decoded.tenant_id, "default");
        assert_eq!(decoded.provider_account_id, "wx");
        assert_eq!(decoded.ilink_user_id, "alice");
        assert_eq!(decoded.context_token, "ctx");
    }

    #[test]
    fn baseurl_normalization_returns_service_root() {
        assert_eq!(
            normalize_base_url("https://public.example/ilink/bot/"),
            "https://public.example"
        );
        assert_eq!(
            normalize_base_url("https://public.example/proxy/ilink/bot"),
            "https://public.example/proxy"
        );
        assert_eq!(
            normalize_base_url("https://public.example"),
            "https://public.example"
        );
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

        let view = standard_message_view(&state, &message);

        assert_eq!(view["client_id"], "m1");
        assert_eq!(view["from_user_id"], "wxid_a");
        assert_eq!(view["to_user_id"], "wx");
        assert_eq!(view["create_time_ms"], 1781703356000_i64);
        assert_eq!(view["create_time"], 1781703356_i64);
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

        let view = standard_message_view(&state, &message);

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

        let view = standard_message_view(&state, &message);

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
        let wechat = WechatState::new(std::env::temp_dir().join("webox-ilink-test"));
        AppState {
            api_token: "token".to_string(),
            tenant_id: "default".to_string(),
            provider_account_id: "wx".to_string(),
            public_base_url: Some("http://127.0.0.1:8080/ilink/bot".to_string()),
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]),
            media_store: MediaStore::new(
                std::env::temp_dir().join(format!("webox-ilink-media-{}", uuid::Uuid::new_v4())),
            ),
            wechat,
        }
    }
}
