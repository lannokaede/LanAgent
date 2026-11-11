# LanAgent 个人 Harness

个人入口复用 `internal/harness` 的模型—工具循环，面向工作目录任务、Go 后端学习与代码练习。面试训练通过 `interview_start/status/next/submit_answer` 与 `review_list` 接入同一对话；`cmd/interview` 只保留启动别名。

## 启动

在仓库根目录运行。首次可执行 `go run ./cmd/setup`，填写或粘贴 API 配置，各入口共享读取；也可使用 `configs/harness` 或 `configs/interview` 本地覆盖配置，或设置 `LLM_BASE_URL`、`LLM_MODEL`、`LLM_API_KEY`。密钥不写入示例文件。

```bash
go run ./cmd/harness              # 终端自动显示启动菜单
go run ./cmd/harness -menu        # 显式菜单，可选择历史会话
go run ./cmd/harness -workspace . -prompt "读取 README.md 并说明项目结构"
go run ./cmd/harness -config configs/interview -workspace .
# 显式打开文件写入和固定诊断命令
go run ./cmd/harness -workspace . -allow-write -allow-commands
# 检索官方知识、解答和测试同一道固定编程题
go run ./cmd/harness -config configs/interview -workspace . \
  -exercise context-cancel/v1 -timeout 3m \
  -prompt "先检索 context 取消的资料，解释这道题，再给出实现并用沙箱测试；失败时修正后再测，引用来源和代码哈希"
# 模型/工具边界恢复；保持原工作目录、工具开关、MCP 配置和题目一致
go run ./cmd/harness -workspace . -session YOUR_SESSION_ID -resume
```

默认不依赖业务数据库；`configs/interview` 的知识检索需要已有 Milvus 与 Embedding 服务。`-exercise` 需要 Docker 和预先构建的 `lanagent/practice-go:1.26.9-v1` 镜像。其他题目为 `concurrent-collect/v1`、`worker-exit/v1`。沙箱工作目录沿用 `practice.work_dir`（默认 `.runtime/practice`），应位于 Docker daemon 可见的本机目录；不使用可能被 Snap 隔离的 `/tmp`。固定测试与代码限制沿用练习模块，模型不能选择自己的测试或任意容器命令。

交互命令：

| 命令 | 行为 |
| --- | --- |
| `/menu`、`/sessions` | 释放当前会话锁并进入会话管理 |
| `/help` | 显示交互命令 |
| `/exit` | 保留状态并结束对话；菜单启动时返回菜单 |
| `/paste` | 多行输入，以单独一行 `/submit` 提交 |
| `/status` | 显示未完成运行的阶段及不确定执行 ID |
| `/resume` | 继续当前任务，不重新发送用户任务 |
| `/resolve execution_id 人工核对结果` | 将已观察到的真实结果写入对应工具观察；不执行工具 |
| `/discard` | 放弃已知状态的未完成任务；不确定执行必须先核对 |

任务失败后不能直接用新问题覆盖未完成任务。`/resolve` 是人工核对接口，不是让模型自行宣称成功的工具。

## 文件、命令与 Skills

目录和读取工具默认启用，以 `os.Root` 限定在选定工作目录内；读取最多 32 KiB，可按 `next_offset` 分页。完整文件一次读取时返回 `sha256`，分页片段另有 `chunk_sha256`；文件哈希针对原始 UTF-8 字节。练习 `code_hash` 沿用原练习模块的 JSON 编码字符串 SHA-256 契约，与文件哈希用途不同。

`-allow-write` 注册 `write_file`：写入至多 32 KiB UTF-8 文本，父目录须存在。新文件的 `expected_sha256` 为空；覆盖现有文件需提供其当前哈希。相同内容直接返回幂等成功，目标版本不匹配则拒绝。临时文件同步后原子替换；不支持删除、任意补丁或超大文件编辑，禁止通过文件工具改写 `.git` 控制数据。哈希检查不是与外部编辑器之间的跨进程事务锁。

`-allow-commands` 注册 `run_command`，只接受 `go_version`、`git_status`、`git_diff` 三个固定诊断动作。无 Shell、无自由参数；禁用 Git filters、hooks、fsmonitor 和子模块探查，不作为用户代码的执行环境。执行用户 Go 代码使用 `-exercise` 的 Docker 工具。

Skills 放在工作目录的 `.lanagent/skills/<name>/SKILL.md`。文件以 YAML front matter 声明 `name` 和 `description`；`name` 必须匹配目录名。启动扫描最多 64 项，每项最多 32 KiB，只把目录元数据加入模型上下文；模型需要某项技能时，通过 `load_skill` 获取正文。技能文本不能开启写入、命令或额外工具权限。

