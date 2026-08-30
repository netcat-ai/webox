#!/usr/bin/env bash
set -euo pipefail

export WEBOX_ROOT="${WEBOX_ROOT:-/webox}"
export WEBOX_STATE_DIR="${WEBOX_STATE_DIR:-${WEBOX_ROOT}/state}"
export WEBOX_WEAGENT_STATE_DIR="${WEBOX_WEAGENT_STATE_DIR:-${WEBOX_STATE_DIR}/weagent}"

mkdir -p "$WEBOX_STATE_DIR" "$WEBOX_WEAGENT_STATE_DIR"
chown webox:webox "$WEBOX_STATE_DIR" "$WEBOX_WEAGENT_STATE_DIR"
chmod 700 "$WEBOX_WEAGENT_STATE_DIR"

exec gosu webox "${WEBOX_ROOT}/wecomproxy/bin/wecomproxy"
