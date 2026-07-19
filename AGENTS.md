# Sub2API 私有仓库协作规则

## 先读什么

在检查、修改或部署前，依次读取：

1. 本文件。
2. 工作区交接文件：/Users/bricoleur/Documents/小红书 cil/SUB2API_SERVER_HANDOFF.md
3. 私有升级清单：tasks/PRIVATE_UPGRADE_CHECKLIST_CN.md
4. 涉及运维时再读：docs/PRIVATE_OPERATIONS_RUNBOOK_CN.md

## 代码真源

- 私有 GitHub 仓库 ligerry1999-png/sub2api-private 的 main 是唯一正式代码真源。
- 本地规范路径是 /Users/bricoleur/Documents/小红书 cil/sub2api-private。
- Wei-Shaw/sub2api 只用于比较和合并官方更新，不能直接覆盖私有仓库。
- /opt/sub2api-release、/opt/sub2api-src 及其他服务器目录都不是源码真源。

开始任务时必须同时取得本地 HEAD 和 GitHub 实时 main，不能只看可能过期的 origin/main：

~~~bash
git status --short
git branch --show-current
git rev-parse HEAD
git remote -v
git ls-remote origin refs/heads/main
~~~

需要更新本地远端引用时，先确认工作区状态，再执行：

~~~bash
git fetch --prune origin
git merge-base HEAD origin/main
git merge-base --is-ancestor HEAD origin/main
git merge-base --is-ancestor origin/main HEAD
~~~

如果 merge-base 不存在，说明历史被替换或重写：

- 立即停止普通 git pull、merge、rebase、reset 和强推。
- 不删除旧仓库，不把旧历史硬接到新 main。
- 将旧仓库改名归档，再从 GitHub main 克隆到规范路径。
- 核验新克隆的 HEAD、remote、工作区和关键私有功能后再继续。

## 默认只读

- 用户只要求检查、解释、诊断或汇报时，不修改本地、GitHub 或服务器。
- 没有明确授权时，不提交、不推送、不运行部署、不重启、不停账号、不压测。
- 服务器检查先只读；不要回显 OAuth token、API Key、数据库密码、SSH 私钥或完整环境变量。

生产服务器连接参数只从工作区交接文件读取。基础健康检查：

~~~bash
docker ps
docker stats --no-stream
curl -fsS http://127.0.0.1:18080/health
free -h
df -h /
~~~

读取容器配置时只输出必要白名单字段，例如镜像 ID、状态、启动时间、重启次数、健康状态和挂载；禁止直接打印完整 docker inspect 环境变量。

## 版本对齐

判断“本地、GitHub、线上是否对齐”至少核验：

1. 本地 HEAD 和工作区是否干净。
2. GitHub 实时 main SHA，而不是本地缓存的 origin/main。
3. GitHub 最后一个实际执行了 Deploy Sub2API 步骤的成功工作流及其 headSha。
4. 线上 /opt/sub2api-release/.release-commit。
5. 运行镜像的 org.opencontainers.image.revision 标签。
6. /app/sub2api --version 输出中的 commit。
7. 内外网健康、容器健康和重启次数。

在带提交溯源的工作流首次正式部署前，旧镜像可能显示 commit: docker，且没有 revision 标签。此时只能用部署记录、镜像时间和发布目录文件哈希交叉核验，并明确说明证据等级，不能声称镜像自证了 SHA。

## 修改与验证

- 保留 tasks/PRIVATE_UPGRADE_CHECKLIST_CN.md 列出的全部私有能力。
- 未传 result_delivery=file_url 时必须保持 Base64 兼容行为。
- 不把线上数据、账号、令牌、.env、数据库或图片目录提交到 Git。
- 优先运行与改动直接相关的轻量测试；生产服务器禁止运行任何源码构建。
- 最低自检：

~~~bash
git diff --check
git status --short
~~~

涉及 Dockerfile 或部署工作流时，还要核验版本文件、提交 SHA 注入、镜像 revision 标签、发布提交标记和失败回滚路径。

## 部署红线

- 正式构建只在 GitHub Actions 执行。
- 生产服务器只能接收构建产物、docker load 和容器切换；禁止运行 docker build、Go 构建或前端构建。
- 部署前必须确认图片任务空闲并检查 CPU、内存和磁盘。
- 先运行 deploy=false 的构建验证，再对同一提交人工选择 deploy=true。
- 健康检查、Nginx 校验或私有路由探针失败时必须回滚旧镜像。
- 不删除 Postgres、Redis、账号池、密钥、图片日志、异步任务数据、.env 或数据卷。

## 文档同步

以下事实变化时必须同步三层文档：

- Agent 操作规则：更新 AGENTS.md。
- 人类运维流程：更新 docs/PRIVATE_OPERATIONS_RUNBOOK_CN.md。
- 服务器入口、路径、现状或新任务启动语：更新工作区 SUB2API_SERVER_HANDOFF.md。

永久事实必须写绝对日期和精确提交，不能使用模糊时间词。
