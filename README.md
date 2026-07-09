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

缺少 WeChat deb 时，可以先验证 Rust、运行时依赖和 agentgateway 安装：

```bash
docker build --target runtime-base -t webox:runtime-base-check .
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

- `GET /healthz`
- `GET|POST /ilink/bot/get_bot_qrcode?bot_type=3`
- `GET /ilink/bot/get_qrcode_status?qrcode=...`
- `POST /ilink/bot/getupdates`
- `POST /ilink/bot/sendmessage`

根路径也暴露同义标准端点：`/get_bot_qrcode`、`/get_qrcode_status`、`/getupdates`、`/sendmessage`。
这是为了兼容把 `baseurl` 设为 `http://host/ilink/bot` 后再拼相对路径的 iLink 客户端。

`/ilink/bot/get_bot_qrcode` 返回 agentgateway 捕获到的最新微信登录二维码：

```json
{
  "qrcode": "access-log-...",
  "qrcode_img_content": "data:image/png;base64,..."
}
```

`/ilink/bot/get_qrcode_status` 轮询本地 WeChat 登录状态。扫码后会主动尝试提取 DB key；能读取消息时返回
`confirmed`，并返回后续业务请求使用的 `bot_token` 和 `baseurl`：

```json
{
  "status": "confirmed",
  "bot_token": "webox",
  "ilink_bot_id": "default",
  "ilink_user_id": "default",
  "baseurl": "http://127.0.0.1:38080/ilink/bot"
}
```

`/ilink/bot/getupdates` 使用标准 `get_updates_buf` 不透明游标：

```json
{
  "get_updates_buf": "",
  "base_info": { "channel_version": "2.0.0" }
}
```

响应包含 `ret`、`msgs` 和新的 `get_updates_buf`。每条消息包含 `context_token`，回复时必须原样放进
`/ilink/bot/sendmessage` 的 `msg.context_token`。

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

`WEBOX_PUBLIC_BASE_URL` 可覆盖登录确认返回的 `baseurl`。如果不设置，默认从请求 `Host` 派生
`http://host/ilink/bot`。

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
3. 启动 `agentgateway` v1.3.1，本地 admin API 默认监听 `127.0.0.1:15000`，并把它的 CA 写入系统和 NSS 信任库。
4. 启动镜像内置的 Linux 微信。
5. 只给微信进程注入代理环境变量，让登录流量经过 agentgateway。
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

默认配置仍让 `agentgateway` 自己维护 `/webox/agentgateway/request-log.sqlite`，但 `weagent` 不直接读取
这个 SQLite。二维码捕获默认读取 `agentgateway` JSON access log，路径是
`/webox/logs/agentgateway.log`。`agentgateway` 的 `/api/logs/search` 和 `/api/logs/get` 目前只作为兼容
查询路径保留；实测 v1.3.1 普通 HTTPS MITM 请求不会进入该 API 的 log store。
JSON access log 中的 body 字段是 base64 形式的原始字节，`weagent` 会先解码再提取登录 URL 或图片。

`agentgateway` 启动时工作目录是配置文件所在目录。默认挂载到 `/webox/agentgateway/config.yaml` 时，
YAML 里的 `sqlite://request-log.sqlite`、`certificates/webox-ca.pem` 都解析到 `/webox/agentgateway`
下面。
