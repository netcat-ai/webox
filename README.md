# Webox

在 Docker 中运行 Linux 微信，并把真实客户端投影成供本地自动化程序使用的按 Room 增量消息与发送接口。
消息由真实微信客户端收发；Webox 不修改微信，也不代理微信流量。

Webox 是底层微信适配器，不是完整的机器人产品。它不包含 agent runtime、触发规则、prompt、模型调用或自动回复逻辑；本地消费端按 Room 保存消费进度、决定是否处理并调用 HTTP 接口回复。

## 工作原理

- 收消息：读取 Linux 微信本地数据库，按消费者提交的 Room `seq` 返回文本、回复、链接、图片和文件增量。
- 发消息：根据 `roomid` 定位私聊或群聊，通过微信 UI 依次发送文本、图片和文件。
- 确认发送：再次读取本地数据库，确认消息已出现在正确会话中。
- 交换媒体：通过 Room 目录中的 `inbox/` 和 `outbox/` 在 Webox 和本地消费端之间共享文件。

## 风险提示

Webox 不是微信官方产品，并使用 UI 自动化操作微信。微信更新、风控或平台规则变化都可能导致功能失效、账号受限或封禁。

- 请使用专用账号，不要使用承担支付、工作或重要社交关系的主账号。
- 请遵守微信服务协议和当地法律法规，使用风险由使用者自行承担。
- 项目不保证持续可用，也不对账号封禁、数据丢失或其他损失负责。
- 状态目录包含微信登录信息、本地数据和 Webox token，请妥善保管，不要对外公开。

## 启动与登录

准备 Docker 和一个专门给 Webox 使用的微信账号。克隆仓库后启动容器：

```bash
cp .env.example .env
docker compose up -d
```

在浏览器中打开微信桌面：

```text
http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale
```

首次启动或登录失效时，通过这个 noVNC 页面手动扫码或确认已保存账号。Webox 不再通过 HTTP 暴露二维码，也不会自动点击登录按钮。登录状态持久化在 `.env` 中 `WEBOX_STATE_DIR` 指向的目录；登录完成后可以检查就绪状态：

```bash
curl http://127.0.0.1:38080/healthz
```

返回值中的 `ready` 为 `true` 表示微信数据库和主界面已经完成初始化。给需要接收入站消息的联系人或群聊设置以 `webox.` 开头的备注，例如 `webox.test`。

## 接入本地消费端

Webox token 位于宿主机状态目录的 `weagent/api-token`，默认对应 `./data/state/weagent/api-token`。除 `/healthz` 外，请求都必须携带以下 headers：

```http
AuthorizationType: ilink_bot_token
Authorization: Bearer <token>
X-WECHAT-UIN: <non-empty-client-id>
```

Webox 提供以下 HTTP 接口：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/healthz` | 进程和微信初始化状态 |
| `GET` | `/ilink/bot/userinfo` | 当前微信账号身份 |
| `GET` | `/ilink/bot/contacts?remark=webox.test` | 按备注精确查询联系人或群聊的 `roomid` |
| `POST` | `/ilink/bot/getupdates` | 按 Room `seq` 长轮询接收消息 |
| `POST` | `/ilink/bot/sendmessage` | 发送文本、图片和文件 |

`contacts` 只做 `remark` 精确匹配，并返回未删除记录；`contacts` 数组保留重复备注，调用方可以据此拒绝不唯一的发送目标。

接收消息时提交已经持久化的 Room 进度：

```json
{
  "rooms": {
    "room1": {"seq": 5},
    "room2": {"seq": 10}
  }
}
```

响应只包含有 ready 消息的 Room，并会自动加入新发现的 `webox.` 会话。每条消息的 `seq` 是该 Room
消息表的 `local_id`。私聊和群聊都使用非空 `roomid`，群聊 ID 以 `@chatroom` 结尾，`tolist` 当前固定为空。
消费者应在消息持久化成功后才推进对应 Room 的 `seq`；完整规则见
[架构说明的“收消息”章节](docs/architecture.md#收消息)。

发送文本时，将接收消息或 `contacts` 查询得到的 `roomid` 原样传回：

```json
{
  "msgs": [
    {
      "msgid": "client-message-1",
      "roomid": "target-roomid",
      "tolist": [],
      "msgtype": "text",
      "text": {"content": "你好"}
    }
  ]
}
```

图片和文件通过 `<state>/shared/rooms/<roomid>/` 下的 `inbox/`、`outbox/` 交换，消息中的 `sdkfileid`
使用相对于 Room 目录的路径。完整字段语义和可靠性限制见 [架构说明](docs/architecture.md)。

## 参考与致谢

- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)
- [wx-cli](https://github.com/jackwener/wx-cli)
- [SQLCipher](https://github.com/sqlcipher/sqlcipher)

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等开源项目。
