ARG GO_BUILDER_IMAGE=golang:1.26-bookworm
ARG DEBIAN_RUNTIME_IMAGE=debian:bookworm-slim

FROM ${GO_BUILDER_IMAGE} AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY wecom ./wecom
COPY docker/wecom/sdk_arm_v3_20250205.tgz /tmp/sdk_arm_v3_20250205.tgz
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    echo 'f8f29e52737740aacc34f64a52be8f2cc48a4bbcb3482880297d9eaac6c54671  /tmp/sdk_arm_v3_20250205.tgz' | sha256sum -c - && \
    sdk_dir="$(go list -m -f '{{.Dir}}' github.com/NICEXAI/WeWorkFinanceSDK)" && \
    if [ "$(go env GOARCH)" = "arm64" ]; then \
      mkdir -p /tmp/wecom-sdk && \
      tar -xzf /tmp/sdk_arm_v3_20250205.tgz -C /tmp/wecom-sdk && \
      test "$(cat /tmp/wecom-sdk/C_sdk/version.txt)" = "20250205" && \
      cp /tmp/wecom-sdk/C_sdk/libWeWorkFinanceSdk_C.so "$sdk_dir/lib/" && \
      cp /tmp/wecom-sdk/C_sdk/WeWorkFinanceSdk_C.h "$sdk_dir/lib/"; \
    fi && \
    mkdir -p /out/lib && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -extldflags=-s" -o /out/weagent ./cmd/weagent && \
    CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -extldflags=-s" -o /out/wecomproxy ./cmd/wecomproxy && \
    cp "$sdk_dir/lib/libWeWorkFinanceSdk_C.so" /out/lib/

FROM ${DEBIAN_RUNTIME_IMAGE} AS novnc-assets
RUN apt-get update && \
    apt-get install -y --no-install-recommends novnc && \
    mkdir -p /out && cp -a /usr/share/novnc/. /out/

FROM ${DEBIAN_RUNTIME_IMAGE} AS runtime-base
ARG APT_DEBIAN_MIRROR=
ARG APT_DEBIAN_SECURITY_MIRROR=

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=zh_CN.UTF-8 \
    LC_ALL=zh_CN.UTF-8 \
    LIBGL_ALWAYS_SOFTWARE=1 \
    LD_LIBRARY_PATH=/webox/weagent/lib

RUN set -eux; \
    if [ -n "$APT_DEBIAN_MIRROR" ] && [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|http://deb.debian.org/debian|$APT_DEBIAN_MIRROR|g" /etc/apt/sources.list.d/debian.sources; \
    fi; \
    if [ -n "$APT_DEBIAN_SECURITY_MIRROR" ] && [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|http://deb.debian.org/debian-security|$APT_DEBIAN_SECURITY_MIRROR|g; s|http://security.debian.org/debian-security|$APT_DEBIAN_SECURITY_MIRROR|g" /etc/apt/sources.list.d/debian.sources; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates dbus dpkg ffmpeg gosu libcap2-bin locales \
      openbox tini websockify x11vnc xclip xdotool xsettingsd xvfb \
      fonts-wqy-microhei fonts-noto-color-emoji \
      libatomic1 libnss3 libgbm1 libasound2 libpulse0 libxss1 libxdamage1 libxkbcommon-x11-0 \
      libxcb-icccm4 libxcb-image0 libxcb-keysyms1 libxcb-render-util0 libxcb-xkb1 libxcb-cursor0 \
      libgtk-3-0 libatk1.0-0 libatk-bridge2.0-0 libatspi2.0-0 libcups2 \
      libxcomposite1 libxrandr2 libxfixes3 libxtst6 libxshmfence1 libdrm2; \
    sed -i 's/# zh_CN.UTF-8 UTF-8/zh_CN.UTF-8 UTF-8/' /etc/locale.gen; \
    locale-gen; \
    useradd -M -d /webox/state/home -u 1000 -s /bin/bash webox; \
    mkdir -p /webox/wechat /webox/weagent/bin /webox/state /webox/runtime; \
    chown -R webox:webox /webox; \
    apt-get clean; \
    rm -rf \
      /var/lib/apt/lists/* \
      /usr/share/doc/* \
      /usr/share/man/* \
      /usr/share/locale/*

COPY --from=novnc-assets /out/ /usr/share/novnc/

FROM runtime-base AS runtime-wechat

RUN --mount=type=bind,source=docker/wechat,target=/tmp/wechat,ro \
    set -eux; \
    arch="$(dpkg --print-architecture)"; \
    case "$arch" in \
      amd64) deb="/tmp/wechat/WeChatLinux_x86_64.deb" ;; \
      arm64) deb="/tmp/wechat/WeChatLinux_arm64.deb" ;; \
      *) echo "unsupported architecture for bundled WeChat: $arch" >&2; exit 1 ;; \
    esac; \
    if [ ! -f "$deb" ]; then \
      echo "missing bundled WeChat package: $deb" >&2; \
      echo "put WeChatLinux_x86_64.deb or WeChatLinux_arm64.deb under docker/wechat before building" >&2; \
      exit 1; \
    fi; \
    dpkg-deb -x "$deb" /webox/wechat; \
    dpkg-deb -f "$deb" Version > /webox/wechat/.webox-version; \
    test -x /webox/wechat/opt/wechat/wechat

FROM runtime-wechat AS runtime

COPY --from=build /out/weagent /webox/weagent/bin/weagent
COPY --from=build /out/lib/ /webox/weagent/lib/
COPY docker/scripts/webox-identity.sh docker/scripts/entrypoint.sh /webox/weagent/bin/

RUN chmod 755 /webox/weagent/bin/weagent /webox/weagent/bin/*.sh && \
    setcap cap_sys_ptrace=ep /webox/weagent/bin/weagent

VOLUME ["/webox/state"]
EXPOSE 8080 6080

ENTRYPOINT ["/usr/bin/tini", "--", "/webox/weagent/bin/entrypoint.sh"]

# The archive proxy has no desktop WeChat, SQLCipher, X11, noVNC, clipboard or
# UI sender dependency. It intentionally starts from the plain Debian image.
FROM ${DEBIAN_RUNTIME_IMAGE} AS runtime-wecom

ENV DEBIAN_FRONTEND=noninteractive \
    LD_LIBRARY_PATH=/webox/wecomproxy/lib

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates gosu tini tzdata zlib1g && \
    useradd -M -d /webox/state/home -u 1000 -s /usr/sbin/nologin webox && \
    mkdir -p /webox/wecomproxy/bin /webox/wecomproxy/lib /webox/state && \
    chown -R webox:webox /webox && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/* /usr/share/doc/* /usr/share/man/*

COPY --from=build /out/wecomproxy /webox/wecomproxy/bin/wecomproxy
COPY --from=build /out/lib/ /webox/wecomproxy/lib/
COPY docker/scripts/wecomproxy-entrypoint.sh /webox/wecomproxy/bin/

RUN chmod 755 /webox/wecomproxy/bin/wecomproxy /webox/wecomproxy/bin/wecomproxy-entrypoint.sh

VOLUME ["/webox/state"]
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "/webox/wecomproxy/bin/wecomproxy-entrypoint.sh"]
