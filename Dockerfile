ARG RUST_BUILDER_IMAGE=rust:1.96-bookworm
ARG DEBIAN_RUNTIME_IMAGE=debian:bookworm

FROM ${RUST_BUILDER_IMAGE} AS build
WORKDIR /src
COPY Cargo.toml Cargo.lock ./
COPY src ./src
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/src/target \
    cargo build --release --locked --bin weagent && \
    mkdir -p /out && cp target/release/weagent /out/weagent

FROM ${DEBIAN_RUNTIME_IMAGE} AS runtime-base
ARG APT_DEBIAN_MIRROR=
ARG APT_DEBIAN_SECURITY_MIRROR=

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=zh_CN.UTF-8 \
    LC_ALL=zh_CN.UTF-8 \
    LIBGL_ALWAYS_SOFTWARE=1

RUN set -eux; \
    if [ -n "$APT_DEBIAN_MIRROR" ] && [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|http://deb.debian.org/debian|$APT_DEBIAN_MIRROR|g" /etc/apt/sources.list.d/debian.sources; \
    fi; \
    if [ -n "$APT_DEBIAN_SECURITY_MIRROR" ] && [ -f /etc/apt/sources.list.d/debian.sources ]; then \
      sed -i "s|http://deb.debian.org/debian-security|$APT_DEBIAN_SECURITY_MIRROR|g; s|http://security.debian.org/debian-security|$APT_DEBIAN_SECURITY_MIRROR|g" /etc/apt/sources.list.d/debian.sources; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      ca-certificates dbus dbus-x11 dpkg ffmpeg gosu libcap2-bin locales \
      openbox procps tini x11-utils xclip xdotool xsettingsd xvfb xz-utils \
      fonts-wqy-zenhei fonts-wqy-microhei fonts-noto-cjk fonts-noto-color-emoji \
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
    rm -rf /var/lib/apt/lists/*

COPY --from=build /out/weagent /webox/weagent/bin/weagent
COPY docker/scripts/webox-identity.sh docker/scripts/entrypoint.sh /webox/weagent/bin/

RUN chmod 755 /webox/weagent/bin/weagent /webox/weagent/bin/*.sh && \
    setcap cap_sys_ptrace=ep /webox/weagent/bin/weagent

FROM runtime-base AS runtime

COPY docker/wechat/ /tmp/wechat/
RUN set -eux; \
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
    test -x /webox/wechat/opt/wechat/wechat; \
    rm -rf /tmp/wechat

VOLUME ["/webox/state"]
EXPOSE 8080

ENTRYPOINT ["/usr/bin/tini", "--", "/webox/weagent/bin/entrypoint.sh"]
