#!/usr/bin/env bash
set -euo pipefail

image="${WEBOX_RUNTIME_IMAGE:-webox:runtime-base-check}"
probe_url="${WEBOX_VERIFY_URL:-https://httpbingo.org/anything/getloginqrcode?uuid=webox-verify}"
keep_tmp="${WEBOX_VERIFY_KEEP_TMP:-0}"
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/webox-agentgateway-verify.XXXXXX")"

cleanup() {
  if [ "$keep_tmp" = "1" ]; then
    echo "[verify-agentgateway] keeping tmp dir: $tmp_dir"
  else
    rm -rf "$tmp_dir"
  fi
}
trap cleanup EXIT

if ! docker image inspect "$image" >/dev/null 2>&1; then
  echo "[verify-agentgateway] missing image: $image" >&2
  echo "[verify-agentgateway] run: docker build --target runtime-base -t $image ." >&2
  exit 2
fi

cp "$repo_root/docker/agentgateway/config.example.yaml" "$tmp_dir/config.yaml"
mkdir -p "$tmp_dir/certificates"
openssl req -x509 -newkey rsa:2048 -sha256 -days 1 -nodes \
  -keyout "$tmp_dir/certificates/webox-ca-key.pem" \
  -out "$tmp_dir/certificates/webox-ca.pem" \
  -subj "/CN=webox verify agentgateway CA" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" >/dev/null 2>&1

docker run --rm \
  -e WEBOX_VERIFY_URL="$probe_url" \
  -v "$tmp_dir:/work" \
  "$image" \
  bash -lc '
    set -euo pipefail
    cd /work
    agentgateway -f /work/config.yaml > /work/agentgateway.log 2>&1 &
    gateway_pid=$!
    cleanup_gateway() {
      kill "$gateway_pid" 2>/dev/null || true
      wait "$gateway_pid" 2>/dev/null || true
    }
    trap cleanup_gateway EXIT

    ready=0
    for _ in $(seq 1 80); do
      if timeout 1 bash -c "</dev/tcp/127.0.0.1/18080" 2>/dev/null; then
        ready=1
        break
      fi
      sleep 0.25
    done
    if [ "$ready" != "1" ]; then
      echo "agentgateway did not listen on 18080" >&2
      exit 3
    fi

    curl -fsS --max-time 25 \
      -x http://127.0.0.1:18080 \
      --cacert /work/certificates/webox-ca.pem \
      -H "content-type: application/json" \
      -d "{\"probe\":\"qrcode\"}" \
      "$WEBOX_VERIFY_URL" >/work/response.json

    sleep 1
    grep -q "\"route\":\"default/webox-dynamic-https\"" /work/agentgateway.log
    grep -q "getloginqrcode" /work/agentgateway.log
    grep -q "\"request.body\":\"eyJwcm9iZSI6InFyY29kZSJ9\"" /work/agentgateway.log
    grep -q "\"response.body\":" /work/agentgateway.log
  '

echo "[verify-agentgateway] captured request.body and response.body from HTTPS MITM"
