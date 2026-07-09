#!/usr/bin/env bash
set -euo pipefail

export WEBOX_ROOT="${WEBOX_ROOT:-/webox}"
export DISPLAY="${DISPLAY:-:1}"
export AGENTGATEWAY_DIR="${WEBOX_AGENTGATEWAY_DIR:-${WEBOX_ROOT}/agentgateway}"
export WEBOX_STATE_DIR="${WEBOX_STATE_DIR:-${WEBOX_ROOT}/state}"
export WEBOX_LOG_DIR="${WEBOX_LOG_DIR:-${WEBOX_ROOT}/logs}"
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-${WEBOX_ROOT}/runtime}"
export WEBOX_XVFB_FBDIR="${WEBOX_XVFB_FBDIR:-${XDG_RUNTIME_DIR}/xvfb}"
export WEBOX_HOME="${WEBOX_HOME:-${WEBOX_STATE_DIR}/home}"
export HOME="$WEBOX_HOME"
export WECHAT_BIN="${WECHAT_BIN:-${WEBOX_ROOT}/wechat/opt/wechat/wechat}"
export WEBOX_WEAGENT_STATE_DIR="${WEBOX_WEAGENT_STATE_DIR:-${WEBOX_STATE_DIR}/weagent}"
export WEBOX_AGENTGATEWAY_API_BASE="${WEBOX_AGENTGATEWAY_API_BASE:-http://127.0.0.1:${WEBOX_AGENTGATEWAY_ADMIN_PORT:-15000}}"
export WEBOX_PROXYCHAINS_CONF="${WEBOX_PROXYCHAINS_CONF:-${WEBOX_STATE_DIR}/proxychains.conf}"

mkdir -p "$WEBOX_ROOT" "$AGENTGATEWAY_DIR" "$WEBOX_STATE_DIR" "$WEBOX_LOG_DIR" "$XDG_RUNTIME_DIR" "$WEBOX_XVFB_FBDIR" "$HOME"
chown -R webox:webox "$AGENTGATEWAY_DIR" "$WEBOX_STATE_DIR" "$WEBOX_LOG_DIR" "$XDG_RUNTIME_DIR" "$WEBOX_XVFB_FBDIR" "$HOME"
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

start_agent() {
  local cmd="${WEBOX_AGENT_CMD:-}"
  if [ -z "$cmd" ]; then
    cmd="${WEBOX_ROOT}/weagent/bin/weagent"
  fi
  echo "[entrypoint] starting agent: $cmd"
  gosu webox bash -lc "exec $cmd" >"$(log_path weagent)" 2>&1 &
  register_critical weagent "$!"
}

