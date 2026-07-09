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
- `POST /ilink/sendmessage`
- `POST /ilink/getupdates`
- `POST /ilink/ack`
- `GET /ilink/login/qrcode/latest`
- `GET /ilink/login/qrcode/events`

`/ilink/getupdates` 使用 `after_id` + `limit`，响应只返回 `updates`。每个收到的微信消息会被投影为
`message.received` update，payload 中包含 `room`、`message` 和可直接用于回复的 `context_token`。
`/ilink/sendmessage` 支持 `context_token`，也支持显式传 `room.outbound_target` 或 `room.external_room_id`。
登录二维码通过 iLink login 相关接口暴露。

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
5. 通过代理环境变量启动微信。
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

默认配置使用官方 `agentgateway` v1.3.1 的 SQLite request log：
`/webox/agentgateway/request-log.sqlite`。`weagent` 不直接读取 SQLite，而是调用 agentgateway 本地
`/api/logs/search` 和 `/api/logs/get`，从 log attributes 中读取 `request.body` / `response.body`。

`agentgateway` 启动时工作目录是配置文件所在目录。默认挂载到 `/webox/agentgateway/config.yaml` 时，
YAML 里的 `sqlite://request-log.sqlite`、`certificates/webox-ca.pem` 都解析到 `/webox/agentgateway`
下面。
