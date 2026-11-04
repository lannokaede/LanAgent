# LanAgent 外部工具与 MCP 适配调研

核查日期：2026-11-03。本文核查官方文档、官方仓库及工具注册源码；未使用真实账号调用付费服务，未做效果、成本或吞吐量实测。供应商能力不等于 LanAgent 已完成的功能。正式部署应固定版本并重新执行 `initialize → tools/list → tools/call` 兼容性测试。

## 选择结论

总体最有价值的组合是：**一个网页搜索/提取供应商 + Context7 + GitHub 只读工具**。第一阶段先完成一个联网供应商与知识入库链路，第二阶段再接文档和源码研究。PDF/图片资料确有需求时补 Docling；只有普通提取失败且需要页面交互时再用 Playwright。Firecrawl 与 Tavily 先选一个，避免功能、费用和上下文重复。

| 候选 | 适合 LanAgent 的任务 | 优先级 | 当前接入可行性 |
|---|---|---|---|
| Tavily MCP | 查找面经、技术文章，筛选 URL 后提取，定界抓站 | P0 候选 | HTTP Bearer 可直接配置；工具名称须核对实际发现结果 |
| Firecrawl MCP | 网页搜索、正文/结构化提取、站点发现与抓取 | P0 候选 | 托管 Streamable HTTP Bearer 可配置；必须约束复杂工具参数 |
| Context7 | 按库与版本检索文档，为答案与练习提供依据 | P1 | 当前官方支持 HTTP Bearer，可直接配置 |
| GitHub 官方 MCP | 查源码、issue、release，跟踪 Go 生态变化 | P1 | PAT Bearer 或本地 stdio；远端可用只读 URL |
| Docling Serve / Docling MCP | PDF、DOCX、扫描资料转为结构化文本 | P2 按需 | Serve 是 REST，不能直接填入 MCP URL；另有官方 MCP 包 |
| Playwright MCP | 动态网页的导航、快照、必要交互 | P2 回退 | stdio 最直接；HTTP 模式需核对 `/mcp` 与实际协议 |

这些优先级是针对当前项目的设计判断，不是供应商能力排名。

## 已有客户端边界

本地代码依据：[MCP 适配器](../../internal/extension/mcp.go)。配置仅支持 `name/url/command/args/token_env/tools`，HTTP 使用 Streamable HTTP 并只加 Bearer；不支持 OAuth 流程、自定义 header、旧 SSE transport 或每服务独立 env。stdio 子进程继承进程环境。工具按显式白名单注册并在启动时固定；原始名称转换为稳定的 `mcp_<hash>`，描述仍保留服务名和原始工具名。

因此，本文的“可直接配置”仅指接口形式适配，仍须实测 schema、返回内容、超时和模型调用；不代表已经连通。供应商官方的 `mcpServers` JSON 也不能原样复制为本项目的 `servers` JSON。

## 1. Tavily MCP：较窄的搜索与提取组合

