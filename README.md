# webox

单容器运行 Linux 微信、虚拟 Display 和 Rust `weagent`，把真实客户端投影成标准 iLink 接口。

## 启动

```bash
cp .env.example .env
mkdir -p data/state data/logs
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

如果代理不可用，再把 `.env` 切到 `docs/docker-mirrors.md` 里的国内镜像 fallback。

## 目标接口

协议标准以 https://www.wechatbot.dev/zh/protocol 为准。

- `GET /healthz`
- `GET /get_bot_qrcode?bot_type=3`
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

`/get_bot_qrcode` 返回从当前 WeChat 登录窗口解码并裁剪出的二维码。weagent 只暴露协议文档定义的根路径端点，
不保留 `/ilink/bot/*` 或项目早期的自定义 API。

```json
{
  "qrcode": "xvfb-qr-...",
  "qrcode_img_content": "data:image/png;base64,..."
}
```

`/get_qrcode_status` 只轮询本地 WeChat 登录状态，不承担初始化工作。后台初始化器在检测到主窗口登录成功后自动
提取并验证 DB key；能读取消息时返回 `confirmed`，并返回后续业务请求使用的 `bot_token` 和 `baseurl`。当前只从本机 WeChat 状态推导
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

容器启动后所有 iLink 路由立即可用。登录初始化尚未完成时，`getupdates` 保持标准长轮询并返回空消息；
`sendmessage` 明确返回未就绪。状态变成 `confirmed` 后，收发接口自动可用，不存在额外 `/init` 调用。

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

文本发送只有在 WeChat 本地 DB 中读回同一会话的精确文本后才返回 `ret=0`，避免窗口被弹窗遮挡或搜索选错时误报成功。
发送前还会要求联系人搜索词在本地联系人库中精确唯一；同名联系人需要先设置唯一备注。

## 核心边界

- `weagent` 对外只提供标准 iLink 协议和健康检查。
- 收消息来自 WeChat 本地 DB 解密读取。
- 发消息通过 UI 自动化操作 Linux WeChat 客户端。
- 登录二维码图像从 Xvfb 中定位、解码并裁剪。
- 不保留 WOC `/agent/*` API。
- 不内置 msghub-style actor/message/task 数据库。
- 不把 WeChat DB 的内部 cursor、scanner meta 作为 iLink 响应字段暴露。

## 运行边界

容器启动后会：

1. 生成持久 machine-id，并默认伪装为 deepin 23。
2. 启动 Xvfb + openbox，并把 framebuffer 写到 `/webox/runtime/xvfb/Xvfb_screen0`。
3. 启动 Rust `weagent`。
4. 启动镜像内置的 Linux 微信并直接连接上游。
5. 后台观察登录窗口；持久账号页会自动点击“登录”，主窗口出现后自动提取并验证 DB key。

entrypoint 使用 `tini` 加最小 shell supervisor。`Xvfb`、`openbox`、`weagent` 或 WeChat
循环任一关键进程退出时，容器退出；Compose 的 `restart: unless-stopped` 负责重启容器。
`openbox` 只提供窗口激活、层级和焦点管理；没有窗口管理器时，`xdotool` 无法稳定操作 WeChat 主窗和文件选择器。

首次扫码后，WeChat 可能在容器重启时要求手机再次确认“登录”，这是官方客户端的会话恢复流程；持久化 HOME 和 DB key 不会丢失。

容器内工作目录统一在 `/webox` 下：

- `/webox/wechat`：镜像内置 Linux WeChat 安装目录，不挂载覆盖。
- `/webox/weagent`：weagent 二进制和启动脚本。
- `/webox/state`：machine-id、NSS DB、WeChat HOME 和运行状态。
- `/webox/logs`：进程日志。

Compose 按子目录挂载 `./data/*`，不要把整个 `/webox` 作为一个 bind mount 覆盖掉。

`weagent` 从 `WEBOX_QR_SCREENSHOT_PATH` 指向的 Xvfb framebuffer 读取登录窗口。它先检测 WeChat 蓝色二维码，
再用 QR 解码器确认、提取登录 URL并按二维码边界裁剪，只把裁剪后的 `data:image/png;base64,...` 返回给
`qrcode_img_content`。WeChat 直接联网，不安装额外 CA，也不修改客户端网络进程。

为从同一用户运行的 WeChat 进程内存提取本地 DB key，Compose 只增加 `SYS_PTRACE`，并只给 `weagent` 二进制
`cap_sys_ptrace=ep`；容器不使用 `privileged` 或 `seccomp=unconfined`。
