# webox

单容器运行 Linux 微信、虚拟 Display 和 Rust `weagent`，把真实客户端投影成标准 iLink 接口。

## 启动

```bash
cp .env.example .env
mkdir -p data/state
docker compose up -d --build
```

构建镜像前需要把微信 Linux deb 放到 `docker/wechat/`：

- `docker/wechat/WeChatLinux_x86_64.deb`
- `docker/wechat/WeChatLinux_arm64.deb`

从微信官方 Linux 页面下载：https://linux.weixin.qq.com/ 。镜像构建会把 deb 内置进去；容器启动后不会动态下载或更新微信。
本地 deb 已被 `.gitignore` 忽略，不提交到仓库。

缺少 WeChat deb 时，可以先验证 Rust 和运行时依赖：

```bash
docker build --target runtime-base -t webox:runtime-base-check .
WEBOX_RUNTIME_IMAGE=webox:runtime-base-check WEBOX_PREFLIGHT_SKIP_WECHAT_DEB=1 scripts/preflight-container.sh
```

完整构建后检查当前 Docker 架构需要的内置 WeChat deb 和运行时依赖：

```bash
docker compose build
scripts/preflight-container.sh
```

验证真实 Linux WeChat 登录二维码提取：

```bash
docker build -t webox:local .
scripts/verify-wechat-qrcode-capture.sh
```

服务默认暴露在 `http://127.0.0.1:38080`。

默认 Compose 配置使用官方 base image 和 Debian apt 源，依赖 Docker daemon 的全局代理：

```dotenv
RUST_BUILDER_IMAGE=rust:1.96-bookworm
DEBIAN_RUNTIME_IMAGE=debian:bookworm
APT_DEBIAN_MIRROR=
APT_DEBIAN_SECURITY_MIRROR=
```

Docker Hub 连接问题应通过 Docker daemon 全局代理或用户自己的阿里云 ACR 加速器处理，项目不改写到第三方 registry 代理。详见 `docs/docker-mirrors.md`。

## 目标接口

协议标准以 https://www.wechatbot.dev/zh/protocol 为准。

- `GET /healthz`
- `GET|POST /ilink/bot/get_bot_qrcode?bot_type=3`
- `GET /ilink/bot/get_qrcode_status?qrcode=...`
- `POST /ilink/bot/getupdates`
- `POST /ilink/bot/sendmessage`
- `POST /ilink/bot/getconfig`
- `POST /ilink/bot/sendtyping`
- `POST /ilink/bot/msg/notifystart`
- `POST /ilink/bot/msg/notifystop`
- `GET /c2c/download`

`/ilink/bot/get_bot_qrcode` 返回从当前 WeChat 登录窗口解码出的二维码 URL。协议文档使用 GET；当前 SDK 使用带本地 token 的 POST，两种方法进入同一个签发流程。POST 请求体为：

```json
{ "local_token_list": [] }
```

```json
{
  "qrcode": "xvfb-qr-...",
  "qrcode_img_content": "http://weixin.qq.com/x/..."
}
```

`/ilink/bot/get_qrcode_status` 只轮询本地 WeChat 登录状态，不承担初始化工作。后台初始化器在检测到主窗口登录成功后自动
提取并验证 DB key；能读取消息时返回 `confirmed`，并返回后续业务请求使用的 `bot_token` 和 `baseurl`。当前只从本机 WeChat 状态推导
`wait`、`scaned`、`confirmed`。未确认的二维码会话最多保留 5 分钟；超时或 WeChat 刷新二维码后，旧 ID 返回 `expired`，客户端重新调用
`/ilink/bot/get_bot_qrcode` 时 weagent 会点击 WeChat 的过期二维码刷新区域并提取新图。确认结果只会返回给本进程实际签发的二维码 ID，未知 ID 始终返回 `expired`：

```json
{
  "status": "confirmed",
  "bot_token": "<generated-or-configured-token>",
  "ilink_bot_id": "default",
  "ilink_user_id": "default",
  "baseurl": "http://127.0.0.1:38080"
}
```

`/ilink/bot/getupdates` 使用标准 `get_updates_buf` 不透明游标：

```json
{
  "get_updates_buf": "",
  "base_info": { "channel_version": "2.0.0" }
}
```

响应会按 iLink 语义长轮询最多 35 秒，包含 `ret`、`msgs` 和新的 `get_updates_buf`。每条消息包含 `context_token`，回复时必须原样放进
`/ilink/bot/sendmessage` 的 `msg.context_token`。首次空游标只建立当前数据库基线，不回放登录前历史；游标按会话和消息分片保存精确的 `(create_time, local_id)`，并使用持久 API token 签名，客户端只能原样回传。

容器启动后所有 iLink 路由立即可用。状态变成 `confirmed` 后，收发接口自动可用，不存在额外 `/init` 调用。已建立的微信会话退出后，`getupdates` 返回 `ret=-14`，客户端应重新进入二维码登录流程。

`sendmessage` 使用标准 `msg.client_id` 做进程内幂等：同一请求重试会返回第一次的结果，不会再次操作微信 UI；同一
`client_id` 携带不同内容会被拒绝。缓存最多保留最近一批请求并受 1024 条上限约束，容器重启后清空。

```json
{
  "msg": {
    "to_user_id": "wxid_xxx",
    "context_token": "...",
    "text": "hello"
  },
  "base_info": { "channel_version": "2.0.0" }
}
```

