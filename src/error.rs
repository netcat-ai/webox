use axum::http::StatusCode;
use axum::response::{IntoResponse, Response};
use axum::Json;
use serde_json::json;

#[derive(Debug, thiserror::Error)]
pub enum ApiError {
    #[error("{0}")]
    BadRequest(String),
    #[error("{0}")]
    Unauthorized(String),
    #[error("{0}")]
    Internal(String),
    #[error("{0}")]
    Unsupported(String),
    #[error("{0}")]
    Unavailable(String),
}

impl ApiError {
    pub fn bad_request(value: impl Into<String>) -> Self {
        Self::BadRequest(value.into())
    }
}

impl IntoResponse for ApiError {
    fn into_response(self) -> Response {
        let (status, code, detail) = match &self {
            ApiError::BadRequest(detail) => {
                (StatusCode::BAD_REQUEST, "invalid_request", detail.as_str())
            }
            ApiError::Unauthorized(detail) => {
                (StatusCode::UNAUTHORIZED, "unauthorized", detail.as_str())
            }
            ApiError::Unsupported(detail) => {
                (StatusCode::NOT_IMPLEMENTED, "unsupported", detail.as_str())
            }
            ApiError::Unavailable(detail) => (
                StatusCode::SERVICE_UNAVAILABLE,
                "unavailable",
                detail.as_str(),
            ),
            ApiError::Internal(detail) => {
                tracing::error!(error = %detail, "request failed with an internal error");
                return (
                    StatusCode::INTERNAL_SERVER_ERROR,
                    Json(json!({ "error": "internal_error", "detail": "internal server error" })),
                )
                    .into_response();
            }
        };
        (status, Json(json!({ "error": code, "detail": detail }))).into_response()
    }
}

impl From<anyhow::Error> for ApiError {
    fn from(value: anyhow::Error) -> Self {
        Self::Internal(value.to_string())
    }
}
