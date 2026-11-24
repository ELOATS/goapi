# goapi — Go 后端生产级模板（Chi + net/http）

架构、分层、请求和事件链路、发布流程见 [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)。本地逐项验证功能见 [docs/LOCAL_VERIFICATION.md](docs/LOCAL_VERIFICATION.md)。上线前按 [docs/LAUNCH_CHECKLIST.md](docs/LAUNCH_CHECKLIST.md) 逐项检查。

## 快速开始
> Windows 上第一次跑遇到奇怪的报错（`go test` 被拦截、`curl` 返回 502、构建找不到 `go.sum` 等），先看「本地开发常见问题」。

```bash
go mod tidy          # 生成 go.sum（首次必须执行）
make run             # 本地关闭认证；Windows 无 make 时：$env:AUTH_DISABLED="true"; go run ./cmd/api
make test            # 带 -race 的测试
```

```bash
curl -s localhost:8080/healthz
curl -s -X POST localhost:8080/api/v1/users -H "Content-Type: application/json" \
  -d '{"name":"Alice","email":"alice@example.com"}'
```

## 使用 Postgres
```bash
docker compose up -d db                     # 启动 Postgres 17
# Linux/macOS
DATABASE_URL=postgres://app:app@localhost:15432/app?sslmode=disable go run ./cmd/api
# Windows PowerShell
$env:DATABASE_URL="postgres://app:app@localhost:15432/app?sslmode=disable"; go run ./cmd/api
```
启动时会自动执行 `internal/store/migrations/` 下的迁移（goose）。
不设置 `DATABASE_URL` 时自动退回内存存储。

一键起整套：`docker compose up --build`

集成测试（会清空 users 表）：
```bash
TEST_DATABASE_URL=postgres://app:app@localhost:15432/app_test?sslmode=disable go test ./internal/store/
```

## 列表与分页
```bash
curl -s "localhost:8080/api/v1/users?limit=20"
# 响应：{"items":[...],"next_cursor":"eyJ0Ijoi..."}，最后一页不返回 next_cursor
curl -s "localhost:8080/api/v1/users?limit=20&cursor=<next_cursor>"
```
采用 keyset（游标）分页，排序键为 `(created_at DESC, id DESC)`：翻页深度不影响性能，
并发写入也不会导致漏读或重复。`limit` 取值 1–100，默认 20。
游标带 HMAC-SHA256 签名（密钥 `CURSOR_SECRET`），被篡改或自己拼的游标返回 400。

## 事务
业务层通过 `TxManager.WithTx(ctx, fn)` 声明事务边界，仓储自动从 ctx 取出事务：
```go
err := s.tx.WithTx(ctx, func(ctx context.Context) error {
    u, err := s.users.Create(ctx, name, email)   // 同一事务
    if err != nil { return err }                 // 返回 error → 回滚
    return s.events.Append(ctx, evt)             // 同一事务
})                                               // 返回 nil → 提交
```
- `fn` 返回 error 或 panic 都会回滚；嵌套 `WithTx` 会加入外层事务
- 创建用户时会在同一事务写入 `outbox_events`（transactional outbox），
  之后由独立的 relay 进程投递到 Kafka / NATS 等，保证"数据写入"与"事件发出"一致
- 内存模式下 `NoopTxManager` 不提供回滚，仅用于开发

## 可观测性
```bash
docker compose up --build
# 发几个请求后：
#   链路：  http://localhost:16686   （Jaeger，服务名 goapi）
#   指标：  http://localhost:9091    （Prometheus，告警在 /alerts）
#   面板：  http://localhost:3000    （Grafana，默认首页即 goapi 面板）
#   告警：  http://localhost:9093    （Alertmanager）
#   原始：  curl localhost:9090/metrics
```

| 信号 | 实现 | 说明 |
|---|---|---|
| 链路 | OpenTelemetry + OTLP/HTTP | HTTP server span（名称如 `GET /api/v1/users/{id}`）→ 每条 SQL 一个子 span（otelpgx），每次 Redis 限流判定一个子 span（redisotel）；支持 W3C `traceparent` 透传 |
| 指标 | Prometheus，独立端口 `:9090` | `http_requests_total`、`http_request_duration_seconds`、`http_requests_in_flight`、`db_pool_*`、Go runtime / 进程指标 |
| 日志 | slog | 每条带 ctx 的日志自动附加 `trace_id` / `span_id`，可从日志直接跳到链路 |
| 剖析 | net/http/pprof | `PPROF_ENABLED=true` 时在指标端口开启 `/debug/pprof/` |

常用 PromQL：
```promql
# 每个路由的 P99 延迟
histogram_quantile(0.99, sum by (le, route) (rate(http_request_duration_seconds_bucket[5m])))
# 5xx 错误率
sum(rate(http_requests_total{status=~"5.."}[5m])) / sum(rate(http_requests_total[5m]))
# 连接池排队（持续 > 0 说明池子不够或有慢查询）
rate(db_pool_empty_acquire_total[5m])
```

要点：
- 指标的 `route` 标签用路由模板，未匹配的路径统一为 `unmatched`，不会因为 ID 或扫描器造成基数爆炸
- `/healthz`、`/readyz` 不产生 span
- 生产环境建议 `TRACE_SAMPLE_RATIO=0.05~0.1`；上游已决定采样的请求会跟随上游（ParentBased）
- 指标端口和 pprof 只应在内网可访问

