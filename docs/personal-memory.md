# 个人 SQLite 记忆

个人 Harness 默认启用本地 SQLite 记忆，保存用户明确指定的偏好、目标与项目事实，供新会话复用。通过命令显式写入，不从普通提问推断事实，不调用模型自动抽取。

使用纯 Go 驱动 `modernc.org/sqlite`。开发时 Go 下载模块，编译后的用户程序无需另装数据库服务、SQLite CLI 或 C 编译器。默认数据库是 `state-dir/personal-memory.db`，state-dir 默认为用户缓存目录中的 `lanagent/harness`。WAL 运行时还有辅助文件，备份应使用 SQLite 一致性备份或所有进程关闭后复制，不能运行中只复制主文件。

## 本地使用

从仓库根目录运行，下列管理命令不需要模型配置和密钥，也不发模型请求：

```bash
go run ./cmd/harness -workspace .. -prompt '/remember user career.target Go 后端 Agent 实习'
go run ./cmd/harness -workspace .. -prompt '/remember user answer.style 先解释原理，再给 Go 示例'
go run ./cmd/harness -workspace .. -prompt '/remember project architecture Go Harness + Milvus'
go run ./cmd/harness -workspace .. -prompt '/memory user'
go run ./cmd/harness -workspace .. -prompt '/memory project'
# 同一个 key 再写就是纠正
go run ./cmd/harness -workspace .. -prompt '/remember user career.target Agent 开发实习'
go run ./cmd/harness -workspace .. -prompt '/forget user career.target'
```

交互模式支持相同命令。`user` 在同一个数据库的工作目录/会话间共享，`project` 绑定规范化绝对工作目录。key 允许字母、数字、点、下划线和短横线，最长 128 字符；正文最多 4096 UTF-8 字节。

正常聊天仍需模型配置。新 session 自动读取已保存事实，无需指定旧 session。`-memory-db` 选择另一档案数据库，`-memory=false` 完全关闭个人记忆。个人记忆由 `-memory` 控制，不复用旧 HTTP/Mem0 的 `memory.enabled` 配置。

无向量服务时，每个作用域选最近六条，按约 6000 字节预算组装；较早记录可用 `/memory` 查看（每次最多 200 条）或开启语义召回。普通聊天不会自动写入记忆。

## 可选 Milvus 索引

配置需要有效的 `embedding` 和 `rag.milvus_url` / 可选 token。个人记忆只复用连接信息，使用独立集合 `lanagent_personal_memory_v1`，不需要开启知识库 RAG。

```yaml
embedding:
  base_url: https://dashscope.aliyuncs.com/compatible-mode/v1
  model: text-embedding-v4
  dimensions: 1024
rag:
  enabled: false
  milvus_url: http://127.0.0.1:19530
```

凭据使用 `EMBEDDING_API_KEY` / `MILVUS_TOKEN` 环境变量或 Git 忽略的本地配置。启用向量功能后，记忆正文会发给配置的 Embedding 服务，也会写入 Milvus；本地 SQLite 不等于所有数据均留在本机。

```bash
# 第一次明确建集合，并处理已有任务
go run ./cmd/harness -config configs/interview -workspace .. -memory-vectors -memory-init -prompt '/memory sync'
# 后续检查已有集合、后台同步、语义召回
go run ./cmd/harness -config configs/interview -workspace .. -memory-vectors
# 查看状态、重新排队失败任务、显式同步最多100个已就绪任务
go run ./cmd/harness -workspace .. -prompt '/memory status'
go run ./cmd/harness -workspace .. -prompt '/memory retry'
go run ./cmd/harness -config configs/interview -workspace .. -memory-vectors -prompt '/memory sync'
```

`configs/interview` 还开启知识库检查；只需要个人记忆时，可用独立配置令 `rag.enabled: false`。向量模式当前需要完整 CLI 配置及合法聊天模型设置，但 `/memory sync` 不调用聊天模型。

未启用向量时写入仍生成持久 Outbox，以后开启同步可补齐。短命令退出不保证同步完成，以 sync 和状态为准；后台 worker 随 CLI 生命周期运行，不是常驻服务。运行中的查询故障回退本地记录，显式启用向量模式时启动会检查集合和连接，启动失败需修复或关闭该开关。

## 实现与一致性

代码在 `internal/personalmemory/{store,service,milvus}.go`，CLI 组装在 `internal/cli/run.go`，临时请求注入在 `internal/harness/model.go`。

| 表 | 内容 |
| --- | --- |
| `pm_meta` | profile 身份与已绑定索引身份 |
| `pm_facts` | 当前正文、scope/key、版本、删除标记、来源、vector ID、时间 |
| `pm_operations` | 操作 ID、输入摘要、原始结果回执 |
| `pm_outbox` | 不可变版本任务、状态、尝试次数、租约和 owner |

每次写入同事务更新事实、创建 Outbox 并保存回执。Store 的 expected_version=0 表示创建，纠正/删除需匹配当前版本。CLI 先读版本，再条件修改，并发冲突拒绝覆盖。相同操作 ID 与输入返回原始回执，改输入复用 ID 则冲突。若命令结果丢失，应先用 `/memory` 核对；CLI 不承诺跨进程重发命令自动复用操作 ID。

多个 CLI 用独立连接访问同一数据库，通过 WAL、5 秒 busy timeout 和短事务协调；每个 Store 最多一个连接。事务读取版本前先取得写锁。profile 身份持久化且参与作用域哈希，新建数据库不会误用旧 profile 向量。

Outbox 是 pending → running → done。常规失败指数退避，累计五次失败进入 failed，`/memory retry` 重新排队。running 租约 120 秒，单任务外部处理期限 45 秒；到期可重领，完成时校验 owner，拒绝旧 worker 修改新租约。sync 只处理已就绪或租约到期的任务，不绕过退避。

向量主键为 `SHA256(memory_id/version)`，同版本重复 upsert 幂等。更新清理 `< version`，删除清理 `<= tombstone_version`，避免删除更高版本。过期任务跳过，当前版本已有自己的 Outbox。

```text
显式编辑 → SQLite事务（事实 + Outbox + 回执）
             → worker领取、核对版本 → Embedding/Milvus → owner校验后完成

新会话/下一次模型请求
  → 近期事实 + 可选语义候选vector_id
  → SQLite过滤旧版本、删除记录、其他作用域
  → 读取SQLite正文 → 按预算临时注入低信任上下文
```

召回材料不会写入旧历史和检查点，后续请求可观察到纠正/删除。晚到的旧写入可能残留物理向量，但 SQLite 校验仍阻止它进入召回；不承诺跨库事务或瞬时物理一致性。索引绑定包含端点、集合、Embedding 模型身份及维度，拒绝静默切换；迁移需要另做显式重建。

删除只阻止未来记忆召回，不会擦除旧聊天、回执或 Outbox。数据库未加密，不含企业多租户登录鉴权。MD 可以作为人工知识文档，当前正式记忆通过 CLI 管理。

## 验证

```bash
go test ./...
go test -race ./internal/personalmemory ./internal/harness ./internal/personal ./cmd/harness -count=1
CGO_ENABLED=0 go build ./cmd/harness
```

测试使用真实 SQLite/WAL，覆盖事务回滚、回执、纠正删除、独立连接 CAS、任务领取、租约恢复和 fencing；CLI 测试覆盖离线管理、新 session、项目隔离和临时注入。Milvus/Embedding 使用 HTTP 替身验证协议与旧候选过滤，不等于真实供应商/向量服务验收。
