mod config;
mod error;
mod ilink;
mod media_store;
mod qr_source;
mod signed_payload;
mod ui_sender;
mod wechat_db;
mod wechat_state;

use crate::config::Config;
use crate::ilink::{AppState, LoginSession};
use crate::media_store::MediaStore;
use crate::qr_source::QrSource;
use crate::ui_sender::UiSender;
use crate::wechat_state::{InitializationState, WechatState};
use anyhow::Context;
use axum::routing::{get, post};
use axum::Router;
use std::sync::Arc;
use tokio::net::TcpListener;
use tower_http::trace::TraceLayer;

#[derive(Default)]
struct PostLoginUiState {
    dismissed: bool,
}

impl PostLoginUiState {
    fn should_dismiss(&mut self, state: InitializationState) -> bool {
        match state {
            InitializationState::WaitingForLogin => {
                self.dismissed = false;
                false
            }
            InitializationState::Ready => !self.dismissed,
        }
    }

    fn mark_dismissed(&mut self) {
        self.dismissed = true;
    }
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "weagent=info,tower_http=info".into()),
        )
        .json()
        .init();

    let config = Config::from_env()?;
    let wechat = WechatState::new(config.state_dir.clone(), config.api_token.clone());
    wechat.ensure_state_dir()?;
    let qr_source = QrSource::new(config.qr_screenshot_path.clone());
    let mut initializer = spawn_wechat_initializer(wechat.clone(), qr_source.clone());
    let state = Arc::new(AppState {
        api_token: config.api_token.clone(),
        provider_account_id: config.provider_account_id.clone(),
        sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
        qr_source,
        media_store: MediaStore::new(config.media_dir.clone()),
        login_session: Arc::new(std::sync::Mutex::new(LoginSession::default())),
        send_receipts: Arc::new(std::sync::Mutex::new(Default::default())),
        remark_reminders: Arc::new(std::sync::Mutex::new(Default::default())),
        wechat,
    });

    let app = build_router(state);

    let listener = TcpListener::bind(&config.listen_addr)
        .await
        .with_context(|| format!("bind {}", config.listen_addr))?;
    tracing::info!("weagent listening on {}", config.listen_addr);
    let server = axum::serve(listener, app).with_graceful_shutdown(shutdown_signal());
    tokio::select! {
        result = server => {
            initializer.abort();
            result?;
        }
        result = &mut initializer => {
            return Err(anyhow::anyhow!("wechat initializer exited unexpectedly: {result:?}"));
        }
    }
    Ok(())
}

fn spawn_wechat_initializer(
    wechat: WechatState,
    qr_source: QrSource,
) -> tokio::task::JoinHandle<()> {
    tokio::spawn(async move {
        tokio::time::sleep(std::time::Duration::from_secs(3)).await;
        let mut ready_logged = false;
        let mut no_qr_checks = 0_u8;
        let mut post_login_ui = PostLoginUiState::default();
        loop {
            let state = wechat.clone();
            let init = tokio::task::spawn_blocking(move || state.initialize_if_ready()).await;
            match init {
                Ok(Ok(InitializationState::Ready)) => {
                    if post_login_ui.should_dismiss(InitializationState::Ready) {
                        tokio::time::sleep(std::time::Duration::from_millis(300)).await;
                        let state = wechat.clone();
                        match tokio::task::spawn_blocking(move || {
                            state.dismiss_post_login_overlay()
                        })
                        .await
                        {
                            Ok(Ok(true)) => {
                                post_login_ui.mark_dismissed();
                                tracing::info!("dismissed post-login WeChat overlay");
                            }
                            Ok(Ok(false)) => {}
                            Ok(Err(err)) => {
                                tracing::warn!(error = %err, "could not dismiss post-login WeChat overlay")
                            }
                            Err(err) => {
                                tracing::warn!(error = %err, "post-login overlay task failed")
                            }
                        }
                    }
                    if !ready_logged {
                        tracing::info!("wechat automatic initialization is ready");
                        ready_logged = true;
                    }
                    no_qr_checks = 0;
                }
                Ok(Ok(InitializationState::WaitingForLogin)) => {
                    ready_logged = false;
                    post_login_ui.should_dismiss(InitializationState::WaitingForLogin);
                    let qr_visible = match qr_source.latest().await {
                        Ok(qr) => qr.is_some(),
                        Err(error) => {
                            tracing::warn!(error = %error, "could not inspect WeChat login QR code");
                            false
                        }
                    };
                    if qr_visible {
                        no_qr_checks = 0;
                    } else {
                        no_qr_checks = no_qr_checks.saturating_add(1);
                        if no_qr_checks >= 3 {
                            let state = wechat.clone();
                            match tokio::task::spawn_blocking(move || {
                                state.click_saved_account_login()
                            })
                            .await
                            {
                                Ok(Ok(true)) => {
                                    tracing::info!("activated saved-account WeChat login");
                                }
                                Ok(Ok(false)) => {}
                                Ok(Err(err)) => {
                                    tracing::warn!(error = %err, "could not activate saved-account login")
                                }
                                Err(err) => {
                                    tracing::warn!(error = %err, "saved-account login task failed")
                                }
                            }
                            no_qr_checks = 0;
                        }
                    }
                }
                Ok(Err(err)) => {
                    ready_logged = false;
                    let detail = format!("{err:#}");
                    wechat.record_init_error(detail.clone());
                    tracing::warn!(error = %detail, "wechat automatic initialization is not ready");
                    tokio::time::sleep(std::time::Duration::from_secs(2)).await;
                }
                Err(err) => {
                    ready_logged = false;
                    wechat.record_init_error(err.to_string());
                    tracing::warn!(error = %err, "wechat automatic initialization task failed");
                }
            }
            tokio::time::sleep(std::time::Duration::from_secs(1)).await;
        }
    })
}

