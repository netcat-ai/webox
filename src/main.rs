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
        public_base_url: config.public_base_url.clone(),
        sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
        qr_source: QrSource::new(
            config.agentgateway_api_base.clone(),
            config.qr_match_terms.clone(),
        )
        .with_log_path(config.agentgateway_log_path.clone()),
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
        .route(
            "/get_bot_qrcode",
            get(ilink::get_bot_qrcode).post(ilink::get_bot_qrcode),
        )
        .route("/get_qrcode_status", get(ilink::get_qrcode_status))
        .route("/getupdates", post(ilink::get_updates))
        .route("/sendmessage", post(ilink::send_message))
        .route("/getconfig", post(ilink::get_config))
        .route("/sendtyping", post(ilink::send_typing))
        .route(
            "/ilink/bot/get_bot_qrcode",
            get(ilink::get_bot_qrcode).post(ilink::get_bot_qrcode),
        )
        .route(
            "/ilink/bot/get_qrcode_status",
            get(ilink::get_qrcode_status),
        )
        .route("/ilink/bot/getupdates", post(ilink::get_updates))
        .route("/ilink/bot/sendmessage", post(ilink::send_message))
        .route("/ilink/bot/getconfig", post(ilink::get_config))
        .route("/ilink/bot/sendtyping", post(ilink::send_typing))
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
    use std::fs;
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
                    .uri("/ilink/login/qrcode/latest")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn standard_login_qrcode_route_is_exposed_without_auth() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/bot/get_bot_qrcode?bot_type=3")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["qrcode"], "");
        assert_eq!(body["qrcode_img_content"], "");
    }

    #[tokio::test]
    async fn standard_login_status_reports_waiting_state() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/bot/get_qrcode_status?qrcode=qrc_test")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["status"], "wait");
        assert!(body.get("bot_token").is_none());
    }

    #[tokio::test]
    async fn standard_login_status_confirms_with_key_material() {
        let state_dir =
            std::env::temp_dir().join(format!("webox-router-login-{}", uuid::Uuid::new_v4()));
        let db_dir = state_dir.join("db_storage");
        fs::create_dir_all(&db_dir).unwrap();
        fs::write(
            state_dir.join("wechat.key"),
            serde_json::to_vec(&serde_json::json!({
                "version": 1,
                "wxid": "wxid_test",
                "key": "webox-weagent",
                "source": "test",
                "keysFile": null,
                "dbDir": db_dir.to_string_lossy(),
                "keys": { "message/msg_0.db": "00".repeat(32) },
                "createdAt": 1,
                "updatedAt": 2
            }))
            .unwrap(),
        )
        .unwrap();
        let app = build_router(test_state_with_dir(
            state_dir.clone(),
            Some("https://public.example/ilink/bot".to_string()),
        ));

        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/bot/get_qrcode_status?qrcode=qrc_test")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        fs::remove_dir_all(state_dir).ok();
        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["status"], "confirmed");
        assert_eq!(body["bot_token"], "token");
        assert_eq!(body["baseurl"], "https://public.example/ilink/bot");
    }

    #[tokio::test]
    async fn standard_getupdates_requires_bearer_token() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getupdates")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"get_updates_buf":""}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn standard_getconfig_returns_typing_ticket() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getconfig")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"ilink_user_id":"alice","base_info":{"channel_version":"2.0.0"}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["ret"], 0);
        assert!(body["typing_ticket"]
            .as_str()
            .is_some_and(|value| !value.is_empty()));
    }

    #[tokio::test]
    async fn standard_sendtyping_accepts_generated_ticket() {
        let app = build_router(test_state());
        let getconfig = app
            .clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getconfig")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"ilink_user_id":"alice"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();
        let body: Value =
            serde_json::from_slice(&to_bytes(getconfig.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        let ticket = body["typing_ticket"].as_str().unwrap();

        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/sendtyping")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(format!(
                        r#"{{"ilink_user_id":"alice","typing_ticket":"{ticket}","status":1}}"#
                    )))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["ret"], 0);
    }

    #[tokio::test]
    async fn old_custom_updates_route_is_not_exposed() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/getupdates")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"after_id":0}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    fn test_state() -> Arc<AppState> {
        let state_dir = std::env::temp_dir().join(format!("webox-router-{}", uuid::Uuid::new_v4()));
        test_state_with_dir(
            state_dir,
            Some("http://127.0.0.1:8080/ilink/bot".to_string()),
        )
    }

    fn test_state_with_dir(
        state_dir: std::path::PathBuf,
        public_base_url: Option<String>,
    ) -> Arc<AppState> {
        let wechat = WechatState::new(state_dir);
        Arc::new(AppState {
            api_token: "token".to_string(),
            tenant_id: "default".to_string(),
            provider_account_id: "wx".to_string(),
            public_base_url,
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]),
            wechat,
        })
    }
}