## MCP 扩展

```bash
go run ./cmd/harness -workspace . -mcp-config examples/harness/mcp.example.json
```

示例中的地址和工具名需要对应你自己的 MCP 服务。配置支持 Streamable HTTP（`url`）或 stdio（`command`、`args`），二者只能选其一。每个服务必须声明非空 `tools` 白名单；远端工具以稳定哈希命名空间注册，描述中保留原服务器/工具名。参数在本地按远端 JSON Schema 校验，schema 远程引用不自动下载。HTTP Token 通过 `token_env` 指定的环境变量读取，重定向拒绝，关闭 CLI 时关闭连接和子进程。

MCP 进程和服务具有其自身权限，不是 Docker 沙箱；显式配置 MCP 服务就是信任并启用所列能力。注册表只开放白名单，不从 Skill 或模型输出动态添加工具。连接配置及 Token 的哈希参与恢复校验，不把 Token 写入会话。远端请求无法取得可靠响应时进入结果核对；不承诺外部系统 exactly-once。

## 按需查询网页

通过 [官方网页 MCP 配置](../examples/harness/official-web-mcp.example.json) 可接入不需要搜索服务商 Key 的站点搜索与抓取。先构建 MCP 并初始化网页索引，步骤见[官方网页搜索](official-web.md)。模型按问题选择工具，不会每轮自动搜索，也不再要求所有技术问题都检索知识库。

`-web-policy auto` 为默认值；`off` 在宿主侧拦截 MCP 原名为 `web_search`、`web_fetch` 的调用。菜单设置和会话启动记录会保留策略，恢复任务时需要一致。用户自然语言“不联网”由模型遵守，需要确定性禁用时使用参数。它不限制模型、Embedding 或其他第三方工具的网络连接。

## 上下文治理

个人 CLI 默认使用 `routing.mode: rules`。独立问候、感谢及“直接回答：…”“不使用工具：…”这类明确请求可以省略工具描述和工具循环，仍使用当前模型、完整会话和当前记忆回答，支持多轮会话。明确无工具请求即使提及文件或最新信息也不开放工具，由模型说明缺少证据的限制。规则不按“解释”“是什么”等词自动判断通用知识问答。

`routing.mode: hybrid` 可额外调用分类模型，使用原始请求与有界近期对话，不向分类器提供系统规则、个人记忆或工具观察。普通概念问答可进入直答；文件、网页核实、历史指代、面试状态和动作类请求保守进入 ReAct。分类超时、错误、未知类别或服务不支持严格 JSON Schema 时回退 ReAct。记录失败属于宿主错误，会终止执行，不作为分类或摘要降级处理。

在忽略版本控制的 `config.local.yaml` 中启用，例如：

```yaml
routing:
  mode: hybrid
  timeout: 3s
  classifier:
    model: YOUR_SCHEMA_COMPATIBLE_MODEL
telemetry:
  enabled: true
  file: .runtime/traces.jsonl
```

分类模型的 Base URL、模型和 Key 默认继承聊天配置；可以分别使用 `ROUTER_BASE_URL`、`ROUTER_MODEL`、`ROUTER_API_KEY` 覆盖。分类预算可设为 0–30 秒之间的正数。配置兼容的分类模型只说明接口可用；实际节省的费用与延迟需要单独测量。

直答继续复用记忆召回、上下文预算、模型网关、流式输出和检查点；路由和来源随检查点保存，中断恢复不重新分类。直答收到意外工具调用时拒绝执行。旧检查点和已执行工具批次仍沿用原路径。

当前目标、系统规则、近期消息与成对的工具调用/结果优先保留。请求内较长工具观察截断到约 4 KiB，保留代码哈希、测试状态、退出码等关键字段，原结果仍保存在执行状态中。输入预算仍以保守 UTF-8 字节估算，不等于供应商计费 token。

预算触发裁剪时，默认用无工具的模型请求概括有界历史，超时或失败回退确定性裁剪/摘录。摘要始终作为低信任历史材料，不能覆盖系统规则；`-summary=false` 关闭模型摘要。摘要会额外调用当前模型，有界输入和有损压缩不能保证保留所有历史事实。完整会话不因上下文压缩被改写。

## 持久化与恢复

默认目录为 `os.UserCacheDir()/lanagent/harness`，可用 `-state-dir` 指定。会话及未完成执行均绑定工作目录。工具集合、权限、MCP 连接身份和固定题目也参与恢复校验，避免换工具/换题后继续旧执行。

