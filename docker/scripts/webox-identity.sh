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
