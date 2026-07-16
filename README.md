# Webox

在 Docker 中运行 Linux 微信，并把它接入 OpenClaw。  
消息由真实微信客户端收发。Webox 不修改微信，也不代理微信流量。

## 工作原理

- 收消息：读取并解析 Linux 微信的本地数据库，将新消息转换为 iLink 接口数据。
- 发消息：根据联系人或群聊备注定位会话，通过 GUI 自动化输入并发送文本。
- 确认发送：再次读取本地数据库，确认消息已出现在正确的会话中。

## 风险提示

Webox 不是微信官方产品，使用 UI 自动化操作微信。使用过程中可能因微信更新、风控或平台规则导致功能失效、账号受限或封禁。

- 请使用专用账号，不要使用承担支付、工作或重要社交关系的主账号。
- 请遵守微信服务协议和当地法律法规，使用风险由使用者自行承担。
- 项目不保证服务持续可用，也不对账号封禁、数据丢失或其他损失负责。
- 状态目录中包含微信登录信息和本地数据，请妥善保管，不要对外公开。

## 准备

- 安装 Docker。
- 安装并初始化 OpenClaw `2026.7.1` 或更高版本。
- 准备一个专门给 Webox 使用的微信账号。首次体验不建议使用主账号。
- 给需要自动回复的联系人或群聊设置唯一备注，备注必须以 `webox.` 开头，例如 `webox.测试群`。

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

微信桌面可以在浏览器中查看：

```text
http://127.0.0.1:6080/vnc.html?autoconnect=1&resize=scale
```

不要在这里提前扫码。下一步让 OpenClaw 发起登录，OpenClaw 才能保存登录信息。

## 2. 接入 OpenClaw

安装微信插件：

```bash
openclaw config set channels.wecom.enabled false
openclaw plugins install @netcat-ai/openclaw-weixin
openclaw config set plugins.entries.openclaw-weixin.enabled true
```

连接 Webox 并登录：

```bash
openclaw config set channels.openclaw-weixin.baseUrl http://127.0.0.1:38080
openclaw channels login --channel openclaw-weixin --account webox
```

用手机微信扫描终端中的二维码并确认。

重启 OpenClaw：

```bash
openclaw gateway restart
```

## 3. 测试

测试不是必需步骤。如果要验证收发，可以临时使用另一个微信账号：

- 私聊：给 Webox 账号发送一条消息。
- 群聊：先给群设置 `webox.` 开头的备注，再在群里 @机器人发送消息。

测试用的联系人或群聊需要在 Webox 登录的微信中设置 `webox.` 开头的备注。

收到自动回复后，可以用下面的命令确认 Webox 已成功操作微信发送消息：

```bash
docker logs --since 5m webox 2>&1 | grep 'WeChat text sent'
```

## 参考与致谢

- [OpenClaw 微信插件](https://github.com/Tencent/openclaw-weixin)
- [iLink 协议文档](https://www.wechatbot.dev/zh/protocol)
- [WechatOnCloud](https://github.com/Gloridust/WechatOnCloud)
- [wx-cli](https://github.com/jackwener/wx-cli)
- [wechat-decrypt](https://github.com/ylytdeng/wechat-decrypt)

感谢 Xvfb、Openbox、x11vnc、noVNC、xdotool 等开源项目。
