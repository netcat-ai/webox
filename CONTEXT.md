# Webox

**Webox Cursor（Webox 游标）**：
消费者提交并在整个 `getupdates` 响应成功落盘后推进的单一字符串水位。WeChat 来源使用跨 Room 合并后的
`sort_seq`，空游标默认从最近五天开始且每次最多推进一小时。Webox 不持久化消费进度。
_Avoid_：Room Sequence、`next_seq`、Webox 消费 checkpoint

**Unified Update（统一更新）**：
WeChat DB 生成的扁平消息响应；消息沿用企业微信会话存档 JSON 形状并包含 `roomid`，消费者按 Room 分开保存和
调度。响应 `cursor` 只表示整个响应已消费到的位置，不属于单条消息。
_Avoid_：按 Room 分组的响应、消息 `seq`、跨 Room Codex batch

**WeChat Room Source Location（WeChat Room 数据源位置）**：
一个 Room 在微信本地消息存储中的实际物理位置；它是微信数据库布局的一部分，不由 Webox 自行规定。
_Avoid_：推测的分库位置、Room 消费进度

**Reply Message（回复消息）**：
一条带有当前正文并指向同一 Room 内较早消息的入站消息；使用 `reply.title` 表达当前正文，使用
Parent Message Reference 表达直接回复关系，不内嵌父消息内容。
_Avoid_：Mixed Message、拼接引用正文的 Text Message、递归消息树

**Parent Message Reference（父消息引用）**：
Reply Message 对其直接父消息的轻量引用，包含父消息的稳定消息 ID，以及发送者、消息类型和消息时间摘要。
完整父消息由消费端在同一 Room 的既有有序消息流中按消息 ID 查找；摘要与完整消息冲突时以完整消息为准。
_Avoid_：完整父消息副本、跨 Room 引用、时间上的上一条消息
