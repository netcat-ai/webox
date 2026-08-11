# Webox

**Room Incremental Update（Room 增量更新）**：
Webox 根据消费者提交的每个 Room `seq`，从微信事实源返回该 Room 的有序新增消息。进度由消费者持久化，Webox
不维护全局 cursor 或 Agent Context Log。
_Avoid_：全局消息队列、opaque polling cursor、Webox 消费 checkpoint

**Room Source Location（Room 数据源位置）**：
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
