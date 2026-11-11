# Harness 能力与验收记录

验证日期：2026-11-11（America/Los_Angeles）。本轮目标是补齐当前五项简历能力的集成与验证，并增强规则/分类路由、模型有限重试和执行追踪。保留原有会话、工具权限及数据存储职责。

## 如何复现

```bash
# 无密钥、无外部模型或向量服务的回归
./scripts/verify-harness.sh -mode offline

# 真实服务；读取已有配置，在独立目录、数据库和集合中验收
# classifier model 必须支持严格 JSON Schema；名称由自己的网关提供
ROUTER_MODEL=YOUR_SCHEMA_COMPATIBLE_MODEL \
  ./scripts/verify-harness.sh -mode all -config configs/interview \
  -milvus-url http://127.0.0.1:19530 \
  -classifier-timeout 15s \
  -report .runtime/harness-validation/acceptance.json

# 只重测某个场景，避免重复调用其他真实服务
ROUTER_MODEL=YOUR_SCHEMA_COMPATIBLE_MODEL \
  ./scripts/verify-harness.sh -mode live -only live_hybrid_classification

# 核验已有语料快照；完整抓取流程见 README 的面试知识库部分
./scripts/verify-harness.sh -mode offline -only corpus_snapshot \
  -corpus-dir YOUR_CORPUS_DIRECTORY
```

执行环境：Go 1.26.9，真实聊天服务使用当前网关的 `deepseek-v4.1-flash`，分类成功路径使用该网关的 `gpt-6-luna`，Embedding 使用 `text-embedding-v4`、1024 维，Docker 镜像为 `lanagent/practice-go:1.26.9-v1`。本机验收通过参数使用已运行的 Milvus 19531 端口，不修改原有 19530 配置，也不重启其他容器。

分类真实验收预算为 15 秒，应用默认仍为规则模式；混合分类的默认预算为 3 秒。模型名称用于标识本次实际配置，不代表成本或质量比较。本轮没有证明增加分类模型会降低总费用或延迟。

## 五项能力对应

| 简历能力 | 实现位置 | 自动化验证 | 真实验证 |
| --- | --- | --- | --- |
| 有界 ReAct、工具治理与分流 | `internal/harness`、`internal/extension` | 路由、协议、预算、白名单及参数校验测试；非法分类回退；直答拒绝工具 | 两轮明确直答；独立分类模型输出直答决策；真实文件读取 |
| 上下文预算与摘要降级 | `internal/contextbudget`、`internal/harness/context.go` | 完整工具组裁剪、摘要失败回退、关键验证字段保留 | 注入合成历史触发一次真实摘要并完成回答 |
| 记忆一致性与异步同步 | `internal/personalmemory` | 事务回滚、操作回执、独立连接竞争、租约、旧版本及删除过滤 | SQLite + 真实 Embedding/Milvus；纠正和删除后重新引入旧向量，召回仍过滤 |
| 检查点与副作用核对 | `internal/personal`、`internal/extension/reconcile.go` | 已完成工具不重放、未知结果停止、收据参数哈希校验 | 真实模型读取文件后，经本地代理注入持续 503，再恢复原任务 |
| 模型并发、有限重试与追踪 | `internal/llm`、`internal/observability` | 两种传输的重试边界、取消、槽位释放、记录失败终止、追踪隔离及用量恢复 | 真实模型调用、重试元数据、任务/模型/工具 span、跨恢复用量汇总 |

未知副作用收据核对由 SDK 内存连接和确定性测试验证；本轮没有向真实业务知识库制造不确定写操作。真实记忆/RAG 写入仅发生在新建的验收集合中，并在结束后删除这些集合。

## 真实服务结果

最终完整验收报告位于 `.runtime/harness-validation/acceptance.json`，对应 Markdown 报告位于同目录。此目录忽略版本控制；下表仅保留可公开的验证结论。

