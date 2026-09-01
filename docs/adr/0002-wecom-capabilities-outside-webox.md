# 企业微信运行能力不属于 Webox

状态：accepted

Webox 只负责 Linux 个人微信的数据库读取、媒体交换和 UI 发送，不再提供企业微信会话存档、客户联系查询、
Host Action 发送或独立 `wecomproxy` runtime。企业微信会话存档的凭据、Finance SDK、解密和媒体下载由
`netcat-api` 管理；aicat 等消费端直接调用该服务，不再通过 Webox 间接访问企业微信。

Webox 的 HTTP 路径、请求与响应字段以及公开 `wecom.Message` contract 保持不变。个人微信消息继续沿用企业微信
会话存档 JSON 的字段形状；这只是协议结构，不表示 Webox 提供企业微信运行能力。

## Consequences

- 删除 Finance SDK、`wecomproxy` binary、企业微信容器镜像和相关环境配置。
- 删除企业微信联系人解析、Host Action 和 AppleScript。
- Webox 不持有企业微信凭据，也不调用企业微信 API。
- Webox 的个人微信消费者不需要迁移接口协议。
