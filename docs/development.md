# 开发与交付约束

本地提交前与 GitHub Actions 使用同一检查入口：

```bash
./scripts/check.sh
```

需要仓库声明的 Go 工具链、Git 和 Bash；race 检查需要对应平台支持的 C 编译器。脚本会从仓库根目录执行，任何检查失败即返回非零退出码。

| 检查 | 约束与结果 |
| --- | --- |
| `scripts/check-private-files.sh` | 只检查 Git 索引中的文件路径，拒绝私有目录、本地凭据配置和业务数据库进入提交 |
| `gofmt -l` | 检查已跟踪及未跟踪的 Go 源文件，跳过被忽略的运行数据；仅报告，不自动改源码 |
| `go mod tidy -diff` | 检测依赖清单漂移，输出所需差异；不改 `go.mod`、`go.sum` |
| `go test -mod=readonly ./...` | 验证协议、状态、失败路径和架构依赖，依赖不完整时直接失败 |
| `go test -mod=readonly -race ./...` | 检查测试实际执行路径中的数据竞争 |
| `go vet -mod=readonly ./...` | 执行 Go 静态检查 |
| `go build -mod=readonly -trimpath -o TEMP_DIR ./cmd/...` | 编译全部命令，临时二进制在结束时清理 |

修改依赖时显式运行 `go mod tidy`，确认 `go.mod`、`go.sum` 的差异后一起提交。检查入口仍可能下载依赖到 Go 模块缓存，源码与依赖清单保持只读。

开发过程可以先运行针对性测试，或省略耗时的 race 检查：

```bash
go test ./internal/harness -run TestResume
./scripts/check.sh --quick
```

`--quick` 用于本地迭代。CI 固定执行默认全量入口；提交前也应跑完整检查。默认测试不依赖个人模型凭据、在线 Milvus 或 Docker；需要这些依赖的验收另外执行并保留实际结果。

## 提交与推送

- 一个提交围绕一个明确目的组织，实现、相关测试和说明可以一起提交。新提交使用 `feat:`、`fix:`、`refactor:`、`docs:`、`test:`、`build:`、`ci:` 或 `chore:` 加具体摘要，描述实际结果；历史提交保留原样。
- 修改 Go 依赖时一并提交 `go.mod` / `go.sum`；修改演示工具的 npm 依赖时一并提交 `package.json` / `package-lock.json`，通过 `npm ci` 安装锁定版本。
- 提交前核对 `git status --short`、`git diff --cached --stat` 和暂存差异，运行完整 `./scripts/check.sh`；真实模型、向量库与 Docker 场景按变更范围另外验收，记录实际执行或跳过的情况。
- 功能分支可通过 PR 记录问题、行为与验证；直接更新主分支时也执行相同检查。推送前 fetch 并检查本地与远端差异，使用普通 fast-forward 推送；远端已变化时先核对并整合，不能用强制覆盖代替处理分歧。
- 推送后再次 fetch，核对本地提交与远端分支的 SHA，并查看该提交对应的 GitHub Actions 结果。上传成功与 CI 通过分别确认；构建附件、Release 和部署各自遵循对应流程。

`out/` 中的个人简历、工作笔记以及 `.runtime/`、`.lanagent/state/` 运行目录通过仓库规则排除；Docker 构建也排除这些目录和 `node_modules` 等开发产物。路径检查同时拒绝强制加入索引的 `.env`、`.env.*`、`*.local.json`、`*.local.yaml`、`*.local.yml` 以及数据库和 SQLite sidecar 文件。

安全配置示例使用 `.env.example`、`config.example.yaml` 或 `config.local.example.yaml` 等显式名称。固定数据库测试夹具可放在 `testdata/` 或 `fixtures/`；此例外不覆盖私有目录或本地配置命名。检查只读路径，不读取文件内容；示例和夹具仍应保持固定、无真实凭据。规则误报时先核对文件职责，不能以 `git add -f` 绕过检查。

## 修改实现时的约束

- **职责与依赖**：CLI 负责输入输出，application 负责装配与资源所有权，Harness 通过 Provider 和工具注册表执行。核心包直接依赖由 `internal/architecture` 检查。新增依赖应同时说明谁持有状态、谁关闭资源，以及失败由谁处理。
- **并发与取消**：限制在途工作及等待队列，阻塞操作传递 context。后台 worker 有停止与等待流程；调用方先等待工作退出，再关闭其连接和数据库。测试应验证取消、容量耗尽、恢复与重复执行等实际行为。
- **错误与副作用**：调用重试遵守网关定义的可重试错误类型与总期限；工具副作用不使用模型重试机制。不确定结果保留待核对状态，通过原执行收据恢复。
- **持久化**：事务事实、执行检查点、本地轨迹和向量候选各自拥有明确职责。改字段或提交顺序时，覆盖旧数据兼容、部分完成和失败后的再进入路径。
- **验证与数据**：新增功能的测试跨公开接口验证行为。服务地址、密钥、个人会话、语料正文及临时验收集合不提交到仓库；示例配置只保留占位符。

模块接口、状态顺序和失败处理的完整约定见[执行架构](architecture.md)。修改这些约定时，同时更新实现、相关测试和文档，避免使用说明与执行行为分离。

## 真实服务验收

```bash
./scripts/verify-harness.sh -mode offline

ROUTER_MODEL=YOUR_SCHEMA_COMPATIBLE_MODEL \
  ./scripts/verify-harness.sh -mode all -config configs/interview \
  -milvus-url http://127.0.0.1:19530 \
  -report .runtime/harness-validation/acceptance.json
```

真实验收使用隔离工作目录、SQLite 和临时 Milvus 集合，报告区分通过、失败及未执行，原始失败记录与重跑结果分别保存。场景和环境要求见[验收记录](harness-validation.md)。离线测试、真实功能验收与并发容量评测分别报告结果。
