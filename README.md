# Webox

在单个 Docker 容器中运行 Linux 微信，并把真实客户端转换为兼容企业微信 AI Bot 的本地 WebSocket 服务，可直接接入 OpenClaw。

Webox 从微信本地数据库读取消息，通过 UI 自动化发送文本；不修改微信客户端，也不代理微信网络流量。

## 快速开始

要求 Docker 支持运行 Linux `amd64` 或 `arm64` 容器。

```bash
docker run -d \
  --name webox \
  --cap-add SYS_PTRACE \
  -e WEBOX_BOT_ID=webox \
  -e WEBOX_BOT_SECRET=change-this-secret \
  -p 127.0.0.1:38080:8080 \
  -p 127.0.0.1:6080:6080 \
  -v webox-state:/webox/state \
  ghcr.io/netcat-ai/webox:main
```

浏览器打开以下地址，扫描微信客户端显示的登录二维码：

```text
http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale
```

桌面端口默认只监听本机且没有额外密码。远程访问请使用 SSH 隧道，不要直接暴露到公网。

## 接入 OpenClaw

安装企业微信官方插件：

```bash
openclaw plugins install @wecom/wecom-openclaw-plugin
```

配置插件连接 Webox：

```bash
openclaw config set channels.wecom.enabled true
openclaw config set channels.wecom.botId webox
openclaw config set channels.wecom.secret change-this-secret
openclaw config set channels.wecom.websocketUrl ws://127.0.0.1:38080/wecom
openclaw config set channels.wecom.sendThinkingMessage false
openclaw gateway restart
```

`botId` 和 `secret` 必须与 Webox 的 `WEBOX_BOT_ID`、`WEBOX_BOT_SECRET` 一致。

Webox 接受企业微信的流式回复帧，但不会向微信逐段发送：`finish=false` 只更新内存缓存，`finish=true` 才发送一条完整消息。

## 多微信账号

每个微信账号使用独立容器、状态目录、端口和 Bot ID：

```bash
docker run -d \
  --name webox-a \
  --cap-add SYS_PTRACE \
  -e WEBOX_BOT_ID=webox-a \
  -e WEBOX_BOT_SECRET=change-this-secret-a \
  -p 127.0.0.1:38081:8080 \
  -p 127.0.0.1:6081:6080 \
  -v webox-a-state:/webox/state \
  ghcr.io/netcat-ai/webox:main
```

不要让多个容器共享状态目录。OpenClaw 多账号场景建议隔离私聊会话：

```bash
openclaw config set session.dmScope per-account-channel-peer
```

## 升级

状态保存在 Docker volume 中，重新创建容器不会删除微信登录和数据库密钥：

```bash
docker pull ghcr.io/netcat-ai/webox:main
docker rm -f webox
```

然后重新执行“快速开始”中的 `docker run`。微信可能在容器重启后要求手机再次确认登录。

## 从源码构建

```bash
git clone https://github.com/netcat-ai/webox.git
cd webox
```

从[微信 Linux 官网](https://linux.weixin.qq.com/)下载当前架构安装包，放入以下任一位置：

- `docker/wechat/WeChatLinux_x86_64.deb`
- `docker/wechat/WeChatLinux_arm64.deb`

然后构建并启动：

```bash
cp .env.example .env
mkdir -p data/state
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
scripts/preflight-container.sh
```

运行测试：

```bash
go test ./...
```

## 当前限制

- 只接收和发送文本消息；图片、语音、视频、文件和卡片暂不投递给 OpenClaw。
- OpenClaw 的流式回复会合并为一条完整微信消息。
- 私聊联系人和群聊需要设置唯一备注，Webox 依赖备注在微信 UI 中定位会话。
- 单个 Bot ID 同时只保留一个活动 WebSocket 连接。
- 一个状态目录只能对应一个 Webox 实例。

协议实现、数据流和安全边界见[架构文档](docs/architecture.md)，镜像源配置见[Docker 镜像说明](docs/docker-mirrors.md)。

## 参考与致谢

- [企业微信 AI Bot SDK](https://github.com/WecomTeam/aibot-node-sdk)
- [OpenClaw 企业微信官方插件](https://github.com/WecomTeam/wecom-openclaw-plugin)
- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)
- [wx-cli](https://github.com/jackwener/wx-cli)
- [wechat-decrypt](https://github.com/ylytdeng/wechat-decrypt)

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等基础开源组件。

## 许可证

Webox 自有代码使用 [MIT License](LICENSE)。第三方改编代码见 [Third-party notices](THIRD_PARTY_NOTICES.md)。
