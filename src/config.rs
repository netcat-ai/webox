use std::env;
use std::path::PathBuf;

#[derive(Clone, Debug)]
pub struct Config {
    pub listen_addr: String,
    pub api_token: String,
    pub agentgateway_api_base: String,
    pub qr_match_terms: Vec<String>,
    pub state_dir: PathBuf,
}

impl Config {
    pub fn from_env() -> Self {
        Self {
            listen_addr: normalize_listen_addr(&env_or("WEBOX_LISTEN_ADDR", "0.0.0.0:8080")),
            api_token: env_or("WEBOX_API_TOKEN", "webox"),
            agentgateway_api_base: trim_base_url(&env_or(
                "WEBOX_AGENTGATEWAY_API_BASE",
                "http://127.0.0.1:15000",
            )),
            qr_match_terms: parse_terms(&env::var("WEBOX_QR_MATCH_TERMS").unwrap_or_else(|_| {
                "getloginqrcode,loginqrcode,qrcode,qr_code,qrlogin,uuid,login".to_string()
            })),
            state_dir: env::var("WEBOX_WEAGENT_STATE_DIR")
                .map(PathBuf::from)
                .unwrap_or_else(|_| PathBuf::from("/webox/state/weagent")),
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

fn trim_base_url(raw: &str) -> String {
    raw.trim().trim_end_matches('/').to_string()
}

fn parse_terms(raw: &str) -> Vec<String> {
    let mut out = Vec::new();
    for part in raw.split([',', '\n', '\t', ' ']) {
        let term = part.trim().to_ascii_lowercase();
        if !term.is_empty() && !out.contains(&term) {
            out.push(term);
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn listen_addr_accepts_go_style_port_only() {
        assert_eq!(normalize_listen_addr(":8080"), "0.0.0.0:8080");
        assert_eq!(normalize_listen_addr("127.0.0.1:8080"), "127.0.0.1:8080");
    }

    #[test]
    fn parse_terms_deduplicates() {
        assert_eq!(parse_terms("qrcode, login qrcode"), vec!["qrcode", "login"]);
    }
}
