use crate::error::ApiError;
use crate::qr_source::QrSource;
use crate::ui_sender::UiSender;
use crate::wechat_state::{message_update_id, WechatState};
use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use base64::engine::general_purpose::URL_SAFE_NO_PAD;
use base64::Engine;
use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::Arc;

#[derive(Clone)]
pub struct AppState {
    pub api_token: String,
    pub tenant_id: String,
    pub provider_account_id: String,
    pub wechat: WechatState,
    pub sender: Arc<tokio::sync::Mutex<UiSender>>,
    pub qr_source: QrSource,
}

#[derive(Debug, Deserialize)]
pub struct SendMessageRequest {
    #[serde(default)]
    pub context_token: Option<String>,
    #[serde(default)]
    pub room: RoomInput,
    #[serde(default)]
    pub message_type: Option<String>,
    #[serde(default)]
    pub body: Value,
}

#[derive(Debug, Default, Deserialize)]
pub struct RoomInput {
    #[serde(default)]
    pub external_room_id: Option<String>,
    #[serde(default)]
    pub outbound_target: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct SendMessageResponse {
    pub accepted: bool,
    pub message: Value,
}

#[derive(Debug, Deserialize)]
pub struct GetUpdatesRequest {
    #[serde(default)]
    pub after_id: Option<i64>,
    #[serde(default)]
    pub limit: Option<usize>,
}

#[derive(Debug, Deserialize)]
pub struct AckRequest {
    #[serde(default)]
    pub update_ids: Vec<i64>,
    #[serde(default)]
    pub task_results: Vec<TaskResultInput>,
}

#[derive(Debug, Deserialize)]
pub struct TaskResultInput {
    pub task_id: i64,
    pub status: String,
    #[serde(default)]
    pub error: Option<String>,
}

#[derive(Debug, Serialize)]
pub struct AckTaskView {
    pub id: i64,
    pub status: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error: Option<String>,
}

#[derive(Debug, Deserialize)]
pub struct LimitQuery {
    #[serde(default)]
    pub limit: Option<usize>,
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
        "cursor": state.wechat.current_cursor(),
    }))
}

pub async fn send_message(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<SendMessageRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let message_type = request.message_type.as_deref().unwrap_or("text");
    if message_type != "text" {
        return Err(ApiError::bad_request(
            "only text messages are supported initially",
        ));
    }
    let target = send_target(&request)?.ok_or_else(|| {
        ApiError::bad_request(
            "context_token, room.outbound_target, or room.external_room_id is required",
        )
    })?;
    let text = text_body(&request.body)
        .ok_or_else(|| ApiError::bad_request("body.text or string body is required"))?;
    let receipt = state
        .sender
        .lock()
        .await
        .send_text(target, text)
        .await
        .map_err(|err| ApiError::Internal(err.to_string()))?;
    Ok(Json(SendMessageResponse {
        accepted: true,
        message: serde_json::to_value(receipt)
            .map_err(|err| ApiError::internal(err.to_string()))?,
    }))
}

pub async fn get_updates(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<GetUpdatesRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let result = state
        .wechat
        .poll_messages_after_id(request.after_id.unwrap_or(0), request.limit.unwrap_or(100))
        .map_err(|err| ApiError::Internal(err.to_string()))?;
    let updates = result
        .messages
        .iter()
        .map(|message| {
            let room = room_view(&state, message);
            let body = message_body(message);
            let id = message_update_id(message);
            let created_at = message_time(message);
            json!({
                "id": id,
                "event_type": "message.received",
                "resource_type": "message",
                "resource_id": id,
                "payload": {
                    "context_token": context_token_for_room(&room),
                    "room": room,
                    "message": {
                        "id": id,
                        "room_id": room["id"],
                        "external_message_id": external_message_id(message),
                        "sender_id": sender_id(message),
                        "sender_name": sender_name(message),
                        "message_time": created_at,
                        "message_type": message_type(message),
                        "body": body,
                        "created_at": created_at,
                    },
                },
                "created_at": created_at,
            })
        })
        .collect::<Vec<_>>();
    Ok(Json(json!({ "updates": updates })))
}

