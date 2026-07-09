use anyhow::{Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use reqwest::StatusCode;
use serde::{Deserialize, Serialize};
use serde_json::Value;

#[derive(Clone)]
pub struct QrSource {
    base_url: String,
    client: reqwest::Client,
    match_terms: Vec<String>,
}

#[derive(Clone, Debug, Serialize, PartialEq)]
#[serde(rename_all = "snake_case")]
pub struct QrEvent {
    pub id: String,
    #[serde(rename = "type")]
    pub kind: String,
    pub captured_at: String,
    pub method: String,
    pub url: String,
    pub host: String,
    pub path: String,
    pub request_body_base64: Option<String>,
    pub response_status: Option<i64>,
    pub response_body_base64: Option<String>,
    pub response_text: Option<String>,
    pub matched_by: Vec<String>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct SearchLogsRequest {
    limit: i64,
    include_attributes: bool,
}

#[derive(Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct SearchLogsResponse {
    logs: Vec<LogEntry>,
}

#[derive(Clone, Debug, Deserialize)]
#[serde(rename_all = "camelCase")]
struct LogEntry {
    id: String,
    completed_at: String,
    http_status: Option<i64>,
    attributes: Option<Value>,
}

#[derive(Debug, Serialize)]
#[serde(rename_all = "camelCase")]
struct GetLogRequest<'a> {
    id: &'a str,
    include_payload: bool,
}

#[derive(Debug, Deserialize)]
struct GetLogResponse {
    log: Option<LogEntry>,
}

impl QrSource {
    pub fn new(base_url: String, match_terms: Vec<String>) -> Self {
        Self {
            base_url,
            client: reqwest::Client::new(),
            match_terms,
        }
    }

    pub async fn latest(&self) -> Result<Option<QrEvent>> {
        Ok(self.search_newest(1).await?.into_iter().next())
    }

    pub async fn recent(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let mut events = self.search_newest(limit).await?;
        events.reverse();
        Ok(events)
    }

    async fn search_newest(&self, limit: usize) -> Result<Vec<QrEvent>> {
        let wanted = limit.clamp(1, 100);
        let scan_limit = (wanted * 20).clamp(100, 500) as i64;
        let search = self
            .client
            .post(format!("{}/api/logs/search", self.base_url))
            .json(&SearchLogsRequest {
                limit: scan_limit,
                include_attributes: true,
            })
            .send()
            .await
            .context("query agentgateway logs search api")?;
        ensure_success(search.status())?;
        let response: SearchLogsResponse = search
            .json()
            .await
            .context("decode agentgateway logs search response")?;

        let mut out = Vec::with_capacity(wanted);
        for entry in response.logs {
            let Some(candidate) = self.entry_to_event(&entry) else {
                continue;
            };
            let event = match self.get_log(&entry.id).await {
                Ok(Some(full)) => self.entry_to_event(&full).unwrap_or(candidate),
                Ok(None) | Err(_) => candidate,
            };
            out.push(event);
            if out.len() >= wanted {
                break;
            }
        }
        Ok(out)
    }

    async fn get_log(&self, id: &str) -> Result<Option<LogEntry>> {
        let response = self
            .client
            .post(format!("{}/api/logs/get", self.base_url))
            .json(&GetLogRequest {
                id,
                include_payload: true,
            })
            .send()
            .await
            .with_context(|| format!("query agentgateway log {id}"))?;
        ensure_success(response.status())?;
        Ok(response
            .json::<GetLogResponse>()
            .await
            .context("decode agentgateway log get response")?
            .log)
    }

