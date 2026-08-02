# 官方 v0.1.165 能力保护清单

这份清单解决一个长期维护问题：以后迁移私有功能时，不能只确认“私有功能还在”，还要确认新版官方修复没有被旧代码覆盖。

## 必须同时保留的官方能力

| 官方能力 | 当前代码证据 | 防回退方式 |
|---|---|---|
| Codex / Responses 的输入条目 ID、namespace 工具和流式事件兼容 | `backend/internal/service/openai_responses_item_id.go`、`openai_responses_namespace.go` 及对应测试 | 后端完整单元测试 |
| OpenAI OAuth 与 API Key 请求输入清理 | `backend/internal/service/openai_gateway_forward.go` 及 `openai_gateway_apikey_item_id_test.go` | 后端完整单元测试 |
| 提示词安全审计 | `backend/internal/server/routes/prompt_audit_route_coverage_test.go` | 每个新增 POST 路由必须登记为“已审计”或说明“不含提示词” |
| 可信代理与真实客户端 IP 解析 | `backend/internal/pkg/ip/`、`backend/internal/server/http_ingress_test.go` | 后端完整单元测试 |
| 组合分组路由 | `backend/internal/server/routes/gateway.go` 的 `compositeTarget` | 私有生图路由继续经过组合分组中间件 |
| 官方异步生图对象存储底座 | `backend/internal/service/image_storage.go`、`image_storage_settings.go` | 本轮只保留底座，不把私有任务接口强行接入对象存储 |
| v0.1.165 数据库迁移 | `backend/migrations/137_*` 至 `190_*` | 禁止修改已上线的 `136_image_logs.sql`，正式部署前必须恢复备份演练 |
| 依赖安全版本 | `golang.org/x/image v0.43.0`、`golang.org/x/text v0.39.0` | GitHub Security Scan 和依赖校验 |
| Docker 跨架构构建修复 | 根目录 `Dockerfile` 的 `BUILDPLATFORM` / `TARGETARCH` 流程 | GitHub `deploy=false` 只构建验证 |

## 本轮已经发生过的一次有效拦截

私有 `/v1/image-jobs/*` 路由迁入后，官方的安全审计覆盖测试立即报出三个未分类 POST 路由。修复方式是：

- 生图和改图入口登记为“必须执行提示词安全审计”；
- 取消任务入口登记为“不含用户提示词的控制操作”；
- 不删除、不绕过官方测试。

这说明以官方稳定版为底座、保留官方测试的策略正在真正发挥作用，而不只是留一份说明文档。

## 正式部署前仍需完成

1. 常规 CI、Security Scan、`deploy=false` 构建验证必须针对同一个提交全部通过。
2. 用生产 PostgreSQL 备份恢复隔离数据库，并运行新版本迁移。
3. 让当前旧镜像连接已经迁移的隔离数据库，验证回滚兼容性。
4. 以上结果与精确提交 SHA 绑定后，才允许执行 `deploy=true`。
