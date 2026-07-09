use crate::error::ApiError;
use crate::qr_source::QrSource;
use crate::ui_sender::UiSender;
use crate::wechat_state::WechatState;
use axum::extract::{Query, State};
use axum::http::{HeaderMap, StatusCode};
use axum::response::IntoResponse;
use axum::Json;
use serde::{Deserialize, Serialize};
use serde_json::{json, Value};
use std::sync::Arc;

#[derive(Clone)]
pub struct AppState {
    pub api_token: String,
    pub wechat: WechatState,
    pub sender: Arc<tokio::sync::Mutex<UiSender>>,
    pub qr_source: QrSource,
}

#[derive(Debug, Deserialize)]
pub struct SendMessageRequest {
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
    pub cursor: Option<String>,
    #[serde(default)]
    pub after_id: Option<i64>,
    #[serde(default)]
    pub limit: Option<usize>,
}

#[derive(Debug, Deserialize)]
pub struct AckRequest {
    #[serde(default)]
    pub update_ids: Vec<Value>,
    #[serde(default)]
    pub task_results: Vec<Value>,
}

#[derive(Debug, Deserialize)]
pub struct LimitQuery {
    #[serde(default)]
    pub limit: Option<usize>,
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
    let target = request
        .room
        .outbound_target
        .or(request.room.external_room_id)
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .ok_or_else(|| {
            ApiError::bad_request("room.outbound_target or room.external_room_id is required")
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
    let cursor = request.cursor.as_deref();
    let result = state
        .wechat
        .poll_messages(cursor, request.limit.unwrap_or(100))
        .map_err(|err| ApiError::Internal(err.to_string()))?;
    let updates = result
        .messages
        .iter()
        .enumerate()
        .map(|(idx, message)| {
            let fallback_id = request.after_id.unwrap_or_default() + idx as i64 + 1;
            json!({
                "id": message.get("id").cloned().unwrap_or_else(|| json!(fallback_id)),
                "event_type": "message.received",
                "resource_type": "message",
                "resource_id": message.get("id").cloned().unwrap_or_else(|| json!(fallback_id)),
                "payload": message,
            })
        })
        .collect::<Vec<_>>();
    Ok(Json(json!({
        "cursor": result.cursor,
        "updates": updates,
        "messages": result.messages,
        "meta": result.meta,
    })))
}

pub async fn ack(
    State(state): State<Arc<AppState>>,
    headers: HeaderMap,
    Json(request): Json<AckRequest>,
) -> Result<impl IntoResponse, ApiError> {
    authenticate(&state, &headers)?;
    Ok(Json(json!({
        "acked_update_ids": request.update_ids,
        "tasks": request.task_results,
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
    Ok(Json(json!({ "found": event.is_some(), "event": event })))
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
    Ok(Json(json!({ "events": events })))
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
}