`to_user_id` 是标准 SDK 会携带的兼容字段，weagent 发送路由只信任 `context_token`，避免第三方绕开入站会话上下文直发。
`context_token` 使用 `WEBOX_API_TOKEN` 做 HMAC 签名，客户端只需原样透传；修改目标或更换 API token 后，旧 token 会失效。
未配置 `WEBOX_API_TOKEN` 时，weagent 会在 `/webox/state/weagent/api-token` 生成并持久化随机令牌，容器重启不会改变它。
微信已登录时，二维码接口只允许 `local_token_list` 含当前 token 的标准客户端恢复连接，不会向未认证调用者直接签发 token。

`WEBOX_PUBLIC_BASE_URL` 可覆盖登录确认返回的 `baseurl`。这里应配置服务根地址，例如
`https://webox.example.com`。如果通过带路径前缀的反向代理暴露服务，这里应包含该前缀。如果不设置，默认从请求
`Host` 派生 `http://host`。

媒体相关路径：

业务 POST 请求必须带标准 iLink 请求头：

```http
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
X-WECHAT-UIN: <base64(String(random_uint32))>
```

- `WEBOX_MEDIA_STORE_DIR`：入站媒体的有时效加密缓存，默认 `/webox/state/weagent/media`。

`/ilink/bot/getconfig` 生成签名 `typing_ticket`。Linux WeChat UI 没有可靠的输入状态动作，因此 `/ilink/bot/sendtyping` 明确返回 HTTP 501，不伪造成功。
`/ilink/bot/msg/notifystart` 和 `/ilink/bot/msg/notifystop` 接收标准 SDK 启停通知并返回 `ret=0`。

二进制图片、语音、视频和文件发送不受支持，`sendmessage` 会明确返回 HTTP 501，不伪造成功。调用方应提供外部可访问 URL，weagent 将 URL 作为普通文本发送。

收到的本地图片、视频和文件会加密后映射为标准 iLink media item，客户端通过 `/c2c/download` 获取加密字节。普通文件只有在微信已下载到本地后才能返回附件；未落盘时退回文本项。

入站媒体 capability 按内容复用，重复提交同一 `get_updates_buf` 不会反复创建缓存对象；缓存同时受 24 小时 TTL 和 1GB 总容量限制。

文本发送只有在 WeChat 本地 DB 中读回同一会话的精确文本后才返回 `ret=0`，避免窗口被弹窗遮挡或搜索选错时误报成功。
群聊发送要求本地联系人记录存在备注，并用该备注搜索后选择第一个结果；部署方必须保证备注能唯一定位目标会话。

## 核心边界

- `weagent` 对外只提供标准 iLink 协议和健康检查。
- 收消息来自 WeChat 本地 DB 解密读取。
- 发消息通过 UI 自动化操作 Linux WeChat 客户端。
- 登录二维码从 Xvfb 中定位并解码为登录 URL。
- 不保留 WOC `/agent/*` API。
- 不内置 msghub-style actor/message/task 数据库。
- 不把 WeChat DB 的内部 cursor、scanner meta 作为 iLink 响应字段暴露。

## 运行边界

容器启动后会：

1. 生成持久 machine-id。
2. 启动 Xvfb + openbox，并把 framebuffer 写到 `/webox/runtime/xvfb/Xvfb_screen0`。
3. 启动 Rust `weagent`。
4. 启动镜像内置的 Linux 微信并直接连接上游。
5. 后台观察登录窗口；持久账号页会自动点击“登录”，主窗口出现后自动提取并验证 DB key。

entrypoint 使用 `tini` 加最小 shell supervisor。`Xvfb`、`openbox`、`weagent` 或 WeChat
任一关键进程退出时，容器退出；Compose 的 `restart: unless-stopped` 负责重启容器。
`openbox` 只提供窗口激活、层级和焦点管理；没有窗口管理器时，`xdotool` 无法稳定操作 WeChat 主窗和文件选择器。

首次扫码后，WeChat 可能在容器重启时要求手机再次确认“登录”，这是官方客户端的会话恢复流程；持久化 HOME 和 DB key 不会丢失。

容器内工作目录统一在 `/webox` 下：

- `/webox/wechat`：镜像内置 Linux WeChat 安装目录，不挂载覆盖。
- `/webox/weagent`：weagent 二进制和启动脚本。
- `/webox/state`：machine-id、NSS DB、WeChat HOME 和运行状态。

Compose 只持久化 `./data/state`，不要把整个 `/webox` 作为一个 bind mount 覆盖掉。所有进程写容器
stdout/stderr，由 Compose 的 Docker 日志驱动统一轮转；使用 `docker compose logs` 查看。

`weagent` 从 `WEBOX_QR_SCREENSHOT_PATH` 指向的 Xvfb framebuffer 读取登录窗口。它先检测 WeChat 蓝色二维码，
再用 QR 解码器确认并提取登录 URL，作为标准 `qrcode_img_content` 返回。WeChat 直接联网，不安装额外 CA，也不修改客户端网络进程。

为从同一用户运行的 WeChat 进程内存提取本地 DB key，Compose 只增加 `SYS_PTRACE`，并只给 `weagent` 二进制
`cap_sys_ptrace=ep`；容器不使用 `privileged` 或 `seccomp=unconfined`。