| 文件 | 内容 |
| --- | --- |
| `<id>.json` | 已完成轮次及最后提交的 run ID |
| `<id>.pending.json` | 未完成图阶段、消息、剩余调用、累计预算、执行 ID 和最终响应 |
| `<id>.jsonl` | 同步写入的运行事件与原始工具结果 |
| `<id>.launch.json` | 工作目录、资源配置、权限、MCP 路径和任务参数；不含凭据 |
| `<id>.interview.json` | 同一对话中的训练进度、真实答案与工具回执 |
| `<id>.lock` | OS advisory lock 的载体；进程退出自动释放，文件保留 |

在模型决策、工具执行前和工具结果完成后保存检查点。已完成工具结果先持久化再发送 UI 事件，UI 断开不导致工具重放。执行 ID 在同一运行/轮次/调用间稳定；执行前记录“正在执行”，结果和剩余批次一起落盘。若这两个时点之间中断，恢复停止并要求核对；不会猜测工具是否执行成功。

已完成轮次快照是提交点。若快照已提交但 pending 文件尚未清理，重新打开时通过 run ID 识别已提交运行并清理 pending，而非再次执行。旧版本的成功快照可以继续使用。锁文件本身不用人工删除，不能在运行中删除它来绕过锁。

默认每任务最多 10 个 ReAct 模型轮次、20 次工具调用；整批工具超预算在执行前拒绝。上游请求尝试次数独立统计：默认每个模型调用最多尝试 3 次，仅重试 HTTP 429、502、503、504，使用从 250ms 起的指数退避与抖动，尊重 `Retry-After`。首次获得并发槽位后，重试、退避和再次排队共享原模型调用期限；退避时释放槽位。取消、鉴权及参数错误、流解析或回调失败、已经开始的 SSE 流均不重试。分类与摘要也通过网关，受到各自更短的期限约束。

参数上限 128 KiB，原始工具观察上限 256 KiB，工具期限 65 秒，总任务期限默认 2 分钟，可用 `-timeout` 调整。工具仍需配合 context。每个个人 CLI 调用最多执行两次固定 Go 测试，持久运行的工具总预算在恢复时继续累计。

## 执行追踪与用量

启用遥测后，CLI 退出或切换会话时刷新并关闭本次 exporter。每次执行或恢复建立 `agent.task`，分类、模型调用与工具执行共享父追踪；恢复使用新的 attempt ID，通过稳定 run ID 与旧执行关联。

会话 JSONL 新增 `model_attempt_started/completed`、`model_retry`、`route_selected` 与 `run_statistics` 元数据事件。最终统计从持久日志恢复，覆盖此前失败的尝试及分类、摘要调用。仅累计供应商实际返回的 Token 用量，`unknown_usage_attempts` 大于零表示统计不完整，不能用零代替费用。旧日志没有模型尝试记录时保留原有累计值。统计读取上限为单会话 256 MiB，超过时明确报错。

本地会话记录用于恢复，包含原始输入与工具结果；独立的 OTel 文件或 OTLP 只包含 ID、状态、耗时、模型名及用量等元数据，不输出提示词、参数、结果或凭据。

## 验证

```bash
go test ./...
go test -race ./internal/harness ./internal/personal ./internal/workspace ./internal/extension ./internal/interview ./cmd/harness
PRACTICE_DOCKER_TEST=1 go test ./cmd/harness -run '^TestPersonalExerciseDockerPath$' -v -count=1
```

覆盖版本冲突、路径逃逸、Skills 元数据/正文分离、真实 SDK 内存连接的 MCP 白名单与参数校验、模型摘要降级、持久结果不重放、不确定执行人工核对，以及面试追问/续接。CLI 集成使用本机模型替身；Docker 用真实镜像验证代码哈希与测试结果。

启用 `configs/interview` 时自动提供内置面试 Skill 和状态工具。用户可在提问、讲解、检索和面试之间自然切换；进度由工具保存，不依赖模型记住题号。恢复仍需原配置与工具权限，详细行为见[统一面试训练](knowledge-and-interview.md)。

## 启动菜单

菜单支持新建、按序号选择历史会话、按标题/目录/ID 搜索、十条一页翻页，以及编辑启动设置。工作目录与资源配置分开选择，权限开关在启动前显示；MCP 只记配置文件路径，Token 继续从原环境读取。历史中的“未完成”会话使用保存的启动参数恢复，“需核对”先查操作回执；查不到时保留 pending，不自动重做。

终端无参数启动自动显示菜单，`-menu` 可显式开启。已有脚本与管道输入不自动弹菜单；命令参数继续有效。菜单不会部署 MCP、创建知识索引或替用户填写凭据；选择的资源仍需已配置可用。
