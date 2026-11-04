# 知识库与 Go 后端面试

## 数据与配置

采集程序位于 `internal/corpus`，种子来自 Go、Redis、PostgreSQL 官方文档。最多 30 个种子，当前 17 个；不递归、不登录，校验 robots、同源重定向和响应大小，请求间隔 1 秒。manifest 记录来源、主题、时间和 SHA-256，导入先核对清单及文件哈希。

首次可用 `go run ./cmd/setup` 配置聊天 API、`go run ./cmd/setup -kind embedding` 配置向量 API，共享保存后无需逐个入口填写。`configs/interview/config.yaml` 声明知识库和基础配置。聊天密钥使用 `LLM_API_KEY`，向量密钥使用 `EMBEDDING_API_KEY`。可通过忽略的 `config.local.yaml` 配置本机值，`MILVUS_URL`、`MILVUS_TOKEN` 支持环境变量覆盖。向量集合元信息校验模型身份和维度，不混用不匹配的索引。

```bash
mkdir -p .runtime/corpora
go run ./cmd/knowledge crawl -dir .runtime/corpora/go-backend
go run ./cmd/knowledge import-corpus -config configs/interview -dir .runtime/corpora/go-backend
go run ./cmd/knowledge search -config configs/interview -query 'Go channel synchronization happens before' -top-k 3
```

采集目录应是新的目录；既有导入可重跑，已完成的同版本文档会跳过。部分导入失败会报告错误，不把它算作整库成功。知识库服务由使用者单独部署，库中数据不随源码发布。

## 同一对话中的面试与续接

```bash
go run ./cmd/harness -config configs/interview -workspace .
# 继续同一对话，包括待答题和复习记录
go run ./cmd/harness -config configs/interview -workspace . -session SESSION_ID
```

自然语言示例：“开始 3 题 Go 并发面试”→“不懂，先讲一下”→“继续面试”→输入答案→“总结薄弱点”。模型根据提示词和上下文选择已注册工具，不另起固定面试程序。知识库配置只决定资源是否可用；同一进程内可以阅读文件、检索、使用 MCP 和代码验证。

内置 `go-interview` Skill 在启用 RAG 时自动提供元数据，通过 `load_skill` 按需加载完整步骤。工作目录有同名 Skill 时以本地版本为准；Skill 不改变工具权限。

| 工具 | 作用 |
| --- | --- |
| `interview_start(topic, questions?)` | 开始或恢复该主题的训练；切换主题时保留原训练 |
| `interview_status()` | 读取待答题、完成数及主机绑定的当前 input_ref |
| `interview_next()` | 返回同一待答题；没有待答题时检索证据并提出下一题 |
| `interview_submit_answer(question_id, input_ref)` | 点评主机保存的当前用户原文，原子保存结果与进度 |
| `review_list()` | 读取本段对话中已保存的薄弱点 |

主题包括 `all`、`go`、`concurrency`、`runtime`、`database`、`redis`、`engineering`。题数默认 5，范围 1–16，按已完成答题轮次计数，追问也占一次。基础题低分且模型提出追问时最多追问一次；参考要点由程序维护，反馈 JSON 校验分数、维度和来源索引。

正常回答直接输入。多行内容使用 `/paste`，以单独一行 `/submit` 结束；点评使用整段原始输入。模型只传题目和输入引用，不能传一段自己生成的答案冒充用户。刚提出题目的同一用户轮次不能立即提交答案；重复提交同一输入不会重复点评或推进。

讲解、查询、文件操作不会自行修改面试进度。是否评分和何时继续由模型根据用户意图选择工具，程序负责状态一致性，不做关键词硬切换。意图理解属于模型行为，仍可能判断错误；可以要求它保留题目并继续讲解。

统一会话目录包含 `SESSION_ID.json`、`SESSION_ID.pending.json`、`SESSION_ID.jsonl` 与 `SESSION_ID.interview.json`。最后一个文件保存该对话的训练主题、待答证据、历史和操作回执，由同一会话锁保护。短期模型上下文摘要不会替代这些真实记录。面试状态和操作结果一起原子保存，核对时可复用已提交结果；`-reconcile` 支持本地面试操作与已有研究 MCP 操作，缺少回执时保留人工 `/resolve`。

兼容命令 `go run ./cmd/interview -topic concurrency -questions 3` 会把初始请求交给统一 CLI。旧的独立 `.runtime/interviews/*.json` 不会自动导入，也不会删除；新的 `-session` 传统一会话 ID。原来独立程序的 `/submit`、`-log-dir`、`-followups` 使用方式由上述统一交互替代。

## 自由学习与代码验证

```bash
go run ./cmd/harness -config configs/interview -workspace .
go run ./cmd/harness -config configs/interview -workspace . -exercise context-cancel/v1 -timeout 3m
```

个人入口可检索证据、解释 Go 问题，并调用绑定的固定代码题测试工具。代码 SHA-256 证据沿用练习模块的 JSON 编码字符串哈希契约；它和文件工具的原始 UTF-8 字节哈希用途不同。测试结果只对应返回的那份代码。