pub async fn ack(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<AckRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let tasks = request
        .task_results
        .into_iter()
        .map(|task| AckTaskView {
            id: task.task_id,
            status: task.status,
            error: task.error,
        })
        .collect::<Vec<_>>();
    Ok(Json(json!({
        "acked_update_ids": request.update_ids,
        "tasks": tasks,
    })))
}

pub async fn latest_qrcode(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let event = state
        .qr_source
        .latest()
        .await
        .map_err(|err| ApiError::Upstream(err.to_string()))?;
    let qrcode = event.as_ref().map(|event| event.to_login_qrcode());
    Ok(Json(json!({
        "found": qrcode.is_some(),
        "qrcode": qrcode,
        "event": event,
    })))
}

pub async fn qrcode_events(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Query(query): Query<LimitQuery>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    let events = state
        .qr_source
        .recent(query.limit.unwrap_or(50))
        .await
        .map_err(|err| ApiError::Upstream(err.to_string()))?;
    let qrcodes = events
        .iter()
        .map(|event| event.to_login_qrcode())
        .collect::<Vec<_>>();
    Ok(Json(json!({ "qrcodes": qrcodes, "events": events })))
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

fn send_target(request: &SendMessageRequest) -> Result<Option<String>, ApiError> {
    if let Some(target) = request
        .room
        .outbound_target
        .as_ref()
        .or(request.room.external_room_id.as_ref())
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
    {
        return Ok(Some(target));
    }
    let Some(token) = request.context_token.as_deref() else {
        return Ok(None);
    };
    let context = decode_context_token(token)
        .map_err(|err| ApiError::bad_request(format!("invalid context_token: {err}")))?;
    Ok(non_empty(&context.outbound_target).or_else(|| non_empty(&context.external_room_id)))
}

fn room_view(state: &AppState, message: &Value) -> Value {
    let external_room_id = message
        .get("roomid")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_string();
    let room_type = if external_room_id.ends_with("@chatroom") {
        "group"
    } else {
        "direct"
    };
    json!({
        "id": stable_positive_id(&external_room_id),
        "tenant_id": state.tenant_id,
        "channel": "wechat",
        "provider_account_id": state.provider_account_id,
        "external_room_id": external_room_id,
        "room_type": room_type,
        "display_name": external_room_id,
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

fn message_type(message: &Value) -> String {
    message
        .get("msgtype")
        .and_then(Value::as_str)
        .unwrap_or("text")
        .to_string()
}

fn message_time(message: &Value) -> String {
    let millis = message
        .get("msgtime")
        .and_then(Value::as_i64)
        .unwrap_or_default();
    DateTime::<Utc>::from_timestamp_millis(millis)
        .unwrap_or(DateTime::<Utc>::UNIX_EPOCH)
        .to_rfc3339()
}

fn external_message_id(message: &Value) -> String {
    value_str(message, "msgid").to_string()
}

fn sender_id(message: &Value) -> String {
    value_str(message, "from").to_string()
}

fn sender_name(message: &Value) -> String {
    sender_id(message)
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
    fn extracts_text_body() {
        assert_eq!(text_body(&json!("hello")).as_deref(), Some("hello"));
        assert_eq!(
            text_body(&json!({ "text": "hello" })).as_deref(),
            Some("hello")
        );
        assert_eq!(
            text_body(&json!({ "content": "hello" })).as_deref(),
            Some("hello")
        );
        assert_eq!(text_body(&json!({ "text": "" })), None);
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
        let request = SendMessageRequest {
            context_token: Some(token),
            room: RoomInput::default(),
            message_type: Some("text".to_string()),
            body: json!({ "text": "hello" }),
        };

        assert_eq!(send_target(&request).unwrap().as_deref(), Some("alice"));
    }

    #[test]
    fn maps_wechat_message_to_ilink_body() {
        let message = json!({
            "msgid": "m1",
            "from": "wxid_a",
            "roomid": "alice",
            "msgtime": 1781703356000_i64,
            "msgtype": "text",
            "text": { "content": "hello" },
        });

        assert_eq!(message_body(&message), json!({ "text": "hello" }));
        assert_eq!(message_time(&message), "2026-06-17T13:35:56+00:00");
    }
}
