# Webox

在 Docker 中运行 Linux 微信，并通过 HTTP 接口连接
[`aicat`](https://github.com/netcat-ai/aicat)。消息由真实微信客户端收发；Webox 不修改微信，也不代理微信流量。

## 工作原理

- 收消息：读取 Linux 微信本地数据库，把文本、链接、图片和文件转换为 Webox 消息。
- 发消息：根据 `roomid` 定位私聊或群聊，通过微信 UI 依次发送文本、图片和文件。
- 确认发送：再次读取本地数据库，确认消息已出现在正确会话中。
- 交换媒体：通过宿主机挂载的 `/webox/state/media` 在 Webox、aicat 和 Codex 之间共享文件。

## 风险提示

Webox 不是微信官方产品，并使用 UI 自动化操作微信。微信更新、风控或平台规则变化都可能导致功能失效、账号受限或封禁。

- 请使用专用账号，不要使用承担支付、工作或重要社交关系的主账号。
- 请遵守微信服务协议和当地法律法规，使用风险由使用者自行承担。
- 项目不保证持续可用，也不对账号封禁、数据丢失或其他损失负责。
- 状态目录包含微信登录信息、本地数据和 Webox token，请妥善保管，不要对外公开。

## 启动与登录

准备 Docker、aicat 和一个专门给 Webox 使用的微信账号。克隆仓库后启动容器：

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

返回值中的 `ready` 为 `true` 表示微信数据库和主界面已经完成初始化。给需要交给 aicat 处理的联系人或群聊设置以 `webox.` 开头的备注，例如 `webox.test`。

## 接入 aicat

在 `~/.aicat/aicat.yaml` 的 `claw.agents` 中配置 Webox。`tokenFile` 和 `sharedDir` 都必须是宿主机绝对路径：

```yaml
version: 1

claw:
  agents:
    - id: personal
      wechatID: jlyfish
      url: http://127.0.0.1:38080
      tokenFile: /absolute/path/to/webox/data/state/weagent/api-token
      sharedDir: /absolute/path/to/webox/data/state/media
      trigger:
        regex:
          enabled: true
          pattern: '^\\s*虾虾'
        chat:
          enabled: false
```

`claw.agents` 非空时，`aicat serve` 会自动启用 Claw。`wechatID` 是当前账号可见的微信号；aicat 会在 polling 前通过 Webox 核对账号身份。详细配置见 [aicat README](https://github.com/netcat-ai/aicat#readme)。

Webox 提供以下 HTTP 接口：

| 方法 | 路径 | 用途 |
| --- | --- | --- |
| `GET` | `/healthz` | 进程和微信初始化状态 |
| `GET` | `/ilink/bot/userinfo` | 当前微信账号身份 |
| `GET` | `/ilink/bot/contacts?remark=webox.test` | 按备注精确查询联系人或群聊的 `roomid` |
| `POST` | `/ilink/bot/getupdates` | 长轮询接收消息 |
| `POST` | `/ilink/bot/sendmessage` | 发送文本、图片和文件 |

除 `/healthz` 外，其余接口使用 `state/weagent/api-token` 中的 token 鉴权。
`contacts` 只做 `remark` 精确匹配，并返回未删除记录；`contacts` 数组保留重复备注，调用方可以据此拒绝不唯一的发送目标。

## 参考与致谢

- [aicat](https://github.com/netcat-ai/aicat)
- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)
- [wx-cli](https://github.com/jackwener/wx-cli)
- [SQLCipher](https://github.com/sqlcipher/sqlcipher)

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等开源项目。
