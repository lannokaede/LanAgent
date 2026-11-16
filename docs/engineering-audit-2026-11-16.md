# 工程性与 GitHub 交付审查

审查日期：2026-11-16（America/Los_Angeles）。起始版本：`7f985b2bc92c8c18aac834eaf9de739701625f0b`。范围包括执行核心、会话恢复、服务与存储、构建与依赖、Git 内容及远端状态。个人简历不在修改范围。

项目已有明确的模块职责、资源所有权、有界并发、持久化恢复和共享 CI 检查。本次发现五组具体问题并实施修复；单元测试通过与缺陷审查分别记录，避免用测试绿灯替代行为判断。

## 确认的问题与修复

| 问题 | 原行为及影响 | 修复与验证 |
| --- | --- | --- |
| P2：会话替换缺少目录持久化 | 只同步临时文件再 Rename；主机掉电窗口中，目录项尚未提交可能丢失新的检查点 | Unix 上增加目录同步并传递失败；故障注入验证执行标记保存失败时工具不执行、已完成答案重启后不再调用模型 |
| P2：非流式接受不完整终态 | `length` / `content_filter` 可作为成功回答或工具调用返回，与 SSE 约束不一致 | 拒绝截断/拦截及未知终态，保留空终态兼容；本机 HTTP → Gateway → Harness 验证只请求一次、残余工具不执行 |
| P2：研究 worker 存储失败忙循环 | queued → running 保存失败后立即重选同一任务，持续磁盘故障导致热循环 | 区分正常取消与持久化失败；故障时停止 worker，通过 job_status 暴露错误，保留任务供修复存储后重启；新测试在旧代码复现失败，修复后恢复仅入库一次 |
| P2：私有产物忽略规则不随仓库传播 | `out/` 仅在本地 Git exclude，Docker `COPY . .` 也未排除它 | 版本化 Git/Docker 排除规则；共享检查拒绝强制暂存私有路径；新仓库负向测试与 Docker 构建上下文检查 |
| 依赖安全补丁落后 | Go 扫描报告 10 个去重 OSV，其中 8 个有调用路径；演示工具 npm 审计报告 2 个高危依赖项，来自同一公告 | 最小补丁组升级至 Go 1.26.11、OTel 1.45.0、x/net 0.62.0、Playwright 1.55.1；复扫与兼容性验证见下表 |

代码证据：[`personal/durability_test.go`](../internal/personal/durability_test.go)、[`llm/nonstream_termination_test.go`](../internal/llm/nonstream_termination_test.go)、[`research/worker_checkpoint_test.go`](../internal/research/worker_checkpoint_test.go)、[`check-private-files.sh`](../scripts/check-private-files.sh)。

目录同步改善 Unix 平台的掉电持久化前提，具体保证仍取决于文件系统与存储。Windows 保留文件数据同步与现有替换流程，不额外承诺 Unix 目录 fsync 语义；新增三项持久化行为测试已在 Windows 原生执行。