## 面板与告警
- Grafana 面板：`deploy/grafana/dashboards/goapi.json`（启动时自动加载，也可手动导入任意 Grafana）
- 记录规则：`deploy/prometheus/rules.yml`（多窗口错误率、按路由 QPS 与 P99）
- 告警规则：`deploy/prometheus/alerts.yml`，处理步骤见 `deploy/RUNBOOK.md`
- 校验规则语法：`make check-rules`

| 告警 | 级别 | 条件 |
|---|---|---|
| Watchdog | none | 始终触发的心跳，用于死信监控 |
| GoapiInstanceDown | critical | 实例 1 分钟抓不到 |
| GoapiTargetMissing | critical | 没有任何抓取目标 5 分钟 |
| GoapiCrashLooping | warning | 30 分钟内重启 > 2 次 |
| GoapiErrorBudgetFastBurn | critical | 1h 与 5m 燃烧率都 > 14.4（99.9% SLO） |
| GoapiErrorBudgetSlowBurn | warning | 6h 与 30m 燃烧率都 > 6 |
| GoapiHighLatencyP99 | warning | 路由 P99 > 1s 持续 10 分钟（流量 > 0.5 rps） |
| GoapiDBPoolSaturated | warning | 连接池使用率 > 90% 持续 5 分钟 |
| GoapiDBPoolWaiting | warning | 池空排队 > 1 次/秒 持续 10 分钟 |
| GoapiDBAcquireCanceled | critical | 有请求等连接超时 |
| GoapiGoroutineLeak | warning | goroutine > 10000 持续 15 分钟 |

## 告警通知（Alertmanager）
链路：Prometheus → Alertmanager（分组 / 路由 / 抑制 / 静默）→ `cmd/alertbridge` → 飞书、钉钉

| 级别 | 接收方 | 推送时间 | 首次等待 | 重复提醒 |
|---|---|---|---|---|
| critical | 飞书 + 钉钉 | 全天候 | 10s | 1h |
| warning | 飞书 | 工作日 09:00–19:00（Asia/Taipei） | 30s | 12h |

1. 复制 `.env.example` 为 `.env`，填入飞书 / 钉钉机器人的 webhook 地址和签名密钥
   （不填也能跑：alertbridge 会把消息打印到日志）
2. `docker compose up --build`
3. 发一条测试告警：
   ```powershell
   # Windows PowerShell
   Invoke-RestMethod -Method Post -Uri http://localhost:9093/api/v2/alerts -ContentType "application/json; charset=utf-8" -InFile deploy/alertmanager/test-alert.json
   # 或 make alert-test
   ```
4. 约 10 秒后群里收到消息；`docker compose logs -f alertbridge` 可看发送结果

抑制规则：没有抓取目标时压掉所有 goapi 告警；实例宕机时压掉该实例的其它告警；
快速燃烧压掉慢速燃烧；连接等待超时压掉连接池饱和 / 排队。
校验配置：`make check-alertmanager`

## CI（GitHub Actions）
`.github/workflows/ci.yml`，PR 和推送到 main 时运行：

| Job | 内容 |
|---|---|
| Lint | `go mod tidy` 无差异、gofmt、go vet、golangci-lint |
| Test | `go test -race -shuffle=on`，启动 Postgres 17 服务容器跑集成测试，输出覆盖率 |
| Vulnerability scan | govulncheck（用最新补丁版 Go 扫描标准库漏洞） |
| Monitoring & deploy configs | promtool 校验配置 + 告警规则单元测试、amtool 校验、Grafana JSON、`docker compose config` |
| Docker | 以上全部通过后构建 api、alertbridge、relay 和 consumer 镜像；PR 只构建，main 和 `v*` 标签推送到 GHCR |

镜像地址：`ghcr.io/<owner>/<repo>/api`、`ghcr.io/<owner>/<repo>/alertbridge`，
标签为短 SHA、`latest`（main）和语义化版本（`v1.2.3` 标签）。

本地跑同样的检查：`make ci`（集成测试需要先 `make db-up` 并设置 `TEST_DATABASE_URL`）。
依赖更新：`.github/dependabot.yml` 每周检查 Go 模块、Actions 和 Docker 基础镜像。

## 部署到 Kubernetes
清单在 `deploy/k8s/`（kustomize）：`base/` 是通用配置；`overlays/staging/` 和 `overlays/production/` 覆盖命名空间、镜像版本、副本数、采样率，并加上入口（Gateway API）；`overlays/kind/` 用于本地 kind 集群。
数据库假定是托管的 Postgres（RDS、Cloud SQL 等），不在集群内部署。

集群需要先装好：Gateway API CRD（standard channel）、Envoy Gateway、cert-manager（并开启 Gateway API 支持）、metrics-server。

**先在本地用 kind 验证**：`.\scripts\kind.ps1 up`、`build`、`deploy`、`check`，会起一个 3 节点集群、装好上面这些组件并部署 `overlays/kind`，自动检查网关、证书、Pod Security、NetworkPolicy、滚动更新不丢请求等，见 [docs/LOCAL_VERIFICATION.md](docs/LOCAL_VERIFICATION.md) 第 12 节。

