# Sub2API 私有版运维手册

本手册面向第一次接手该私有部署的人和 AI。它只记录可重复执行的运维流程；版本状态以实时检查为准。

## 1. 真源层级

1. 正式代码真源：GitHub 私有仓库 ligerry1999-png/sub2api-private 的 main。
2. 本地规范工作副本：/Users/bricoleur/Documents/小红书 cil/sub2api-private。
3. 官方上游 Wei-Shaw/sub2api：只用于比较和合并更新。
4. 服务器发布目录和运行目录：只用于部署与诊断，不能反向当作源码。

私有功能清单见 ../tasks/PRIVATE_UPGRADE_CHECKLIST_CN.md。服务器地址、SSH 私钥位置和工作区入口见工作区 SUB2API_SERVER_HANDOFF.md。

## 2. 新任务开场检查

先读 AGENTS.md 和工作区交接文件，再运行：

~~~bash
cd "/Users/bricoleur/Documents/小红书 cil/sub2api-private"
git status --short
git branch --show-current
git rev-parse HEAD
git remote -v
git ls-remote origin refs/heads/main
git log -5 --oneline --decorate
~~~

git ls-remote 才是 GitHub 实时值；origin/main 只是上次 fetch 后的本地缓存。

## 3. 本地与 GitHub 出现分叉时

先获取远端引用：

~~~bash
git fetch --prune origin
git merge-base HEAD origin/main
git merge-base --is-ancestor HEAD origin/main
git merge-base --is-ancestor origin/main HEAD
~~~

判断方式：

- 本地是 origin/main 祖先：本地普通落后，可在工作区干净时快进。
- origin/main 是本地祖先：本地有未推送提交，先审查提交再决定。
- 两边都有独立提交：正常分叉，先查清来源，不直接强推。
- merge-base 不存在：历史被替换或重写，禁止 pull、rebase、reset --hard 和 force push。

无共同祖先时采用可恢复换位：

1. 确认旧工作区没有未提交改动。
2. 用提交短 SHA 和绝对日期给旧目录改名归档。
3. 从 GitHub main 克隆到新临时目录。
4. 核验新克隆 HEAD、remote 和工作区。
5. 将新克隆移动到规范路径。
6. 保留旧归档，直到新副本完成至少一次修改、测试和交付。

2026-07-19 核验记录：

- 旧本地仓库：d24570a4，与当时 GitHub main 无共同祖先。
- GitHub main：e680938b。
- 旧仓库已归档到 /Users/bricoleur/Documents/小红书 cil/sub2api-private-legacy-d24570a4-20260719。
- 规范路径已从 GitHub main 重新克隆，基础提交为 e680938b。

这是一条历史记录，不替代实时检查。

## 4. 服务器只读健康检查

默认不改配置、不重启、不构建。连接方式从工作区交接文件读取。连接后先运行：

~~~bash
docker ps -a
docker stats --no-stream
curl -fsS http://127.0.0.1:18080/health
curl -fsS https://sub2api.tanzhongyu.asia/health
free -h
df -h /
~~~

至少确认：

- sub2api-private、PostgreSQL、Redis、chatgpt2api 都在运行。
- Sub2API、PostgreSQL、Redis 为 healthy，或有等价的只读探针证据。
- ChatGPT2API 的 /health 返回 200。
- 容器没有异常重启。
- 可用内存和磁盘剩余量足够。

磁盘接近 85% 时先分类空间来源。禁止优先删除数据库、Redis、账号池、当前镜像、.env 或业务图片。图片日志、异步结果和 ChatGPT2API 图片副本的保留策略不同，必须分开处理。

## 5. 四方版本核验

### 本地

~~~bash
git status --short
git rev-parse HEAD
cat backend/cmd/server/VERSION
~~~

### GitHub

~~~bash
git ls-remote origin refs/heads/main
gh run list --repo ligerry1999-png/sub2api-private --workflow deploy-server.yml --limit 10
~~~

不能只看工作流 conclusion=success。Deploy Server 支持 deploy=false，必须继续检查目标 run 中 Upload release bundle 和 Deploy Sub2API 两个步骤是否实际 success。

### 线上发布与镜像

~~~bash
cat /opt/sub2api-release/.release-commit
cat /opt/sub2api-release/backend/cmd/server/VERSION
docker image inspect sub2api:private-prod --format '{{ index .Config.Labels "org.opencontainers.image.revision" }}'
docker exec sub2api-private /app/sub2api --version
~~~

带溯源的新部署应满足：

~~~text
GitHub 部署 run 的 headSha
= /opt/sub2api-release/.release-commit
= 镜像 org.opencontainers.image.revision
= 二进制 --version 中的 commit
~~~

任何一项不同都不能判定为完全对齐。

### 兼容旧镜像

2026-07-19 检查时，线上版本为 0.1.157-private.1，部署来源有充分证据指向 e680938b，但旧工作流构建的二进制显示 commit: docker，镜像也没有 revision 标签。该旧镜像只能通过 GitHub 部署记录、构建/启动时间和发布目录关键文件哈希交叉核验。

提交溯源修复必须经过下一次明确授权的 GitHub Actions 正式部署后才会在线上生效；文档或工作流修改本身不授权部署和重启。

## 6. 安全部署

1. 从 GitHub main 的已知提交建立独立分支。
2. 本地做代码审查和轻量检查。
3. 推送后等待后端 CI、安全扫描通过。
4. 对同一提交运行 Deploy Server，先选择 deploy=false。
5. 检查生产图片任务和内容工厂任务均空闲。
6. 对同一提交人工选择 deploy=true。
7. GitHub Actions 构建镜像并写入版本溯源；服务器不编译源码。
8. Actions 备份当前镜像，加载新镜像并切换容器。
9. 验证内外网健康、Nginx、私有路由、版本 SHA、容器资源和重启次数。
10. 任一关键检查失败，恢复 sub2api:private-prev。

未经用户明确授权，不执行第 6 步。

## 7. 回滚与数据边界

- 镜像回滚只替换应用镜像，不删除数据库、Redis 或挂载数据。
- 运行数据主要挂载在 /opt/sub2api-src/deploy/tanzhongyu/data；每次以 docker inspect 的实际 Mounts 为准。
- .env、账号凭据、OAuth token、数据库密码和 SSH 私钥不得进入 Git、日志或聊天。
- 不从服务器复制源码覆盖 GitHub。
- 不在生产服务器运行 docker build、docker compose build、go build、pnpm build 或 npm build。

## 8. 文档维护

以下变化必须同步：

- 真源、分支策略、部署门禁：AGENTS.md 和本手册。
- 服务器地址、SSH 入口、运行路径、线上组件：工作区 SUB2API_SERVER_HANDOFF.md。
- 私有功能和升级验收：tasks/PRIVATE_UPGRADE_CHECKLIST_CN.md。
- 已完成的版本升级事实：使用绝对日期和精确 commit/run 编号，不使用模糊时间词。
