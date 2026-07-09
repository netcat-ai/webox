#!/usr/bin/env bash
set -euo pipefail

STATE_DIR="${WEBOX_STATE_DIR:-/webox/state}"
ID_FILE="$STATE_DIR/machine-id"

mkdir -p "$STATE_DIR"

if [ ! -s "$ID_FILE" ]; then
  if [ -r /proc/sys/kernel/random/uuid ]; then
    tr -d '-' < /proc/sys/kernel/random/uuid | tr 'A-F' 'a-f' > "$ID_FILE"
  else
    head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$ID_FILE"
  fi
fi

MID="$(tr -dc 'a-f0-9' < "$ID_FILE" | head -c 32)"
if [ "${#MID}" -ne 32 ]; then
  MID="$(tr -d '-' < /proc/sys/kernel/random/uuid | tr 'A-F' 'a-f' | head -c 32)"
  printf '%s\n' "$MID" > "$ID_FILE"
fi

printf '%s\n' "$MID" > /etc/machine-id 2>/dev/null || true
mkdir -p /var/lib/dbus
printf '%s\n' "$MID" > /var/lib/dbus/machine-id 2>/dev/null || true
rm -f /.dockerenv 2>/dev/null || true

if [ "${WEBOX_SPOOF_OS:-1}" = "1" ]; then
  cat > /etc/os-release <<'OSEOF'
PRETTY_NAME="deepin 23"
NAME="deepin"
VERSION_ID="23"
VERSION="23"
VERSION_CODENAME=beige
ID=deepin
ID_LIKE=debian
HOME_URL="https://www.deepin.org/"
BUG_REPORT_URL="https://bbs.deepin.org/"
OSEOF
fi