```bash
cp deploy/k8s/base/secrets.env.example deploy/k8s/base/secrets.env                       # 填 DATABASE_URL
cp deploy/k8s/base/alertbridge-secrets.env.example deploy/k8s/base/alertbridge-secrets.env
# 修改 overlays/production 里的镜像地址、版本号，以及 gateway.yaml 里的域名和 gatewayClassName
make k8s-render    # 先看渲染结果
make k8s-deploy    # 迁移 Job 完成后再等滚动更新完成
```

| 资源 | 要点 |
|---|---|
| 入口 | Gateway API + Envoy Gateway：`Gateway`（HTTP 跳转 HTTPS，cert-manager 签发证书）、`HTTPRoute`（只暴露 `/api`，请求超时 30s）、`ClientTrafficPolicy`（客户端 IP 识别）。ingress-nginx 已于 2026 年 3 月停止维护，不再使用 |
| Deployment | 不写 replicas（交给 HPA）；`maxUnavailable: 0` 滚动更新；跨节点 / 可用区分散；非 root、只读根文件系统、去掉全部 capabilities |
| 探针 | startup 和 liveness 用 `/healthz`（不依赖数据库），readiness 用 `/readyz`（Ping 数据库，关闭期间 503） |
| 优雅关闭 | `terminationGracePeriodSeconds: 40` > 摘流量 10s + 等待在途请求 20s |
| 资源 | 只设内存 limit，不设 CPU limit；`GOMEMLIMIT` 设为内存 limit 的约 90% |
| HPA | CPU 70%，2–10 副本（prod 至少 3）；扩容快、缩容慢 |
| PDB | 驱逐时最多同时少 1 个 |
| 迁移 | `api migrate` 子命令跑在 Job 里；`make k8s-deploy` 先只应用配置和 Job，迁移成功后才更新 Deployment。迁移需向后兼容（滚动期间新旧版本并存） |
| NetworkPolicy | API 业务端口只允许 Envoy Gateway 的代理（`envoy-gateway-system` 命名空间）访问，防止集群内其它 Pod 伪造 `X-Envoy-External-Address`；指标端口、relay、consumer、alertbridge 只对 monitoring 命名空间开放 |
| 配置 / 密钥 | configMapGenerator / secretGenerator 生成带哈希的名字，内容一变就自动滚动更新 |

## 自动部署（CD）
`.github/workflows/deploy.yml`，在 CI 成功之后运行：

| 触发 | 环境 | 审批 |
|---|---|---|
| push 到 main | staging（命名空间 `goapi-staging`） | 自动 |
| 打 `v*` 标签 | production（命名空间 `goapi`） | 需要 Environment 审批 |
| 手动 Run workflow | 任选，可填旧标签用于回滚 | production 仍需审批 |

每次部署：确认镜像存在 → `kubectl diff` 预览 → 迁移 Job → 滚动更新 → 冒烟测试（集群内 `/readyz` 和公网入口）。
滚动更新或冒烟测试失败会自动 `rollout undo`。两个环境用的都是 CI 构建的 `sha-<7位提交>` 镜像。

**一次性设置**（仓库 Settings → Environments，分别建 `staging` 和 `production`）：

| 名称 | 类型 | 说明 |
|---|---|---|
| `KUBECONFIG_B64` | secret | `base64 -w0 kubeconfig`，建议只授权对应命名空间 |
| `DATABASE_URL` | secret | 该环境的数据库 |
| `FEISHU_WEBHOOK_URL` / `FEISHU_SECRET` / `DINGTALK_WEBHOOK_URL` / `DINGTALK_SECRET` | secret | 可选 |
| `APP_URL` | variable | 公网地址，例如 `https://api.example.com`，用于冒烟测试 |

production 还要勾选 **Required reviewers**，并在 Deployment branches and tags 里只允许 `v*` 标签。
GHCR 包如果是私有的，集群需要 imagePullSecret（或把包设为对集群可见）。

## 认证与权限
`/api/v1` 下所有接口都要求 `Authorization: Bearer <access token>`（OIDC 签发的 JWT）。
`/healthz`、`/readyz` 和指标端口不需要认证。

| 接口 | 需要的角色 |
|---|---|
| `GET /api/v1/me` | 任意有效 token（返回自己的 sub 和角色，用来排查 403） |
| `GET /api/v1/users`、`GET /api/v1/users/{id}` | `goapi-viewer` 或 `goapi-admin` |
| `POST /api/v1/users` | `goapi-admin` |

- 校验：签名（JWKS，自动缓存和随密钥轮换刷新）、`iss`、`aud`、`exp`，只接受 RS256 / ES256
- 没有 token 或 token 无效 → 401，带 `WWW-Authenticate`；角色不够 → 403
- 暂时拉不到身份提供方的公钥、无法判断 token 是否有效 → 503 和 `Retry-After: 5`，客户端稍后重试即可，不会误以为 token 失效
- 创建用户时，token 的 `sub` 会写进 `outbox_events.actor`，事件里能看到是谁操作的
- 启动时请求 discovery 文档，身份提供方不可达则直接退出

| 变量 | 说明 |
|---|---|
| `OIDC_ISSUER_URL` | 例如 Keycloak `https://<host>/realms/<realm>`；必须与 discovery 文档的 issuer 完全一致 |
| `OIDC_AUDIENCE` | token 的 `aud` 必须包含它 |
| `OIDC_ROLES_CLAIM` | 默认 `roles`；Keycloak 用 `realm_access.roles`；Auth0 用自定义声明的完整键名 |
| `AUTH_DISABLED` | `true` 时不校验 token，所有请求视为本地管理员；`APP_ENV=prod` 时禁止 |

