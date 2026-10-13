# Sub2API 私有仓库交接说明

> 审计日期：2026-10-13（Asia/Shanghai）
>
> 这份文档给下一台电脑和后续维护者使用。代码以私有 GitHub 仓库为准，线上运行状态以 GitHub Actions 发布记录和服务器只读检查为准。

## 先看结论

- 私有仓库：`https://github.com/ligerry1999-png/sub2api-private.git`
- 当前私有 `main`：`8a0ba6d48188a1003b08eb583c15e344905beec4`
- 当前线上版本：`0.2.14-private.1`
- 当前官方上游：`v0.2.16`，上游 `main` 为 `4d9ac3e482106d2da3a39fe3e546d8d6801f1cc1`
- 因此，私有仓库当前比官方上游落后 `v0.2.15`、`v0.2.16` 及之后的主分支修复；本次盘点没有合并，也没有部署这些更新。
- 最近一次正式部署已成功：GitHub Actions `Deploy Server` run `37775820800`。
- 最近一次定时安全扫描失败：run `38234055170`。根因是私有仓库仍使用 Go `1.27.0`，扫描发现 Go 标准库漏洞，修复版本为 Go `1.27.2`。这不是一次线上宕机，也没有触发部署，但下一次升级前必须处理。

## 私有功能清单

下面这些能力是当前私有部署必须保留的功能。它们不是官方上游 `v0.2.16` 的完整组成部分，或者在私有仓库中有专门的部署和行为改造。

### 1. 持久异步生图队列

接口：

- `POST /v1/image-jobs/images/generations`
- `POST /v1/image-jobs/images/edits`
- `GET /v1/image-jobs/:job_id`
- `GET /v1/image-jobs/:job_id/result`
- `POST /v1/image-jobs/:job_id/cancel`

作用：请求先落盘并返回 `202 + job_id`，后台再调用上游生图。客户端断线或 Sub2API 重启后，任务不会因为 HTTP 连接结束而直接丢失。

实现位置：

- `backend/internal/handler/openai_image_jobs.go`
- `backend/internal/handler/openai_image_job_dispatcher.go`
- `backend/internal/repository/openai_image_job_queue.go`
- `backend/internal/service/openai_image_job_queue.go`
- `docs/PRIVATE_IMAGE_JOB_QUEUE_CN.md`

默认限制：最多 2,000 个未完成任务、请求正文总计 5 GiB、调度器最多 64 个工作线程。实际同时调用上游的数量仍受用户并发数和生图账号并发数限制。

队列采用“至少执行一次”语义：极少数情况下，上游已经收到请求但本地还没来得及保存结果就断电，任务可能重新执行。任务会带固定幂等键以降低重复概率，但不能保证所有上游都绝对去重。

### 2. 全渠道图片日志和后台查看

私有版把 OpenAI Images、Responses 图片工具、Grok 图片入口和异步任务的最终结果统一记录到 `image_logs`，并在管理后台提供图片日志列表、缩略图和原图查看。

实现位置：

- `backend/migrations/136_image_logs.sql`
- `backend/internal/service/image_log.go`
- `backend/internal/service/image_log_cleanup.go`
- `backend/internal/handler/admin/image_log_handler.go`
- `frontend/src/views/admin/ImageLogsView.vue`
- `frontend/src/api/admin/imageLogs.ts`

图片日志还记录安全的请求摘要、请求哈希、上游结果元数据、真实 MIME、尺寸和 Alpha 信息，不保存完整 Key、Cookie、Authorization 或完整提示词。

默认保留期是 7 天，清理任务只删除终态记录；正在排队、执行中或状态未知的任务必须保留。

### 3. ChatGPT2API 图片 worker 桥接和回退

私有版可以把符合条件的 OpenAI OAuth 图片请求转发给同一 Docker 网络里的 `chatgpt2api` worker；worker 失败时可以回到 Sub2API 原生图片链路。

实现位置：

- `backend/internal/service/openai_images_worker_bridge.go`
- `deploy/tanzhongyu/docker-compose.yml`
- `deploy/tanzhongyu/.env.example`

