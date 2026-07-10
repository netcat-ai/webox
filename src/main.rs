mod config;
mod error;
mod ilink;
mod media_store;
mod qr_source;
mod ui_sender;
#[allow(dead_code)]
mod wechat_db;
mod wechat_state;

use crate::config::Config;
use crate::ilink::AppState;
use crate::media_store::{MediaStore, MAX_MEDIA_UPLOAD_BYTES};
use crate::qr_source::QrSource;
use crate::ui_sender::UiSender;
use crate::wechat_state::WechatState;
use anyhow::Context;
use axum::extract::DefaultBodyLimit;
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
                .unwrap_or_else(|_| "weagent=info,tower_http=info".into()),
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
        qr_source: QrSource::new(config.qr_screenshot_path.clone()),
        media_store: MediaStore::new(config.media_dir.clone()),
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
        .route("/get_bot_qrcode", get(ilink::get_bot_qrcode))
        .route("/get_qrcode_status", get(ilink::get_qrcode_status))
        .route("/getupdates", post(ilink::get_updates))
        .route("/sendmessage", post(ilink::send_message))
        .route("/getconfig", post(ilink::get_config))
        .route("/sendtyping", post(ilink::send_typing))
        .route("/msg/notifystart", post(ilink::notify_start))
        .route("/msg/notifystop", post(ilink::notify_stop))
        .route("/getuploadurl", post(ilink::get_upload_url))
        .route(
            "/c2c/upload",
            post(ilink::cdn_upload).layer(DefaultBodyLimit::max(MAX_MEDIA_UPLOAD_BYTES)),
        )
        .route("/c2c/download", get(ilink::cdn_download))
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
                    .uri("/get_bot_qrcode?bot_type=3")
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
    async fn legacy_ilink_bot_routes_are_not_exposed() {
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

        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn standard_login_qrcode_rejects_post() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/get_bot_qrcode?bot_type=3")
                    .header("iLink-App-Id", "bot")
                    .header("iLink-App-ClientVersion", "131072")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"local_token_list":["token"]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::METHOD_NOT_ALLOWED);
    }

    #[tokio::test]
    async fn standard_login_status_reports_waiting_state() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/get_qrcode_status?qrcode=qrc_test")
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
            Some("https://public.example".to_string()),
        ));

        let response = app
            .oneshot(
                Request::builder()
                    .uri("/get_qrcode_status?qrcode=qrc_test")
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
        assert_eq!(body["baseurl"], "https://public.example");
    }

    #[tokio::test]
    async fn standard_getupdates_requires_bearer_token() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getupdates")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"get_updates_buf":""}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
    }

    #[tokio::test]
    async fn standard_posts_require_authorization_type() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getupdates")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"get_updates_buf":""}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "unauthorized");
        assert!(body["detail"]
            .as_str()
            .is_some_and(|value| value.contains("AuthorizationType")));
    }

    #[tokio::test]
    async fn standard_posts_require_x_wechat_uin() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getupdates")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"get_updates_buf":""}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::UNAUTHORIZED);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "unauthorized");
        assert!(body["detail"]
            .as_str()
            .is_some_and(|value| value.contains("X-WECHAT-UIN")));
    }

    #[tokio::test]
    async fn standard_getconfig_returns_typing_ticket() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getconfig")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
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
                    .uri("/getconfig")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
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
                    .uri("/sendtyping")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
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
    async fn standard_getuploadurl_returns_local_upload_url() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getuploadurl")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(upload_url_body(16)))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["ret"], 0);
        assert!(body["upload_param"]
            .as_str()
            .is_some_and(|value| !value.is_empty()));
        assert!(body["upload_full_url"]
            .as_str()
            .is_some_and(|value| value.starts_with("http://127.0.0.1:8080/c2c/upload?")));
    }

    #[tokio::test]
    async fn local_cdn_upload_and_download_round_trips_encrypted_bytes() {
        let app = build_router(test_state());
        let getupload = app
            .clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/getuploadurl")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(upload_url_body(16)))
                    .unwrap(),
            )
            .await
            .unwrap();
        let body: Value =
            serde_json::from_slice(&to_bytes(getupload.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        let upload_param = body["upload_param"].as_str().unwrap();
        let encrypted = vec![9_u8; 16];
        let upload = app
            .clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri(format!(
                        "/c2c/upload?encrypted_query_param={upload_param}&filekey=filekey123"
                    ))
                    .body(Body::from(encrypted.clone()))
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(upload.status(), StatusCode::OK);
        let download_param = upload
            .headers()
            .get("x-encrypted-param")
            .and_then(|value| value.to_str().ok())
            .unwrap()
            .to_string();

        let download = app
            .oneshot(
                Request::builder()
                    .uri(format!(
                        "/c2c/download?encrypted_query_param={download_param}"
                    ))
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(download.status(), StatusCode::OK);
        assert_eq!(
            to_bytes(download.into_body(), usize::MAX).await.unwrap(),
            encrypted
        );
    }

    #[tokio::test]
    async fn standard_lifecycle_notifications_accept_bearer_token() {
        let app = build_router(test_state());
        for path in ["/msg/notifystart", "/msg/notifystop"] {
            let response = app
                .clone()
                .oneshot(
                    Request::builder()
                        .method("POST")
                        .uri(path)
                        .header("AuthorizationType", "ilink_bot_token")
                        .header("X-WECHAT-UIN", "default")
                        .header("authorization", "Bearer token")
                        .header("content-type", "application/json")
                        .body(Body::from(r#"{"base_info":{"channel_version":"2.0.0"}}"#))
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

    fn upload_url_body(filesize: u64) -> String {
        format!(
            r#"{{
                "filekey":"filekey123",
                "media_type":1,
                "to_user_id":"alice",
                "rawsize":11,
                "rawfilemd5":"5eb63bbbe01eeed093cb22bb8f5acdc3",
                "filesize":{filesize},
                "no_need_thumb":true,
                "aeskey":"11111111111111111111111111111111",
                "base_info":{{"channel_version":"2.0.0"}}
            }}"#
        )
    }

    fn test_state() -> Arc<AppState> {
        let state_dir = std::env::temp_dir().join(format!("webox-router-{}", uuid::Uuid::new_v4()));
        test_state_with_dir(state_dir, Some("http://127.0.0.1:8080".to_string()))
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
            qr_source: QrSource::new(None),
            media_store: MediaStore::new(
                std::env::temp_dir().join(format!("webox-router-media-{}", uuid::Uuid::new_v4())),
            ),
            wechat,
        })
    }
}
