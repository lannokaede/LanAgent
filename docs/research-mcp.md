# 资料研究与知识库 MCP

`cmd/research-mcp` 与 `cmd/harness` 放在同一个仓库、同一个 Go module，分别编译启动；共享 `internal/research`、现有采集和 RAG 实现。使用方可以只运行 MCP，不需要启动聊天模型或个人 CLI。当前首版面向单个受信任用户，不提供多租户隔离。

## 已完成的范围

第一阶段提供 12 项 MCP 工具：`web_search`、`web_fetch`、`read_artifact`、`search_artifact`、`knowledge_sources`、`knowledge_document`、`knowledge_stage`、`knowledge_commit`、`knowledge_search`、`job_status`、`job_cancel`、`operation_status`。关闭写权限时不注册 commit/cancel，其余 10 项保留；不连接知识库时，知识检索明确报告未配置。

功能包括：来源白名单、正文工件、每页至多 2 KiB 的 UTF-8 分页（完整 JSON 不超过 3500 字节，避免再次被上下文预览裁掉）、内容哈希检查、候选去重与分块、按确切 manifest 入库、持久任务、执行 ID 与参数绑定的操作记录、关闭/重启续接，以及 Harness 的主机核对入口。普通工具错误不会包含供应商原始响应体或索引器私密错误。

面试进度、答题与复习工具现已接入统一 Harness 对话，详见[统一面试训练](knowledge-and-interview.md)。研究 MCP 保持独立工具提供方；GitHub/Context7、批量 crawl、PDF 和 profile 分析仍按[整体设计](tooling-design.md)后续推进。

## 本机运行

```bash
go build -o bin/lanagent-research-mcp ./cmd/research-mcp
# 默认 stdio，只读正式知识库；无需模型、向量库或爬虫账号
./bin/lanagent-research-mcp -data-dir .runtime/research
```

默认 `-web official` 复用现有官方爬虫，只允许 go.dev、redis.io、www.postgresql.org，检查 robots、同源跳转、正文大小和限速。`web_search` 查询独立 SQLite 站点索引，不访问搜索服务商；使用前运行 `research-mcp index refresh`。`web_fetch` 实时读取页面并更新该索引，不自动写入向量库。初始化、按需调用和关闭策略见[官方网页搜索](official-web.md)。

如需搜索面经与其他白名单页面，配置独立的 Firecrawl Key，然后使用 `-web firecrawl`。它与聊天/Embedding Key 不共用：

```bash
# Bash 中隐藏输入，Key 不进入命令历史
read -rsp 'Firecrawl API Key: ' FIRECRAWL_API_KEY
export FIRECRAWL_API_KEY
printf '\n'
./bin/lanagent-research-mcp -web firecrawl -data-dir .runtime/research
```

联网 adapter 只调用 Firecrawl search 与 Markdown scrape，模型不能设置浏览器 actions、Alexandria、请求头、供应商地址或 Key。首次连接需实测账号权限和额度；无凭据不保证可用。默认来源允许 go.dev、redis.io、www.postgresql.org、nowcoder.com、juejin.cn 及其子域；可用 `-allowed-hosts` 设置明确的主机列表。

搜索结果仍按白名单过滤，所以可能返回零条。抓取后的最终 URL 也会校验；云端抓取中的 DNS/中间跳转由供应商控制，本模块不宣称能约束供应商内部网络。非官方来源先进入面经/社区候选通道，来源分类并不证明内容一定是面经或答案正确。

网络请求预算按数据目录持久化：默认最多 20 次供应商搜索、100 次页面获取，失败请求同样计数；操作员可以通过 `-max-search-calls`、`-max-fetch-calls` 提高限额。重启不重置预算。这是请求上限，不是账单估算，也不替代供应商额度设置。

## 启用知识入库

先完成已有 Embedding 配置：`go run ./cmd/setup -kind embedding`，并准备 Milvus。当前 Embedding adapter 使用 text-embedding-v4，默认 1024 维。以下命令不需要聊天模型，但会调用 Embedding：

