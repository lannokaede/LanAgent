# 服务端工程约束与验收

验证日期：2026-11-15（America/Los_Angeles）。Go 1.26.9。本次补强模型等待容量、MCP HTTP 服务准入与退出，以及本地和 CI 共享的交付约束。

[执行架构](architecture.md) · [开发检查](development.md) · [研究 MCP 参数](research-mcp.md)

## 容量与失败边界

| 位置 | 约束 | 容量耗尽或失败时 |
| --- | --- | --- |
| 模型网关 | 在途由 `llm.concurrency` 配置，等待默认 32，`queue_limit: 0` 不排队 | 返回可用 `errors.Is` 识别的 `llm.ErrOverloaded`；不增加供应商尝试，不自动重试本地过载 |
| 模型重试 | 重试重新准入，全部尝试、退避和再次排队共享期限 | 退避释放槽位；排队满直接返回；取消释放等待位置 |
| HTTP 短请求 | 默认同时 8 个；正文最多 128 KiB，鉴权与 Origin 检查先执行 | 快速返回 HTTP 503 和 `Retry-After: 1` |
| GET SSE | 独立于短请求的同等容量 | 长连接不占用 POST 槽；GET 容量满时拒绝新增连接 |
| 实际工具回调 | 独立于 HTTP 连接的同等容量 | HTTP 断开后仍持有回调槽，直到回调退出；回调满时返回 MCP 工具错误 |
| 请求关联 | 服务端覆盖生成标记，关联表最多 `2 × 并发容量` | 解析失败、通知、断开与关闭均移除关联 |
| HTTP 退出 | 停止准入，先排空；超时取消和断连，再关闭 SDK 会话、等待工具退出 | `Serve` / `Close` 返回后，命令才关闭业务存储；超时可用 `errors.Is` 识别 |
| 交付 | 格式、依赖漂移、测试、race、vet、全部命令编译共用一个脚本 | 任一步失败阻止检查通过；检查不自动改写源码或依赖清单 |

这些限额按进程生效。持久入库由单 worker 执行，前台返回任务 ID；记忆 Outbox 仍负责事实与向量索引之间的最终一致性。进程重启恢复、执行收据核对和失败降级各有验证，容量与副本数、跨节点故障切换分别讨论。

## 行为验证

| 场景 | 已验证事实 | 测试 |
| --- | --- | --- |
| 模型受控突发 | 保持 2 个在途和 4 个等待，再追加 64 个并发请求，均快速拒绝；供应商调用仍为 2，未生成新尝试事件 | `TestGatewayBurstBoundsActiveAndWaitingWork` |
| 排队取消及零队列 | 取消归还等待位，后续请求可重新进入；0 表示槽满立即拒绝 | `TestGatewayQueuedCancellationReleasesWaitingPosition`、`TestGatewayZeroQueueAndConfigurationValidation` |
| 重试与背压 | 重试遇到满队列停止，重新等待继承原模型期限 | `TestGatewayRetryReacquisitionHonorsFullQueue`、`TestGatewayRetryQueueKeepsOriginalModelDeadline` |
| HTTP 突发 | 保持 4 个真实 SDK 工具回调，再发 28 个并发请求，全部在释放工作前返回 503；峰值工具回调为 4，完成后容量归还 | `internal/mcphttp/server_test.go` |
| HTTP 断开 | 请求结束只释放 HTTP 容量；未结束的回调继续占实际工具容量 | `TestActualToolSlotSurvivesHTTPDisconnect` |
| 期限和会话 | 请求期限传入真实 SDK 回调；空闲会话到期回收，关联信息清理 | `TestRequestDeadlineCancelsActualToolAndCleansAssociations`、`TestIdleSessionExpires` |
| 退出与嵌入 | 正常排空、超时取消、SDK 会话结束、嵌入 http.Handler 的 GET SSE 退出均可等待完成 | `TestServeDrainsBeforeReturningAndClosesSessions`、`TestServeDrainTimeoutCancelsAndJoinsCallbacks`、`TestEmbeddedCloseCancelsIdleSSEBeforeJoiningHandlers` |

工具必须响应 context；Go 无法强行停止忽略取消的任意函数。关闭流程会等待工具收尾，保证仍被使用的依赖不会提前关闭。HTTP 传输的依赖约束由架构测试检查，避免向 CLI 或业务存储反向耦合。

测试源码：[模型准入](../internal/llm/admission_test.go)、[HTTP 与 SDK](../internal/mcphttp/server_test.go)、[架构约束](../internal/architecture/dependencies_test.go)。

## 实际执行结果

| 验证 | 结果 | 私有记录 |
| --- | --- | --- |
| 统一交付检查 | 格式、依赖漂移、全仓普通测试/race/vet、全部命令编译通过 | `service-delivery-checks-20261115.log` |
| 离线验收 | 15 个包，208 个测试通过（含子测试），1 项 Docker 开关测试跳过；17 文档、1,206 片段的校验快照保持一致 | `service-engineering-offline-20261115.json` |
| 真实 loopback 进程 | 同一隔离目录两次启动，共 12 次请求；401/403、10 项只读工具、空来源查询正确；两次 SIGTERM 均退出 0，目录可重新打开 | `service-http-smoke-20261115.json` |
| 真实聊天服务 | `deepseek-v4.1-flash` 两轮直答，2 次模型尝试，0 重试；耗时 7.852 秒，返回的用量已记录 | `service-model-smoke-20261115.json` |

记录在 `.runtime/harness-validation/`，不提交个人运行数据。并发验证使用受控阻塞和本机真实 HTTP / SDK，证明资源约束及取消语义；没有据此推算真实模型吞吐量、P95、节费比例或容灾 RTO。Embedding、Milvus 与 Docker 的既有真实验收保留在 [2026-11-11 验收记录](harness-validation.md)。

## 复现

```bash
./scripts/check.sh

# 只重验准入、请求取消与退出边界
go test -race ./internal/llm ./internal/mcphttp ./cmd/research-mcp -count=3

# 离线集成和已有语料快照
./scripts/verify-harness.sh -mode offline \
  -corpus-dir YOUR_CORPUS_DIRECTORY \
  -report .runtime/harness-validation/service-engineering-offline.json

# 使用已有配置，只验证真实聊天链路
./scripts/verify-harness.sh -mode live -only live_multi_turn_direct \
  -config configs/interview \
  -report .runtime/harness-validation/service-model-smoke.json
```

独立 HTTP 进程通过 `go build ./cmd/research-mcp` 构建后，以临时数据目录和临时访问 Token 启动；标准 MCP initialize → notifications/initialized → tools/list → tools/call 验证接入，发送 SIGTERM 后确认退出，再用同一数据目录重新启动。入口参数与客户端配置见[研究 MCP 使用说明](research-mcp.md)。