在身份提供方里：创建角色 `goapi-viewer`、`goapi-admin` 并分配给用户或服务账号；
确保 access token 的 `aud` 包含 `OIDC_AUDIENCE`（Keycloak 需要加 audience mapper）。

```bash
curl -H "Authorization: Bearer $TOKEN" https://api.example.com/api/v1/me
```

**本地验证认证**：叠加 `docker-compose.auth.yml` 会启动一个预置好用户和角色的 Keycloak，api 开启认证：
```powershell
$env:COMPOSE_FILE = "docker-compose.yml;docker-compose.auth.yml"
docker compose up -d --build
$t = .\scripts\get-token.ps1 alice            # alice=admin，bob=viewer，carol=无角色；密码和用户名相同
Invoke-RestMethod http://localhost:8080/api/v1/me -Headers @{ Authorization = "Bearer $t" }
```
完整步骤（401/403、拉不到公钥返回 503、带 token 跑 k6）见 [docs/LOCAL_VERIFICATION.md](docs/LOCAL_VERIFICATION.md) 第 11 节。

## 限流
三条规则，都只作用于 `/api/v1`（探针和指标端口不限流）：

| 规则 | 键 | 位置 | 默认 |
|---|---|---|---|
| ip | 客户端 IP（IPv6 按 /64 归并） | 认证之前：无效 token 的洪水在验签前就被挡住 | 20 rps，突发 40 |
| user | token 的 `sub` | 认证之后：换 IP 也绕不过去 | 10 rps，突发 20 |
| write | token 的 `sub` | 只用于写接口（目前是 `POST /api/v1/users`），在角色检查之后：没有权限的请求直接 403，不占额度 | 2 rps，突发 10 |

- 算法：GCRA（等价于令牌桶），被拒绝的请求不消耗额度
- 超限返回 429 + `Retry-After`；每个响应都带 `RateLimit-Limit` / `RateLimit-Remaining` / `RateLimit-Reset`（秒）
- 设置 `REDIS_URL` 时多个副本共享计数（Lua 脚本原子执行，以 Redis 时钟为准）；不设置则每个实例单独计数
- 一个请求经过多条规则时，响应头里的 `RateLimit-*` 以最后一条（最严的）为准
- 用 Redis 时，每次判定在 Jaeger 里是一个 `evalsha` 子 span（redisotel）
- Redis 出错时**放行请求**（fail open），计入 `http_rate_limit_decisions_total{result="error"}` 并触发 `GoapiRateLimiterErrors` 告警
- 在反向代理后面必须设 `HTTP_TRUST_PROXY=true`，否则所有请求的 IP 都是代理的地址

## 事件投递（outbox relay）
`cmd/relay` 把 `outbox_events` 里的事件投递到 NATS JetStream，主题为 `goapi.events.<事件类型>`（例如 `goapi.events.user.created`）。

```
API 事务: INSERT users + INSERT outbox_events ──提交──▶ NOTIFY
relay:   SELECT ... FOR UPDATE SKIP LOCKED ─▶ JetStream 发布并等待确认 ─▶ UPDATE published_at ─▶ COMMIT
```

- **至少一次**：发布后、提交前崩溃会重发。消息带 `Nats-Msg-Id`，JetStream 在去重窗口（默认 2 分钟）内丢弃重复；窗口外由消费方按 `Event-Id` 幂等
- **多副本安全**：`SKIP LOCKED` 让各实例认领不同的事件。不保证全局顺序，需要有序时消费方按 `Event-Id` 或业务版本处理
- **低延迟**：插入触发器 `pg_notify` 唤醒 relay；没有通知时每秒轮询一次兜底
- **失败处理**：暂时性错误（NATS 不可用、超时）一直重试，relay 退避 1s→30s；永久性错误（消息过大、事件类型非法）直接进死信（`dead_at`），不再重试
- **链路**：写入事件时保存 `traceparent`，relay 的发布 span 挂在原请求的 trace 下，并写入消息头；消费方用 `telemetry.NATSHeaderCarrier(msg.Header)` 提取即可接上（不要用 `propagation.HeaderCarrier`：它会把键规范化成 `Traceparent`，而 NATS 消息头区分大小写，读不到其它客户端按 W3C 写入的小写 `traceparent`）
- **消息头**：`Event-Id`、`Event-Type`、`Aggregate-Id`、`Occurred-At`、`Actor`、`traceparent`
- **清理**：已投递超过 `OUTBOX_RETENTION`（默认 7 天）的事件分批删除；死信保留到人工处理
- **监控**：`outbox_pending_events`、`outbox_oldest_pending_age_seconds`、`outbox_dead_events`、投递速率和延迟；告警 `GoapiRelayAllDown`、`GoapiOutboxLagging`、`GoapiOutboxDeadEvents`

本地：`docker compose up --build` 会同时启动 NATS 和 relay；单独跑 `make run-relay`。
看消息：`nats sub 'goapi.events.>'`（需要安装 nats CLI）或打开 http://localhost:18222/jsz?streams=1。

