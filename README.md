# webox

单容器运行 Linux 微信、虚拟 Display、`agentgateway` MITM 网关和 `weagent`。

## 启动

```bash
cp .env.example .env
mkdir -p data/agentgateway data/state data/logs
docker compose up -d --build
```

如果 `data/agentgateway/config.yaml` 不存在，entrypoint 会自动复制镜像内置的默认配置。已经验证过的
自定义配置仍然可以挂载到这个路径覆盖默认值。

构建镜像前需要把微信 Linux deb 放到 `docker/wechat/`：

- `docker/wechat/WeChatLinux_x86_64.deb`
- `docker/wechat/WeChatLinux_arm64.deb`

从微信官方 Linux 页面下载：https://linux.weixin.qq.com/ 。镜像构建会把 deb 内置进去；容器启动后不会动态下载或更新微信。
本地 deb 已被 `.gitignore` 忽略，不提交到仓库。

缺少 WeChat deb 时，可以先验证 Rust、运行时依赖和 agentgateway 安装：

```bash
docker build --target runtime-base -t webox:runtime-base-check .
WEBOX_PREFLIGHT_SKIP_WECHAT_DEB=1 scripts/preflight-container.sh
```

完整构建前检查当前 Docker 架构需要的内置 WeChat deb 和运行时依赖：

```bash
scripts/preflight-container.sh
```

验证 agentgateway MITM 能把 HTTPS 请求/响应 body 写入 JSON access log：

```bash
scripts/verify-agentgateway-capture.sh
```

这个脚本用当前默认 agentgateway 配置代理一个测试 HTTPS 请求，并检查 `request.body`、`response.body`。
它验证的是 `weagent` 默认使用的 JSON access log 路径；`/api/logs/search` 在 v1.4.0-alpha.1 下可能仍返回空数组。

验证真实 Linux WeChat 启动后是否能捕获登录二维码：

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

如果代理不可用，再把 `.env` 切到 `docs/docker-mirrors.md` 里的国内镜像 fallback。

## 目标接口

协议标准以 https://www.wechatbot.dev/zh/protocol 为准。

- `GET /healthz`
- `GET|POST /get_bot_qrcode?bot_type=3`
- `GET /get_qrcode_status?qrcode=...`
- `POST /getupdates`
- `POST /sendmessage`
- `POST /getconfig`
- `POST /sendtyping`
- `POST /msg/notifystart`
- `POST /msg/notifystop`
- `POST /getuploadurl`
- `POST /c2c/upload`
- `GET /c2c/download`

`/ilink/bot/*` 也暴露同义端点，只作为兼容旧版 SDK 或旧逆向资料的别名；新的对接方应使用协议文档里的根路径端点。

`/get_bot_qrcode` 返回 agentgateway 捕获到的最新微信登录二维码。协议主路径是 `GET`，weagent 也接受部分 SDK
会发出的 `POST` 和 `local_token_list`，但不把历史 token 复制成独立登录状态：

```json
{
  "local_token_list": ["<bot_token>"]
}
```

```json
{
  "qrcode": "access-log-...",
  "qrcode_img_content": "data:image/png;base64,..."
}
```

`/get_qrcode_status` 轮询本地 WeChat 登录状态。扫码后会主动尝试提取 DB key；能读取消息时返回
`confirmed`，并返回后续业务请求使用的 `bot_token` 和 `baseurl`。当前只从本机 WeChat 状态推导
`wait`、`scaned`、`confirmed`，不伪造 `binded_redirect`、`need_verifycode` 这类远端 iLink 状态：

```json
{
  "status": "confirmed",
  "bot_token": "webox",
  "ilink_bot_id": "default",
  "ilink_user_id": "default",
  "baseurl": "http://127.0.0.1:38080"
}
```

`/getupdates` 使用标准 `get_updates_buf` 不透明游标：

```json
{
  "get_updates_buf": "",
  "base_info": { "channel_version": "2.0.0" }
}
```

响应会按 iLink 语义长轮询最多 35 秒，包含 `ret`、`msgs` 和新的 `get_updates_buf`。每条消息包含 `context_token`，回复时必须原样放进
`/sendmessage` 的 `msg.context_token`。

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

`WEBOX_PUBLIC_BASE_URL` 可覆盖登录确认返回的 `baseurl`。这里应配置服务根地址，例如
`https://webox.example.com`。如果不设置，默认从请求 `Host` 派生 `http://host`。为兼容旧配置，末尾的
`/ilink/bot` 会被自动去掉。

媒体相关路径：

业务 POST 请求必须带标准 iLink 请求头：

```http
AuthorizationType: ilink_bot_token
Authorization: Bearer <bot_token>
X-WECHAT-UIN: <base64(String(random_uint32))>
```

- `WEBOX_MEDIA_STORE_DIR`：本地 CDN shim 保存待上传 metadata 和加密媒体，默认 `/webox/state/weagent/media`。
- `WEBOX_MEDIA_TRANSFER_DIR`：发送前解密出的临时文件目录，默认 `/webox/state/weagent/transfer`。

