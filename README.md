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
- `POST /ilink/sendmessage`
- `POST /ilink/getupdates`
- `POST /ilink/ack`
- `GET /ilink/login/qrcode/latest`
- `GET /ilink/login/qrcode/events`

`/ilink/getupdates` 使用 `after_id` + `limit`，响应只返回 `updates`。每个收到的微信消息会被投影为
`message.received` update，payload 中包含 `room`、`message` 和可直接用于回复的 `context_token`。
`/ilink/sendmessage` 支持 `context_token`，也支持显式传 `room.outbound_target` 或 `room.external_room_id`。
发送请求通过 UI 自动化同步执行，响应返回 iLink `task` 视图；成功时 `task_type=send_message`、
`status=acked`。登录二维码通过 iLink login 相关接口暴露。

`/ilink/login/qrcode/latest` 返回最新捕获的登录二维码投影：

```json
{
  "found": true,
  "qrcode": {
    "type": "wechat.login_qrcode",
    "status": "captured",
    "login_url": "https://login.weixin.qq.com/...",
    "image_data_uri": "data:image/png;base64,..."
  },
  "event": {}
}
```

其中 `qrcode` 是给第三方 agent 消费的稳定字段，`event` 是 agentgateway 原始捕获事件，仅用于诊断。

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