## API 文档（OpenAPI）
- 规范：`api/openapi.yaml`（OpenAPI 3.0.3），内嵌进二进制
- 在线查看：http://localhost:8080/api/docs（Swagger UI，点 Authorize 填 token 后可以直接调用）；原始文件 `/api/openapi.yaml`
- `DOCS_ENABLED=false` 时不提供这两个地址（production overlay 默认关闭）

规范是手写的（spec-first），由测试保证它和代码一致：

| 检查 | 在哪里 | 发现什么问题 |
|---|---|---|
| 契约测试 `TestOpenAPIContract` | `go test` | 用真实路由、中间件、处理器发 18 个请求，按规范校验请求和响应：字段多了少了、类型不对、出现没写的状态码都会失败；规范里每个接口至少覆盖一次 |
| 路由对照 `TestRoutesMatchSpec` | `go test` | 加了路由没写文档，或文档里的接口已经删了 |
| 规范检查 | CI Lint，`make openapi-lint` | 规范本身的错误和不规范写法（Redocly） |
| 不兼容变更 | CI（仅 PR），`make openapi-diff` | 和目标分支相比删了接口、删了字段、改了类型、加了必填参数（oasdiff） |

改接口的顺序：先改 `api/openapi.yaml`，再改代码，跑 `go test ./internal/httpserver/` 直到通过。
生成客户端：`npx @openapitools/openapi-generator-cli generate -i api/openapi.yaml -g typescript-fetch -o client/`（或其它语言）。

## 示例事件消费者
`cmd/consumer` 订阅 `goapi.events.user.created`，为新用户在 `welcome_notifications` 里记录一条待发送的欢迎通知。
它演示了消费方该怎么写；新增事件处理只需要实现一个 `consumer.Handler` 并注册。

| 机制 | 做法 |
|---|---|
| 拉取 | JetStream 持久拉取消费者（`CONSUMER_NAME`），多副本共享，消息分给各副本 |
| 确认 | 手动确认（AckExplicit）：处理成功并提交事务后才 Ack |
| 去重 | `processed_events (consumer, event_id)` 和业务写入**在同一个事务里**：重复消息插入不进去，直接 Ack，Handler 不执行 |
| 重试 | 暂时性错误 `NakWithDelay`，间隔 1s、2s、4s……最长 5 分钟；处理时间长时定期发送 InProgress 心跳 |
| 死信 | 尝试 `CONSUMER_MAX_DELIVER` 次（默认 10）仍失败，或返回 `consumer.Permanent(err)`，写入 `consumer_dead_letters` 后 Term |
| 重放 | 修复后运行 `consumer replay-dead-letters -limit 100`，重新发布到原主题 |
| 链路 | 从消息头提取 `traceparent`，`consume user.created` span 接在 API 请求 → relay 发布之后 |
| 关闭 | 处理完手上这条再退出；同一批里还没开始的立即 Nak，交给其它副本 |

写 Handler 时：数据库写入都用传入的 `db`（同一个事务）；不要在里面直接调用无法回滚的外部接口（例如发邮件），
而是先写一条待办记录，或者确保那个调用本身幂等。

本地：`docker compose up --build` 会一起启动；单独跑 `make run-consumer`。
创建用户后看 `SELECT * FROM welcome_notifications;`，Jaeger 里能看到同一条 trace 从 HTTP 请求一直到消费。

## 压测（k6）
脚本在 `loadtest/`，用 Docker 运行 k6，不需要本地安装。

| 脚本 | 用途 | 默认负载 | 通过标准 |
|---|---|---|---|
| `smoke.js` | 每个接口走一遍，确认环境可用 | 1 次 | 所有检查通过 |
| `load.js` | 固定速率混合负载，验证延迟和错误率 | 50 rps，5 分钟 | 错误率 < 0.1%；各接口 P99 < 1s；没有 429 |
| `stress.js` | 逐步加压到极限，观察 HPA 扩容 | 20 → 300 rps，约 19 分钟 | P99 < 1s；错误率超过 1% 持续 1 分钟自动停止 |
| `ratelimit.js` | 验证限流额度、429 格式、IP 限流在认证之前 | 约 1 分 45 秒 | 放行次数在预期值 ±15% 内；429 带 `Retry-After` 和 `RateLimit-*` |

混合负载：70% 列表（其中 20% 翻到下一页）、25% 按 ID 查询、5% 创建（`WRITE_RATIO`，只读压测设为 0）。
阈值和告警对齐：P99 1s 对应 `GoapiHighLatencyP99`，错误率 0.1% 对应 99.9% 可用性 SLO。

**本地**
```powershell
# 验证限流：用普通的 compose（限流开着）
docker compose up -d --build
make loadtest-smoke
make loadtest-ratelimit

# 测延迟和吞吐：叠加 docker-compose.loadtest.yml 关掉限流
docker compose -f docker-compose.yml -f docker-compose.loadtest.yml up -d --build
make loadtest-load RATE=100 DURATION=3m
make loadtest-cleanup
```
Windows 上没有 make 时：
```powershell
docker run --rm -i --add-host=host.docker.internal:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=http://host.docker.internal:8080 -e RATE=100 grafana/k6 run /loadtest/load.js
```

