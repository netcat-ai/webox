#!/usr/bin/env bash
set -euo pipefail

image="${WEBOX_IMAGE:-webox:local}"
port="${WEBOX_VERIFY_PORT:-39082}"
timeout_seconds="${WEBOX_VERIFY_TIMEOUT_SECONDS:-90}"
keep_container="${WEBOX_VERIFY_KEEP_CONTAINER:-0}"
container="${WEBOX_VERIFY_CONTAINER:-webox-qrcode-$(date +%s)}"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/webox-qrcode-verify.XXXXXX")"

cleanup() {
  if [ "$keep_container" = "1" ]; then
    echo "[verify-wechat-qrcode] keeping container: $container"
    echo "[verify-wechat-qrcode] keeping tmp dir: $tmp_dir"
  else
    docker rm -f "$container" >/dev/null 2>&1 || true
    rm -rf "$tmp_dir"
  fi
}
trap cleanup EXIT

if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "[verify-wechat-qrcode] missing image: $image" >&2
  echo "[verify-wechat-qrcode] run: docker build -t $image ." >&2
  exit 2
fi

mkdir -p "$tmp_dir/agentgateway" "$tmp_dir/state" "$tmp_dir/logs"

docker run -d \
  --name "$container" \
  -p "127.0.0.1:${port}:8080" \
  -e RUST_LOG="${RUST_LOG:-webox=debug,tower_http=info}" \
  -v "$tmp_dir/agentgateway:/webox/agentgateway" \
  -v "$tmp_dir/state:/webox/state" \
  -v "$tmp_dir/logs:/webox/logs" \
  "$image" >/dev/null

ready=0
for _ in $(seq 1 80); do
  if curl -fsS --max-time 2 "http://127.0.0.1:${port}/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 0.5
done
if [ "$ready" != "1" ]; then
  echo "[verify-wechat-qrcode] weagent did not become ready" >&2
  tail -n 120 "$tmp_dir/logs/weagent.log" 2>/dev/null || true
  tail -n 120 "$tmp_dir/logs/agentgateway.log" 2>/dev/null || true
  exit 3
fi

deadline=$((SECONDS + timeout_seconds))
last_response=""
while [ "$SECONDS" -lt "$deadline" ]; do
  last_response="$(curl -fsS --max-time 5 "http://127.0.0.1:${port}/get_bot_qrcode?bot_type=3" || true)"
  if printf '%s' "$last_response" | grep -q '"qrcode":"[^"]'; then
    echo "$last_response"
    echo "[verify-wechat-qrcode] captured login qrcode"
    exit 0
  fi
  sleep 2
done

echo "[verify-wechat-qrcode] qrcode was not captured within ${timeout_seconds}s" >&2
echo "[verify-wechat-qrcode] last response: ${last_response:-<empty>}" >&2
grep -n "getloginqrcode\\|checkloginqrcode\\|response.body\\|failed to terminate TLS" "$tmp_dir/logs/agentgateway.log" 2>/dev/null | tail -n 80 >&2 || true
exit 4
