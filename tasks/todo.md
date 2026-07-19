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
- [x] 将生产保留策略收口为：生图日志 7 天、异步任务结果 2 天、ChatGPT2API 图片副本 3 天

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

## 异步生图 job 可观测性和取消接口

- [x] 为 `/v1/image-jobs/*` 失败结果补充 `error_type` 和 `retryable`
- [x] 增加 `/v1/image-jobs/:job_id/cancel`，允许客户端取消旧 job
- [x] 确认取消不会被后台完成结果覆盖
- [x] 补充路由和状态机测试

## 验证记录

- [x] 前端生产构建通过，`ImageLogsView` 已打进前端产物
- [x] 服务器端 Docker build 和 `go build ./cmd/server` 已通过，Deploy Server workflow 成功
- [x] 本轮前端 `npm run typecheck` 和 `npm run build` 通过
- [x] 本轮 Go 关键包测试通过：`./internal/service`、`./internal/handler/admin`、`./internal/repository`、`./cmd/server`
- [x] `git diff --check` 通过
- [x] 硬重启后线上旧容器恢复健康：Sub2API/Postgres/Redis 均 healthy，内部 `/health` 正常
- [x] Sub2API 0.1.134 私有升级分支部署成功：GitHub Actions run `27115336617` 成功，线上容器 healthy，`/v1/image-jobs/*` 返回鉴权而非 404，`/api/v1/admin/image-logs` 返回鉴权而非 404
- [x] 本轮 ChatGPT2API 生图桥接失败收口：CI run `27353810567` 通过，Security Scan run `27353810196` 通过，Deploy Server run `27355014141` 部署成功；线上容器 healthy，私有路由探针返回鉴权而非 404
- [x] 本轮异步生图 job 可观测性和取消接口：临时 Go 1.26.4 执行 `gofmt`，`go test ./internal/handler ./internal/server/routes` 通过，`git diff --check` 通过

## Sub2API 0.1.151 私有兼容升级

- [x] 建立当前私有分支的可回滚基线，获取官方 `v0.1.145..v0.1.151` 提交范围
- [x] 选择性应用 Codex 0.144.1 客户端兼容更新，并为 ChatGPT 图像上游的 `tool_choice` 协议变化补充窄范围回退
- [x] 保留异步 `/v1/image-jobs/*`、图片公网 URL、图片日志、ChatGPT2API 桥接和部署保护逻辑
- [x] 为图片工具请求构造补充回归测试，覆盖原始 `tools`/`tool_choice` 与兼容重试请求
- [x] 运行格式检查和 GitHub Actions build-only 验证（run `29188351811` 通过）
- [x] 图片任务空闲后，使用 GitHub Actions 部署并验证健康检查、私有路由（run `29188525248` 通过；线上健康检查与五个私有路由均正常鉴权）
- [ ] 等待下一次真实带密钥的生图请求，确认上游 `tool_choice=image_generation` 400 会自动重试为 `tool_choice=auto`

## 0.1.151 私有兼容升级复盘

- [x] 记录最终合入范围、验证结果、上线版本和一键回滚点：版本 `0.1.151-private.1`，代码提交 `a860cb8a`，构建 run `29188351811`，部署 run `29188525248`；回滚可在部署工作流选择上一成功分支/镜像，或将服务器镜像切回部署前的 `sub2api:private-prod` 备份

## Sub2API 0.1.157 私有安全升级

- [x] 从线上 `0.1.151-private.1` 对应提交建立独立升级分支和回滚点
- [x] 合并官方 `v0.1.157`，优先采用 Codex 流式、HTTP/2、调度和 GPT-5.6 修复
- [x] 处理官方异步图片任务与私有 `/v1/image-jobs/*` 的重叠，保持客户端现有接口契约不变
- [x] 保留图片公网 URL、图片日志、ChatGPT2API 桥接、失败分类、取消接口和存储清理逻辑
- [x] 保留 OpenAI Team/Personal 空间信息导入、备份和账号列表显示
- [x] 恢复图片 `tool_choice` 被上游拒绝时改用 `auto` 的窄范围兼容重试
- [x] 部署工作流增加当前镜像备份和失败自动回滚，服务器不执行源码构建
- [x] 将 `golang.org/x/image` 升级到 v0.43.0，修复生图日志 WebP 解码可触发的已知崩溃漏洞
- [x] 补充或更新私有路由、流式 Responses、图片结果交付和取消状态机回归测试
- [x] 仅在 GitHub Actions 运行后端/前端测试并构建镜像，不在生产服务器编译
- [x] 检查线上运行任务，空闲后通过手动 GitHub Actions 部署
- [x] 部署后验证健康、Codex Responses 路由、同步/异步生图路由、公网 URL、图片日志和资源占用；GPT-5.6 与真实生图上游调用等待自然流量继续观察
- [x] 记录构建 run、部署 run、上线版本和一键回滚点

## 0.1.157 私有安全升级复盘

- [x] 首次上线版本：`0.1.157-private.1`，升级集成提交：`f38eb77779d43da5026a6bae7be3a1c5cf5fe56f`
- [x] CI run `29494354222`、Security Scan run `29494354184`、build-only run `29494359699` 均通过
- [x] 正式部署 run `29495089391` 通过；GitHub Actions 构建约 5 分钟，服务器加载镜像与切换容器约 25 秒
- [x] 2026-07-16 随后合入公网图片结果基址修复和 7 天图片日志保留，最终线上提交为 `e680938b6c8330eb7755b0354060c99ffe3a12a1`，正式部署 run `29510177822` 通过
- [x] 线上 `/health` 返回 200，公开设置返回 `0.1.157-private.1`；Codex Responses、图片日志和五个异步生图私有路由均返回鉴权响应而非 404
- [x] Nginx 配置检查通过；ChatGPT2API worker 仍为默认关闭；部署后无 5xx、无卡住的图片 job
- [x] 回滚镜像：`sub2api:private-prev`；代码回滚分支：`backup/deployed-v151-20260716`；升级前旧主分支：`backup/pre-v157-main-20260716`
- [x] GitHub `main` 已对齐为验证后的正式代码真源；不把服务器目录当作源码真源，也不在服务器编译

## 2026-07-19 维护交接收口

- [x] 发现旧本地仓库 `d24570a4` 与 GitHub `main` `e680938b` 无共同祖先，保留旧目录后从 GitHub 重新克隆规范工作副本
- [x] 新增根目录 `AGENTS.md` 和私有运维手册，补齐新任务入口、历史分叉处理、四方版本核验和生产红线
- [x] 部署工作流改为把 GitHub SHA 写入二进制、OCI revision 标签和线上 `.release-commit`，并在切换容器前后校验三者一致
- [ ] 待明确授权后，将维护分支合入 GitHub `main`；先运行 build-only，后续正式部署时再让提交溯源在线上生效