**staging**（不要对 production 跑：会创建数据，也会触发告警）
1. 准备 token：`TOKENS` 传逗号分隔的多个 token，按 VU 轮流使用；写请求需要 `goapi-admin` 角色。
2. 单台压测机的请求都来自同一个 IP、少数几个用户，会被限流挡住。压 `load` / `stress` 前临时调高额度：
   ```bash
   kubectl -n goapi-staging set env deployment/goapi-api \
     RATE_LIMIT_IP_RPS=100000 RATE_LIMIT_IP_BURST=100000 RATE_LIMIT_USER_RPS=100000 RATE_LIMIT_USER_BURST=100000 \
     RATE_LIMIT_WRITE_RPS=100000 RATE_LIMIT_WRITE_BURST=100000
   # 结束后恢复（或重新部署一次）
   kubectl -n goapi-staging set env deployment/goapi-api \
     RATE_LIMIT_IP_RPS- RATE_LIMIT_IP_BURST- RATE_LIMIT_USER_RPS- RATE_LIMIT_USER_BURST- \
     RATE_LIMIT_WRITE_RPS- RATE_LIMIT_WRITE_BURST-
   ```
3. 看 HPA：staging 最多 3 个副本，要观察更大范围的扩容先临时调大
   `kubectl -n goapi-staging patch hpa goapi-api --type merge -p '{"spec":{"maxReplicas":10}}'`，
   同时运行 `NS=goapi-staging ./loadtest/watch-hpa.sh | tee hpa.csv`（Windows 用 `watch-hpa.ps1`），在 Grafana 上对照 QPS、P99、连接池。
4. 运行：`make loadtest-stress LT_BASE_URL=https://staging-api.example.com TOKENS=$TOKENS TARGET_RPS=500`
5. 清理：在 staging 数据库执行 `make loadtest-cleanup` 里的两条 SQL。

注意：
- 副本数上去之后，数据库连接数是 副本数 × `DB_MAX_CONNS`，要小于 Postgres 的 `max_connections`；压测中 `db_pool_empty_acquire_total` 持续增长说明瓶颈在连接池或数据库。
- `dropped_iterations` 不为 0 说明压测机本身跟不上（VU 不够或 CPU 满了），结果不可信。
- `ratelimit.js` 的 `EXPECT_RPS` / `EXPECT_BURST` 填两条规则里更严的那条；没有 Redis 且有多个副本时，实际额度是额度 × 副本数，测试会失败。

## 代码检查
和 CI 一样在 Docker 里跑，不需要本地安装：
```powershell
docker run --rm -v "${PWD}:/app" -w /app golangci/golangci-lint:latest golangci-lint run ./...
docker run --rm -v "${PWD}:/src" -w /src -v goapi-gomod:/go/pkg/mod golang:1.26 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```
规则在 `.golangci.yml`：internal 包不要求导出符号写文档注释；测试文件不检查 errcheck、noctx、bodyclose、gosec。

## 本地开发常见问题
下面都是实际遇到过的问题。症状看起来像代码有 bug，其实是本地环境的原因。

| 症状 | 原因 | 解决 |
|---|---|---|
| `go test` 报 `fork/exec ...\go-build...\xxx.test.exe: An Application Control policy has blocked this file`，部分包 FAIL | Windows 的应用控制策略（Smart App Control 或企业策略）拦截了 `%TEMP%` 里刚编译出来、没有签名的测试程序。编译其实已经通过了 | 在 Docker 里跑测试，见下文。不要为此关闭安全策略；公司电脑找 IT 把开发目录加白名单 |
| `curl.exe` 访问 `localhost:8080` 返回 `502 Bad Gateway`，响应头里有 `Proxy-Connection`，响应体为空；加了 `-s` 时什么都不输出 | 本机配置了 HTTP 代理（`HTTP_PROXY` 等环境变量），`curl.exe` 把请求发给了代理，代理连不到你本机的服务。`Invoke-RestMethod` 默认绕过本地地址，所以不受影响 | 每条命令加 `--noproxy "*"`，或在当前窗口执行 `$env:NO_PROXY = "localhost,127.0.0.1"`；长期在代理软件里把这两个地址加入绕过列表 |
| 镜像构建报 `go.sum: not found`，或 lint 报 `missing go.sum entry`，接着出现一串 `undefined: pgx` | `go.mod` / `go.sum` 被覆盖或丢失。最常见的原因是把模板压缩包整包解压到了已有目录 | `git checkout -- go.mod go.sum` 恢复；没提交过就执行 `go mod tidy` 重新生成。更新模板文件时排除这两个文件（见下文） |
| 镜像构建报 `go.mod requires go >= 1.xx (running go 1.yy; GOTOOLCHAIN=local)` | `go mod tidy` 因为依赖升级调高了 `go.mod` 里的 go 版本，Dockerfile 的 `GO_VERSION` 没跟上 | 把 Dockerfile 的 `ARG GO_VERSION` 改成和 `go.mod` 一致。CI 的 Lint 会检查两者是否一致 |
| `docker compose up` 报 `port is already allocated` | 本机或其它容器占用了端口 | 见下文「本地环境的项目名」里的端口表，在 `.env` 里改端口 |
| 集成测试报 `password authentication failed for user "app"` | 连到了别的 PostgreSQL：本机安装的、其它容器的，或者同名 compose 项目早先用别的密码初始化过的数据卷 | 现在 compose 项目名固定为 `goapi-template`，宿主机端口改成了 15432，一般不会再遇到。还遇到时，用 `docker ps` 看看谁占用了端口 |
| 集成测试报 `lookup db ... no such host`，或者 api、relay 连不上数据库 | 同名 compose 项目删除重建过网络，已经在运行的 db 容器还连着旧网络 | `docker compose up -d --force-recreate db redis nats`。测试脚本已经改成通过宿主机端口连接，不受这个影响 |
| 某个容器启动后立即退出，日志里只有 `exec ...: exec format error` | 本地缓存的镜像 CPU 架构和本机不一致（例如拿到了 arm64 的镜像） | `docker rmi <镜像>`，再用 `docker pull --platform linux/amd64 <镜像>` 重新拉取；Apple Silicon 机器换成 `linux/arm64`。把 `latest` 固定成具体版本可以减少这类问题 |