fn build_router(state: Arc<AppState>) -> Router {
    Router::new()
        .route("/healthz", get(ilink::health))
        .route(
            "/ilink/bot/get_bot_qrcode",
            get(ilink::get_bot_qrcode_without_tokens).post(ilink::get_bot_qrcode),
        )
        .route(
            "/ilink/bot/get_qrcode_status",
            get(ilink::get_qrcode_status),
        )
        .route("/ilink/bot/getupdates", post(ilink::get_updates))
        .route("/ilink/bot/sendmessage", post(ilink::send_message))
        .route("/ilink/bot/getconfig", post(ilink::get_config))
        .route("/ilink/bot/sendtyping", post(ilink::send_typing))
        .route("/ilink/bot/msg/notifystart", post(ilink::notify_connection))
        .route("/ilink/bot/msg/notifystop", post(ilink::notify_connection))
        .route("/c2c/download", get(ilink::cdn_download))
        .fallback(ilink::not_found)
        .layer(TraceLayer::new_for_http())
        .with_state(state)
}

async fn shutdown_signal() {
    #[cfg(unix)]
    {
        let mut terminate =
            tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
                .expect("install SIGTERM handler");
        tokio::select! {
            _ = tokio::signal::ctrl_c() => {},
            _ = terminate.recv() => {},
        }
    }
    #[cfg(not(unix))]
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

    #[test]
    fn post_login_escape_runs_once_per_login_session() {
        let mut ui = PostLoginUiState::default();

        assert!(!ui.should_dismiss(InitializationState::WaitingForLogin));
        assert!(ui.should_dismiss(InitializationState::Ready));
        ui.mark_dismissed();
        assert!(!ui.should_dismiss(InitializationState::Ready));

        assert!(!ui.should_dismiss(InitializationState::WaitingForLogin));
        assert!(ui.should_dismiss(InitializationState::Ready));
    }

    #[test]
    fn post_login_escape_retries_until_it_succeeds() {
        let mut ui = PostLoginUiState::default();

        assert!(ui.should_dismiss(InitializationState::Ready));
        assert!(ui.should_dismiss(InitializationState::Ready));
    }

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
        assert!(body.get("hasWechatKey").is_none());
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
    async fn standard_login_qrcode_waits_for_a_real_wechat_qrcode() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/get_bot_qrcode?bot_type=3")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"local_token_list":[]}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "unavailable");
    }

    #[tokio::test]
    async fn root_qrcode_route_is_not_exposed() {
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

        assert_eq!(response.status(), StatusCode::NOT_FOUND);
    }

    #[tokio::test]
    async fn documented_get_login_qrcode_is_supported() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/bot/get_bot_qrcode?bot_type=3")
                    .header("iLink-App-Id", "bot")
                    .header("iLink-App-ClientVersion", "131072")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::SERVICE_UNAVAILABLE);
    }

    #[tokio::test]
    async fn login_qrcode_rejects_unsupported_bot_type() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .uri("/ilink/bot/get_bot_qrcode?bot_type=2")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
    }

    #[tokio::test]
    async fn standard_login_status_reports_waiting_state() {
        let state = test_state();
        state
            .login_session
            .lock()
            .unwrap()
            .register_qrcode("qrc_test");
        let app = build_router(state);
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
    async fn login_status_does_not_release_token_for_unknown_qrcode() {
        let state_dir =
            std::env::temp_dir().join(format!("webox-router-login-{}", uuid::Uuid::new_v4()));
        let db_dir = state_dir.join("db_storage");
        fs::create_dir_all(&db_dir).unwrap();
        fs::write(
            state_dir.join("wechat.key"),
            serde_json::to_vec(&serde_json::json!({
                "wxid": "wxid_test",
                "dbDir": db_dir.to_string_lossy(),
                "keys": { "message/msg_0.db": "00".repeat(32) }
            }))
            .unwrap(),
        )
        .unwrap();
        let app = build_router(test_state_with_dir(state_dir.clone()));

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
        assert_eq!(body["status"], "expired");
        assert!(body.get("bot_token").is_none());
    }

    #[tokio::test]
    async fn standard_getupdates_requires_bearer_token() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getupdates")
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
    async fn standard_getupdates_rejects_an_invalid_cursor_before_long_polling() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getupdates")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"get_updates_buf":"tampered"}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::BAD_REQUEST);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "invalid_request");
    }

    #[tokio::test]
    async fn standard_sendmessage_reports_unavailable_wechat_session() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/sendmessage")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(r#"{"msg":{"text":"hello"}}"#))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::OK);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["ret"], -14);
        assert_eq!(body["errcode"], -14);
    }

    #[tokio::test]
    async fn standard_posts_require_authorization_type() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getupdates")
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
                    .uri("/ilink/bot/getupdates")
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
                    .uri("/ilink/bot/getconfig")
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
    async fn standard_sendtyping_reports_unsupported_semantics() {
        let app = build_router(test_state());
        let getconfig = app
            .clone()
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/getconfig")
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
                    .uri("/ilink/bot/sendtyping")
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

        assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "unsupported");
    }

    #[tokio::test]
    async fn outbound_upload_routes_are_not_exposed() {
        let app = build_router(test_state());
        for path in ["/ilink/bot/getuploadurl", "/c2c/upload"] {
            let response = app
                .clone()
                .oneshot(
                    Request::builder()
                        .method("POST")
                        .uri(path)
                        .body(Body::empty())
                        .unwrap(),
                )
                .await
                .unwrap();
            assert_eq!(response.status(), StatusCode::NOT_FOUND);
        }
    }

    #[tokio::test]
    async fn binary_media_send_is_explicitly_unsupported() {
        let app = build_router(test_state());
        let response = app
            .oneshot(
                Request::builder()
                    .method("POST")
                    .uri("/ilink/bot/sendmessage")
                    .header("AuthorizationType", "ilink_bot_token")
                    .header("X-WECHAT-UIN", "default")
                    .header("authorization", "Bearer token")
                    .header("content-type", "application/json")
                    .body(Body::from(
                        r#"{"msg":{"item_list":[{"type":4,"file_item":{"media":{}}}]}}"#,
                    ))
                    .unwrap(),
            )
            .await
            .unwrap();

        assert_eq!(response.status(), StatusCode::NOT_IMPLEMENTED);
        let body: Value =
            serde_json::from_slice(&to_bytes(response.into_body(), usize::MAX).await.unwrap())
                .unwrap();
        assert_eq!(body["error"], "unsupported");
    }

    #[tokio::test]
    async fn standard_lifecycle_notifications_accept_bearer_token() {
        let app = build_router(test_state());
        for path in ["/ilink/bot/msg/notifystart", "/ilink/bot/msg/notifystop"] {
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

    fn test_state() -> Arc<AppState> {
        let state_dir = std::env::temp_dir().join(format!("webox-router-{}", uuid::Uuid::new_v4()));
        test_state_with_dir(state_dir)
    }

    fn test_state_with_dir(state_dir: std::path::PathBuf) -> Arc<AppState> {
        let wechat = WechatState::new(state_dir, "test-token");
        Arc::new(AppState {
            api_token: "token".to_string(),
            provider_account_id: "wx".to_string(),
            sender: Arc::new(tokio::sync::Mutex::new(UiSender::new(wechat.clone()))),
            qr_source: QrSource::new(None),
            media_store: MediaStore::new(
                std::env::temp_dir().join(format!("webox-router-media-{}", uuid::Uuid::new_v4())),
            ),
            login_session: Arc::new(std::sync::Mutex::new(LoginSession::default())),
            send_receipts: Arc::new(std::sync::Mutex::new(Default::default())),
            remark_reminders: Arc::new(std::sync::Mutex::new(Default::default())),
            wechat,
        })
    }
}