    fn entry_to_event(&self, entry: &LogEntry) -> Option<QrEvent> {
        let attributes = entry.attributes.as_ref()?;
        let method = attr_string(attributes, "http.method").unwrap_or_default();
        let host = attr_string(attributes, "http.host").unwrap_or_default();
        let path = attr_string(attributes, "http.path").unwrap_or_default();
        let request_body = attr_string(attributes, "request.body");
        let response_body = attr_string(attributes, "response.body");
        let response_status = entry
            .http_status
            .or_else(|| attr_i64(attributes, "http.status"));
        let url = build_url(&host, &path);
        let haystack = [
            method.as_str(),
            host.as_str(),
            path.as_str(),
            url.as_str(),
            request_body.as_deref().unwrap_or_default(),
            response_body.as_deref().unwrap_or_default(),
        ]
        .join("\n")
        .to_ascii_lowercase();
        let matched_by = self
            .match_terms
            .iter()
            .filter(|term| haystack.contains(term.as_str()))
            .cloned()
            .collect::<Vec<_>>();
        if matched_by.is_empty() {
            return None;
        }
        Some(QrEvent {
            id: entry.id.clone(),
            kind: "wechat.login_qrcode".to_string(),
            captured_at: entry.completed_at.clone(),
            method,
            url,
            host,
            path,
            request_body_base64: request_body.map(|body| STANDARD.encode(body.as_bytes())),
            response_status,
            response_body_base64: response_body
                .as_ref()
                .map(|body| STANDARD.encode(body.as_bytes())),
            response_text: response_body,
            matched_by,
        })
    }
}

fn ensure_success(status: StatusCode) -> Result<()> {
    if status.is_success() {
        return Ok(());
    }
    anyhow::bail!("agentgateway api returned {status}")
}

fn attr_string(attributes: &Value, key: &str) -> Option<String> {
    attr_value(attributes, key).and_then(|value| match value {
        Value::String(value) => Some(value.clone()),
        Value::Number(value) => Some(value.to_string()),
        Value::Bool(value) => Some(value.to_string()),
        _ => None,
    })
}

fn attr_i64(attributes: &Value, key: &str) -> Option<i64> {
    attr_value(attributes, key).and_then(|value| match value {
        Value::Number(value) => value.as_i64(),
        Value::String(value) => value.parse().ok(),
        _ => None,
    })
}

fn attr_value<'a>(attributes: &'a Value, key: &str) -> Option<&'a Value> {
    if let Some(value) = attributes.get(key) {
        return Some(value);
    }
    let mut current = attributes;
    for part in key.split('.') {
        current = current.get(part)?;
    }
    Some(current)
}

fn build_url(host: &str, path: &str) -> String {
    if path.starts_with("http://") || path.starts_with("https://") {
        return path.to_string();
    }
    if host.is_empty() {
        return path.to_string();
    }
    if path.starts_with('/') {
        format!("https://{host}{path}")
    } else if path.is_empty() {
        format!("https://{host}")
    } else {
        format!("https://{host}/{path}")
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    #[test]
    fn event_matches_flat_agentgateway_attributes() {
        let source = QrSource::new("http://127.0.0.1:15000".to_string(), vec!["qrcode".into()]);
        let entry = LogEntry {
            id: "log-1".to_string(),
            completed_at: "2026-07-09T00:00:00Z".to_string(),
            http_status: Some(200),
            attributes: Some(json!({
                "http.method": "POST",
                "http.host": "login.weixin.qq.com",
                "http.path": "/cgi-bin/micromsg-bin/getloginqrcode",
                "request.body": "device=linux",
                "response.body": "qrcode-bytes"
            })),
        };
        let event = source.entry_to_event(&entry).expect("qr event");
        assert_eq!(
            event.url,
            "https://login.weixin.qq.com/cgi-bin/micromsg-bin/getloginqrcode"
        );
        assert_eq!(
            event.response_body_base64,
            Some(STANDARD.encode("qrcode-bytes"))
        );
    }

    #[test]
    fn event_matches_nested_attributes() {
        let source = QrSource::new("http://127.0.0.1:15000".to_string(), vec!["uuid".into()]);
        let entry = LogEntry {
            id: "log-2".to_string(),
            completed_at: "2026-07-09T00:00:00Z".to_string(),
            http_status: None,
            attributes: Some(json!({
                "http": { "method": "GET", "host": "weixin.qq.com", "path": "/login" },
                "response": { "body": "uuid=abc" }
            })),
        };
        let event = source.entry_to_event(&entry).expect("qr event");
        assert_eq!(event.response_status, None);
        assert_eq!(event.matched_by, vec!["uuid"]);
    }
}