### 在 Docker 里跑测试（Windows）
和 CI 一样是 Linux 环境，不受 Windows 应用控制策略影响：
```powershell
.\scripts\test-in-docker.ps1                # 单元测试
.\scripts\test-in-docker.ps1 -Integration   # 含 Postgres / Redis / NATS 集成测试（自动启动这三个服务）
```
PowerShell 不允许运行脚本时，先执行 `Set-ExecutionPolicy -Scope Process Bypass`，只对当前窗口生效。

### 更新模板文件时不要覆盖 go.mod / go.sum
模板压缩包里的 `go.mod` 只列了很少的依赖，也没有 `go.sum`；你本地经过 `go mod tidy` 的版本才是完整的。更新时先提交当前状态，再复制除这两个文件以外的所有文件：
```powershell
git add -A; git commit -m "before template update"
Expand-Archive .\goapi.zip -DestinationPath $env:TEMP\goapi-new -Force
robocopy "$env:TEMP\goapi-new\goapi" . /E /XF go.mod go.sum
git diff --stat
go mod tidy                                 # 新代码引入了新依赖时补上
```

### 换行符
`.gitattributes` 规定 `.ps1` / `.bat` 用 CRLF，其它文件一律用 LF。Windows 上如果 Git 把 Makefile、shell 脚本转成了 CRLF，
放进 Linux 容器会报 `bad interpreter` 或 Makefile 语法错误。第一次加上 `.gitattributes` 之后，执行一次：
```powershell
git add --renormalize .
git commit -m "normalize line endings"
```

## 本地环境的项目名
`docker-compose.yml` 固定了项目名 `goapi-template`，容器、网络、数据卷都带这个前缀
（例如 `goapi-template-db-1`、`goapi-template_pgdata`），不会和其它叫 goapi 的 compose 项目混用。

映射到宿主机的端口特意避开了常用端口，不和本机或其它容器里的 PostgreSQL / Redis / NATS 冲突：

| 服务 | 宿主机端口 | 环境变量 | 容器之间访问 |
|---|---|---|---|
| PostgreSQL | 15432 | `GOAPI_DB_PORT` | `db:5432` |
| Redis | 16379 | `GOAPI_REDIS_PORT` | `redis:6379` |
| NATS | 14222 | `GOAPI_NATS_PORT` | `nats:4222` |
| NATS 监控页 | 18222 | `GOAPI_NATS_MONITOR_PORT` | `nats:8222` |

这几个端口也被占用时，在项目根目录的 `.env` 里改，例如 `GOAPI_DB_PORT=25432`。
`docker compose`、`make` 和 `scripts/test-in-docker.ps1` 都读同样的变量（`make` 和脚本读的是当前 shell 的环境变量，不读 `.env`）。

## 集成测试的数据库
集成测试用独立的库 `app_test`，不碰本地环境用的 `app`：测试会清空表，还会留下测试数据
（例如一条 payload 不完整的 `user.created` 事件，如果和本地环境共用，会被 relay 投递出去、在 consumer 里进死信）。
新数据卷首次启动时由 `deploy/postgres-init/` 自动建库；已有的数据卷由 `scripts/test-in-docker.ps1` 或 `make db-test-create` 补建。

## 目录结构
```
api/                OpenAPI 规范、Swagger UI
cmd/api/            入口：配置、日志、信号处理、优雅关闭；`api migrate` 只跑迁移
cmd/consumer/       示例事件消费者；`consumer replay-dead-letters` 重放死信
cmd/relay/          outbox relay：投递事件到 NATS JetStream
cmd/alertbridge/    Alertmanager → 飞书 / 钉钉 转发服务（仅标准库）
docs/               架构说明、本地验证手册、上线前检查清单
scripts/            test-in-docker.ps1（在 Docker 里跑测试）、get-token.ps1 / forge-token.ps1（本地 Keycloak）、kind.ps1（本地 K8s）
loadtest/           k6 压测脚本、HPA 记录脚本
deploy/kind/        kind 集群配置、集群内依赖（Postgres / Redis / NATS）
internal/config/    环境变量配置 + 校验
internal/httpserver 服务器超时、路由、中间件（日志 / panic 恢复）
internal/auth/      OIDC / JWT 认证、角色授权；authtest 在测试里签发 token
internal/handler/   HTTP 层：解析、校验、游标编解码、错误映射
internal/service/   业务层：事务边界、分页逻辑
internal/httpx/     JSON 编解码、统一错误体
internal/domain/    模型与领域错误（不依赖 HTTP/DB）
internal/consumer/  消费者通用部分：拉取、去重、事务、重试、死信、重放、指标；WelcomeHandler 示例
internal/outbox/    relay 核心：认领、发布、死信、清理、LISTEN、指标；NATS 发布器
internal/ratelimit/ GCRA 限流：内存 / Redis 实现、中间件、指标
internal/telemetry/ 链路追踪初始化、Prometheus 指标、日志 trace 关联
internal/store/     数据访问：Postgres（pgx）、内存实现、TxManager、迁移
```

