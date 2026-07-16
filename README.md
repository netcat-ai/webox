# Webox

在单个 Docker 容器中运行 Linux 微信，并把真实客户端转换为标准 iLink HTTP 接口，可直接接入 OpenClaw。

Webox 的 agent 使用 Go 实现：从微信本地数据库读取消息，通过 UI 自动化发送文本；不修改微信客户端，也不代理微信网络流量。

## 前置要求

- Docker 支持运行 Linux `amd64` 或 `arm64` 容器。
- 已安装并初始化 OpenClaw，版本不低于 `2026.3.28`。
- 准备一个专门登录 Webox 的微信账号，以及另一个用于收发测试消息的微信账号。
- 在 Webox 登录的微信中，给允许 Agent 处理的联系人和群聊设置以 `webox.` 开头的唯一备注。Webox 默认只向 iLink 输出这些会话的消息，并使用备注在微信界面中定位回复目标。

不要使用承担支付、工作或重要社交关系的主微信账号做首次验证。

## 1. 启动 Webox

```bash
docker run -d \
  --name webox \
  --cap-add SYS_PTRACE \
  -p 127.0.0.1:38080:8080 \
  -p 127.0.0.1:6080:6080 \
  -v webox-state:/webox/state \
  ghcr.io/netcat-ai/webox:main
```

可通过 noVNC 查看真实微信桌面：

```text
http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale
```

桌面端口默认只监听本机且没有额外密码。远程访问请使用 SSH 隧道，不要直接暴露到公网。

此时不要直接在 noVNC 中扫码。首次接入应由 OpenClaw 发起 iLink 登录，这样 OpenClaw 才能保存 Webox 签发的 token。

## 2. 接入 OpenClaw

关闭旧的企业微信通道，安装 iLink 微信插件：

```bash
openclaw config set channels.wecom.enabled false
openclaw plugins install @netcat-ai/openclaw-weixin
openclaw config set plugins.entries.openclaw-weixin.enabled true
```

将插件登录地址指向 Webox，并创建独立账号：

```bash
openclaw config set channels.openclaw-weixin.baseUrl http://127.0.0.1:38080
openclaw channels login --channel openclaw-weixin --account webox
```

用手机微信扫描终端显示的二维码并确认。这个二维码来自 Webox 容器中的真实 Linux 微信登录窗口，不是文件传输助手流程。

确认后等待 Webox 完成数据库初始化：

```bash
until curl -fsS http://127.0.0.1:38080/healthz | grep -q '"ready":true'; do
  echo "waiting for WeChat login and initialization..."
  sleep 2
done
openclaw gateway restart
```

显式允许 agent 的普通回复由当前会话通道发回微信。否则某些 OpenClaw 运行环境会使用 `message_tool_only`，agent 虽然生成了正文，通道却不会自动发送：

```bash
openclaw config set messages.visibleReplies automatic
openclaw config set messages.groupChat.visibleReplies automatic
openclaw gateway restart
```

群聊默认使用 OpenClaw 的 mention gate；配置机器人昵称“虾虾”为文本唤醒模式：

```bash
openclaw config set 'channels.openclaw-weixin.groups["*"]' '{"requireMention":true}' --strict-json
openclaw config set messages.groupChat.mentionPatterns '["虾虾"]' --strict-json
openclaw gateway restart
```

因此私聊只要备注为 `webox.` 前缀即可触发；群聊还需要 @机器人，或消息正文包含“虾虾”。

`--account webox` 用于避免复用其他 iLink 服务的账号、token 和 `baseUrl`。每个 Webox 实例使用不同的账号名。

如果微信已经提前登录，而 OpenClaw 没有保存过这个 Webox volume 对应的 token，登录接口不会把 token 直接交给匿名调用者。请在 noVNC 中退出或切换微信账号，让登录二维码重新出现，再执行上面的 `openclaw channels login`。如果 OpenClaw 已保存相同 token，则插件可直接恢复，无需重新扫码。

## 3. 验证私聊

1. 在 Webox 登录的微信中，给测试联系人设置唯一备注，例如 `webox.私聊测试`。
2. 使用另一个微信账号向 Webox 账号发送：`只回复 WEBOX_DM_OK`。
3. 确认该会话只收到一条 `WEBOX_DM_OK`，不应再出现 `✅ 已收到`。
4. 检查 Webox 已完成 UI 发送和数据库回读验证：

```bash
docker logs --since 5m webox 2>&1 | grep 'WeChat text sent'
```

## 4. 验证群聊

1. 建立一个同时包含 Webox 账号和测试账号的群聊。
2. 在 Webox 登录的微信中给该群设置唯一备注，例如 `webox.群聊测试`。
3. 使用测试账号在群内 @机器人发送：`虾虾，只回复 WEBOX_GROUP_OK`。
4. 确认同一个群聊只收到一条回复，并再次检查 `WeChat text sent` 日志。

私聊和群聊都收到唯一回复，且两次发送均有成功日志，才算完成端到端验证。`scripts/preflight-container.sh` 只检查镜像依赖，不能代替真实消息验收。

### 自动化真实 E2E

