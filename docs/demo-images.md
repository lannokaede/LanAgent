# GitHub 演示图维护指南

README 的四张演示图由真实 Go CLI 执行输出生成，再用 HTML 模板与 Playwright 排版截图。它们是可复现的离线流程演示，不是运行中的终端截图，也不代表真实模型能力。图片只保留命令和运行输出。场景说明、外部服务替身、公共参数省略、归一化和 JSON 字段节选均在 README 图片上下说明，不放进图片。

## 一键更新

推荐 Linux / WSL 或下面的 GitHub Actions。需要 Go（版本见 `go.mod`）、Node.js 24+、Chromium 和中文字体。初次下载依赖、浏览器可能需要联网；安装完成后，脚本只使用 Go 本地构建和回环地址服务，无需模型密钥、Milvus、PostgreSQL 或 Docker。

在仓库根目录运行：

```bash
npm ci
npx playwright install chromium
# Ubuntu / WSL 缺少浏览器系统依赖时：
# npx playwright install --with-deps chromium
# 缺少中文字体时：sudo apt-get install fonts-noto-cjk
go mod download
npm run demo:images
```

已安装其他 Chromium 时，可指定浏览器可执行文件：

```bash
CHROMIUM_PATH=/path/to/chromium npm run demo:images
```

脚本不会使用仓库中某位开发者的绝对浏览器路径。没有可用浏览器会明确失败，不能只跑场景却跳过出图。生成后逐张打开 `docs/images/demos/*.png`，检查中文、换行和 README 图文对应关系，再将新图、脚本及相关文档一起提交。

本地没有依赖缓存时，Go 构建仍可能下载模块；首次准备依赖并不属于“断网可运行”的保证。

## 目录与维护点

| 位置                               | 修改什么                                                  |
| ---------------------------------- | --------------------------------------------------------- |
| `scripts/demo-image-fixtures.mjs`  | 四个场景的标识、样例文件、固定答案、外部服务替身          |
| `scripts/generate-demo-images.mjs` | CLI 调用、行为断言、输出节选、归一化与截图生成            |
| `scripts/demo-terminal.html`       | 配色、字体、排版；所有内容本地渲染，无远程资源            |
| `docs/images/demos/`               | 提交到 Git 的四张 PNG，README 使用相对链接                |
| `.runtime/demo-images/`            | 本地报告、捕获输出、替身请求摘要、HTML 与暂存 PNG，不提交 |

修改输入后，要同时调整对应替身响应及断言。模型替身必须根据本轮实际工具观察或记忆输入回答；不能直接粘贴一整段假 CLI 日志。面试评分固定在样例中，参考回答仍来自项目代码，检索证据是固定样例，来源 URL 不表示本次联网抓取。

## 四个场景如何验证

1. **工具执行**：检查 JSONL 的工具结果确实包含新建示例文件的正文，最终回答包含其中的约束。
2. **跨会话记忆**：通过 `/remember` 保存与纠正，两个不同 session 各只有一轮用户输入；替身必须收到 SQLite 当前版本，第二个会话不能回答旧目标。确认召回材料未固化进会话历史。
3. **恢复**：工具成功后返回一次 503。检查 pending 位于 model 阶段且含工具观察；恢复后 JSONL 工具完成次数保持 1，最终会话提交且 pending 清理。这个场景没有演示未知副作用的人工核对。
4. **面试**：统一 Harness CLI 接收原始答案，调用本机 Embedding/Milvus/模型替身；校验实际保存的问答、分数和引用，确认程序参考答案，并通过 review_list / write_file 保存复盘 Markdown。固定评分不作为模型质量证明。

所有场景和四张图片的排版检查通过后，才将暂存图片复制到 README 图片目录。失败退出码非零，报告 `success: false`；原有已提交图片可能仍在，但不能视为本轮生成成功。

## 隔离、输出与故障排查

- 每次创建独立临时工作区、配置和状态库，显式传给 CLI；示例目录只有固定样例文件。
- 覆盖模型地址与假密钥、清除相关私有连接环境变量；外部替身仅绑定 `127.0.0.1` 随机端口。不读取个人 `config.local.yaml` 或日常会话数据库。
- 子进程有期限与输出上限，正常退出和失败都会关闭替身、浏览器并删除临时数据库。强制终止整个进程或系统断电可能留下操作系统临时文件。
- 对临时路径、SHA-256 标识和时间做一致归一化，JSON 记忆输出只展示 scope/key/content/version；不改变状态、事实内容和版本含义。
- 所有截图宽度 1440px，高度按内容生成；程序检查横向溢出并拒绝超过 2000px 的图，不静默裁掉失败信息。不同系统字体和 Chromium 版本可能产生像素差异，不要求图片逐字节相同。

失败先看 `.runtime/demo-images/report.json` 的 error/stack，再看 `transcripts.json` 和 `fixtures.json`。图片或中文缺字需要人工检查，不能仅靠 DOM 几何检测证明。连续执行两次应均报告四个场景成功。

## 在 GitHub Actions 中更新

提交这些文件到 GitHub 后，进入仓库 **Actions → Generate demo images → Run workflow**。工作流安装 Go、Node.js、Chromium 和中文字体，运行同一个生成命令并上传 artifact。

成功时下载 `lanagent-demo-images`，其中包含四张 PNG；报告 artifact 包含行为校验报告、标准化输出与 HTML。将 PNG 覆盖到本地 `docs/images/demos/`，检查后自行提交。工作流仅有 `contents: read` 权限，不会自动 commit 或 push，也不使用生产密钥。

工作流是否执行成功，以仓库 Actions 记录为准。
