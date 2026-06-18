# 私有版升级验收清单

这份清单用于每次合并官方 Sub2API 更新、改部署脚本、改镜像构建方式之后做验收。目标是避免官方升级把我们自己的私有功能覆盖掉。

## 当前部署模型

- 当前线上运行的是我们私有仓库构建出来的 Docker 镜像：`sub2api:private-prod`。
- 它不是官方原版镜像。镜像只是程序打包形式，私有镜像里包含我们自己改过的代码。
- 构建发生在 GitHub Actions；服务器只负责 `docker load` 和 `docker compose up`，避免 2核4G 服务器本地构建时 CPU/内存过载。
- ChatGPT2API 生图桥接通过内网服务名调用：`http://chatgpt2api:80/internal/v1`。

## 私有功能清单

每次升级后必须确认这些功能还存在：

- OpenAI 图片接口优先走 ChatGPT2API 生图桥接，失败时允许回落到 Sub2API 原生通道。
- 异步生图任务路由存在：
  - `POST /v1/image-jobs/images/generations`
  - `POST /v1/image-jobs/images/edits`
  - `GET /v1/image-jobs/{job_id}`
  - `GET /v1/image-jobs/{job_id}/result`
- 生图日志后台页面可打开，能看到提示词、用户、密钥、账号、耗时和缩略图。
- 生图日志图片文件按 URL 按需加载，不把大图直接塞进列表接口。
- 生图日志清理策略保留：生产默认保留 15 天，定时清理过期数据库记录和本地图片文件。
- 异步 image-jobs 结果默认保留 2 天，ChatGPT2API 底层图片副本默认保留 3 天。
- 账号管理页能显示 OpenAI 工作区/账号空间信息，例如 `account_name`、`workspace_name`。
- 账号经理角色只能管理账号，不能访问密钥、生图日志等不该看的后台模块。
- GitHub Actions 的 Deploy Server workflow 默认手动触发，且默认只构建不部署。

## 代码合并前

- 对比官方更新时，重点检查 `backend/internal/server/routes/gateway.go`，确认私有 image-jobs 路由没有丢。
- 检查 `backend/internal/handler/openai_image_jobs.go` 是否还在，且 handler 能被路由注册。
- 检查 `deploy/tanzhongyu/docker-compose.yml` 是否还包含 ChatGPT2API worker 和 image logs 相关环境变量。
- 检查 `.github/workflows/deploy-server.yml` 是否仍然：
  - 在 GitHub Actions 构建 Docker 镜像。
  - 上传镜像包到服务器。
  - 在服务器执行 `docker load`。
  - 部署后探测私有路由。
- 不要把部署方式退回服务器本地 `docker build`。

## 部署前

- 先跑 CI 和 Security Scan。
- 先跑 Deploy Server 的 build-only 模式，也就是 `deploy=false`。
- 部署前检查服务器磁盘空间，系统盘剩余空间建议大于 `8G`。
- 如果磁盘占用超过 80%，优先清理 Docker BuildKit 缓存和旧系统日志，不要先碰业务数据。

## 部署后

- `/health` 必须通过。
- 私有 image-jobs 路由探针必须不是 `404` 或 `000`。没带密钥返回 `401` 是正常的，说明路由存在且鉴权生效。
- `docker ps` 里 `sub2api-private` 必须是 `healthy`。
- 确认线上镜像名是 `sub2api:private-prod`。
- 测试至少一次文字模型调用、一次普通生图、一次异步生图提交和轮询。
- 打开后台生图日志页，确认最新图片能显示缩略图，点击可打开原图。

## 磁盘巡检

重点看这些目录：

- `/opt/sub2api-src/deploy/tanzhongyu/data/image_logs`：Sub2API 生图日志图片，包含原图和缩略图。
- `/opt/sub2api-src/deploy/tanzhongyu/data/image_jobs`：异步生图任务结果，`result.json` 可能包含图片数据，增长会很快。
- `/opt/chatgpt2api/data/images`：ChatGPT2API 自己保存的图片文件。
- `/opt/sub2api-src/deploy/tanzhongyu/postgres_data`：Sub2API 数据库。
- `/var/lib/docker`：Docker 镜像和容器层。
- `/var/log`：系统日志。

## 当前磁盘快照

2026-06-01 巡检结果：

- 系统盘：`68G` 总容量，已用约 `33G`，可用约 `36G`，占用约 `48%`。
- `/opt/sub2api-src`：约 `19G`，是当前最大目录。
- Sub2API 生图日志：`/opt/sub2api-src/deploy/tanzhongyu/data/image_logs` 约 `12G`，约 `8188` 个文件，包含原图和缩略图。
- Sub2API 异步任务结果：`/opt/sub2api-src/deploy/tanzhongyu/data/image_jobs` 约 `6.4G`，约 `19243` 个任务目录，`result.json` 可能包含图片响应体。
- ChatGPT2API 图片文件：`/opt/chatgpt2api/data/images` 约 `6.4G`，约 `2610` 个图片文件。
- Sub2API 数据库：`postgres_data` 约 `377M`，数据库本身不是磁盘占用大头。
- Sub2API 应用日志：约 `103M`。
- 系统日志：`/var/log` 约 `277M`。
- Docker 镜像：约 `4.4G`，其中未被当前容器使用的镜像约 `2.7G` 可回收。
- Docker BuildKit 构建缓存：`0B`，已经清理干净。

## 可安全清理

通常可以优先清：

- Docker BuildKit 构建缓存。
- 不再使用的旧 Docker 镜像。
- 过大的系统 journal 日志。
- apt 包缓存。

## 不要随便清理

没有备份和明确策略时，不要直接删除：

- Postgres 数据目录。
- Redis 数据目录。
- 账号池和密钥相关数据。
- 当前运行镜像。
- 近 15 天生图日志。
- 近 2 天异步 image-jobs 结果。

## 回滚原则

- 服务器保留上一版 `/opt/sub2api-release.prev`，必要时优先回滚代码包和镜像。
- 回滚前先确认数据库 schema 没有做不可逆升级。
- 如果只是前端或路由丢失，优先重新部署上一版私有镜像，不要重装服务器。
