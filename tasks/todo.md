# 生图日志改造计划

- [x] 新增 Sub2API 生图日志表和后端仓储/服务
- [x] 在 ChatGPT2API 桥接和 Sub2API 原生生图完成后记录图片、提示词、用户、时间
- [x] 新增后台 API 和前端“生图日志”页面
- [x] 本地构建和关键测试通过后，部署到服务器让你查看

## 后续优化

- [x] 把列表里的缩略图从 base64 inline 改成静态 URL/按需加载，进一步减小接口响应体
- [x] 补一套图片日志保留/清理策略，避免磁盘长期增长
- [x] 补充生产日志中的阶段耗时观察，确认慢请求主要落在 ChatGPT2API 网关还是 Sub2API 原生兜底
- [ ] 修复 GitHub Actions 里 frontend 依赖安装的失败项，让 CI 全绿

## 安全升级计划

- [x] 取消会压垮服务器的本机 Docker build 部署任务
- [x] 临时禁用 Deploy Server workflow，避免误触发自动部署
- [x] 改造部署流程：GitHub Actions 构建镜像，服务器只执行 docker load 和 compose up
- [x] 改造部署触发方式：只允许手动触发，默认只构建不部署
- [x] 推送到 main 后先跑一次 build-only 验证
- [x] 验证通过后再手动选择 deploy=true 执行升级

## 异步生图路由恢复

- [x] 确认线上新版镜像缺少 `/v1/image-jobs/*` 路由，导致本地脚本提交异步生图任务 404
- [x] 从历史定制分支恢复 Image Jobs handler、路由和路由注册测试
- [x] 本地轻量验证路由测试通过
- [x] GitHub Actions build-only 验证通过
- [x] 手动 deploy=true 部署并确认线上异步路由恢复

## 验证记录

- [x] 前端生产构建通过，`ImageLogsView` 已打进前端产物
- [x] 服务器端 Docker build 和 `go build ./cmd/server` 已通过，Deploy Server workflow 成功
- [x] 本轮前端 `npm run typecheck` 和 `npm run build` 通过
- [x] 本轮 Go 关键包测试通过：`./internal/service`、`./internal/handler/admin`、`./internal/repository`、`./cmd/server`
- [x] `git diff --check` 通过
- [x] 硬重启后线上旧容器恢复健康：Sub2API/Postgres/Redis 均 healthy，内部 `/health` 正常
