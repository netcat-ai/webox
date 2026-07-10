use crate::wechat_state::WechatState;
use anyhow::{anyhow, bail, Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use serde::Serialize;
use std::env;
use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::thread;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
use uuid::Uuid;

const MAX_TEXT_LEN: usize = 5000;
const MAX_FILE_BYTES: usize = 256 * 1024 * 1024;
const UPDATE_ID_SCALE: i64 = 1_000_000;

#[derive(Clone)]
pub struct UiSender {
    wechat: WechatState,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SendReceipt {
    pub accepted: bool,
    pub client_msg_id: String,
    pub target: SendTargetView,
}

#[derive(Debug, Clone, Serialize)]
#[serde(rename_all = "camelCase")]
pub struct SendTargetView {
    pub id: String,
    pub query: String,
    pub is_group: bool,
}

impl UiSender {
    pub fn new(wechat: WechatState) -> Self {
        Self { wechat }
    }

    pub async fn send_text(&self, to: String, text: String) -> Result<SendReceipt> {
        let wechat = self.wechat.clone();
        tokio::task::spawn_blocking(move || send_text_blocking(&wechat, to, text))
            .await
            .context("join ui sender task")?
    }

    pub async fn send_file(
        &self,
        to: String,
        filename: String,
        data: Vec<u8>,
    ) -> Result<SendReceipt> {
        let wechat = self.wechat.clone();
        tokio::task::spawn_blocking(move || send_file_blocking(&wechat, to, filename, data))
            .await
            .context("join ui file sender task")?
    }
}

fn send_text_blocking(wechat: &WechatState, to: String, text: String) -> Result<SendReceipt> {
    let to = to.trim().to_string();
    if to.is_empty() || to.len() > 200 {
        bail!("recipient is empty or too long");
    }
    if text.is_empty() || text.len() > MAX_TEXT_LEN {
        bail!("text is empty or too long");
    }
    let recipient = wechat.resolve_recipient(&to)?;
    if recipient.is_group && !recipient.search_uses_remark {
        bail!("group send requires a unique remark to avoid selecting the wrong chat");
    }

    let client_msg_id = Uuid::new_v4().simple().to_string();
    let after_id = current_update_id_floor();
    let b64_to = STANDARD.encode(recipient.display.as_bytes());
    let b64_text = STANDARD.encode(text.as_bytes());
    let script = [
        "set -e".to_string(),
        "display=\"${DISPLAY:-}\"".to_string(),
        "if [ -z \"$display\" ]; then for x in /tmp/.X11-unix/X*; do [ -e \"$x\" ] || continue; display=\":${x##*X}\"; break; done; fi".to_string(),
        "export DISPLAY=\"${display:-:1}\"".to_string(),
        "command -v xclip >/dev/null 2>&1 || { echo \"xclip not installed\" >&2; exit 127; }".to_string(),
        "command -v xdotool >/dev/null 2>&1 || { echo \"xdotool not installed\" >&2; exit 127; }".to_string(),
        "command -v timeout >/dev/null 2>&1 || { echo \"timeout not installed\" >&2; exit 127; }".to_string(),
        "clip_pid=\"\"".to_string(),
        "cleanup_clip() { if [ -n \"${clip_pid:-}\" ]; then kill \"$clip_pid\" 2>/dev/null || true; wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; fi; }".to_string(),
        "set_clip() { cleanup_clip; printf '%s' \"$1\" | base64 -d | xclip -selection clipboard -target UTF8_STRING -loops 5 -i >/dev/null 2>&1 & clip_pid=$!; sleep 0.25; }".to_string(),
        "paste_clip() { xdotool key --clearmodifiers ctrl+v; for i in $(seq 1 30); do if ! kill -0 \"$clip_pid\" 2>/dev/null; then wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; sleep 0.1; return 0; fi; sleep 0.1; done; echo \"wechat did not read clipboard\" >&2; return 3; }".to_string(),
        "trap cleanup_clip EXIT".to_string(),
        "if command -v xprop >/dev/null 2>&1; then for browser_win in $({ xdotool search --name '微信' 2>/dev/null || true; xdotool search --name 'WeChat' 2>/dev/null || true; } | sort -u); do class=\"$(xprop -id \"$browser_win\" WM_CLASS 2>/dev/null || true)\"; case \"$class\" in *wechat*) ;; *) xdotool windowclose \"$browser_win\" 2>/dev/null || true; sleep 0.3;; esac; done; fi".to_string(),
        "win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || { active=\"$(xdotool getactivewindow 2>/dev/null || true)\"; active_name=\"\"; if [ -n \"$active\" ]; then active_name=\"$(xdotool getwindowname \"$active\" 2>/dev/null || true)\"; case \"$active_name\" in *微信*|*WeChat*) win=\"$active\";; esac; fi; }".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name '微信' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name 'WeChat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || { echo \"visible WeChat window not found\" >&2; exit 2; }".to_string(),
        "xdotool windowactivate \"$win\"".to_string(),
        "sleep 0.2".to_string(),
        "eval \"$(xdotool getwindowgeometry --shell \"$win\")\"".to_string(),
        "root_x=${X:-0}; root_y=${Y:-0}; root_w=${WIDTH:-1856}; root_h=${HEIGHT:-857}".to_string(),
        "input_x=$((root_x + 500)); input_y=$((root_y + root_h - 157)); send_x=$((root_x + root_w - 67)); send_y=$((root_y + root_h - 34))".to_string(),
        "main_win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"; if [ -n \"$main_win\" ]; then win=\"$main_win\"; xdotool windowactivate \"$win\"; xdotool windowraise \"$win\" 2>/dev/null || true; sleep 0.2; fi; xdotool key --clearmodifiers Escape; sleep 0.1; xdotool mousemove $((root_x + 31)) $((root_y + 96)) click 1; sleep 0.2; xdotool key --clearmodifiers ctrl+f; sleep 0.3".to_string(),
        "xdotool key --clearmodifiers ctrl+a; sleep 0.05; xdotool key --clearmodifiers BackSpace; sleep 0.05; xdotool key --clearmodifiers ctrl+a; sleep 0.05; xdotool key --clearmodifiers Delete; sleep 0.2; ".to_string()
            + &format!("set_clip {}", shell_quote_single(&b64_to))
            + "; paste_clip; sleep 1.8; xdotool key --clearmodifiers Down Down Down Down Down Down Return",
        "sleep 1.5".to_string(),
        "xdotool mousemove \"$input_x\" \"$input_y\" click 1".to_string(),
        "sleep 0.2".to_string(),
        "xdotool key --clearmodifiers ctrl+a BackSpace".to_string(),
        "sleep 0.2".to_string(),
        format!("set_clip {}", shell_quote_single(&b64_text)),
        "paste_clip".to_string(),
        "sleep 0.2".to_string(),
        "xdotool mousemove \"$send_x\" \"$send_y\" click 1".to_string(),
        "sleep 0.5".to_string(),
    ]
    .join("; ");

    if env::var("WEBOX_UI_SEND_DRY_RUN").ok().as_deref() == Some("1") {
        return Ok(receipt(client_msg_id, &recipient));
    }

    let output = Command::new("timeout")
        .args(["60s", "bash", "-lc", &script])
        .output()
        .context("run xdotool sender")?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let detail = if stderr.trim().is_empty() {
            stdout.trim()
        } else {
            stderr.trim()
        };
        return Err(anyhow!("send failed: {detail}"));
    }
    for _ in 0..20 {
        if wechat
            .has_text_message_after(after_id, &recipient.username, &text)
            .context("verify sent text in WeChat db")?
        {
            return Ok(receipt(client_msg_id, &recipient));
        }
        thread::sleep(Duration::from_millis(500));
    }
    bail!("send verification failed: message was not found in WeChat db")
}

fn send_file_blocking(
    wechat: &WechatState,
    to: String,
    filename: String,
    data: Vec<u8>,
) -> Result<SendReceipt> {
    let to = to.trim().to_string();
    if to.is_empty() || to.len() > 200 {
        bail!("recipient is empty or too long");
    }
    if data.is_empty() || data.len() > MAX_FILE_BYTES {
        bail!("file is empty or too large");
    }
    let recipient = wechat.resolve_recipient(&to)?;
    if recipient.is_group && !recipient.search_uses_remark {
        bail!("group send requires a unique remark to avoid selecting the wrong chat");
    }

    let client_msg_id = Uuid::new_v4().simple().to_string();
    let transfer_dir = media_transfer_dir();
    fs::create_dir_all(&transfer_dir)?;
    let safe_name = safe_media_file_name(&filename, &client_msg_id);
    let media_path = transfer_dir.join(format!("webox-send-{safe_name}"));
    fs::write(&media_path, data)?;

    let media_path = media_path.to_string_lossy().to_string();
    let b64_media_path = STANDARD.encode(media_path.as_bytes());
    let b64_to = STANDARD.encode(recipient.display.as_bytes());
    let script = [
        "set -e".to_string(),
        "display=\"${DISPLAY:-}\"".to_string(),
        "if [ -z \"$display\" ]; then for x in /tmp/.X11-unix/X*; do [ -e \"$x\" ] || continue; display=\":${x##*X}\"; break; done; fi".to_string(),
        "export DISPLAY=\"${display:-:1}\"".to_string(),
        "command -v xclip >/dev/null 2>&1 || { echo \"xclip not installed\" >&2; exit 127; }".to_string(),
        "command -v xdotool >/dev/null 2>&1 || { echo \"xdotool not installed\" >&2; exit 127; }".to_string(),
        "command -v timeout >/dev/null 2>&1 || { echo \"timeout not installed\" >&2; exit 127; }".to_string(),
        "clip_pid=\"\"".to_string(),
        "cleanup_clip() { if [ -n \"${clip_pid:-}\" ]; then kill \"$clip_pid\" 2>/dev/null || true; wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; fi; }".to_string(),
        "set_text_clip() { cleanup_clip; printf '%s' \"$1\" | base64 -d | xclip -selection clipboard -target UTF8_STRING -loops 5 -i >/dev/null 2>&1 & clip_pid=$!; sleep 0.25; }".to_string(),
        "paste_clip() { xdotool key --clearmodifiers ctrl+v; for i in $(seq 1 30); do if ! kill -0 \"$clip_pid\" 2>/dev/null; then wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; sleep 0.1; return 0; fi; sleep 0.1; done; echo \"wechat did not read clipboard\" >&2; return 3; }".to_string(),
        "trap cleanup_clip EXIT".to_string(),
        format!("file_b64={}", shell_quote_single(&b64_media_path)),
        "if command -v xprop >/dev/null 2>&1; then for browser_win in $({ xdotool search --name '微信' 2>/dev/null || true; xdotool search --name 'WeChat' 2>/dev/null || true; } | sort -u); do class=\"$(xprop -id \"$browser_win\" WM_CLASS 2>/dev/null || true)\"; case \"$class\" in *wechat*) ;; *) xdotool windowclose \"$browser_win\" 2>/dev/null || true; sleep 0.3;; esac; done; fi".to_string(),
        "win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || { active=\"$(xdotool getactivewindow 2>/dev/null || true)\"; active_name=\"\"; if [ -n \"$active\" ]; then active_name=\"$(xdotool getwindowname \"$active\" 2>/dev/null || true)\"; case \"$active_name\" in *微信*|*WeChat*) win=\"$active\";; esac; fi; }".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name '微信' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name 'WeChat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"".to_string(),
        "[ -n \"$win\" ] || { echo \"visible WeChat window not found\" >&2; exit 2; }".to_string(),
        "xdotool windowactivate \"$win\"".to_string(),
        "sleep 0.2".to_string(),
        "eval \"$(xdotool getwindowgeometry --shell \"$win\")\"".to_string(),
        "root_x=${X:-0}; root_y=${Y:-0}; root_w=${WIDTH:-1856}; root_h=${HEIGHT:-857}".to_string(),
        "input_x=$((root_x + 500)); input_y=$((root_y + root_h - 157)); file_x=$((root_x + 433)); file_y=$((root_y + root_h - 194)); send_x=$((root_x + root_w - 67)); send_y=$((root_y + root_h - 34))".to_string(),
        "main_win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"; if [ -n \"$main_win\" ]; then win=\"$main_win\"; xdotool windowactivate \"$win\"; xdotool windowraise \"$win\" 2>/dev/null || true; sleep 0.2; fi; xdotool key --clearmodifiers Escape; sleep 0.1; xdotool mousemove $((root_x + 31)) $((root_y + 96)) click 1; sleep 0.2; xdotool key --clearmodifiers ctrl+f; sleep 0.3".to_string(),
        "xdotool key --clearmodifiers ctrl+a; sleep 0.05; xdotool key --clearmodifiers BackSpace; sleep 0.05; xdotool key --clearmodifiers ctrl+a; sleep 0.05; xdotool key --clearmodifiers Delete; sleep 0.2; ".to_string()
            + &format!("set_text_clip {}", shell_quote_single(&b64_to))
            + "; paste_clip; sleep 1.8; xdotool key --clearmodifiers Down Down Down Down Down Down Return",
        "sleep 1.5".to_string(),
        "xdotool mousemove \"$input_x\" \"$input_y\" click 1".to_string(),
        "sleep 0.2".to_string(),
        "xdotool key --clearmodifiers ctrl+a BackSpace".to_string(),
        "sleep 0.2".to_string(),
        "xdotool mousemove \"$file_x\" \"$file_y\" click 1".to_string(),
        "sleep 0.8".to_string(),
        "xdotool key --clearmodifiers ctrl+l".to_string(),
        "sleep 0.2".to_string(),
        "printf '%s' \"$file_b64\" | base64 -d | xdotool type --delay 1 --file -".to_string(),
        "sleep 0.2".to_string(),
        "xdotool key --clearmodifiers Return".to_string(),
        "sleep 1.2".to_string(),
        "xdotool mousemove \"$send_x\" \"$send_y\" click 1".to_string(),
        "sleep 0.5".to_string(),
    ]
    .join("; ");

    if env::var("WEBOX_UI_SEND_DRY_RUN").ok().as_deref() == Some("1") {
        return Ok(receipt(client_msg_id, &recipient));
    }

    let output = Command::new("timeout")
        .args(["140s", "bash", "-lc", &script])
        .output()
        .context("run xdotool file sender")?;
    if !output.status.success() {
        let stderr = String::from_utf8_lossy(&output.stderr);
        let stdout = String::from_utf8_lossy(&output.stdout);
        let detail = if stderr.trim().is_empty() {
            stdout.trim()
        } else {
            stderr.trim()
        };
        return Err(anyhow!("send file failed: {detail}"));
    }
    Ok(receipt(client_msg_id, &recipient))
}

fn receipt(client_msg_id: String, recipient: &crate::wechat_db::Recipient) -> SendReceipt {
    SendReceipt {
        accepted: true,
        client_msg_id,
        target: SendTargetView {
            id: recipient.username.clone(),
            query: recipient.display.clone(),
            is_group: recipient.is_group,
        },
    }
}

fn media_transfer_dir() -> PathBuf {
    env::var("WEBOX_MEDIA_TRANSFER_DIR")
        .ok()
        .map(PathBuf::from)
        .unwrap_or_else(|| PathBuf::from("/webox/state/weagent/transfer"))
}

fn safe_media_file_name(raw: &str, fallback: &str) -> String {
    let value = raw
        .rsplit(['/', '\\'])
        .next()
        .unwrap_or(raw)
        .split(['?', '#'])
        .next()
        .unwrap_or("")
        .chars()
        .map(|ch| {
            if ch.is_ascii_alphanumeric() || matches!(ch, '.' | '-' | '_') {
                ch
            } else {
                '_'
            }
        })
        .take(180)
        .collect::<String>()
        .trim_matches('.')
        .to_string();
    if value.is_empty() {
        fallback.to_string()
    } else {
        value
    }
}

fn shell_quote_single(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

fn current_update_id_floor() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs()
        .min(i64::MAX as u64) as i64
        * UPDATE_ID_SCALE
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_quote_single_handles_quotes() {
        assert_eq!(shell_quote_single("a'b"), "'a'\"'\"'b'");
    }
}
