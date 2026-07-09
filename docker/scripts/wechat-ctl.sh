#!/usr/bin/env bash
set -u

STATE_DIR="${WEBOX_STATE_DIR:-/webox/state}"
STATUS_FILE="$STATE_DIR/wechat-status.json"
INSTALL_DIR="${WECHAT_INSTALL_DIR:-/webox/wechat}"
VERSION_FILE="$INSTALL_DIR/.webox-version"

wechat_bin() { echo "$INSTALL_DIR/opt/wechat/wechat"; }
is_installed() { [ -x "$(wechat_bin)" ]; }
cur_version() { [ -f "$VERSION_FILE" ] && cat "$VERSION_FILE" || echo ""; }

write_status() {
  local phase="$1" percent="$2" message="$3" installed=false version
  is_installed && installed=true
  version="$(cur_version)"
  mkdir -p "$STATE_DIR"
  cat > "$STATUS_FILE.tmp" <<EOF
{"phase":"$phase","percent":$percent,"installed":$installed,"version":"$version","message":"$message","updatedAt":$(date +%s)}
EOF
  mv -f "$STATUS_FILE.tmp" "$STATUS_FILE"
}

print_status() {
  if [ -f "$STATUS_FILE" ]; then
    cat "$STATUS_FILE"
  elif is_installed; then
    echo "{\"phase\":\"done\",\"percent\":100,\"installed\":true,\"version\":\"$(cur_version)\",\"message\":\"installed\",\"updatedAt\":$(date +%s)}"
  else
    echo "{\"phase\":\"idle\",\"percent\":0,\"installed\":false,\"version\":\"\",\"message\":\"not installed\",\"updatedAt\":$(date +%s)}"
  fi
}

case "${1:-status}" in
  status) print_status ;;
  install|update)
    write_status error 0 "wechat is bundled at image build time"
    echo "wechat is bundled at image build time; rebuild the image with docker/wechat/WeChatLinux_*.deb" >&2
    exit 1
    ;;
  *) echo "usage: $0 {status}" >&2; exit 1 ;;
esac
