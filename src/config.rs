use std::env;
use std::path::PathBuf;

#[derive(Clone, Debug)]
pub struct Config {
    pub listen_addr: String,
    pub api_token: String,
    pub tenant_id: String,
    pub provider_account_id: String,
    pub public_base_url: Option<String>,
    pub qr_screenshot_path: Option<PathBuf>,
    pub state_dir: PathBuf,
    pub media_dir: PathBuf,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            listen_addr: normalize_listen_addr(&env_or("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
            api_token: env_or("WEBOX_API_TOKEN", "webox"),
            tenant_id: env_or("WEBOX_TENANT_ID", "default"),
            provider_account_id: env_or("WEBOX_PROVIDER_ACCOUNT_ID", "default"),
            public_base_url: optional_string(
                &env::var("WEBOX_PUBLIC_BASE_URL").unwrap_or_default(),
            )
            .map(|value| value.trim_end_matches('/').to_string()),
            qr_screenshot_path: optional_path(&env_or(
                "WEBOX_QR_SCREENSHOT_PATH",
                "/webox/runtime/xvfb/Xvfb_screen0",
            )),
            state_dir: env::var("WEBOX_WEAGENT_STATE_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|_| PathBuf::from("/webox/state/weagent")),
            media_dir: env::var("WEBOX_MEDIA_STORE_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|_| PathBuf::from("/webox/state/weagent/media")),
        }
    }
}

fn env_or(key: &str, fallback: &str) -> String {
    env::var(key)
        .ok()
        .map(|value| value.trim().to_string())
        .filter(|value| !value.is_empty())
        .unwrap_or_else(|| fallback.to_string())
}

fn normalize_listen_addr(raw: &str) -> String {
    let value = raw.trim();
    if let Some(port) = value.strip_prefix(':') {
        return format!("0.0.0.0:{port}");
    }
    value.to_string()
}

fn optional_path(raw: &str) -> Option<PathBuf> {
    let value = raw.trim();
    (!value.is_empty()).then(|| PathBuf::from(value))
}

fn optional_string(raw: &str) -> Option<String> {
    let value = raw.trim();
    (!value.is_empty()).then(|| value.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn listen_addr_accepts_go_style_port_only() {
        assert_eq!(normalize_listen_addr(":8080"), "0.0.0.0:8080");
        assert_eq!(normalize_listen_addr("127.0.0.1:8080"), "127.0.0.1:8080");
    }
}
