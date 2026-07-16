# Sub2API 私有版升级清单

## 唯一代码真源

- GitHub 私有仓库的 `main` 是正式代码真源。
- 服务器只运行 GitHub Actions 构建出的镜像，不在服务器修改源码或编译。
- 每次升级从官方 release tag 建立干净分支，再逐项迁移私有功能；不要把服务器目录反向当成源码。

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

## 安全部署顺序

1. 推送独立升级分支，不直接覆盖线上。
2. 运行后端 CI、安全扫描和 `Deploy Server` 的 `deploy=false` 构建验证。
3. 检查线上没有运行中的图片任务，服务器 CPU、内存和磁盘正常。
4. 再运行同一提交的 `Deploy Server`，选择 `deploy=true`。
5. GitHub Actions 构建并上传镜像；服务器只执行 `docker load` 和 `docker compose up`。
6. 验证 `/health`、版本、Codex Responses、图片接口、私有路由和资源占用。
7. 验证成功后，把升级分支对齐到 `main`，并保留升级前备份分支作为回滚点。

## 禁止事项

- 不在 2 核 4 GB 服务器运行 `docker build`、`docker compose build`、Go 或前端编译。
- 不因升级删除 Postgres、Redis、账号池、密钥、图片日志或数据卷。
- 不只看首页和 `/health`；必须探测私有路由，防止官方升级覆盖定制功能。
