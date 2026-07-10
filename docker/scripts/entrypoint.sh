#!/usr/bin/env bash
set -euo pipefail

export WEBOX_ROOT="${WEBOX_ROOT:-/webox}"
export DISPLAY="${DISPLAY:-:1}"
export WEBOX_STATE_DIR="${WEBOX_STATE_DIR:-${WEBOX_ROOT}/state}"
export WEBOX_LOG_DIR="${WEBOX_LOG_DIR:-${WEBOX_ROOT}/logs}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-${WEBOX_ROOT}/runtime}"
export WEBOX_XVFB_FBDIR="${WEBOX_XVFB_FBDIR:-${XDG_RUNTIME_DIR}/xvfb}"
export WEBOX_HOME="${WEBOX_HOME:-${WEBOX_STATE_DIR}/home}"
export WEBOX_WX_HOME="${WEBOX_WX_HOME:-${WEBOX_HOME}}"
export HOME="$WEBOX_HOME"
export WECHAT_BIN="${WECHAT_BIN:-${WEBOX_ROOT}/wechat/opt/wechat/wechat}"
export WEBOX_WEAGENT_STATE_DIR="${WEBOX_WEAGENT_STATE_DIR:-${WEBOX_STATE_DIR}/weagent}"

mkdir -p "$WEBOX_ROOT" "$WEBOX_STATE_DIR" "$WEBOX_LOG_DIR" "$XDG_RUNTIME_DIR" "$WEBOX_XVFB_FBDIR" "$HOME"
chown -R webox:webox "$WEBOX_STATE_DIR" "$WEBOX_LOG_DIR" "$XDG_RUNTIME_DIR" "$WEBOX_XVFB_FBDIR" "$HOME"
chmod 700 "$XDG_RUNTIME_DIR"

"${WEBOX_ROOT}/weagent/bin/webox-identity.sh"

critical_pids=()
critical_names=()

log_path() {
  echo "${WEBOX_LOG_DIR}/$1.log"
}

register_critical() {
  critical_names+=("$1")
  critical_pids+=("$2")
}

stop_children() {
  trap - TERM INT EXIT
  for pid in "${critical_pids[@]}"; do
    kill "$pid" 2>/dev/null || true
  done
  wait 2>/dev/null || true
}

terminate() {
  echo "[entrypoint] stopping"
  stop_children
  exit 143
}

wait_critical() {
  trap terminate TERM INT
  trap stop_children EXIT
  while true; do
    set +e
    wait -n
    local status=$?
    set -e
    for i in "${!critical_pids[@]}"; do
      local pid="${critical_pids[$i]}"
      if ! kill -0 "$pid" 2>/dev/null; then
        echo "[entrypoint] critical process exited: ${critical_names[$i]} pid=$pid status=$status" >&2
        stop_children
        exit "$status"
      fi
    done
  done
}

start_display() {
  Xvfb "$DISPLAY" -screen 0 "${WEBOX_SCREEN:-1280x800x24}" -fbdir "$WEBOX_XVFB_FBDIR" -nolisten tcp >"$(log_path xvfb)" 2>&1 &
  register_critical xvfb "$!"
  sleep 1

  gosu webox openbox >"$(log_path openbox)" 2>&1 &
  register_critical openbox "$!"

  local xsettings="${WEBOX_STATE_DIR}/xsettingsd.conf"
  cat > "$xsettings" <<'EOF'
Xft/Antialias 1
Xft/Hinting 1
Xft/HintStyle "hintslight"
Xft/RGBA "rgb"
Xft/DPI 98304
Gtk/FontName "WenQuanYi Micro Hei 10"
EOF
  chown webox:webox "$xsettings"
  gosu webox xsettingsd --config="$xsettings" >"$(log_path xsettingsd)" 2>&1 &
}

start_agent() {
  local cmd="${WEBOX_AGENT_CMD:-${WEBOX_ROOT}/weagent/bin/weagent}"
  echo "[entrypoint] starting agent: $cmd"
  gosu webox bash -lc "exec $cmd" >"$(log_path weagent)" 2>&1 &
  register_critical weagent "$!"
}

start_wechat_loop() {
  while true; do
    if [ ! -x "$WECHAT_BIN" ]; then
      echo "[entrypoint] bundled wechat binary is missing: $WECHAT_BIN" >&2
      exit 1
    fi
    echo "[entrypoint] starting wechat"
    gosu webox dbus-run-session -- "$WECHAT_BIN" || true
    sleep 2
  done
}

start_display
start_agent
start_wechat_loop &
register_critical wechat "$!"
wait_critical
