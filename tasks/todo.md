# 生图日志改造计划

- [x] 新增 Sub2API 生图日志表和后端仓储/服务
- [x] 在 ChatGPT2API 桥接和 Sub2API 原生生图完成后记录图片、提示词、用户、时间
- [x] 新增后台 API 和前端“生图日志”页面
- [x] 本地构建和关键测试通过后，部署到服务器让你查看

## 后续优化

- [x] 把列表里的缩略图从 base64 inline 改成静态 URL/按需加载，进一步减小接口响应体
- [x] 补一套图片日志保留/清理策略，避免磁盘长期增长
- [x] 补充生产日志中的阶段耗时观察，确认慢请求主要落在 ChatGPT2API 网关还是 Sub2API 原生兜底
- [x] 修复 GitHub Actions 里 frontend 依赖安装的失败项，让 CI 全绿

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
- [x] 部署脚本增加私有路由探针，防止后续升级再次漏掉 `/v1/image-jobs/*`

## 服务器安全清理记录

- [x] 只清理 Docker BuildKit 构建缓存、systemd journal 旧日志和 apt 包缓存
- [x] 保留账号池、Postgres、Redis、Sub2API 图片日志、ChatGPT2API 数据和当前运行镜像
- [x] 系统盘从 84% 降到 47%，服务容器保持运行且 healthy
- [x] 补充私有版升级验收清单，记录每次升级必须保留的私有功能和部署检查点
- [x] 盘点当前磁盘构成：系统盘约 48%，主要空间来自 Sub2API 生图日志、异步任务结果和 ChatGPT2API 图片文件
- [x] 将生产保留策略收口为：生图日志 15 天、异步任务结果 2 天、ChatGPT2API 图片副本 3 天

## Sub2API 0.1.134 私有升级

- [x] 建立独立升级分支 `upgrade-official-0.1.134-private`
- [x] 使用官方 `v0.1.133..upstream/main` 补丁升级，避免直接覆盖私有分支
- [x] 解决冲突并保留账号经理角色、图片日志、异步 image-jobs、ChatGPT2API 生图桥接
- [x] GitHub Actions build-only 验证通过
- [x] 验证通过后手动 `deploy=true` 部署，服务器只加载镜像和重启容器
- [x] 部署后检查健康状态、CPU/内存、私有路由探针

## ChatGPT2API 生图桥接失败收口

- [x] ChatGPT2API worker 失败时先区分失败类型：额度限制、临时网页错误、无图片、认证错误、不支持请求
- [x] 额度限制或临时网页错误时优先换账号，不再立即走 Sub2API 原生兜底
- [x] Plus 图片额度提示带 `limit resets in ...` 时，只对图片调度设置临时冷却，到期自动恢复，不影响文字/Codex 调用
- [x] 只有账号切换接近耗尽时，才允许最后一次 Sub2API 原生兜底
- [x] `mask`、远程图片 URL、特殊图片参数等 worker 不支持的请求保留直接原生兜底，不做无意义换号

## 验证记录

- [x] 前端生产构建通过，`ImageLogsView` 已打进前端产物
- [x] 服务器端 Docker build 和 `go build ./cmd/server` 已通过，Deploy Server workflow 成功
- [x] 本轮前端 `npm run typecheck` 和 `npm run build` 通过
- [x] 本轮 Go 关键包测试通过：`./internal/service`、`./internal/handler/admin`、`./internal/repository`、`./cmd/server`
- [x] `git diff --check` 通过
- [x] 硬重启后线上旧容器恢复健康：Sub2API/Postgres/Redis 均 healthy，内部 `/health` 正常
- [x] Sub2API 0.1.134 私有升级分支部署成功：GitHub Actions run `27115336617` 成功，线上容器 healthy，`/v1/image-jobs/*` 返回鉴权而非 404，`/api/v1/admin/image-logs` 返回鉴权而非 404
- [x] 本轮 ChatGPT2API 生图桥接失败收口：CI run `27353810567` 通过，Security Scan run `27353810196` 通过，Deploy Server run `27355014141` 部署成功；线上容器 healthy，私有路由探针返回鉴权而非 404