Playwright 公告为 [GHSA-7mvr-c777-76hp](https://github.com/advisories/GHSA-7mvr-c777-76hp)，修复版本为 1.55.1。该工具用于生成演示图片，服务端 Go 二进制不依赖 Node。OTel 1.45 的 URL 路径行为有兼容变化，[官方发布记录](https://github.com/open-telemetry/opentelemetry-go/releases/tag/v1.45.0)说明了这一调整；项目保留无路径地址使用 `/v1/traces` 的配置行为，显式路径原样使用，并以真实 HTTP exporter 验证。

## 工程验证结果

| 检查 | 结果与范围 |
| --- | --- |
| `./scripts/check.sh` | 私有路径、gofmt、依赖漂移、全仓普通测试、race、vet、全部命令编译通过 |
| 离线集成与语料 | 15 个包，217 个测试通过（含子测试）、1 项 Docker 开关测试跳过；17 份文档 / 1,206 片段的已校验快照保持一致 |
| Go 依赖扫描 | govulncheck v1.9.0、Go 1.26.11；最终无 finding。扫描数据库时间为 2026-11-11T02:51:11Z |
| npm 依赖扫描 | 升级后的锁定依赖审计结果为 0 项漏洞，包含开发依赖 |
| 演示工具 | 修补后的 Playwright 配合本机已有 Chromium，四个真实 CLI/固定服务替身场景与截图验证通过；仓库原 PNG 保留 |
| Windows 持久化 | 交叉编译后通过 WSL interop 实际执行新增三项 Windows 原生行为测试 |
| Docker 上下文 | BuildKit tar 导出共 301 个条目，未包含 out、.runtime、node_modules、.git 或本地配置路径 |
| Docker 新镜像 | 两个 Go 1.26.11 镜像构建成功；新练习镜像真实题目、超时/截断及 CLI→Docker 路径通过；研究容器 UID/GID=65532、只读 rootfs、可写 data tmpfs，6 项真实 HTTP 检查和正常 stop 退出 0 |
| 凭据匹配扫描 | 当前本地可达历史中检查 300 个文本 blob，与 2 个已知配置凭据值比较，无匹配；此结果限于该扫描范围 |

初次 govuln 工具下载遇到网络 EOF，重试后完成。Docker 目录导出因宿主 `lchownat` 权限失败，改用 tar 导出验证通过。演示首次编译发生在升级文件仍变动时，稳定后完整重跑通过。研究容器前两轮身份探针分别因缺少 PID 列和复制 tmpfs 的环境限制失败，修正探针后实际身份、HTTP 与退出验证通过。原始失败与最终结果分别保留在 `.runtime/engineering-audit-20261116/`。

本次重新验证本地协议、并发、持久化与交付行为；外部模型、Embedding 和 Milvus 的历史真实验收保留原日期，见[服务端验收](service-engineering-validation.md)和[Harness 验收](harness-validation.md)。

## GitHub 推送规范核对

| 项目 | 审查结论 |
| --- | --- |
| 本地与远端 | 起始时 HEAD、origin/main 与 GitHub API 均为 `7f985b2`，ahead/behind 为 0/0；工作区无已跟踪修改 |
| 已发布内容 | 原 234 个跟踪路径中没有 out、.runtime、本地配置、数据库、PDF 或日志等私有类文件 |
| 仓库完整性 | `git fsck --full` 通过；存在未引用 tree 对象，未发现损坏对象，不改写或清理历史 |
| 提交与身份 | 最近提交围绕对应功能/重构目标，当前提交标题使用 Conventional 前缀；作者归属一致，不同邮箱和未签名不单独构成规范失败 |
| CI 权限与结果 | 三个 workflow 均为 `contents: read`；起始提交的 Go checks 在 push 事件完成且成功 |
| 分支治理 | main 的 protected=false，规则集为空；CI 是推送后验证，当前未配置 PR/required-check 合并门禁 |
| 提交流程 | 新增轻量 PR 模板、单目的提交、依赖锁、私有路径检查、fast-forward 推送与推后 SHA/CI 核对约定 |
| 来源与许可 | NOTICE 已记录来源与许可限制，尚无项目统一 LICENSE；许可选择仍需作者结合来源明确，本轮不重新授权第三方内容 |

个人仓库的直接推送可按已授权流程执行完整本地检查和远端核对。后续多人协作时，建议对 main 设置 PR 与必要 CI 检查、禁止强制覆盖/删除；这是当前仍未启用的协作门禁。

## 复现

```bash
./scripts/check.sh
./scripts/verify-harness.sh -mode offline

go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
npm ci
npm audit

# 镜像内 Go 补丁版本与默认配置保持一致
docker build -t lanagent/practice-go:1.26.11-v1 deployments/practice
docker build -t lanagent/research-mcp:local -f deployments/research/Dockerfile .
```

修改 Go/Node 依赖时同步提交锁文件。仅提交本次实现、测试、检查与说明，保留个人配置与运行数据；最终交付核对本地/远端提交 SHA 和该提交对应的 CI。

修复按检查点持久化、非流式终态、研究 worker、依赖安全补丁、私有产物/提交流程和审查说明分别提交。整个提交链在完整检查后以 fast-forward 更新主分支，保留既有历史。