```bash
./bin/lanagent-research-mcp \
  -config configs/interview -knowledge -allow-knowledge-write \
  -namespace research-go -data-dir .runtime/research
```

默认只检查现有向量集合；确实需要新建时增加 `-init-store`。`-namespace research-go` 只读写 `research-go-official` 与 `research-go-experience` 两个 namespace，不清理已有 `go-backend-interview`。目录首次接入索引器时绑定集合、地址与模型身份；之后不能悄悄换一个索引配置继续旧任务。

工具使用顺序：

1. `web_search` 找候选，`web_fetch` 返回 `source`、`artifact_id` 和预览。
2. 用 `read_artifact` 续读或 `search_artifact` 定位；网页内容不具有工具授权能力。
3. `knowledge_stage` 传 1–8 个工件和来源类型，返回 `candidate_id`、`manifest_hash`、分块数。官方/非官方类型依据来源策略校验，不能将面经假标为官方。
4. `knowledge_commit` 提交确切候选和 manifest，返回 accepted 与 job_id。accepted 不等于已经入库。
5. 稍后 `job_status` 查看 succeeded/failed/cancelled；成功后调用 `knowledge_search`。当前 source_kind 为 official 或 interview_experience，默认 official。

同一次 execution_id 重发返回原任务；相同候选已完成或正在处理时也复用已有任务。改了参数却使用同一 execution_id 会被拒绝。正式提交要求 MCP `_meta` 中的 `io.lanagent/execution_id`；LanAgent 自动提供，其他 MCP 主机需适配该元数据后才能写入，读取与准备候选不需要它。

## 持久化、失败与取消

目录包含 `state.json`、`owner.lock`、`artifacts/*.md`。元数据以受限大小 JSON 快照原子替换并同步文件和目录，进程间锁保证同一目录只有一个所有者。知识任务和来源元数据采用本地文件快照；网页全文索引另存于 `web-index.db`（SQLite，无需数据库服务）；不适合作为共享网络文件系统上的多实例服务。请保留整个私有数据卷。

同一时刻只处理一个入库任务。开始写入前记录 repair 标志；导入期间知识搜索返回 busy。部分失败或取消后，该服务的知识查询保持阻止，修复方式是再次提交**同一候选**并使用新的 execution_id；完成后解除。退出/重启保留未完成任务，重新以写模式启动会继续核对处理；已记录完成的文档版本跳过向量化。

这不等于向量事务或 exactly-once：崩溃在向量写入完成但目录尚未落盘时，恢复可能重新向量化该文档，稳定 chunk ID 避免重复行，但可能重复产生 Embedding 费用。取消是尽力停止，不是回滚。

所有保护适用于这个研究模块。不要同时通过其他 CLI、另一个数据目录或数据库管理工具写它所管理的 namespace；那些入口不会读取此目录的 repair 标志。原有官方知识库不受这两个新 namespace 的修复状态影响。

当前目录上限为 4096 个工件记录、512 个候选、512 个任务、1024 条操作记录及 16 MiB 元数据。达到上限明确报错；未实现自动清理，不能为腾空间直接删除被未完成任务引用的文件。

## Streamable HTTP 与 Harness 接入

```bash
# 同一个终端环境供服务与客户端读取；不打印 Token
export LANAGENT_RESEARCH_TOKEN="$(openssl rand -hex 32)"
./bin/lanagent-research-mcp \
  -listen 127.0.0.1:18090 -data-dir .runtime/research \
  -knowledge -allow-knowledge-write
```

HTTP 仅提供 `/mcp`，要求至少 24 字符的 Bearer Token。带 Origin 的请求必须匹配 `-allowed-origins` 指定的完整来源，默认拒绝。非 loopback 监听需显式 `-allow-remote`，面向公网时使用 TLS 反向代理。当前是单用户访问 Token，不宣称已经实现 MCP OAuth 授权发现。

