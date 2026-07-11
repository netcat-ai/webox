use crate::wechat_state::WechatState;
use anyhow::{anyhow, bail, Context, Result};
use base64::engine::general_purpose::STANDARD;
use base64::Engine;
use std::env;
use std::process::Command;
use std::thread;
use std::time::Duration;
use uuid::Uuid;

const MAX_TEXT_LEN: usize = 5000;

#[derive(Clone)]
pub struct UiSender {
    wechat: WechatState,
}

#[derive(Debug, Clone)]
pub struct SendReceipt {
    pub client_msg_id: String,
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
    let before_send = wechat.room_message_positions(&recipient.username)?;
    let b64_to = STANDARD.encode(recipient.display.as_bytes());
    let b64_text = STANDARD.encode(text.as_bytes());
    let mut script = ui_script_prelude();
    script.extend([
        open_chat_script(&b64_to),
        format!("set_clip {}", shell_quote_single(&b64_text)),
        "paste_clip".to_string(),
        "sleep 0.2".to_string(),
        "xdotool key --clearmodifiers Return".to_string(),
        "sleep 0.5".to_string(),
    ]);
    let script = script.join("; ");

    if env::var("WEBOX_UI_SEND_DRY_RUN").ok().as_deref() == Some("1") {
        return Ok(receipt(client_msg_id));
    }

    run_ui_script("60s", &script, "send text")?;
    for _ in 0..20 {
        if wechat
            .has_text_message_after(&before_send, &recipient.username, &text)
            .context("verify sent text in WeChat db")?
        {
            return Ok(receipt(client_msg_id));
        }
        thread::sleep(Duration::from_millis(500));
    }
    bail!("send verification failed: message was not found in WeChat db")
}

fn ui_script_prelude() -> Vec<String> {
    [
        "set -e",
        "display=\"${DISPLAY:-}\"",
        "if [ -z \"$display\" ]; then for x in /tmp/.X11-unix/X*; do [ -e \"$x\" ] || continue; display=\":${x##*X}\"; break; done; fi",
        "export DISPLAY=\"${display:-:1}\"",
        "command -v xclip >/dev/null 2>&1 || { echo \"xclip not installed\" >&2; exit 127; }",
        "command -v xdotool >/dev/null 2>&1 || { echo \"xdotool not installed\" >&2; exit 127; }",
        "command -v timeout >/dev/null 2>&1 || { echo \"timeout not installed\" >&2; exit 127; }",
        "clip_pid=\"\"",
        "cleanup_clip() { if [ -n \"${clip_pid:-}\" ]; then kill \"$clip_pid\" 2>/dev/null || true; wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; fi; }",
        "set_clip() { cleanup_clip; printf '%s' \"$1\" | base64 -d | xclip -selection clipboard -target UTF8_STRING -loops 5 -i >/dev/null 2>&1 & clip_pid=$!; sleep 0.25; }",
        "paste_clip() { xdotool key --clearmodifiers ctrl+v; for i in $(seq 1 30); do if ! kill -0 \"$clip_pid\" 2>/dev/null; then wait \"$clip_pid\" 2>/dev/null || true; clip_pid=\"\"; sleep 0.1; return 0; fi; sleep 0.1; done; echo \"wechat did not read clipboard\" >&2; return 3; }",
        "trap cleanup_clip EXIT",
        "win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"",
        "[ -n \"$win\" ] || { active=\"$(xdotool getactivewindow 2>/dev/null || true)\"; active_name=\"\"; if [ -n \"$active\" ]; then active_name=\"$(xdotool getwindowname \"$active\" 2>/dev/null || true)\"; case \"$active_name\" in *微信*|*WeChat*) win=\"$active\";; esac; fi; }",
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name '微信' 2>/dev/null | tail -n1 || true)\"",
        "[ -n \"$win\" ] || win=\"$(xdotool search --onlyvisible --name 'WeChat' 2>/dev/null | tail -n1 || true)\"",
        "[ -n \"$win\" ] || { echo \"visible WeChat window not found\" >&2; exit 2; }",
        "xdotool windowactivate \"$win\"",
        "sleep 0.2",
    ]
    .into_iter()
    .map(str::to_string)
    .collect()
}

fn open_chat_script(query_b64: &str) -> String {
    format!(
        "main_win=\"$(xdotool search --onlyvisible --class 'wechat' 2>/dev/null | tail -n1 || true)\"; \
         if [ -n \"$main_win\" ]; then win=\"$main_win\"; xdotool windowactivate \"$win\"; xdotool windowraise \"$win\" 2>/dev/null || true; sleep 0.2; fi; \
         xdotool key --clearmodifiers Escape; sleep 0.1; \
         xdotool key --clearmodifiers ctrl+f; sleep 0.3; \
         xdotool key --clearmodifiers ctrl+a BackSpace; sleep 0.2; \
         set_clip {query}; paste_clip; sleep 1.8; \
         xdotool key --clearmodifiers Return; sleep 1.5",
        query = shell_quote_single(query_b64),
    )
}

fn run_ui_script(timeout: &str, script: &str, action: &str) -> Result<()> {
    let output = Command::new("timeout")
        .args([timeout, "bash", "-lc", script])
        .output()
        .with_context(|| format!("run xdotool {action}"))?;
    if output.status.success() {
        return Ok(());
    }
    let stderr = String::from_utf8_lossy(&output.stderr);
    let stdout = String::from_utf8_lossy(&output.stdout);
    let detail = if stderr.trim().is_empty() {
        stdout.trim()
    } else {
        stderr.trim()
    };
    Err(anyhow!("{action} failed: {detail}"))
}

fn receipt(client_msg_id: String) -> SendReceipt {
    SendReceipt { client_msg_id }
}

fn shell_quote_single(value: &str) -> String {
    format!("'{}'", value.replace('\'', "'\"'\"'"))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn shell_quote_single_handles_quotes() {
        assert_eq!(shell_quote_single("a'b"), "'a'\"'\"'b'");
    }

    #[test]
    fn open_chat_script_uses_reference_selection_flow() {
        let script = open_chat_script("query");

        assert!(script.contains("set_clip 'query'; paste_clip; sleep 1.8"));
        assert!(script.contains("key --clearmodifiers Return; sleep 1.5"));
        assert!(!script.contains("Down"));
        assert!(!script.contains("mousemove"));
        assert!(!script.contains("click"));
    }

    #[test]
    fn ui_prelude_does_not_depend_on_window_geometry() {
        let script = ui_script_prelude().join("; ");

        assert!(!script.contains("getwindowgeometry"));
        assert!(!script.contains("mousemove"));
        assert!(!script.contains("click"));
    }
}