生产环境默认设置为 `GATEWAY_IMAGE_WORKER_ENABLED=false`，所以当前线上主要走原生链路。不要因为仓库里存在 worker 配置，就默认认为线上正在使用 worker。

旁边的独立项目是：`/Users/bricoleur/Documents/小红书 cil/chatgpt2api`。它不是 Sub2API 私有仓库的一部分；如果以后开启 worker，需要把它作为单独仓库重新克隆和部署配置。

### 4. `file_url` 图片结果交付

图片请求可以使用 `result_delivery=file_url`，把结果返回为 `/image-files/...` 文件地址；没有传这个参数时，默认 Base64 行为必须保留。

实现位置：

- `backend/internal/handler/openai_image_jobs.go`
- `backend/internal/service/openai_images_delivery_test.go`
- `backend/internal/server/routes/gateway.go`
- `deploy/tanzhongyu/nginx-sub2api-prod.conf`

当前结果文件落在服务器本地数据目录，不是自动上传到对象存储。异步任务结果默认保留 2 天，ChatGPT2API 图片副本默认保留 3 天。

### 5. 受限账号管理员角色

`account_manager` 是私有版增加的受限后台角色。它可以管理账号和代理相关内容，但不能访问完整管理员功能，例如图片日志和用户管理等受保护页面。

实现位置：

- `backend/internal/domain/constants.go`
- `backend/internal/server/middleware/admin_role_access.go`
- `backend/internal/service/roles.go`
- `frontend/src/router/index.ts`

上游也有很多账号、团队和工作区能力，但不能据此判断 `account_manager` 权限边界已经被官方完整替代；迁移时必须保留私有权限测试。

### 6. GitHub 构建、服务器只加载镜像的部署护栏

生产发布不是在服务器上编译源码，而是：

1. GitHub Runner 拉取指定提交。
2. GitHub Runner 执行 `docker build` 并检查版本和提交 SHA。
3. GitHub Runner 通过 SSH/SCP 发送已构建镜像。
4. 服务器执行 `docker load` 和 `docker compose ... --no-build`。
5. 发布前备份 PostgreSQL、暂停图片队列领取、等待正在执行的任务结束。
6. 新容器健康后恢复队列，并做路由探针和 Nginx 配置检查。
7. 失败时恢复旧镜像。

实现位置：

- `.github/workflows/deploy-server.yml`
- `.github/workflows/database-rehearsal.yml`
- `deploy/database-rehearsal.sh`
- `deploy/tanzhongyu/`
- `scripts/check-private-upgrade-guards.sh`

服务器不能执行 `docker build`、`go build` 或前端构建。这个约束是为了避免编译过程把生产服务器的 CPU、内存和磁盘占满。

## 和官方仓库的当前差异

### 官方已经有、私有版也要继续保留的能力

团队/工作区字段、Codex Responses、模型目录、图片计费、Grok 图片和大量协议兼容代码，官方后续也持续发展。它们不能简单标记为“全是私有功能”。升级时要做逐文件合并，保留官方新行为和私有生图/部署行为的并集。

### 当前明确只在私有仓库中存在的路径

截至本次审计，官方 `upstream/main` 不包含以下私有路径：

- `backend/internal/handler/openai_image_jobs.go`
- `backend/internal/handler/openai_image_job_dispatcher.go`
- `backend/internal/service/image_log.go`
- `backend/internal/service/openai_images_worker_bridge.go`
- `backend/internal/service/openai_images_trace.go`
- `frontend/src/views/admin/ImageLogsView.vue`
- `backend/migrations/136_image_logs.sql`
- `deploy/tanzhongyu/`
- `.github/workflows/database-rehearsal.yml`
- `.github/workflows/deploy-server.yml`
- `docs/PRIVATE_IMAGE_JOB_QUEUE_CN.md`
- `scripts/check-private-upgrade-guards.sh`

这份路径清单不是“全部差异文件清单”，而是下一次升级最容易被误删、最需要人工复核的私有核心路径。

## 下一台电脑的接手步骤

### 现在就做

在新电脑上只需要从私有仓库重新克隆：

