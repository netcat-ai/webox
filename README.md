# Webox

在单个 Docker 容器中运行 Linux 微信，并将真实客户端转换为标准 iLink 接口，可作为 OpenClaw 的本地微信接入端。

Webox 从微信本地数据库读取消息，通过 UI 自动化发送文本，不修改微信客户端，也不代理微信网络流量。

## 快速开始

要求 Docker 支持运行 Linux `amd64` 或 `arm64` 容器。

```bash
git clone https://github.com/netcat-ai/webox.git
cd webox
docker compose up -d
```

确认服务已经启动：

```bash
curl http://127.0.0.1:38080/healthz
docker compose logs --tail 100 webox
```

首次登录前返回 `{"ok":true,"ready":false}` 是正常状态，表示正在等待扫码。

## 接入 OpenClaw

安装支持自定义 `baseUrl` 的微信插件：

```bash
openclaw plugins install @netcat-ai/openclaw-weixin
openclaw config set plugins.entries.openclaw-weixin.enabled true
```

将登录地址指向 Webox，然后开始登录：

```bash
openclaw config set channels.openclaw-weixin.baseUrl http://127.0.0.1:38080
openclaw channels login --channel openclaw-weixin
```

用微信扫描终端中的二维码并在手机上确认。登录后 Webox 会自动完成初始化，无需调用额外接口。Gateway、配对和渠道配置见 [OpenClaw 微信插件文档](https://github.com/Tencent/openclaw-weixin/blob/main/README.zh_CN.md)。

> 必须由 OpenClaw 发起本次扫码登录，否则 OpenClaw 无法保存 Webox 签发的 token。

## 查看微信桌面

浏览器打开：

```text
http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale
```

桌面端口默认只监听本机且没有额外密码。远程访问请使用 SSH 隧道，不要直接将 `WEBOX_DESKTOP_HOST` 暴露到公网。

## 多微信账号

每个微信账号必须使用独立容器、状态目录和端口：

```bash
COMPOSE_PROJECT_NAME=webox-a \
WEBOX_HTTP_PORT=38081 \
WEBOX_DESKTOP_PORT=6081 \
WEBOX_STATE_DIR="$HOME/.webox/wechat-a" \
docker compose up -d
```

登录该账号前切换 OpenClaw 地址：

```bash
openclaw config set channels.openclaw-weixin.baseUrl http://127.0.0.1:38081
openclaw channels login --channel openclaw-weixin
```

不要让多个容器共享状态目录。多账号使用 OpenClaw 时建议隔离私聊会话：

```bash
openclaw config set session.dmScope per-account-channel-peer
```

## 升级

状态保存在 `./data/state`，升级镜像不会删除登录数据：

```bash
docker compose pull webox
docker compose up -d --no-build
```

微信可能在容器重启后要求手机再次确认登录，这是客户端自身的会话恢复流程。

## 从源码构建

从[微信 Linux 官网](https://linux.weixin.qq.com/)下载当前架构的安装包，并放入以下任一位置：

- `docker/wechat/WeChatLinux_x86_64.deb`
- `docker/wechat/WeChatLinux_arm64.deb`

然后构建并启动：

```bash
cp .env.example .env
mkdir -p data/state
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
scripts/preflight-container.sh
```

本地验证 iLink 收发流程：

```bash
cargo run --manifest-path tools/webox-tui/Cargo.toml --locked
```

## 当前限制

- 只支持文本发送；图片、语音、视频和文件发送会明确返回不支持。
- 入站图片、视频和已下载文件可通过 iLink 媒体接口读取。
- 私聊联系人和群聊需要设置唯一备注，Webox 依赖备注在微信 UI 中定位会话。
- 收到未备注目标的消息时，Webox 会向当前登录用户发送一次 `wb-{target_id} 原始昵称` 提醒，并按标记查询历史去重。
- 一个状态目录只能对应一个 Webox 实例。

协议实现、数据流和安全边界见 [架构文档](docs/architecture.md)，镜像源配置见 [Docker 镜像说明](docs/docker-mirrors.md)。

## 参考与致谢

Webox 在独立实现过程中参考了以下项目：

- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)：Linux 微信容器化、本地数据库读取和 UI 自动化思路。
- [TinyClaw](https://github.com/netcat-ai/tinyclaw)：消息通道、iLink 交互形状及系统边界设计。
- [wechatbot](https://github.com/corespeed-io/wechatbot)：iLink 协议文档和多语言 SDK，用于协议兼容性验证。
- [openclaw-weixin](https://github.com/Tencent/openclaw-weixin)：OpenClaw 微信渠道的登录与配置流程。

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等基础开源组件。Webox 与上述项目保持独立，具体实现取舍见[架构文档](docs/architecture.md)。
