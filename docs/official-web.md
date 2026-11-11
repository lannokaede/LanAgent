# 按需查询官方网页

首版覆盖 `go.dev`、`redis.io`、`www.postgresql.org`。不需要搜索服务商账号：自己抓取这些站点，建立本地 SQLite 全文索引，再通过研究 MCP 提供给模型。聊天模型仍需原有配置；网页搜索与抓取不需要 Embedding 或 Milvus。

## 初始化与接入

在仓库根目录运行：

```bash
mkdir -p bin
go build -o bin/lanagent-research-mcp ./cmd/research-mcp
# 初次可先只抓取现有种子页，检查结果
./bin/lanagent-research-mcp index refresh -data-dir .runtime/research -max-pages 17 -max-depth 0
# 后续手动发现链接并刷新，默认最多 100 页、深度 2、总期限 10 分钟
./bin/lanagent-research-mcp index refresh -data-dir .runtime/research
# 使用只读网页工具配置进入对话
go run ./cmd/harness -workspace . \
  -mcp-config examples/harness/official-web-mcp.example.json -timeout 3m
```

示例使用 stdio，CLI 会启动并关闭 MCP 子进程，无需单独启动 HTTP 服务或设置访问 Token。命令路径和数据路径相对于 `-workspace`；更换工作目录时，请复制示例，改用二进制和数据目录的绝对路径。

初始化结果包含 `attempted`、`indexed`、`failures`。部分页面失败时命令退出非零，但已完成的页面保留并可检索。可降低页数排查，修复网络后重新刷新；不会因为失败删除旧页面。命令不读取聊天模型或个人记忆配置。

## 对话方式

可以这样提问：

```text
解释一下 channel 的基本概念。
搜索 Go 官方文档，核实 context 取消如何传递，读取页面并给来源。
读取 https://go.dev/blog/context，再根据正文解释。
不要联网，只根据已有知识解释 context。
```

Harness 不会在每条消息前自动搜索。模型根据问题、工具说明和用户限制，选择直接回答、查本地知识库、搜索或读取网页。自然语言约束依赖模型遵守，不是关键词匹配器；本机固定模型测试验证调用流程，不代表实测了真实模型的判断准确率。

`web_search` 只搜索本地站点索引，不发送网络请求，不保证页面最新，也不是全网搜索。结果提供 `origin: local_site_index`、URL、标题、摘要及 `fetched_at`；抓取时间不等于发布日期。索引为空会提示初始化，没有匹配则返回空列表。

需要核实最新信息时，模型应在搜索后调用 `web_fetch`，真正读取目标页面。只有成功抓取的结果才提供本次读取时间和 artifact；超时、robots 拒绝或页面错误都不能作为已核实的依据。完整正文通过 `read_artifact` 分页读取，或用 `search_artifact` 定位。

覆盖范围取决于已抓取页面，首版不会自动发现所有版本和最新发布页。超出这三个站点的问题需要明确说明范围限制。这是关键词检索，不提供跨语言语义检索；检索英文官方文档时宜使用 `context cancellation` 等英文关键词。英文使用分词索引，中文长词使用 trigram；不足三个字符的查询补充子串检索，最多扫描最近 1000 页的标题和正文前 16384 个 SQLite 字符，因此短词检索不保证覆盖全部正文。

## 关闭网页工具

```bash
go run ./cmd/harness -workspace . \
  -mcp-config examples/harness/official-web-mcp.example.json \
  -web-policy off -prompt '不要联网，解释一下 channel'
```

默认 `-web-policy auto`。`off` 时宿主拦截 MCP 中原名为 `web_search`、`web_fetch` 的工具，即使模型请求调用，也不会向 MCP 发送该工具请求。策略保存在会话启动设置中，菜单第 9 项可切换；恢复未完成任务需保持原策略。

此开关限制上述网页工具，不是进程断网：模型连接、MCP 初始化、Embedding，以及其他名称的第三方联网工具不在其控制范围。示例只开放搜索、抓取和 artifact 读取，不开放入库工具。

## 数据与刷新

- `web-index.db` 位于 research 数据目录，与个人记忆数据库、向量库分离。保存正文、URL、标题、内容哈希、成功抓取时间，以及最近尝试的时间和失败信息。每页事务独立提交；相同内容不重建全文索引。
- 刷新使用原有 17 个官方资料 URL 作为种子，沿同站点的 `/doc/`、`/docs/`、`/ref/`、`/blog/` 链接广度优先发现。默认最多尝试 100 页、深度 2；可用 `-max-pages 1..1000`、`-max-depth 0..5` 调整。候选队列最多 10000 个 URL，索引最多 4096 页。
- 每个站点请求间隔至少 1 秒，包含 robots 请求和重定向；只抓取符合 robots 规则的公开 HTTPS 页面。同源重定向还要检查目标路径的 robots 规则；实际连接检查 DNS 地址并拒绝私网、本机及保留地址，不使用环境 HTTP 代理。
- `web_fetch` 沿用持久化抓取预算，默认每个 research 数据目录累计 100 次，失败也计数；可用 `-max-fetch-calls` 提高。本地索引搜索不消耗供应商搜索预算。维护刷新使用自己的页数/时间上限，不消耗会话抓取预算。
- 搜索和刷新不会自动调用向量化、staging 或 commit。需要长期保存到知识库时，另外启用原有知识入库配置，由用户明确要求后执行。

数据应放在本机文件系统。刷新命令可与同一目录的 MCP 服务并存，SQLite 串行提交写操作；不支持多个主机共享网络磁盘。没有后台定时更新、自动清理或任意站点搜索。

## 验证

```bash
go test ./internal/corpus ./internal/research ./internal/extension ./internal/cli ./cmd/research-mcp
go test ./...
```

自动测试使用本机 HTTP 站点及真实 MCP SDK，覆盖中英文检索、增量更新、重启、链接发现、robots、重定向、地址限制、失败保留、按需工具循环和关闭策略。可控模型只验证工具调用及观察消息传递；真实模型是否选择正确工具需使用自己的模型另行检查。

2026-11-05 本次验证：全量 `go test ./...`、相关包 race 测试及 `go vet ./...` 通过。尝试抓取两篇 Go 官方资料时，运行环境的 DNS 将 `go.dev` 解析到非公网地址，连接被地址检查拒绝，未完成公网抓取验证。没有通过关闭地址检查来绕过该限制。
