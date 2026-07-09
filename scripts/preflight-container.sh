#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
runtime_image="${WEBOX_RUNTIME_BASE_IMAGE:-webox:runtime-base-check}"
skip_wechat_deb="${WEBOX_PREFLIGHT_SKIP_WECHAT_DEB:-0}"
skip_runtime_base="${WEBOX_PREFLIGHT_SKIP_RUNTIME_BASE:-0}"

docker_arch() {
  docker info --format '{{.Architecture}}'
}

wechat_deb_for_arch() {
  case "$1" in
    amd64 | x86_64)
      echo "$repo_root/docker/wechat/WeChatLinux_x86_64.deb"
      ;;
    arm64 | aarch64)
      echo "$repo_root/docker/wechat/WeChatLinux_arm64.deb"
      ;;
    *)
      echo "[preflight] unsupported Docker architecture: $1" >&2
      return 1
      ;;
  esac
}

check_wechat_deb() {
  local arch deb
  arch="$(docker_arch)"
  deb="$(wechat_deb_for_arch "$arch")"
  if [ ! -s "$deb" ]; then
    echo "[preflight] missing bundled WeChat package for Docker architecture $arch:" >&2
    echo "[preflight]   $deb" >&2
    echo "[preflight] put the official Linux WeChat deb at that path before building the runtime image" >&2
    return 2
  fi
  echo "[preflight] found bundled WeChat package: $deb"
}

check_runtime_base() {
  if ! docker image inspect "$runtime_image" >/dev/null 2>&1; then
    echo "[preflight] missing runtime-base image: $runtime_image" >&2
    echo "[preflight] run: docker build --target runtime-base -t $runtime_image ." >&2
    return 3
  fi

  docker run --rm "$runtime_image" bash -lc '
    set -euo pipefail
    test -x /webox/weagent/bin/weagent
    test -x /webox/weagent/bin/entrypoint.sh
    test -x /webox/weagent/bin/wechat-ctl.sh
    test -x /webox/weagent/bin/webox-identity.sh
    for cmd in Xvfb openbox xdotool xclip gosu tini curl; do
      command -v "$cmd" >/dev/null
    done
    ldconfig -p | grep -q "libpulse\\.so\\.0"
    ldconfig -p | grep -q "libpulse-simple\\.so\\.0"
  '
  echo "[preflight] runtime-base image has required process and UI dependencies: $runtime_image"
}

if [ "$skip_wechat_deb" != "1" ]; then
  check_wechat_deb
else
  echo "[preflight] skipped bundled WeChat deb check"
fi

if [ "$skip_runtime_base" != "1" ]; then
  check_runtime_base
else
  echo "[preflight] skipped runtime-base dependency check"
fi

echo "[preflight] container prerequisites ok"