`tests/e2e` 提供双微信账号的自动闭环，覆盖 iLink 私聊、OpenClaw 私聊和 OpenClaw 群聊。首次为两个专用测试账号扫码并在 Peer 端设置唯一联系人/群备注后，runner 会自动完成 Peer UI 发消息、SUT 或 OpenClaw 回复、Peer iLink 最终验收，并在失败时收集容器日志和微信桌面截图：

```bash
docker compose -f tests/e2e/docker-compose.yml up -d
go run ./tests/e2e --peer-target Webox被测账号
go run ./tests/e2e --scenario openclaw-direct --peer-target Webox被测账号
go run ./tests/e2e --scenario openclaw-group --peer-target Webox测试群
```

完整的一次性准备和状态清理说明见 [`tests/e2e/README.md`](tests/e2e/README.md)。

## iLink 接口

Go agent 提供以下路由：

- `GET /healthz`
- `GET|POST /ilink/bot/get_bot_qrcode?bot_type=3`
- `GET /ilink/bot/get_qrcode_status?qrcode=...`
- `POST /ilink/bot/getupdates`
- `POST /ilink/bot/sendmessage`
- `POST /ilink/bot/getconfig`
- `POST /ilink/bot/sendtyping`
- `POST /ilink/bot/msg/notifystart`
- `POST /ilink/bot/msg/notifystop`

`getupdates` 最多长轮询 35 秒。首次空游标只建立当前数据库基线，不回放登录前历史；游标使用持久密钥签名，只能原样回传。每条入站消息带有签名 `context_token`，回复必须原样放入 `sendmessage`。

`sendmessage` 使用 `msg.client_id` 做进程内幂等，并且只有在微信 UI 操作完成、同一会话的精确文本能从本地数据库读回后才返回 `ret=0`。这与原 WeCom 实现的异步入队 ACK 不同。

iLink 业务请求使用标准请求头：

```http
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
X-WECHAT-UIN: <base64(random_uint32)>
```

token 和 provider account ID 自动生成并持久化在 `/webox/state/weagent`。反向代理部署可用 `WEBOX_PUBLIC_BASE_URL=https://webox.example.com` 覆盖二维码确认响应中的 `baseurl`。

Webox 默认启用备注过滤，仅输出备注以 `webox.` 开头的私聊和群聊消息。如需把 Webox 作为不带该安全边界的通用 iLink 服务，可在启动容器时设置：

```bash
-e WEBOX_REMARK_FILTER_ENABLED=false
```

## 故障排查

- `/healthz` 中 `ready=false`：尚未扫码、手机未确认，或微信数据库仍在初始化；打开 noVNC 查看微信状态。
- OpenClaw 登录返回 401：微信已经登录，但 OpenClaw 没有当前 Webox token；让微信回到二维码页后重新由 OpenClaw 发起扫码。
- 能收到消息但微信没有回复：确认联系人或群聊设置了非空且唯一的备注，并查看 `docker logs webox` 中的发送错误。
- 没有收到新消息：确认已禁用 `channels.wecom`，iLink 账号 `baseUrl` 指向当前 Webox，并重启 gateway。
- `sendtyping` 返回 HTTP 501：Linux 微信 UI 没有可靠的输入状态动作，这是预期行为，不影响正文回复。

## 多微信账号

每个微信账号使用独立容器、状态目录、端口和 OpenClaw account：

```bash
docker run -d \
  --name webox-a \
  --cap-add SYS_PTRACE \
  -p 127.0.0.1:38081:8080 \
  -p 127.0.0.1:6081:6080 \
  -v webox-a-state:/webox/state \
  ghcr.io/netcat-ai/webox:main

openclaw config set channels.openclaw-weixin.baseUrl http://127.0.0.1:38081
openclaw channels login --channel openclaw-weixin --account webox-a
```

不要让多个容器共享状态目录。OpenClaw 多账号场景建议隔离私聊会话：

```bash
openclaw config set session.dmScope per-account-channel-peer
```

## 升级

状态保存在 Docker volume 中，重新创建容器不会删除微信登录、iLink token 和数据库密钥：

```bash
docker pull ghcr.io/netcat-ai/webox:main
docker rm -f webox
```

然后重新执行“启动 Webox”中的 `docker run`。微信可能在容器重启后要求手机再次确认登录。

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
go test ./...
```

## 当前限制

- 只支持文本发送；图片、语音、视频和文件发送会明确返回 HTTP 501。
- 非文本入站消息当前转换成 `[图片]`、`[语音]` 等可读文本占位，不提供二进制下载。
- 私聊联系人和群聊需要设置唯一备注，Webox 依赖备注在微信 UI 中定位会话。
- 主动发送必须携带此前入站消息签发的 `context_token`。
- 一个状态目录只能对应一个 Webox 实例。

协议实现、数据流和安全边界见[架构文档](docs/architecture.md)，镜像源配置见[Docker 镜像说明](docs/docker-mirrors.md)。

## 参考与致谢

- [OpenClaw 微信插件](https://github.com/Tencent/openclaw-weixin)
- [iLink 协议文档](https://www.wechatbot.dev/zh/protocol)
- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)
- [wx-cli](https://github.com/jackwener/wx-cli)
- [wechat-decrypt](https://github.com/ylytdeng/wechat-decrypt)

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等基础开源组件。

## 许可证

Webox 自有代码使用 [MIT License](LICENSE)。第三方改编代码见 [Third-party notices](THIRD_PARTY_NOTICES.md)。