`/getconfig` 和 `/sendtyping` 用于兼容 iLink SDK 的输入状态流程。当前实现生成无状态
`typing_ticket`，`sendtyping` 校验 ticket 后返回 `ret=0`；它暂不驱动 Linux WeChat 显示真实输入状态。
`/msg/notifystart` 和 `/msg/notifystop` 接收标准 SDK 启停通知，当前返回 `ret=0`。

媒体发送走标准 iLink 上传链路：

1. 客户端调用 `/getuploadurl`，传 `filekey`、`media_type`、`rawsize`、`rawfilemd5`、`filesize`、`aeskey`。
2. `weagent` 返回 `upload_param` 和本地 `upload_full_url`。
3. 客户端把 AES-128-ECB + PKCS7 加密后的媒体字节上传到 `upload_full_url`。
4. `weagent` 在响应头返回 `x-encrypted-param`。
5. 客户端把 `media.encrypt_query_param` 和 `media.aes_key` 放进 `/sendmessage`。

`/c2c/download` 会按 `encrypt_query_param` 原样返回加密字节；`sendmessage` 会解密并校验 `rawfilemd5` 后通过
Linux WeChat 文件选择器发送图片、视频、语音或文件。Node.js SDK 可直接使用 `upload_full_url`；其他 SDK 如果只拼固定
CDN 地址，需要改成使用返回的上传地址或支持配置 CDN base URL。

## 核心边界

- `weagent` 对外只提供标准 iLink 协议和健康检查。
- 收消息来自 WeChat 本地 DB 解密读取。
- 发消息通过 UI 自动化操作 Linux WeChat 客户端。
- 登录二维码来自 agentgateway 的 MITM 捕获结果。
- 不保留 WOC `/agent/*` API。
- 不内置 msghub-style actor/message/task 数据库。
- 不把 WeChat DB 的内部 cursor、scanner meta 作为 iLink 响应字段暴露。

## 运行边界

容器启动后会：

1. 生成持久 machine-id，并默认伪装为 deepin 23。
2. 启动 Xvfb + openbox。
3. 启动 `agentgateway` v1.4.0-alpha.1，本地 admin API 默认监听 `127.0.0.1:15000`，并把它的 CA 写入系统和 NSS 信任库。
4. 启动镜像内置的 Linux 微信。
5. 默认用 `proxychains4` 包住 WeChat 网络进程，让登录流量经过 agentgateway。
6. 启动 Rust `weagent`。

entrypoint 使用 `tini` 加最小 shell supervisor。`Xvfb`、`openbox`、`agentgateway`、`weagent` 或 WeChat
循环任一关键进程退出时，容器退出；Compose 的 `restart: unless-stopped` 负责重启容器。

容器内工作目录统一在 `/webox` 下：

- `/webox/agentgateway`：agentgateway 配置、SQLite request log、CA。
- `/webox/wechat`：镜像内置 Linux WeChat 安装目录，不挂载覆盖。
- `/webox/weagent`：weagent 二进制和启动脚本。
- `/webox/state`：machine-id、NSS DB、WeChat HOME 和运行状态。
- `/webox/logs`：进程日志。

Compose 按子目录挂载 `./data/*`，不要把整个 `/webox` 作为一个 bind mount 覆盖掉。

`agentgateway` 配置默认从 `/webox/agentgateway/config.yaml` 读取；也可以用 `WEBOX_AGENTGATEWAY_CMD` 直接指定你验证过的启动命令。
默认启动会给 `agentgateway` 单独设置 `RUST_LOG=${WEBOX_AGENTGATEWAY_RUST_LOG:-info}`，避免全局 `RUST_LOG`
把 access log 过滤掉。

默认配置仍让 `agentgateway` 自己维护 `/webox/agentgateway/request-log.sqlite`，但 `weagent` 不直接读取
这个 SQLite。二维码捕获默认读取 `agentgateway` JSON access log，路径是
`/webox/logs/agentgateway.log`。`agentgateway` 的 `/api/logs/search` 和 `/api/logs/get` 目前只作为兼容
查询路径保留；实测 v1.4.0-alpha.1 普通 HTTPS MITM 请求不会进入该 API 的 log store。
JSON access log 中的 body 字段是 base64 形式的原始字节，`weagent` 会先解码再提取登录 URL 或图片。
二维码匹配只接受微信域名里的登录二维码 CGI/响应特征，或响应体里明确出现微信登录二维码 URL，避免把代理探针请求误报成二维码。

`agentgateway` 启动时工作目录是配置文件所在目录。默认挂载到 `/webox/agentgateway/config.yaml` 时，
YAML 里的 `sqlite://request-log.sqlite`、`certificates/webox-ca.pem` 都解析到 `/webox/agentgateway`
下面。

`WEBOX_WECHAT_PROXY_MODE` 支持：

- `proxychains`：默认值。包住主 `wechat` 和 `RadiumWMPF/runtime/WeChatAppEx`，因为 Linux WeChat 会在子进程里丢掉普通代理环境变量。
- `env`：只注入 `HTTP_PROXY`/`HTTPS_PROXY` 等环境变量，用于对比验证。
- `none`：不代理 WeChat 流量。