使用另一个终端时，应从私密配置读取同一 Token，不能重新生成一个不同的 Token：

```bash
go run ./cmd/harness -workspace . \
  -mcp-config examples/harness/research-mcp.example.json \
  -timeout 5m \
  -prompt '抓取 https://go.dev/blog/context，查看正文并准备官方知识候选，入库后给出任务 ID。'
```

示例白名单包含写工具，所以服务需要 `-allow-knowledge-write`；只读部署需同时从客户端白名单删掉 knowledge_commit/job_cancel，否则当前客户端会报告缺少白名单工具。HTTP Key 不写入 GitHub，`FIRECRAWL_API_KEY` 只给服务端，客户端只需要研究 MCP 的访问 Token。

stdio 也可配置：

```json
{
  "servers": [{
    "name": "research",
    "command": "/absolute/path/to/lanagent-research-mcp",
    "args": ["-data-dir", "/private/research", "-web", "official"],
    "tools": ["web_fetch", "read_artifact", "search_artifact", "knowledge_stage", "knowledge_sources"]
  }]
}
```

这份 stdio 示例不写向量，不依赖工作目录中的配置文件；启用 knowledge 时请给出绝对 `-config` 路径。当前 stdio 环境仍继承宿主环境，子进程不是系统沙箱。

若 knowledge_commit 已被远端接收、本地 pending 却没收到可靠响应，保持相同配置执行：

```bash
go run ./cmd/harness -workspace . -session YOUR_SESSION_ID \
  -mcp-config examples/harness/research-mcp.example.json -reconcile
go run ./cmd/harness -workspace . -session YOUR_SESSION_ID \
  -mcp-config examples/harness/research-mcp.example.json -resume
```

核对只调用原服务白名单中的 operation_status，检查执行 ID、工具、参数哈希及原 accepted 结果，之后写回原 checkpoint；不会重新提交入库。记录不存在或不匹配就保留不确定状态，可继续人工 `/resolve`。目前自动核对只支持自有 knowledge_commit 契约。

## Docker 与 GitHub

```bash
docker build -f deployments/research/Dockerfile -t lanagent/research-mcp:local .
# 持久目录需可被镜像内 UID 65532 写入
mkdir -p .runtime/research-docker
sudo chown 65532:65532 .runtime/research-docker
docker run --rm -p 127.0.0.1:18090:18090 \
  -v "$PWD/.runtime/research-docker:/data" \
  -e LANAGENT_RESEARCH_TOKEN \
  lanagent/research-mcp:local
```

镜像默认是官方文档读取模式，无需模型凭据，也不挂载 Docker socket。上例只运行 MCP，不运行 Milvus。容器需要写库时，额外挂载本地私有配置目录并显式给出 knowledge 开关；Milvus 地址必须从容器可达，不能照搬宿主机的 localhost。密钥配置只运行时注入，不写进镜像。

同库提交包含 `cmd/research-mcp`、`internal/research`、部署文件、接入示例、测试与文档。私有数据、凭据及采集原文排除在 Git 和 Docker 构建上下文之外。现有 Go checks 覆盖新入口；`Build research MCP` workflow 可手动运行或通过 research-v* 标签触发，为 Linux amd64/arm64 生成独立二进制附件；不会自动创建 Release、上传容器镜像或发布 MCP Registry。

## 验证

```bash
go test ./...
go test -race ./...
go vet ./...
```

测试包括真实 SDK 协议交互、输入 Schema、主机元数据、任务幂等/重启、失败修复、UTF-8 分页、工件篡改、来源限制、HTTP Token/Origin 及核对不重放。外部供应商协议用本机 HTTP 替身验证；官方网页索引和抓取使用本机站点验证；真实官方站点可不配置 Key 单独检查。Firecrawl 搜索与账号权限需另外配置其 Key 验证。
