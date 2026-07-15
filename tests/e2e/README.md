# Webox live E2E

这套测试使用两个真实微信账号完成私聊闭环：`webox-peer` 通过真实微信 UI 发消息，`webox-sut` 从加密数据库读取消息并通过 iLink 回复，最终由 `webox-peer` 的 iLink 接口确认回复到达。

测试覆盖：

```text
peer UI -> 微信网络 -> SUT DB/WAL -> getupdates -> context_token
        -> sendmessage -> SUT UI -> 微信网络 -> peer DB/WAL -> getupdates
```

## 一次性准备

先构建本地镜像：

```bash
docker build -t webox:local .
```

启动两个使用独立持久 volume 的容器：

```bash
docker compose -f tests/e2e/docker-compose.yml up -d
```

打开两个真实微信桌面并分别登录专用测试账号：

- SUT：<http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale>
- Peer：<http://127.0.0.1:6081/vnc.html?autoconnect=1&resize=scale>

首次需要扫码，容器重建会复用 `webox-e2e-sut-state` 和 `webox-e2e-peer-state`，通常只在微信要求安全确认时再次人工操作。

两个账号需要互为联系人，并在两端设置唯一备注：

- 在 Peer 微信中给 SUT 设置备注，例如 `Webox被测账号`。该值传给 runner 的 `--peer-target`。
- 在 SUT 微信中给 Peer 设置任意非空且唯一的备注，供 SUT 回复时定位会话。

确认两端均已初始化：

```bash
curl -fsS http://127.0.0.1:38080/healthz
curl -fsS http://127.0.0.1:38081/healthz
```

两次响应都应包含 `"ready":true`。

## 运行私聊闭环

```bash
go run ./tests/e2e \
  --scenario direct \
  --peer-target Webox被测账号
```

runner 会自动从两个容器读取本地 iLink token；token 不会打印。一次成功运行会输出：

```json
{
  "RequestText": "WEBOX_E2E_...",
  "ReplyText": "ACK_WEBOX_E2E_...",
  "IncomingID": "...",
  "ReplyMessageID": "..."
}
```

两个 `getupdates` 基线并行建立，但 iLink 长轮询可能让这一阶段耗时约 35 秒。完整场景默认超时为 3 分钟。

如果容器名、端口或 Docker CLI 不同，可使用参数或对应的 `WEBOX_E2E_*` 环境变量覆盖：

```bash
go run ./tests/e2e --help
```

## 失败证据

测试失败会自动写入 `tests/e2e/artifacts/<UTC时间>/`：

- 两个容器最近 10 分钟的日志；
- 两端 `/healthz` 响应；
- 两个微信桌面的截图。

该目录已被 Git 忽略。日志和截图可能包含测试账号信息，不要上传到公开 issue。

## 日常使用

日常修改只需复用已经登录的两个 volume：

```bash
docker compose -f tests/e2e/docker-compose.yml up -d
go run ./tests/e2e --peer-target Webox被测账号
```

停止容器不会删除登录状态：

```bash
docker compose -f tests/e2e/docker-compose.yml down
```

只有需要重新验证完整扫码流程时才删除两个 E2E volume；该操作会清除登录状态：

```bash
docker compose -f tests/e2e/docker-compose.yml down -v
```

真实微信测试应串行执行，不要让多个 runner 同时操作同一个账号或桌面。
