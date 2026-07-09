mod config;
mod error;
mod ilink;
mod qr_source;
mod ui_sender;
#[allow(dead_code)]
mod wechat_db;
mod wechat_state;

use crate::config::Config;
use crate::ilink::AppState;
use crate::qr_source::QrSource;
use crate::ui_sender::UiSender;
use crate::wechat_state::WechatState;
use anyhow::Context;
use axum::routing::{get, post};
use axum::Router;
use std::sync::Arc;
use tokio::net::TcpListener;
use tower_http::trace::TraceLayer;
use tracing::warn;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "webox=info,tower_http=info".into()),
        )
        .json()
        .init();

    let config = Config::from_env();
    if config.api_token == "webox" {
        warn!("using default WEBOX_API_TOKEN; set a strong token before exposing the service");
    }

    let wechat = WechatState::new(config.state_dir.clone());
    wechat.ensure_state_dir()?;
    let state = Arc::new(AppState {
        api_token: config.api_token.clone(),
        tenant_id: config.tenant_id.clone(),
        provider_account_id: config.provider_account_id.clone(),
        sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
        qr_source: QrSource::new(
            config.agentgateway_api_base.clone(),
            config.qr_match_terms.clone(),
        ),
        wechat,
    });

    let app = build_router(state);

    let listener = TcpListener::bind(&config.listen_addr)
        .await
        .with_context(|| format!("bind {}", config.listen_addr))?;
    tracing::info!("weagent listening on {}", config.listen_addr);
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

fn build_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(ilink::health))
        .route("/ilink/sendmessage", post(ilink::send_message))
        .route("/ilink/getupdates", post(ilink::get_updates))
        .route("/ilink/ack", post(ilink::ack))
        .route("/ilink/login/qrcode/latest", get(ilink::latest_qrcode))
        .route("/ilink/login/qrcode/events", get(ilink::qrcode_events))
        .fallback(ilink::not_found)
        .layer(TraceLayer::new_for_http())
        .with_state(state)
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::body::{to_bytes, Body};
    use axum::http::{Request, StatusCode};
    use serde_json::Value;
    use tower::ServiceExt;

    #[tokio::test]
    async fn health_does_not_expose_internal_cursor() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/healthz")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["ok"], true);
        assert!(body.get("cursor").is_none());
    }

    #[tokio::test]
    async fn legacy_login_qrcode_routes_are_not_exposed() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/login/qrcode/latest")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn ilink_login_qrcode_route_is_exposed() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/login/qrcode/latest")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }

    fn test_state() -> Arc<AppState> {
        let state_dir = std::env::temp_dir().join(format!("webox-router-{}", uuid::Uuid::new_v4()));
        let wechat = WechatState::new(state_dir);
        Arc::new(AppState {
            api_token: "token".to_string(),
            tenant_id: "default".to_string(),
            provider_account_id: "wx".to_string(),
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]),
            wechat,
        })
    }
}