| 场景 | 结果 | 可核对的事实 |
| --- | --- | --- |
| 多轮明确直答 | 通过 | 两轮均为 direct；无工具调用，历史保留 |
| 混合分类 | 通过 | 独立分类模型产生 `source=classifier, route=direct`，随后由聊天模型回答 |
| 文件读取后恢复 | 通过 | 本地代理连续注入 3 次 HTTP 503；2 次退避后停止；恢复前后 `read_file` 都只有 1 次 |
| 历史摘要 | 通过 | 合成历史触发 1 次压缩和 1 次成功的真实摘要调用 |
| 记忆与 RAG | 通过 | 纠正/删除过滤有效；真实小样本文档入库并召回；两个隔离集合已清理 |
| 模型选择 Docker 工具 | 通过 | 向模型提供固定正确代码，实际调用 Docker 测试，返回 passed 和对应代码哈希 |

Docker 结果只证明提供给工具的代码通过固定题目测试，不用于宣称模型自主解题能力。真实 RAG 验收使用独立的小样本文档，不等同于重新导入全部 1,206 个片段。

另保留一次真实兼容性结果：聊天模型直接用于严格 Schema 分类时返回 HTTP 400，宿主记录 `source=fallback, route=agent` 并完成回答。这次分类成功路径未通过，随后通过配置独立兼容模型验证成功；原始记录保存在 `.runtime/harness-validation/initial-classifier-fallback.json`。没有将接口不兼容当作成功分类，也没有以普通文本解析替代严格验证。

这些是功能验收及故障注入结果，不是生产容量、P95 延迟、检索准确率或节费比例。JSONL 中统计供应商已返回的用量；失败请求未返回的用量保持未知。

## 语料规模依据

使用现有本地快照，逐文档验证 manifest 中的 SHA-256，再以项目的 512 字符、64 字符重叠策略重新切分。结果为 **17 份文档、1,206 个片段**。这是已校验快照的切分数量，不是本轮对生产 Milvus 集合的实时计数。

Manifest SHA-256：`38c8c6889448c3ef1fc130e3931cb1abec590161c885cfbb690fa821779a6dcf`。

逐源校验和与数量输出在 `.runtime/harness-validation/corpus-evidence.json`。公开仓库提供采集器及来源清单，不提交本地完整语料和个人运行数据；重新抓取更新后的官方文档时，数量可能变化，应使用新报告的实际结果。

| 本地文件 | 官方来源 | 片段数 |
| --- | --- | ---: |
| `go-faq.md` | [来源](https://go.dev/doc/faq) | 251 |
| `effective-go.md` | [来源](https://go.dev/doc/effective_go) | 272 |
| `memory-model.md` | [来源](https://go.dev/ref/mem) | 61 |
| `context.md` | [来源](https://go.dev/blog/context) | 40 |
| `pipelines.md` | [来源](https://go.dev/blog/pipelines) | 58 |
| `race-detector.md` | [来源](https://go.dev/doc/articles/race_detector) | 26 |
| `gc-guide.md` | [来源](https://go.dev/doc/gc-guide) | 172 |
| `slices.md` | [来源](https://go.dev/blog/slices-intro) | 27 |
| `maps.md` | [来源](https://go.dev/blog/maps) | 22 |
| `sql-connections.md` | [来源](https://go.dev/doc/database/manage-connections) | 13 |
| `sql-transactions.md` | [来源](https://go.dev/doc/database/execute-transactions) | 16 |
| `sql-cancel.md` | [来源](https://go.dev/doc/database/cancel-operations) | 8 |
| `diagnostics.md` | [来源](https://go.dev/doc/diagnostics) | 42 |
| `redis-types.md` | [来源](https://redis.io/docs/latest/develop/data-types/) | 18 |
| `redis-persistence.md` | [来源](https://redis.io/docs/latest/operate/oss_and_stack/management/persistence/) | 68 |
| `pg-isolation.md` | [来源](https://www.postgresql.org/docs/current/transaction-iso.html) | 63 |
| `pg-locks.md` | [来源](https://www.postgresql.org/docs/current/explicit-locking.html) | 49 |

## 检查命令

```bash
go test ./...
go test -race ./...
go vet ./...
```

用例覆盖不同并发调用的状态隔离、日志失败不进入分类/摘要回退、旧检查点与部分工具批次恢复。完整终端演示另外将重试次数配置为 1，以稳定展示持续失败后的人工恢复步骤。