官方源码当前注册 `tavily_search`、`tavily_extract`、`tavily_crawl`、`tavily_map`，还有 `tavily_research`、`tavily_feedback`。README 的部分文字仍用 `tavily-search` 等连字符写法，因此**不能从介绍文字猜工具名**；启动后以 `tools/list` 为准。首期只开放前两个；需要站点扩展再加 map/crawl。[官方注册源码](https://github.com/tavily-ai/tavily-mcp/blob/main/src/index.ts)

托管地址为 `https://mcp.tavily.com/mcp/`，支持 Bearer API key；还支持 OAuth 与 URL query key。LanAgent 使用 Bearer，避免密钥进入 URL。本地 `tavily-mcp` 默认 stdio，以 `TAVILY_API_KEY` 读取账号。远端默认参数 header 不是必需；当前客户端没有该 header 支持时，直接在工具参数中给限制即可。[官方配置](https://github.com/tavily-ai/tavily-mcp)

建议用途：先搜索 Go 并发、数据库、面经等主题，筛选可信候选，再 extract 少量正文；记录 URL、日期、内容 hash 与来源类型，不把面经中的解答直接当作事实。成本取决于搜索深度、提取 URL 数量、抓取范围与供应商额度，不在本文引用具体价格。

## 2. Firecrawl MCP：抓取较完整，但适配层应缩窄能力

建议工具白名单：`firecrawl_search`、`firecrawl_scrape`；按需增加 `firecrawl_map`、`firecrawl_crawl`、`firecrawl_check_crawl_status`。官方另有 agent、interact、monitor 等能力，不建议首期全开。托管入口 `https://mcp.firecrawl.dev/v2/mcp` 支持 Bearer API key；官方也有受限免 key 模式和独立 OAuth 入口。本地支持 stdio 与 Streamable HTTP。[官方工具与部署说明](https://github.com/firecrawl/firecrawl-mcp-server)

**参数边界尤其重要**：当前 `firecrawl_scrape` 不只接受 URL，还可通过 `alexandria` 执行其他提供方能力；本地模式还可能接受浏览器 actions。仅把工具名放进白名单不能证明它是“只读网页”。面向本项目应包装成 `web_fetch(url, …)`，拒绝 Alexandria、账号条款、actions 等无关参数，仅允许公开 HTTP(S) 页面并由网关限制出口。[官方 scrape 与 Alexandria 说明](https://github.com/firecrawl/firecrawl-mcp-server#15-alexandria-tools)

建议先让自己的 harness 决定搜索、阅读和停止，避免直接把整个研究任务交给 `firecrawl_agent` 而削弱项目自身编排价值。费用受抓取页数、搜索附带正文、抽取/交互及监控影响；大范围 crawl 须设页数、深度与路径限制。

## 3. Context7：补足文档核对

准确工具名为 `resolve-library-id` 与 `query-docs`。先解析库名，取得 library ID，再按问题查询；可以利用返回的版本信息约束资料。它适合库 API 与示例核对，不替代面经搜索，也不能承诺覆盖所有 Go 库。[官方工具说明](https://github.com/upstash/context7#available-tools)

远端 `https://mcp.context7.com/mcp` **当前支持 `Authorization: Bearer`**，所以不必为了旧示例里的自定义 header 先改 LanAgent。另有 `/mcp/oauth`，当前项目没有 OAuth，应使用 API key 路线；本地 stdio 也有官方包。[官方 MCP 客户端文档](https://context7.com/docs/resources/all-clients)

建议将检索结果保留为带出处的候选证据，不标记为“已经验证正确”。成本主要是文档请求额度与服务限流。实际 Go 库覆盖率应拿项目涉及的 SDK 做小样本连通测试。

## 4. GitHub 官方 MCP：把题目联系到真实源码与版本

推荐最小工具：`get_file_contents`、`search_code`、`search_issues`、`issue_read`、`list_releases`。用于读开源实现、定位修复讨论、确认 release 变化；不开放 issue/PR 创建和代码写入。[官方工具目录](https://github.com/github/github-mcp-server#tools)

托管地址为 `https://api.githubcopilot.com/mcp/`，支持 PAT Bearer 与 OAuth；本地 `github-mcp-server stdio` 使用 `GITHUB_PERSONAL_ACCESS_TOKEN`。当前客户端可走 PAT，无须实现 OAuth 才能开始。[官方部署说明](https://github.com/github/github-mcp-server)

远端可用 `/readonly` URL 设置只读，避免因缺少 `X-MCP-Readonly` header 而受阻；工具集还有 `/x/{toolset}` URL。若需要精确服务端工具筛选、lockdown 等 header，可后续新增受限 `headers_env`。本地可用 `--read-only`、`--tools`。客户端白名单、服务端只读与低权限 token 应同时配置；不采用运行时启用更多工具的模式。[官方服务端配置](https://github.com/github/github-mcp-server/blob/main/docs/server-configuration.md)

费用与额度取决于 GitHub 账号、API/搜索限流及部署资源。PAT 能读到哪些仓库与是否需要组织授权，应按账号实测。

## 5. Docling：文件解析，不是另建一套 RAG

Docling Serve 是 REST 服务，例如 `POST /v1/convert/source`；若启用 `DOCLING_SERVE_API_KEY`，认证 header 为 `X-Api-Key`。**REST URL 不能作为 MCP endpoint 交给当前客户端**，单纯新增自定义 header 也不能把 REST 变成 MCP。[Serve 官方 README](https://github.com/docling-project/docling-serve)、[认证与运行配置](https://github.com/docling-project/docling-serve/blob/main/docs/configuration.md)

官方另外提供 `docling-mcp`，支持 stdio、SSE、Streamable HTTP，可由它转接 Serve。转换工具真实名称包括 `convert_document_into_docling_document`、`convert_directory_files_into_docling_document`，以及缓存查询/清理工具。当前文档提示 MCP 包跨 SDK 大版本兼容问题，应固定可测版本；Python SDK 与 Go SDK 的版本数字不能直接类比。[官方 MCP 部署与兼容说明](https://github.com/docling-project/docling-mcp)、[转换工具源码](https://github.com/docling-project/docling-mcp/blob/main/docling_mcp/tools/conversion.py)

本项目优先考虑自己写窄的 `parse_document(artifact_id)` adapter 调用 Serve：只处理上传后的文件，统一转为已有 corpus 格式，不开放目录批量扫描和另一个向量数据库。若走官方 MCP，转换后还应调用 `export_docling_document_to_markdown` 并确认缓存生命周期；转换返回 document key 并不等于拿到了完整 Markdown。[官方导出工具源码](https://github.com/docling-project/docling-mcp/blob/main/docling_mcp/tools/generation.py) 成本主要在 CPU/GPU、OCR/视觉处理、模型权重、存储与任务队列。

## 6. Playwright MCP：只作为网页抓取回退

可选最小集合：`browser_navigate`、`browser_snapshot`、`browser_click`、`browser_wait_for`、`browser_tabs`、`browser_close`。默认 stdio 可直接适配；官方 standalone 示例以 `--port` 启动，客户端连接 `/mcp`。其 README 同时存在 SSE 参数措辞，应通过目标版本确认 Streamable HTTP 行为，不对旧 `/sse` 地址作兼容承诺。[官方部署与工具说明](https://github.com/microsoft/playwright-mcp)

不要首期开放 `browser_run_code`、上传、下载、任意表单提交与复用真实登录状态。以独立浏览器 profile、无私人 cookie 的临时会话运行；网页外部内容只是数据，不能修改 harness 的系统指令。官方明确指出 Playwright MCP、allowed/blocked origins 均不构成安全边界；现有工作区 `os.Root` 也不约束外部浏览器进程。[官方安全说明](https://github.com/microsoft/playwright-mcp#security)

本地 stdio 没有额外 SaaS 认证，但不等于远端暴露时具备访问控制；共享部署应有网关鉴权与容器/网络隔离。成本来自浏览器资源、页面交互轮次和模型上下文；不把官方宣传的性能视为本项目实测结果。

## 建议的统一适配契约

外部供应商挂在可替换 adapter 后，领域工具以主设计中的 `web_search`、`web_fetch`、`web_crawl`、`parse_document` 等为准；Context7 与 GitHub 保留经过白名单选择的官方工具，不额外复制一套同义工具。工具结果统一包含 `source_url`、`retrieved_at`、`content_hash`、`artifact_id`、`excerpt`、`truncated`、分页/作业标识。抓取原文落 artifact，模型拿短预览，后续按需分页读取。

URL 获取需统一限制 schema、重定向、私网/元数据地址与 DNS 变化；页面及工具结果不授予写入权限。原文转题卡、去重、来源分级、staging、差异、入库与回滚属于 LanAgent 自己的知识领域模块，供应商 MCP 不会自动完成。

长抓取、PDF 转换与远端任务采用持久化 job ID：提交任务、查询状态、读取结果分别记录；网络超时不盲目重新提交收费或写入请求。定时触发器只创建有界任务，由 harness 执行，不让模型自行无限循环或任意注册 cron。

首期无需为接入上述只读服务全面重写 MCP：先补连接诊断、清晰错误、固定版本/工具目录快照与每服务 env 白名单。需要时再补自定义 headers、异步作业恢复及集中出口策略；OAuth、动态工具发现与广泛浏览器自动化放后面。