```bash
git clone https://github.com/ligerry1999-png/sub2api-private.git
cd sub2api-private
git checkout main
git fetch origin --prune
git status --short --branch
git rev-parse HEAD
git ls-remote origin refs/heads/main
```

确认本地 HEAD 和远程 `main` 都是 `8a0ba6d48188a1003b08eb583c15e344905beec4` 后，再开始工作。

官方上游只用于比较：

```bash
git remote add upstream https://github.com/Wei-Shaw/sub2api.git
git fetch upstream --tags --prune
```

不要把 `upstream/main` 直接覆盖私有 `main`，不要在没有升级计划和数据库演练的情况下直接合并 `v0.2.15` 或 `v0.2.16`。

### 需要单独迁移、但不能提交到 GitHub 的东西

- GitHub Actions Secrets：`DEPLOY_HOST`、`DEPLOY_USER`、`DEPLOY_PORT`、`DEPLOY_SSH_KEY`、`CHATGPT2API_INTERNAL_BRIDGE_KEY`。
- 生产服务器上的 `.env`、PostgreSQL 数据、Redis 数据、图片目录、数据库备份和运行态文件。
- 新电脑本地的 API Key。当前这台 Mac 用的是 macOS 钥匙串，不是仓库文件。
- SSH 私钥，例如 `~/.ssh/chatgpt2api_38_246_232_202`。只能通过安全渠道迁移，不能复制到仓库或聊天记录。

仓库里的 `.env.example` 只有示例配置，不包含生产密钥。新电脑要使用本机生图辅助脚本时，需要重新配置本机钥匙串，而不是把 Key 写入脚本。

### 可以重新生成、通常不需要搬运的东西

- `frontend/node_modules/`：当前约 772 MB，重新执行依赖安装即可。
- `frontend/tsconfig*.tsbuildinfo`、`frontend/vite.config.js`、`frontend/vite.config.d.ts`。
- `backend/internal/web/dist/` 等构建产物。
- GitHub Actions Runner 临时导出的镜像包和临时日志。

这些路径已被 Git 忽略或由构建重新生成，不应该作为迁移依赖。

## 当前旧电脑的清理建议

### 先保留

在新电脑完成克隆、分支检查、文档读取和一次本地工作流验证之前，先保留：

- `/Users/bricoleur/Documents/小红书 cil/sub2api-private`
- `/Users/bricoleur/Documents/小红书 cil/chatgpt2api`
- `/Users/bricoleur/Documents/小红书 cil/SUB2API_SERVER_HANDOFF.md`
- `/Users/bricoleur/Documents/小红书 cil-archive-20260926/`

### 新电脑确认后再归档或删除

`小红书 cil-archive-20260926` 约 3.7 GB，包含旧仓库快照、补丁、Git bundle 和测试输出。它是历史恢复材料，不是当前代码真源。确认新电脑能从 GitHub 克隆并正常读取本文件后，可以把它移到外置硬盘或压缩归档，再删除本机副本。

旧归档里可能包含历史运行输出和敏感配置痕迹，迁移时按“敏感备份”处理，不要上传到公开仓库或聊天工具。

### 不要清理

- 不要删除生产服务器上的数据库、Redis、图片目录、`.env` 或备份。
- 不要删除 macOS 钥匙串里的 Sub2API Key，除非已经在新电脑安全保存并验证。
- 不要删除 SSH 私钥，除非已完成安全迁移并确认不再需要旧电脑连接生产服务器。
- 不要删除私有 GitHub 仓库的 Actions Secrets。

## 后续升级建议

1. 先处理 Go `1.27.0` 到 `1.27.2` 的安全扫描问题。
2. 再单独建立 `v0.2.15` / `v0.2.16` 私有升级分支。
3. 逐项复核本文件列出的私有核心路径和数据库迁移。
4. 让同一个精确提交通过 CI、安全扫描、数据库演练。
5. 只有明确需要上线时，才用 GitHub Actions `deploy=true`；服务器继续只接收预构建镜像。

当前不建议做的事：直接把官方 `upstream/main` 强行合并到私有 `main`、在服务器上本地构建、把旧归档当作当前代码真源、或把生产 Key 写入新电脑的普通文件。
