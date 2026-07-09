#!/usr/bin/env bash
set -euo pipefail

version="${AGENTGATEWAY_VERSION:-v1.4.0-alpha.1}"
install_dir="${AGENTGATEWAY_INSTALL_DIR:-/usr/local/bin}"
release_url_base="${AGENTGATEWAY_RELEASE_URL_BASE:-https://github.com/agentgateway/agentgateway/releases/download}"
github_api_url="${AGENTGATEWAY_GITHUB_API_URL:-https://api.github.com/repos/agentgateway/agentgateway/releases/latest}"

if [ "$version" = "skip" ] || [ "$version" = "none" ]; then
  echo "[install-agentgateway] skipping install"
  exit 0
fi

case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  armv7l) arch=arm ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$version" = "latest" ]; then
  version="$(curl -fsSL "$github_api_url" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' \
    | head -1)"
fi
if [ -z "$version" ]; then
  echo "failed to resolve agentgateway version" >&2
  exit 1
fi
case "$version" in
  v* | canary) ;;
  *) version="v${version}" ;;
esac

dist="agentgateway-linux-${arch}"
base_url="${release_url_base%/}/${version}"
tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

echo "[install-agentgateway] downloading ${dist} ${version} from ${base_url}"
curl -fsSL "${base_url}/${dist}" -o "${tmp_dir}/${dist}"
curl -fsSL "${base_url}/${dist}.sha256" -o "${tmp_dir}/${dist}.sha256"

expected="$(awk '{print $1}' "${tmp_dir}/${dist}.sha256")"
actual="$(sha256sum "${tmp_dir}/${dist}" | awk '{print $1}')"
if [ "$actual" != "$expected" ]; then
  echo "checksum mismatch for ${dist}" >&2
  exit 1
fi

install -m 0755 -D "${tmp_dir}/${dist}" "${install_dir}/agentgateway"
"${install_dir}/agentgateway" --version
