# Kubernetes 学习与 LanAgent 部署调研

调研日期：2026-11-05。本文记录建议，不表示已经构建镜像、创建集群或完成部署。

## 定位与学习顺序

CNCF 2025 年调查于 2026-01-20 发布，报告称受访容器使用者中 82% 在生产使用 Kubernetes；Figure 8 的 2025 年有效样本为 351，限定最终用户组织并排除不确定答复。这支持其在云原生容器编排领域仍属主流，不能推广成所有企业或所有分布式系统的占比。来源：[官方发布](https://www.cncf.io/announcements/2026/01/20/kubernetes-established-as-the-de-facto-operating-system-for-ai-as-production-use-hits-82-in-2025-cncf-annual-cloud-native-survey/)、[完整报告](https://www.cncf.io/wp-content/uploads/2026/01/CNCF_Annual_Survey_Report_final.pdf)。

Kubernetes 管理容器工作负载的调度、服务发现、期望副本数和发布过程；应用的数据一致性、幂等性和多实例协调仍需应用设计。参见 [Kubernetes 概述](https://kubernetes.io/docs/concepts/overview/) 与 [Deployment](https://kubernetes.io/docs/concepts/workloads/controllers/deployment/)。

建议从容器镜像、端口、环境变量和卷开始，然后学习以下概念：

- Node / Pod：承载工作负载的节点与最小调度单元；Pod 重建后不保留原身份和临时文件。
- 声明式配置与控制器：提交期望状态，由控制器持续调谐。
- Deployment / Service：副本及发布管理，与访问副本的稳定入口。
- ConfigMap / Secret：普通配置与凭据注入；Secret 的 base64 编码不是加密，仍需限制权限及配置静态加密。
- PV / PVC / StorageClass：存储资源、存储申请与供应方式；卷的节点约束、回收策略和备份需要单独确认。
- startup / readiness / liveness：启动保护、是否接流量与是否需要重启。
- requests / limits、日志、事件、滚动发布与回滚：运行和排障的基础。
- Job / CronJob：一次性和定时批处理；任务仍应允许安全重试。

资料：[入门教程](https://kubernetes.io/docs/tutorials/kubernetes-basics/)、[kind 本地集群](https://kind.sigs.k8s.io/docs/user/quick-start/)、[存储](https://kubernetes.io/docs/concepts/storage/persistent-volumes/)、[探针](https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/)、[Secret 实践](https://kubernetes.io/docs/concepts/security/secrets-good-practices/)、[Job](https://kubernetes.io/docs/concepts/workloads/controllers/job/)。

## 仓库现状与部署边界

以下结论来自本仓库代码与文档：

| 组件 | 当前实现 | 建议 |
| --- | --- | --- |
| `cmd/harness` | 交互 CLI；本地会话、工作目录与 SQLite WAL 记忆 | 第一阶段留在本机，调用远程 MCP；云端交互服务另建 API |
| `cmd/research-mcp` | stdio 或 Streamable HTTP，已提供 Dockerfile | 第一阶段的 K8s 部署目标 |
| `cmd/knowledge` | 显式采集与入库命令 | 后续按需封装成 Job / CronJob，并协调写入 namespace |
| Milvus | 外部 REST 地址 | 第一阶段连接已有服务，避免同时学习数据库集群运维 |
| Go 练习沙箱 | `internal/practice/docker.go` 直接调用 Docker CLI | 本机保留；迁移时改执行器为受限 Job，并单独设计隔离 |

证据入口：`cmd/research-mcp/main.go`、`deployments/research/Dockerfile`、`internal/research/store.go`、`internal/cli/run.go`、`internal/personalmemory/store.go`、`internal/practice/docker.go`、`docs/research-mcp.md`。

## 第一版部署方案

本地 Harness → 端口转发或 TLS 入口 → ClusterIP Service → 单副本 research-mcp → `/data` 持久卷；启用 knowledge 时再连接已有 Milvus 和 Embedding 服务。

建议新增的资源为 Namespace、Secret、PVC、Deployment、Service；有普通配置时加入 ConfigMap。开始使用 kind 与 port-forward，之后再增加 Gateway/Ingress 和 TLS。Gateway/Ingress 需要相应控制器，不能只提交路由资源。参见 [Gateway API](https://kubernetes.io/docs/concepts/services-networking/gateway/)。

实施要点：

1. 复用现有 Dockerfile，从仓库根目录构建。远程集群推送镜像仓库；kind 可以直接加载本地镜像。镜像使用可追踪版本或 digest。
2. 保留 `-listen 0.0.0.0:18090 -allow-remote -data-dir /data`；HTTP Token 由 Secret 注入 `LANAGENT_RESEARCH_TOKEN`，至少 24 字符。官方模式不需要聊天或 Embedding Key；搜索或知识功能按需开启。
3. PVC 挂载 `/data`，保证 UID/GID 65532 可写；根据 CSI 驱动验证 `fsGroup` 的行为。kind 的本地卷只用于练习，不等于生产级跨节点存储，也不能保证删除集群后数据保留。
4. `replicas: 1`，Deployment 使用 `strategy.type: Recreate`。`internal/research/store.go` 对整个目录加进程锁，默认滚动更新可能导致新旧实例竞争目录。Recreate 会产生升级中断；PVC 访问模式本身不能代替应用互斥。StatefulSet 也不会自动使应用具备多副本能力。
5. 当前 HTTP handler 只提供带鉴权的 `/mcp`，没有 `/healthz`；不能配置不存在的 HTTP 探针。初期 TCP startup/readiness 可检查监听端口；正式部署建议新增独立健康接口。liveness 避免把短暂的外部 API 故障当成进程故障。现有入口处理 SIGTERM，仍需验证任务退出及重启恢复。
6. 配置 CPU/内存 requests、limits，非 root、禁止提权、按实际写路径收紧文件系统；读取日志和事件。需要抓取和模型调用的功能要保留必要出站访问。
7. 对公网开放时配置 TLS、Bearer Token、代理流式传输和超时；浏览器请求还需匹配 `-allowed-origins`。
8. 验收真实 MCP 初始化和工具调用、无 Token 拒绝、Pod 重建后的工件/任务恢复、版本更新、备份恢复。只看到 Running 不能证明功能部署成功。

## 后续需要的应用改造

如果目标是多人在线 Agent，应增加服务端 API、身份与用户数据隔离，持久化会话与运行状态，并将长期任务执行从请求生命周期中分离。多副本研究服务需要共享状态、工件存储、任务协调和 MCP 会话处理；不能只改 replicas。具体选型应在需求确定后决定，无需为了上 K8s 立即拆成多个微服务。

原有 Docker 沙箱依赖 Docker daemon；K8s 节点运行容器并不保证业务 Pod 能调用 Docker。不要为直接复用执行器而挂载宿主 Docker socket；K8s Job 替代方案仍需要非 root、资源/网络/权限约束，并根据执行代码的信任程度选择更强的隔离边界。
