use crate::error::ApiError;
use crate::qr_source::{LoginQrCode, QrSource};
use crate::ui_sender::UiSender;
use crate::wechat_state::{message_update_id, LoginStatusKind, WechatState};
use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::Arc;

const ILINK_BASE_PATH: &str = "/ilink/bot";
const TEXT_ITEM_TYPE: i64 = 1;

#[derive(Clone)]
pub struct AppState {
    pub api_token: String,
    pub tenant_id: String,
    pub provider_account_id: String,
    pub public_base_url: Option<String>,
    pub wechat: WechatState,
    pub sender: Arc<tokio::sync::Mutex<UiSender>>,
    pub qr_source: QrSource,
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
pub struct OutboundMessage {
    #[serde(default)]
    pub to_user_id: Option<String>,
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
    let result = match state.wechat.poll_messages_after_id(after_id, 100) {
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
    })))
}

pub async fn send_message(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendMessageRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let _base_info = request.base_info.as_ref();
    let target = outbound_target(&request.msg)?
        .ok_or_else(|| ApiError::bad_request("msg.context_token or msg.to_user_id is required"))?;
    let text = outbound_text(&request.msg)
        .ok_or_else(|| ApiError::bad_request("msg.text or text item is required"))?;
    let receipt = state
        .sender
        .lock()
        .await
        .send_text(target, text)
        .await
        .map_err(|err| ApiError::Internal(err.to_string()))?;
    Ok(Json(json!({
        "ret": 0,
        "client_msg_id": receipt.client_msg_id,
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

pub async fn not_found() -> impl IntoResponse {
    (
        StatusCode::NOT_FOUND,
        Json(json!({ "error": "not_found", "detail": "route not found" })),
    )
}

fn authenticate(state: &AppState, headers: &HeaderMap) -> Result<(), ApiError> {
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
        return value.trim_end_matches('/').to_string();
    }
    let proto = header_string(headers, "x-forwarded-proto").unwrap_or_else(|| "http".to_string());
    let host = header_string(headers, "x-forwarded-host")
        .or_else(|| header_string(headers, "host"))
        .unwrap_or_else(|| "127.0.0.1:8080".to_string());
    format!("{}://{}{}", proto, host, ILINK_BASE_PATH)
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
    let body = message_body(message);
    let text = text_body(&body).unwrap_or_else(|| body.to_string());
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

fn outbound_target(message: &OutboundMessage) -> Result<Option<String>, ApiError> {
    if let Some(token) = message.context_token.as_deref().and_then(non_empty) {
        let context = decode_context_token(&token)
            .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
        return Ok(
            non_empty(&context.outbound_target).or_else(|| non_empty(&context.external_room_id))
        );
    }
    Ok(message.to_user_id.as_deref().and_then(non_empty))
}

fn outbound_text(message: &OutboundMessage) -> Option<String> {
    message.text.as_deref().and_then(non_empty).or_else(|| {
        message
            .item_list
            .iter()
            .find_map(|item| item_text(item).and_then(|value| non_empty(&value)))
    })
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
            to_user_id: Some("alice".to_string()),
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
            to_user_id: None,
            context_token: Some(token),
            text: Some("hello".to_string()),
            item_list: Vec::new(),
        };

        assert_eq!(outbound_target(&message).unwrap().as_deref(), Some("alice"));
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

    fn test_state() -> AppState {
        let wechat = WechatState::new(std::env::temp_dir().join("webox-ilink-test"));
        AppState {
            api_token: "token".to_string(),
            tenant_id: "default".to_string(),
            provider_account_id: "wx".to_string(),
            public_base_url: Some("http://127.0.0.1:8080/ilink/bot".to_string()),
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]),
            wechat,
        }
    }
}