start_display() {
  Xvfb "$DISPLAY" -screen 0 "${WEBOX_SCREEN:-1280x800x24}" -fbdir "$WEBOX_XVFB_FBDIR" -nolisten tcp >"$(log_path xvfb)" 2>&1 &
  register_critical xvfb "$!"
  sleep 1
  openbox >"$(log_path openbox)" 2>&1 &
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

agentgateway_bin() {
  if [ -n "${WEBOX_AGENTGATEWAY_BIN:-}" ]; then
    echo "$WEBOX_AGENTGATEWAY_BIN"
  elif [ -x "${AGENTGATEWAY_DIR}/bin/agentgateway" ]; then
    echo "${AGENTGATEWAY_DIR}/bin/agentgateway"
  else
    echo /usr/local/bin/agentgateway
  fi
}

agentgateway_config() {
  if [ -n "${WEBOX_AGENTGATEWAY_CONFIG:-}" ]; then
    echo "$WEBOX_AGENTGATEWAY_CONFIG"
  else
    echo "${AGENTGATEWAY_DIR}/config.yaml"
  fi
}

agentgateway_config_dir() {
  local config
  config="$(agentgateway_config)"
  cd "$(dirname "$config")" && pwd -P
}

agentgateway_config_path() {
  local config config_dir
  config="$(agentgateway_config)"
  config_dir="$(agentgateway_config_dir)"
  echo "${config_dir}/$(basename "$config")"
}

ensure_agentgateway_config() {
  if [ "${WEBOX_AGENTGATEWAY_ENABLED:-1}" = "0" ] || [ -n "${WEBOX_AGENTGATEWAY_CMD:-}" ]; then
    return
  fi

  local config default_config
  config="$(agentgateway_config)"
  if [ -f "$config" ]; then
    return
  fi

  default_config="${WEBOX_ROOT}/weagent/share/agentgateway/config.example.yaml"
  if [ "$config" = "${AGENTGATEWAY_DIR}/config.yaml" ] && [ -f "$default_config" ]; then
    echo "[entrypoint] installing default agentgateway config: $config"
    mkdir -p "$(dirname "$config")"
    cp "$default_config" "$config"
    chown webox:webox "$config"
    return
  fi

  echo "[entrypoint] missing agentgateway MITM config: $config" >&2
  echo "[entrypoint] copy /webox/weagent/share/agentgateway/config.example.yaml to /webox/agentgateway/config.yaml" >&2
  exit 1
}

ensure_agentgateway_ca() {
  if [ "${WEBOX_AGENTGATEWAY_ENABLED:-1}" = "0" ]; then
    return
  fi
  if [ -n "${WEBOX_AGENTGATEWAY_CMD:-}" ] || [ -n "${WEBOX_AGENTGATEWAY_CA_PATH:-}" ] || [ -n "${WEBOX_AGENTGATEWAY_CA_KEY_PATH:-}" ]; then
    return
  fi

  local config_dir ca key
  config_dir="$(agentgateway_config_dir)"
  ca="${config_dir}/certificates/webox-ca.pem"
  key="${config_dir}/certificates/webox-ca-key.pem"
  if [ -f "$ca" ] && [ -f "$key" ]; then
    return
  fi

  echo "[entrypoint] generating agentgateway CA: $ca"
  mkdir -p "$(dirname "$ca")" "$(dirname "$key")"
  openssl req -x509 -newkey rsa:2048 -sha256 -days 3650 -nodes \
    -keyout "$key" \
    -out "$ca" \
    -subj "/CN=webox agentgateway CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign" >"$(log_path agentgateway-ca)" 2>&1
  chown webox:webox "$ca" "$key"
  chmod 0644 "$ca"
  chmod 0600 "$key"
}

start_agentgateway() {
  if [ "${WEBOX_AGENTGATEWAY_ENABLED:-1}" = "0" ]; then
    echo "[entrypoint] agentgateway disabled"
    return
  fi

  local cmd="${WEBOX_AGENTGATEWAY_CMD:-}"
  if [ -n "$cmd" ]; then
    echo "[entrypoint] starting agentgateway command: $cmd"
    gosu webox bash -lc "$cmd" >"$(log_path agentgateway)" 2>&1 &
    register_critical agentgateway "$!"
    return
  fi

  local bin config config_dir config_path
  bin="$(agentgateway_bin)"
  config="$(agentgateway_config)"
  if [ ! -x "$bin" ]; then
    echo "[entrypoint] agentgateway binary not found or not executable: $bin" >&2
    exit 1
  fi
  if [ ! -f "$config" ]; then
    echo "[entrypoint] missing agentgateway MITM config: $config" >&2
    echo "[entrypoint] copy /webox/weagent/share/agentgateway/config.example.yaml to /webox/agentgateway/config.yaml" >&2
    exit 1
  fi

  config_dir="$(agentgateway_config_dir)"
  config_path="$(agentgateway_config_path)"
  echo "[entrypoint] starting agentgateway from $config_dir: $bin -f $config_path"
  gosu webox env RUST_LOG="${WEBOX_AGENTGATEWAY_RUST_LOG:-info}" sh -c 'cd "$1" && exec "$2" -f "$3"' sh "$config_dir" "$bin" "$config_path" >"$(log_path agentgateway)" 2>&1 &
  register_critical agentgateway "$!"
}

install_nss_ca() {
  local ca="$1"
  local nssdb="$2"
  gosu webox mkdir -p "$nssdb"
  gosu webox certutil -d "sql:${nssdb}" -N --empty-password >/dev/null 2>&1 || true
  gosu webox certutil -d "sql:${nssdb}" -A -t "C,," -n webox-agentgateway -i "$ca" >/dev/null 2>&1 || true
}

trust_agentgateway_ca() {
  if [ "${WEBOX_AGENTGATEWAY_ENABLED:-1}" = "0" ]; then
    return
  fi

  local ca="${WEBOX_AGENTGATEWAY_CA_PATH:-}"
  local config_dir=""
  if [ -z "$ca" ] && [ -z "${WEBOX_AGENTGATEWAY_CMD:-}" ]; then
    config_dir="$(agentgateway_config_dir)"
    for candidate in \
      "${config_dir}/certificates/webox-ca.pem" \
      "${AGENTGATEWAY_DIR}/ca.crt" \
      "${AGENTGATEWAY_DIR}/rootCA.crt" \
      "${AGENTGATEWAY_DIR}/rootCA.pem" \
      "${AGENTGATEWAY_DIR}/agentgateway-ca.crt"; do
      if [ -f "$candidate" ]; then
        ca="$candidate"
        break
      fi
    done
  fi

  local wait_seconds="${WEBOX_AGENTGATEWAY_CA_WAIT_SECONDS:-10}"
  while [ -z "$ca" ] && [ "$wait_seconds" -gt 0 ] && [ -z "${WEBOX_AGENTGATEWAY_CMD:-}" ]; do
    sleep 1
    wait_seconds=$((wait_seconds - 1))
    for candidate in \
      "${config_dir}/certificates/webox-ca.pem" \
      "${AGENTGATEWAY_DIR}/ca.crt" \
      "${AGENTGATEWAY_DIR}/rootCA.crt" \
      "${AGENTGATEWAY_DIR}/rootCA.pem" \
      "${AGENTGATEWAY_DIR}/agentgateway-ca.crt"; do
      if [ -f "$candidate" ]; then
        ca="$candidate"
        break
      fi
    done
  done

  if [ -z "$ca" ]; then
    echo "[entrypoint] agentgateway CA not found; continuing without installing a new trust root"
    return
  fi

  echo "[entrypoint] trusting agentgateway CA: $ca"
  cp "$ca" /usr/local/share/ca-certificates/webox-agentgateway.crt
  update-ca-certificates >"$(log_path update-ca-certificates)" 2>&1 || true

  install_nss_ca "$ca" "${WEBOX_STATE_DIR}/nssdb"
  install_nss_ca "$ca" "${HOME}/.pki/nssdb"
}

wechat_proxy_mode() {
  if [ -n "${WEBOX_WECHAT_PROXY_MODE:-}" ]; then
    echo "$WEBOX_WECHAT_PROXY_MODE"
  elif [ "${WEBOX_AGENTGATEWAY_ENABLED:-1}" = "0" ]; then
    echo "none"
  else
    echo "proxychains"
  fi
}

proxychains_target_from_url() {
  local proxy_url proxy_addr proxy_host proxy_port
  proxy_url="${WEBOX_WECHAT_PROXY_URL:-http://127.0.0.1:${WEBOX_AGENTGATEWAY_PORT:-18080}}"
  proxy_addr="${proxy_url#http://}"
  proxy_addr="${proxy_addr#https://}"
  proxy_addr="${proxy_addr%%/*}"
  proxy_host="${proxy_addr%:*}"
  proxy_port="${proxy_addr##*:}"
  if [ -z "$proxy_host" ] || [ "$proxy_host" = "$proxy_port" ]; then
    proxy_host="${WEBOX_WECHAT_PROXY_HOST:-127.0.0.1}"
    proxy_port="${WEBOX_AGENTGATEWAY_PORT:-18080}"
  fi
  printf '%s %s\n' "$proxy_host" "$proxy_port"
}

ensure_proxychains_config() {
  if [ "$(wechat_proxy_mode)" != "proxychains" ]; then
    return
  fi
  if ! command -v proxychains4 >/dev/null 2>&1; then
    echo "[entrypoint] proxychains4 is required for WEBOX_WECHAT_PROXY_MODE=proxychains" >&2
    exit 1
  fi

  local proxy_host proxy_port
  read -r proxy_host proxy_port < <(proxychains_target_from_url)
  cat > "$WEBOX_PROXYCHAINS_CONF" <<EOF
strict_chain
proxy_dns
remote_dns_subnet 224
tcp_read_time_out 15000
tcp_connect_time_out 8000
localnet 127.0.0.0/255.0.0.0

[ProxyList]
http ${proxy_host} ${proxy_port}
EOF
  chown webox:webox "$WEBOX_PROXYCHAINS_CONF"
  chmod 0644 "$WEBOX_PROXYCHAINS_CONF"
}

wechat_app_ex_bin() {
  echo "${WEBOX_WECHAT_APP_EX_BIN:-${WEBOX_ROOT}/wechat/opt/wechat/RadiumWMPF/runtime/WeChatAppEx}"
}

ensure_wechat_network_wrapper() {
  if [ "$(wechat_proxy_mode)" != "proxychains" ]; then
    return
  fi

  local app real
  app="$(wechat_app_ex_bin)"
  real="${app}.webox-real"
  if [ ! -e "$app" ] && [ ! -e "$real" ]; then
    echo "[entrypoint] WeChatAppEx binary is missing: $app" >&2
    exit 1
  fi
  if [ ! -e "$real" ]; then
    if grep -q "WEBOX_PROXYCHAINS_WRAPPER" "$app" 2>/dev/null; then
      echo "[entrypoint] WeChatAppEx wrapper exists but real binary is missing: $real" >&2
      exit 1
    fi
    mv "$app" "$real"
  fi

  cat > "$app" <<EOF
#!/usr/bin/env bash
set -euo pipefail
# WEBOX_PROXYCHAINS_WRAPPER
# WeChatAppEx owns network sockets and drops the parent proxy env.
conf="\${WEBOX_PROXYCHAINS_CONF:-${WEBOX_PROXYCHAINS_CONF}}"
export PROXYCHAINS_CONF_FILE="\${PROXYCHAINS_CONF_FILE:-\$conf}"
exec proxychains4 -q -f "\$PROXYCHAINS_CONF_FILE" "$real" "\$@"
EOF
  chmod 0755 "$app"
}

start_wechat_loop() {
  local mode proxy_url no_proxy
  mode="$(wechat_proxy_mode)"
  proxy_url="${WEBOX_WECHAT_PROXY_URL:-http://127.0.0.1:${WEBOX_AGENTGATEWAY_PORT:-18080}}"
  no_proxy="${NO_PROXY:-127.0.0.1,localhost}"
  while true; do
    if [ ! -x "$WECHAT_BIN" ]; then
      echo "[entrypoint] bundled wechat binary is missing: $WECHAT_BIN" >&2
      exit 1
    fi
    echo "[entrypoint] starting wechat proxy_mode=$mode"
    if [ "$mode" = "proxychains" ]; then
      gosu webox env \
        WEBOX_PROXYCHAINS_CONF="$WEBOX_PROXYCHAINS_CONF" \
        PROXYCHAINS_CONF_FILE="$WEBOX_PROXYCHAINS_CONF" \
        dbus-run-session -- proxychains4 -q -f "$WEBOX_PROXYCHAINS_CONF" "$WECHAT_BIN" || true
    elif [ "$mode" = "env" ] && [ -n "$proxy_url" ]; then
      gosu webox env \
        HTTP_PROXY="$proxy_url" \
        HTTPS_PROXY="$proxy_url" \
        http_proxy="$proxy_url" \
        https_proxy="$proxy_url" \
        ALL_PROXY="$proxy_url" \
        all_proxy="$proxy_url" \
        NO_PROXY="$no_proxy" \
        no_proxy="$no_proxy" \
        dbus-run-session -- "$WECHAT_BIN" || true
    elif [ "$mode" = "none" ]; then
      gosu webox dbus-run-session -- "$WECHAT_BIN" || true
    else
      echo "[entrypoint] unsupported WEBOX_WECHAT_PROXY_MODE: $mode" >&2
      exit 1
    fi
    sleep 2
  done
}

start_display
ensure_agentgateway_config
ensure_agentgateway_ca
start_agentgateway
trust_agentgateway_ca
ensure_proxychains_config
ensure_wechat_network_wrapper
start_agent
start_wechat_loop &
register_critical wechat "$!"
wait_critical
