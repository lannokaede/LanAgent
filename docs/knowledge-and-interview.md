# 知识库与 Go 后端面试

## 数据与配置

采集程序位于 `internal/corpus`，种子来自 Go、Redis、PostgreSQL 官方文档。最多 30 个种子，当前 17 个；不递归、不登录，校验 robots、同源重定向和响应大小，请求间隔 1 秒。manifest 记录来源、主题、时间和 SHA-256，导入先核对清单及文件哈希。

`configs/interview/config.yaml` 声明聊天、Embedding 和 Milvus 配置。聊天密钥使用 `LLM_API_KEY`，向量密钥使用 `EMBEDDING_API_KEY`。可通过忽略的 `config.local.yaml` 配置本机值，`MILVUS_URL`、`MILVUS_TOKEN` 支持环境变量覆盖。向量集合元信息校验模型身份和维度，不混用不匹配的索引。

```bash
mkdir -p .runtime/corpora
go run ./cmd/knowledge crawl -dir .runtime/corpora/go-backend
go run ./cmd/knowledge import-corpus -config configs/interview -dir .runtime/corpora/go-backend
go run ./cmd/knowledge search -config configs/interview -query 'Go channel synchronization happens before' -top-k 3
```

采集目录应是新的目录；既有导入可重跑，已完成的同版本文档会跳过。部分导入失败会报告错误，不把它算作整库成功。知识库服务由使用者单独部署，库中数据不随源码发布。

## 一问一答与续接

```bash
go run ./cmd/interview -topic concurrency -questions 3
go run ./cmd/interview -topic concurrency -session .runtime/interviews/SESSION.json
go run ./cmd/interview -topic concurrency -followups=false
```

主题包括 `all`、`go`、`concurrency`、`runtime`、`database`、`redis`、`engineering`。回答以 `/submit` 提交，`/exit` 退出。每次展示一道题，提交后结合真实检索结果给出反馈；参考答案由程序维护。基础题低分且模型提出追问时最多追问一次，使用同一知识主题和参考要点，随后继续下一基础题。

JSON 程序校验分数、维度和来源索引；最近三轮历史参与点评。反馈仅为练习参考，来源序号合法不等于已经证明其内容支持所有判断。

状态文件保留题目位置、待答证据、追问和历史，展示题目前与作答后保存。相同状态文件通过 OS 锁避免并发覆盖。`*.jsonl` 记录完成问答，`*-review.md` 收集薄弱题、反馈、参考答案和资料来源。恢复时主题与知识库配置须匹配原会话。

## 自由学习与代码验证

```bash
go run ./cmd/harness -config configs/interview -workspace .
go run ./cmd/harness -config configs/interview -workspace . -exercise context-cancel/v1 -timeout 3m
```

个人入口可检索证据、解释 Go 问题，并调用绑定的固定代码题测试工具。代码 SHA-256 证据沿用练习模块的 JSON 编码字符串哈希契约；它和文件工具的原始 UTF-8 字节哈希用途不同。测试结果只对应返回的那份代码。
