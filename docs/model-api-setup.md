# 模型 API 接入

在仓库根目录操作。支持 OpenAI Compatible Chat Completions；个人 Harness 使用 SSE 和工具调用。

## 逐项填写

```bash
go run ./cmd/setup
```

填写 Base URL、API Key、模型名。Key 在真实终端中隐藏；已有值可回车保留。更换 Base URL 后需要重新输入 Key，避免把旧凭据发往新服务。模型名为空时尝试读取 `/models`，有多个模型时可选序号或直接填写名称。

Base URL 保留服务商原有前缀，例如 `/api/v1`。粘贴完整 `/chat/completions` 地址时会去掉末尾接口路径，避免重复拼接。

默认先发一次最多 64 输出 token 的 SSE 请求，成功再保存；失败不会覆盖之前保存的配置。该检查验证最小回答链路。仅保存可加 `-skip-check`。

## 粘贴整段配置

```bash
go run ./cmd/setup -import -
```

粘贴服务商给的内容，最后单独输入 `/save`，终端粘贴内容隐藏。支持这样的块：

```text
Base URL：
https://YOUR_HOST/api/v1
API Key：
YOUR_KEY
模型名：
YOUR_MODEL
/save
```

也支持 JSON、YAML、`.env` 文件：

```bash
go run ./cmd/setup -import /path/to/private-api.txt
# 服务没有 /models，或希望明确指定模型：
go run ./cmd/setup -import /path/to/private-api.txt -model YOUR_MODEL
```

无需把 Key 放进命令参数。

## 复用已有私有配置

```bash
go run ./cmd/setup -from-config /path/to/existing/config-directory
# 向量接口单独导入，不覆盖聊天接口
go run ./cmd/setup -kind embedding -from-config /path/to/existing/knowledge-config
```

读取的是目录中的 `config.yaml`、可选 `config.local.yaml` 及其环境变量覆盖值，仅复制选择的 API。原配置文件保持原样。

## 查看与检查

```bash
go run ./cmd/setup -show
go run ./cmd/setup -check
go run ./cmd/setup -kind embedding -check
```

`-show` 只显示 Key 是否配置，不显示实际内容。聊天检查发一次极小请求；向量检查生成一个测试向量，使用各自服务额度。

## 共享配置与覆盖关系

用户配置目录中的 `lanagent/api.local.json` 为本机共享配置，Linux 通常位于 `~/.config/lanagent/api.local.json`，权限 0600。它位于仓库之外，不随代码推送。设置一次后：

```bash
go run ./cmd/harness -workspace .
go run ./cmd/interview -topic concurrency -questions 3
go run ./cmd/knowledge search -config configs/interview -query 'Go context cancellation'
```

优先级从高到低：环境变量、当前项目的 `config.local.yaml`、用户共享 API、项目基础配置。如果修改共享值后运行时仍使用旧值，先检查环境变量或项目本地覆盖。

自动化测试可通过 `LANAGENT_API_CONFIG=/path/to/api.local.json` 指定独立文件；`setup -file PATH` 可以写入指定文件，其他入口需使用相同的 `LANAGENT_API_CONFIG` 才会读取它。

Embedding 当前适配 `text-embedding-v4`，维度默认为 1024。更换向量模型或维度后，已有集合身份检查会阻止混用，应使用匹配的新集合。
