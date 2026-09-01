# Webox 使用统一游标协议并独立运行企业微信归档代理

状态：superseded by [0002](0002-wecom-capabilities-outside-webox.md)

个人微信与企业微信会话存档都通过同一个 `getupdates` contract 返回扁平的企业微信会话存档形状消息：请求只带
字符串 `cursor`，响应包含 `messages` 与已消费位置 `cursor`。消息必须包含 `msgid` 和 `roomid`，但不公开单条
消息 `seq`；消费端按 Room 保存和调度，只有全部受影响 Room 都持久化后才提交响应游标。

企业微信使用独立的 `wecomproxy` 和 slim `linux/arm64` 容器，直接以 Finance 全局 `seq` 作为游标。代理保留
解密后的原始 JSON，只规范化 `roomid`，并仅对明确包含媒体的类型下载 `sdkfileid`、替换为 Shared Directory
相对路径。未知类型不需要 Webox 或 aicat 预先支持。

个人微信用 `sort_seq` 合并所有候选 Room：空游标从最近五天开始，先从 `SessionTable.last_timestamp` 选择会话，
再查询 `cursor < sort_seq < upper*1000`。只处理已经结束的秒，每次最多推进一小时；候选消息只按唯一的
`sort_seq` 合并排序，不为 Room 之间定义额外次序。媒体尚未 ready 时可返回可用前缀但保持原游标，当前允许
重放消息重复追加。

## Consequences

- Webox 不保存消费 checkpoint；Finance 也不物化消息数据库。
- 任一 Room 查询失败时不得推进全局游标，否则会造成其他 Room 成功掩盖数据缺失。
- `wecomproxy` 使用官方 `20250205` Linux ARM SDK；它在 Apple Silicon Mac 上通过 Linux ARM 容器运行，
  不是 macOS 原生动态库。
- Finance SDK 不提供发送能力，`sendmessage` 明确返回不支持。