## 已内置的生产实践
- 服务器超时全部显式设置（ReadHeaderTimeout 防 slowloris）
- 优雅关闭：SIGTERM → /readyz 返回 503 → 等待摘流量 → Shutdown 等待在途请求
- 结构化日志（slog，生产 JSON / 开发文本），每条日志带 request_id
- panic 恢复返回 JSON 500 并记录堆栈
- 请求体大小限制、拒绝未知字段、拒绝多个 JSON 对象
- 响应压缩：客户端带 `Accept-Encoding: gzip` 时压缩 JSON / YAML / HTML 响应
- 每个 API 请求有 context 超时，并传递到存储层
- 领域错误集中映射为 HTTP 状态码，不泄露内部错误
- 仅在 HTTP_TRUST_PROXY=true 时信任 X-Forwarded-For
- distroless 非 root 镜像

## 配置（环境变量）
| 变量 | 默认值 |
|---|---|
| APP_ENV | dev |
| HTTP_ADDR | :8080 |
| LOG_LEVEL | info |
| HTTP_READ_HEADER_TIMEOUT | 5s |
| HTTP_READ_TIMEOUT | 15s |
| HTTP_WRITE_TIMEOUT | 30s |
| HTTP_IDLE_TIMEOUT | 120s |
| HTTP_HANDLER_TIMEOUT | 20s（必须小于 WRITE_TIMEOUT） |
| SHUTDOWN_DRAIN_DELAY | 5s |
| SHUTDOWN_TIMEOUT | 25s |
| HTTP_MAX_BODY_BYTES | 1048576 |
| HTTP_TRUST_PROXY | false |
| DATABASE_URL | 空（使用内存存储） |
| DB_MAX_CONNS / DB_MIN_CONNS | 10 / 2 |
| DB_MAX_CONN_LIFETIME / DB_MAX_CONN_IDLE | 30m / 5m |
| DB_QUERY_TIMEOUT | 5s |
| DB_MIGRATE_ON_START | true（多副本建议 false，用独立 Job 迁移） |
| OTEL_EXPORTER_OTLP_ENDPOINT | 空（不导出链路），例如 http://localhost:4318 |
| OTEL_SERVICE_NAME | goapi |
| TRACE_SAMPLE_RATIO | 1.0 |
| METRICS_ADDR | :9090（为空则不启动指标服务） |
| PPROF_ENABLED | false |
| DOCS_ENABLED | true（/api/openapi.yaml 和 /api/docs） |
| OIDC_ISSUER_URL / OIDC_AUDIENCE | 必填（除非 AUTH_DISABLED=true） |
| OIDC_ROLES_CLAIM | roles |
| OIDC_DISCOVERY_URL | 空；服务访问身份提供方的地址和 issuer 不同时设置（例如本地 compose 里 `http://keycloak:8080/realms/goapi`），`iss` 仍按 OIDC_ISSUER_URL 校验 |
| AUTH_DISABLED | false（prod 下不允许为 true） |
| HTTP_CLIENT_IP_HEADER | X-Real-IP（HTTP_TRUST_PROXY=true 时从这个头取客户端 IP；k8s 里配置为 X-Envoy-External-Address） |
| RATE_LIMIT_ENABLED | true |
| RATE_LIMIT_IP_RPS / RATE_LIMIT_IP_BURST | 20 / 40 |
| RATE_LIMIT_USER_RPS / RATE_LIMIT_USER_BURST | 10 / 20 |
| RATE_LIMIT_WRITE_RPS / RATE_LIMIT_WRITE_BURST | 2 / 10（写接口，按用户） |
| CURSOR_SECRET | 空时随机生成（只适合单副本的本地开发）；`APP_ENV=prod` 时必填，至少 32 字节，所有副本共用 |
| CURSOR_SECRET_PREVIOUS | 空；轮换游标密钥时填旧值 |
| REDIS_URL | 空（每个实例单独计数） |
| NATS_URL | relay 用；空则只打印到日志（`APP_ENV=prod` 时必填） |
| NATS_STREAM / NATS_SUBJECT_PREFIX | GOAPI_EVENTS / goapi.events |
| RELAY_BATCH_SIZE / RELAY_POLL_INTERVAL | 100 / 1s |
| RELAY_LISTEN | true（PgBouncer 事务池模式下设为 false） |
| OUTBOX_RETENTION | 168h |
| CONSUMER_NAME | goapi-welcome（JetStream 持久消费者名） |
| CONSUMER_ACK_WAIT / CONSUMER_MAX_DELIVER | 30s / 10 |
| CONSUMER_BATCH_SIZE / CONSUMER_HANDLER_TIMEOUT | 10 / 20s |
| CONSUMER_DEDUP_RETENTION | 336h（必须长于 NATS_STREAM_MAX_AGE） |

k8s 中注意：SHUTDOWN_DRAIN_DELAY + SHUTDOWN_TIMEOUT 应小于 terminationGracePeriodSeconds（默认 30s）。

## 下一步可以加
- 查询变多后引入 sqlc 生成类型安全的查询代码
- 认证中间件（JWT / OIDC）
