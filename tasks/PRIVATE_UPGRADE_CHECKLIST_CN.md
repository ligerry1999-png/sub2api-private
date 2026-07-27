# Sub2API 私有版升级清单

## 唯一代码真源

- GitHub 私有仓库的 `main` 是正式代码真源。
- 本地开始工作时必须用 `git ls-remote origin refs/heads/main` 读取 GitHub 实时 SHA，不能只信缓存的 `origin/main`。
- 如果本地 HEAD 与 GitHub `main` 没有共同祖先，禁止普通 `pull`、`rebase`、`reset --hard` 或强推；归档旧目录并从 GitHub 重新克隆。
- 服务器只运行 GitHub Actions 构建出的镜像，不在服务器修改源码或编译。
- 每次升级从官方 release tag 建立干净分支，再逐项迁移私有功能；不要把服务器目录反向当成源码。
- Agent 操作规则见根目录 `AGENTS.md`，完整运维流程见 `docs/PRIVATE_OPERATIONS_RUNBOOK_CN.md`。

## 2026-07-27 升级基线

- 当前生产代码真源：私有 GitHub `main` 的 `e680938b6c8330eb7755b0354060c99ffe3a12a1`。
- 当前私有版本：`0.1.157-private.1`。
- 当前公开稳定目标：官方 `v0.1.165`，提交 `e9a58c1cb8b5ef626a75c93b4d953fde5e67aa29`。
- 官方 `main` 在稳定标签之后的未发布提交不进入本轮升级。
- 升级分支必须从官方稳定标签建立独立工作目录；当前维护工作目录中的未提交文件不得带入升级基线。

## 必须保留的私有功能

- OpenAI 账号列表显示 Team/Personal 空间信息。
- `account_manager` 受限账号管理员角色。
- `/api/v1/admin/image-logs` 生图日志、缩略图和 7 天清理策略。
- 旧客户端使用的 `/v1/image-jobs/*` 异步生图、失败分类和取消接口。
- `result_delivery=file_url` 公网图片 URL；未传参数时继续兼容 Base64。
- ChatGPT2API 图片 worker 桥接及失败分类；生产默认 `GATEWAY_IMAGE_WORKER_ENABLED=false`。
- 图片工具选择被上游拒绝时，窄范围重试 `tool_choice=auto`。
- Nginx 大图上传配置：100 MB、请求体 600 秒、编辑图请求先完整缓冲。
- 存储保留：生图日志 7 天、异步任务 2 天、ChatGPT2API 图片副本 3 天。

## 对象存储迁移边界

- 公开版的 S3 兼容对象存储目前服务于 `/v1/images/*/async` 任务，不会自动接管私有 `/v1/image-jobs/*`。
- 腾讯云 COS 可按 S3 兼容存储接入，但密钥只能保存在后台加密设置或服务器机密配置中，不能提交到 Git。
- 第一阶段只迁移私有 image job 的最终图片文件；不同时改写图片日志和 ChatGPT2API 图片副本，避免一次改动影响三条链路。
- 私有接口路径、任务状态、失败分类、取消接口、`result_delivery=file_url` 和默认 Base64 行为全部保持不变。
- 对象存储默认使用私有存储桶和限时签名 URL；没有明确业务需要时不把整桶设为公开。
- 对象存储启用前必须验证上传、下载、过期链接、失败回退、清理策略和费用监控。

## 安全部署顺序

1. 推送独立升级分支，不直接覆盖线上。
2. 运行后端 CI、安全扫描和 `Deploy Server` 的 `deploy=false` 构建验证。
3. 检查线上没有运行中的图片任务，服务器 CPU、内存和磁盘正常。
4. 再运行同一提交的 `Deploy Server`，选择 `deploy=true`。
5. GitHub Actions 构建并上传镜像；服务器只执行 `docker load` 和 `docker compose up`。
6. 验证 `/health`、版本、Codex Responses、图片接口、私有路由和资源占用；同时确认部署 run 的 `headSha`、线上 `.release-commit`、镜像 revision 标签和二进制 commit 完全一致。
7. 验证成功后，把升级分支对齐到 `main`，并保留升级前备份分支作为回滚点。

## 禁止事项

- 不在 2 核 4 GB 服务器运行 `docker build`、`docker compose build`、Go 或前端编译。
- 不因升级删除 Postgres、Redis、账号池、密钥、图片日志或数据卷。
- 不只看首页和 `/health`；必须探测私有路由，防止官方升级覆盖定制功能。
- 不把“版本号相同”当作“提交相同”；缺少提交标记时必须明确证据不足，不能靠猜测宣布完全对齐。
