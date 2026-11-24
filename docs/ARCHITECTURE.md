# goapi 架构说明

本文说明 goapi 由哪些部分组成、各层负责什么、一个请求和一个事件分别经过哪些环节，以及代码从提交到上线的过程。
具体的配置项和命令见 [README](../README.md)，告警的处理步骤见 [RUNBOOK](../deploy/RUNBOOK.md)，本地验证见 [LOCAL_VERIFICATION](LOCAL_VERIFICATION.md)，上线前检查见 [LAUNCH_CHECKLIST](LAUNCH_CHECKLIST.md)。

> 状态说明：本模板的 Go 代码、CI/CD 工作流和 Kubernetes 清单都还**没有在真实环境里编译、运行或部署过**。
> 第一次使用时请先跑 `go mod tidy && go vet ./... && go test -race ./...`，并在 staging 上完整走一遍发布流程。

---

## 1. 总览

goapi 由四个进程组成，共用一个代码仓库和一个 Dockerfile（构建参数 `CMD` 决定构建哪一个）：

| 进程 | 目录 | 职责 | 副本 |
|---|---|---|---|
| **api** | `cmd/api` | 对外 HTTP API；`api migrate` 子命令只执行数据库迁移 | HPA 2–10（生产至少 3） |
| **relay** | `cmd/relay` | 把 `outbox_events` 表里的事件投递到 NATS JetStream | 2 |
| **consumer**（示例） | `cmd/consumer` | 订阅 `user.created`，演示消费方的去重、重试和死信；`replay-dead-letters` 子命令重放死信 | 2 |
| **alertbridge** | `cmd/alertbridge` | 把 Alertmanager 的告警转发到飞书、钉钉 | 2 |

依赖的外部组件：

| 组件 | 用途 | 不可用时 |
|---|---|---|
| Envoy Gateway（Gateway API） | 集群入口：TLS 终结、HTTP 跳转 HTTPS、只转发 `/api`、写入可信的客户端 IP | 外部请求进不来；集群内组件之间不受影响 |
| PostgreSQL 13+ | 业务数据、outbox 事件 | api 的 `/readyz` 返回 503，实例被摘流量；relay 暂停投递 |
| OIDC 身份提供方 | 签发和校验 access token | 启动时 discovery 失败则 api 起不来；运行中拉不到公钥时，缓存里的公钥照常可用，需要新公钥的请求返回 503 |
| Redis（可选） | 多副本共享限流计数 | 限流放行所有请求（fail open），并触发告警 |
| NATS JetStream | 事件投递目标 | 事件留在数据库里积压，不丢失，恢复后补发 |
| Prometheus / Alertmanager / Grafana / Jaeger | 指标、告警、面板、链路 | 不影响业务 |

```
              envoy-gateway-system   ┌──────── Kubernetes 命名空间 goapi ────────┐
 客户端 ──HTTPS──▶ Envoy 代理 ──/api──▶ api (×N) ──────────▶ PostgreSQL
                      │                 │  │                   ▲
                      │                 │  └──▶ Redis（限流）   │ LISTEN / SELECT ... SKIP LOCKED
                      │                 │                      │
                      │                 └── 校验 token ──▶ OIDC 身份提供方
                      │                                        │
                      │               relay (×2) ──────────────┘
                      │                 │
                      │                 └──▶ NATS JetStream ──▶ consumer (×2) ──▶ PostgreSQL
                      │
                      │  monitoring 命名空间：Prometheus ──抓取 :9090──▶ api / relay / consumer
                      │                       Alertmanager ──webhook──▶ alertbridge ──▶ 飞书 / 钉钉
                      └────────────────────────────────────────────────────┘
```

---

## 2. 代码分层

```
cmd/api            组装：读配置 → 初始化链路 → 连数据库 → 认证 → 限流 → 启动 HTTP
  │
internal/httpserver  HTTP 服务器、路由、中间件顺序、角色常量
  │
internal/handler     解析请求、校验参数、游标编解码、把领域错误映射成状态码
  │
internal/service     业务逻辑、事务边界、分页
  │
internal/store       数据访问：Postgres（pgx）、内存实现、TxManager、迁移
  │
internal/domain      模型和领域错误，不依赖任何其他包
```

依赖只能自上而下。横向的支撑包：

| 包 | 内容 | 谁依赖它 |
|---|---|---|
| `internal/auth` | OIDC/JWT 校验、`Principal`、`RequireAnyRole`；`authtest` 在测试里签发 token | httpserver、handler |
| `internal/ratelimit` | GCRA 限流（内存 / Redis）、中间件、指标 | httpserver、cmd/api |
| `internal/outbox` | relay 核心逻辑、NATS 发布器、积压指标 | cmd/relay |
| `internal/consumer` | 消费者通用部分：拉取、去重、事务、重试、死信、重放、积压指标；`WelcomeHandler` 示例 | cmd/consumer |
| `internal/telemetry` | 链路初始化、Prometheus 注册表、HTTP 和连接池指标、日志里加 trace_id | 所有 cmd、httpserver |
| `internal/httpx` | JSON 编解码、统一错误响应体 | handler、中间件 |
| `internal/config` | 从环境变量读取并校验配置 | cmd/api |
| `api` | 内嵌的 OpenAPI 规范和 Swagger UI | httpserver |

几条约定：

- **接口定义在使用方**。`handler.UserService`、`service.UserRepo`、`service.TxManager` 都由调用方声明，实现在下一层。所以 service 的单元测试可以直接用内存实现，不需要数据库。
- **领域错误只有三个**：`ErrNotFound`、`ErrConflict`、`ErrInvalidCursor`。store 负责把驱动错误转成它们（查不到行、唯一约束冲突 23505、非法 UUID 22P02），handler 只认这三个，再转成 404、409、400。其余错误一律返回 500，不向客户端暴露细节。
- **操作者通过 ctx 传递**。认证中间件调用 `auth.WithPrincipal`，同时写入 `domain.WithActor`。service 和 store 用 `domain.ActorFrom(ctx)` 读取，不需要依赖 auth 包。
- **事务通过 ctx 传递**。`TxManager.WithTx(ctx, fn)` 把事务放进 ctx，仓储用 `conn(ctx, pool)` 优先取事务，函数签名不需要改。

---

## 3. 一个请求的完整路径

以 `POST /api/v1/users` 为例。请求先经过集群入口：

| 环节 | 作用 | 可能的结果 |
|---|---|---|
| Envoy Gateway（`overlays/*/gateway.yaml`） | 终结 TLS；`http://` 返回 301 跳转；只有 `/api` 前缀会被转发；删掉客户端自带的 `X-Envoy-External-Address`，写入它认定的客户端地址；整个请求最长 30s | 301、404（路径不在 `/api` 下）、504（超过 30s） |

网关的 30s 要大于应用的处理超时（20s），这样慢请求由应用返回带 `request_id` 的 504，而不是被网关直接断开。

之后进入应用，中间件按下面的顺序执行（`internal/httpserver/router.go`）：

| # | 环节 | 作用 | 可能的结果 |
|---|---|---|---|
| 1 | `otelhttp`（最外层，server.go） | 读取上游 `traceparent`，创建 server span。`/healthz`、`/readyz` 不产生 span | — |
| 2 | `RequestID` | 生成 request_id，写进日志和错误响应体 | — |
| 3 | `realIP`（仅 `HTTP_TRUST_PROXY=true`） | 只用配置的那一个头覆盖 RemoteAddr（k8s 里是 Envoy 写入的 `X-Envoy-External-Address`），其他头一律忽略，防止伪造 IP | — |
| 4 | `observe` | 记录 RED 指标；路由匹配完成后，把 span 重命名为 `POST /api/v1/users` | — |
| 5 | `requestLogger` | 结构化访问日志，自动带 trace_id 和 span_id | — |
| 6 | `recoverer` | 捕获 panic，记录堆栈 | 500 |
| 7 | `Compress` | 客户端带 `Accept-Encoding` 时压缩 JSON / YAML / HTML 响应 | — |
| 8 | 按 IP 限流 | 放在认证之前：带无效 token 的洪水在验签之前就被挡住 | 429 |
| 9 | 认证 | 校验签名、iss、aud、exp，只接受 RS256 / ES256；暂时拉不到公钥时返回 503，而不是让客户端以为 token 无效 | 401 / 503 |
| 10 | 按用户限流 | 按 token 的 `sub` 计数，同一账号换 IP 也绕不过去 | 429 |
| 11 | `Timeout` | 给 ctx 加处理超时（`HTTP_HANDLER_TIMEOUT`，默认 20s） | 504 |
| 12 | `AllowContentType` | 只接受 `application/json` | 415 |
| 13 | `RequireAnyRole(goapi-admin)` | 检查角色 | 403 |
| 14 | 写接口限流 | 只用于写接口，按 `sub` 计数，额度比按用户的总额度更严；放在角色检查之后，没有权限的请求不占额度 | 429 |
| 15 | handler | 限制请求体大小，拒绝未知字段和多个 JSON 对象，校验字段 | 400 / 422 |
| 16 | service | 在一个事务里写入用户和 `user.created` 事件 | 409 |
| 17 | store | 每条 SQL 单独超时（`DB_QUERY_TIMEOUT`），每条 SQL 一个子 span | — |

成功时返回 201 和 `Location` 头。所有错误响应体的格式都一样：`{"error": "...", "request_id": "..."}`。

`/healthz`、`/readyz`、`/api/openapi.yaml`、`/api/docs` 不经过第 8–14 步。

---

## 4. 数据模型

迁移文件在 `internal/store/migrations/`，用 goose 管理，编进二进制。

**users**

| 列 | 类型 | 说明 |
|---|---|---|
| id | uuid | 主键，`gen_random_uuid()` |
| name | text | 1–100 字符（CHECK 约束） |
| email | text | `lower(email)` 上有唯一索引，所以不区分大小写 |
| created_at | timestamptz | 默认 `now()` |

索引 `(created_at DESC, id DESC)` 用于游标分页。

**outbox_events**

| 列 | 说明 |
|---|---|
| id | bigint 自增，relay 按它的顺序认领 |
| aggregate_id、event_type、payload | 事件内容，payload 是 jsonb |
| actor | 操作者，即 token 的 `sub`；系统操作时为空 |
| trace_context | 写入时的 `traceparent`，relay 用它把链路接上 |
| published_at | 投递成功的时间；为空表示还没投递 |
| attempts、last_error | 失败次数和最近一次的错误 |
| dead_at | 不为空表示判定为无法投递（死信），不再重试 |

部分索引只覆盖"未投递并且不是死信"的行，所以 relay 每次轮询的开销很小。插入时触发器调用 `pg_notify('outbox_events')`，唤醒 relay。

**分页**：用 keyset（游标）分页，条件是 `WHERE (created_at, id) < (游标)`，排序 `ORDER BY created_at DESC, id DESC`。翻到多深都只扫描 limit 行，并发插入也不会导致漏读或重复。实际多查一条来判断有没有下一页，不需要 `COUNT`。游标对客户端是不透明的字符串：base64url(JSON) 加上 HMAC-SHA256 签名（密钥 `CURSOR_SECRET`，所有副本共用），校验用常量时间比较，被篡改或伪造的游标返回 400。轮换密钥时把旧密钥放进 `CURSOR_SECRET_PREVIOUS`，旧游标在过渡期内仍然有效。

**迁移规则**：滚动发布期间新旧版本会同时运行，回滚后旧版本也要能正常工作，所以每个迁移都必须向后兼容：

1. 加列：先加可空列或带默认值的列，新代码再开始写入。
2. 删列、改名：先发一个不再读写它的版本，下一个版本再删。
3. 大表建索引：用 `CREATE INDEX CONCURRENTLY`，并在 goose 文件里加 `-- +goose NO TRANSACTION`。

---

## 5. 事件投递（transactional outbox）

**为什么这样做**：如果在业务事务提交之后再直接发消息，进程恰好在两步之间崩溃，就会出现"用户建好了，但事件丢了"。outbox 模式把事件和业务数据放在同一个事务里写进数据库，由单独的 relay 进程负责投递，所以两者要么都成功，要么都不发生。

```
api 事务:  INSERT users ─┐
           INSERT outbox_events ─┴─ COMMIT ──▶ 触发器 pg_notify

relay:     收到通知（或每秒轮询一次）
           BEGIN
           SELECT ... WHERE published_at IS NULL AND dead_at IS NULL
                  ORDER BY id LIMIT 100 FOR UPDATE SKIP LOCKED
           逐条发布到 JetStream，等服务端确认（PubAck）
           UPDATE published_at = now() WHERE id = ANY(已成功的)
           COMMIT
```

| 特性 | 实现方式 |
|---|---|
| 至少投递一次 | 消息发出后、提交前崩溃，这条会被重发。消息带 `Nats-Msg-Id`，JetStream 在去重窗口（默认 2 分钟）内丢掉重复的；超出窗口的重复由消费方按 `Event-Id` 幂等处理 |
| 多实例安全 | `FOR UPDATE SKIP LOCKED`：每个实例认领不同的行。代价是**不保证全局顺序** |
| 暂时性错误 | 例如 NATS 不可用、超时。停在出错的那一条，前面成功的照常提交，退避时间从 1s 逐步加到 30s，一直重试 |
| 永久性错误 | 例如消息过大、事件类型不能作主题名。直接标记 `dead_at`，不再重试，也不挡后面的事件 |
| 链路 | 发布 span 挂在原请求的 trace 下，并写进消息头 |
| 关闭 | 收到 SIGTERM 后处理完当前批次再退出（单批最长 30s） |
| 存活检查 | 主循环 2 分钟没动，`/healthz` 返回 503，Pod 会被重启 |
| 清理 | 已投递超过 `OUTBOX_RETENTION`（默认 7 天）的分批删除；死信保留，等人工处理 |

**消息格式**：主题是 `goapi.events.<事件类型>`，例如 `goapi.events.user.created`；消息体是事件的 JSON。消息头：

| 头 | 内容 |
|---|---|
| `Event-Id` | outbox 表的 id，用来做幂等 |
| `Event-Type` | 例如 `user.created` |
| `Aggregate-Id` | 聚合根 ID，例如用户 ID |
| `Occurred-At` | 事件写入时间，RFC 3339 格式 |
| `Actor` | 操作者，没有时不带这个头 |
| `traceparent` | W3C 链路上下文 |
| `Nats-Msg-Id` | `goapi-outbox-<id>`，JetStream 用它去重 |

**对消费方的要求**：

1. 用 `Event-Id` 做幂等，同一事件可能收到不止一次。
2. 不依赖跨事件的顺序；需要有序时，按 `Event-Id` 或业务版本号判断。
3. 用 `telemetry.NATSHeaderCarrier(msg.Header)` 提取链路上下文，就能接上同一条 trace。不要用 HTTP 的 `propagation.HeaderCarrier`：它会把键规范化成 `Traceparent`，而 NATS 消息头区分大小写。非 Go 的消费方按不区分大小写的方式读取 `traceparent` 即可。

### 示例消费者（`cmd/consumer`）

```
JetStream 持久拉取消费者 goapi-welcome（多副本共享）
  Fetch(10) ─▶ 解析消息头 ─▶ 提取 traceparent，开 consumer span
           ─▶ BEGIN
              INSERT INTO processed_events (consumer, event_id) ON CONFLICT DO NOTHING
                ├─ 0 行：处理过了 → ROLLBACK → Ack（Handler 不执行）
                └─ 1 行：Handler(ctx, tx, event) → COMMIT → Ack
           ─▶ 失败：暂时性 → NakWithDelay(1s, 2s, 4s … 5m)
                    永久性或第 10 次 → 写 consumer_dead_letters → Term
```

| 情况 | 结果 |
|---|---|
| 处理成功，但 Ack 丢了 | 消息重投，去重记录挡住，Handler 不会再执行 |
| 处理到一半崩溃 | 事务没提交，AckWait（30s）后重投，从头再来 |
| 两个副本同时拿到同一条重投消息 | 后插入去重记录的一方会等前一方提交，然后得到 0 行，判定为重复 |
| 处理时间超过 AckWait | 每 AckWait/3 发一次 InProgress 心跳，不会被重投给别的副本 |
| 死信写入失败（数据库故障） | 不 Term，按退避稍后重试，保证不丢 |
| 消息缺少 Event-Id 或 Event-Type | 无法去重，直接 Term 并记录错误日志 |

JetStream 消费者的 `MaxDeliver` 设为不限，重试次数由处理器自己控制。否则处理器恰好在最后一次尝试时崩溃，消息会被 JetStream 静默丢弃。去重记录保留 `CONSUMER_DEDUP_RETENTION`（默认 14 天），启动时会校验它比流的保留时间长。

**换成 Kafka**：实现 `outbox.Publisher` 接口（`Publish(ctx, Message) error`，必须在 broker 确认持久化后才返回 nil），其余部分不用改。

---

## 6. 安全

| 方面 | 做法 |
|---|---|
| 认证 | OIDC access token。校验签名（JWKS 缓存，遇到新的 kid 会自动刷新）、`iss`、`aud`、`exp`；只接受 RS256 / ES256，拒绝 `none` 和 HS256。token 只从 Authorization 头读取，不支持放在查询参数里 |
| 授权 | 两个角色：`goapi-viewer` 只读，`goapi-admin` 读写。角色从哪个声明读取由 `OIDC_ROLES_CLAIM` 指定。目前只有角色级控制，没有"只能操作自己的数据"这类资源级检查 |
| 分页游标 | HMAC-SHA256 签名，常量时间比较；生产环境必须配置所有副本共用的密钥 |
| 本地开发 | `AUTH_DISABLED=true` 时所有请求视为本地管理员。`APP_ENV=prod` 时配置校验会拒绝启动 |
| 客户端 IP | 只信任一个指定的头，避免 `True-Client-IP` 等头被伪造后绕过限流 |
| 输入 | 请求体大小限制、拒绝未知字段、拒绝多个 JSON 对象、字段长度和格式校验 |
| 错误 | 内部错误不返回给客户端；数据库连接串、webhook 地址不写进日志和错误信息 |
| 容器 | distroless 镜像，非 root（UID 65532），根文件系统只读，去掉全部 capabilities，seccomp RuntimeDefault；命名空间启用 Pod Security `restricted` |
| 网络 | 指标端口、relay、alertbridge 只允许 `monitoring` 命名空间访问（NetworkPolicy，需要 CNI 支持）；业务端口只允许 Envoy Gateway 的代理访问；网关只暴露 `/api` |
| 凭据 | 密钥放在 Kubernetes Secret 里，由 CD 从 GitHub Environment secrets 写入；不进仓库。生产建议改用 External Secrets |
| 文档 | `DOCS_ENABLED` 在 production 默认关闭 |

---

## 7. 可观测性

| 信号 | 来源 | 去向 |
|---|---|---|
| 链路 | otelhttp（HTTP）、otelpgx（SQL）、redisotel（Redis 限流判定）、relay 的发布 span | OTLP → Jaeger 或 OpenTelemetry Collector |
| 指标 | 各进程的 `:9090/metrics` | Prometheus |
| 日志 | slog：生产是 JSON，开发是文本；带 request_id、trace_id、span_id | stdout |
| 剖析 | `PPROF_ENABLED=true` 时在指标端口开启 `/debug/pprof/` | 手动抓取 |

**主要指标**

| 指标 | 含义 |
|---|---|
| `http_requests_total`、`http_request_duration_seconds` | 按方法、路由模板、状态码统计的请求量和延迟。`route` 标签用路由模板，匹配不到的统一记为 `unmatched`，防止基数爆炸 |
| `db_pool_*` | 连接池使用情况；`db_pool_empty_acquire_total` 增长说明连接不够用 |
| `http_rate_limit_decisions_total` | 限流判定：allowed、limited、error |
| `outbox_pending_events`、`outbox_oldest_pending_age_seconds`、`outbox_dead_events` | 投递积压、最早事件的等待时间、死信数 |
| `outbox_event_delay_seconds` | 从写入到投递成功的延迟 |
| `consumer_messages_total`、`consumer_pending_messages`、`consumer_dead_letters`、`consumer_event_delay_seconds` | 消费结果（processed、duplicate、retry、dead 等）、积压、死信、端到端延迟 |

**告警链路**

```
Prometheus（alerts.yml 共 18 条）
   └─▶ Alertmanager：按 alertname + service 分组，按级别路由，抑制次生告警
          ├─ critical：飞书 + 钉钉，全天候，未恢复时每 1h 提醒一次
          └─ warning ：飞书，只在工作日 09:00–19:00（Asia/Taipei）推送，每 12h 提醒一次
                └─▶ alertbridge ──▶ 飞书卡片 / 钉钉 markdown（附处理手册、Prometheus、静默链接）
```

SLO 是 99.9% 可用性（5xx 比例），用多窗口燃烧率告警：1h 和 5m 窗口同时超过 14.4 倍发 critical，6h 和 30m 窗口同时超过 6 倍发 warning。`Watchdog` 告警永远处于触发状态，上线后应接到死信监控服务，这样 Prometheus 或 Alertmanager 自己挂了也能发现。

---

## 8. 从提交到上线

```
PR ─▶ CI：Lint ─ Test ─ Vulnerability scan ─ Configs ─ OpenAPI breaking（仅 PR）
                                  │
push main ─▶ CI 通过 ─▶ 构建 4 个镜像并推送 GHCR（tag: sha-<7位>）
                                  │
                                  └─▶ Deploy：自动部署 staging
打 v* 标签 ─▶ CI 通过 ─▶ 同上 ─▶ Deploy：production（需要在 GitHub Environment 上审批）
```

**CI**（`.github/workflows/ci.yml`）

| Job | 内容 |
|---|---|
| Lint | `go mod tidy` 无差异、gofmt、go vet、Redocly 检查 OpenAPI、golangci-lint |
| Test | 启动 Postgres、Redis、NATS JetStream，跑 `go test -race -p 1 -shuffle=on`（集成测试共用一个数据库，所以串行），包括 outbox → JetStream → consumer 的端到端测试 |
| Vulnerability scan | 用最新补丁版本的 Go 跑 govulncheck |
| Configs | promtool 检查配置并跑告警规则单元测试、amtool、Grafana JSON、`docker compose config`、kustomize 渲染加 kubeconform 校验（自定义资源的 schema 从 Envoy Gateway、Gateway API、Prometheus Operator 的官方 CRD 现场生成） |
| OpenAPI breaking | 只在 PR 上运行，用 oasdiff 和目标分支对比，发现不兼容的接口改动 |
| Docker | 前面全部通过后构建 api、alertbridge、relay、consumer；PR 只构建不推送 |

**接口契约**：`api/openapi.yaml` 是手写的，由两个测试保证和代码一致。`TestOpenAPIContract` 用真实的路由和处理器发 18 个请求，逐个按规范校验请求和响应。`TestRoutesMatchSpec` 对照路由表，检查有没有路由没写进文档、或者文档里写了不存在的接口。改接口时，先改规范，再改代码。

**CD**（`.github/workflows/deploy.yml`）的每次部署：

1. 确认四个镜像都已经在 GHCR 里。
2. 从 Environment secrets 写入密钥，在工作区把镜像版本写进 overlay（不提交回仓库）。
3. `kubectl diff` 预览变更。
4. **只应用带 `pre-deploy` 标签的资源**（命名空间、配置、密钥、迁移 Job），等迁移完成。迁移失败就停在这里，Deployment 不会被改动。
5. 应用全部资源，等 api、alertbridge、relay、consumer 滚动完成。
6. 冒烟测试：集群内 `/readyz` 返回 200，不带 token 的 API 请求返回 401（证明认证开着）；配置了 `APP_URL` 时再从公网入口测一次。
7. 第 5 或第 6 步失败时，自动 `rollout undo`。**数据库迁移不会回滚**，这就是第 4 节要求迁移向后兼容的原因。

staging 和 production 部署的是同一个 `sha-<7位>` 镜像。同一环境的部署串行执行，不会中途取消。

---

### 压测

`loadtest/` 下有四个 k6 脚本：冒烟、负载（固定速率验证 SLO）、压力（逐步加压观察 HPA）、限流验证。阈值和告警对齐：P99 < 1s、错误率 < 0.1%。CI 只用 `k6 inspect` 检查脚本能否解析，不实际压测。压测只对本地或 staging 运行；单台压测机会被限流挡住，跑负载和压力测试前要关掉或调高限流，跑限流验证时则保持原样。

---

## 9. 运行时行为

**启动顺序（api）**：读取并校验配置 → 初始化链路 → 连接数据库并 Ping → （本地）执行迁移 → OIDC discovery → 连接 Redis → 启动业务端口 `:8080` 和指标端口 `:9090`。任何一步失败都直接退出，不会带着错误配置跑起来。

**优雅关闭（api）**：收到 SIGTERM →`/readyz` 立刻返回 503 → 等 `SHUTDOWN_DRAIN_DELAY`（10s），让负载均衡把实例摘掉 → `Shutdown` 等待在途请求，最多 `SHUTDOWN_TIMEOUT`（20s） → 关闭指标服务 → 关闭 Redis 和数据库连接池 → 刷出剩余的 span。总共不超过 30s，小于 Pod 的 `terminationGracePeriodSeconds`（40s）。

**探针**

| 探针 | 路径 | 检查内容 | 为什么 |
|---|---|---|---|
| startup | `/healthz` | 进程本身 | 启动时要连数据库，给足 60s |
| liveness | `/healthz` | 进程本身，**不检查数据库** | 数据库故障时重启 Pod 没有用，反而会造成连接风暴 |
| readiness | `/readyz` | Ping 数据库，并检查是否正在关闭 | 依赖不可用或正在关闭时摘掉流量 |

**容量**：`HPA maxReplicas × DB_MAX_CONNS` 必须小于 Postgres 的 `max_connections`（前面有 PgBouncer 时一起算）。没有配置 Redis 时，限流按实例计数，实际总额度等于"额度 × 副本数"。`GOMEMLIMIT` 设为容器内存 limit 的约 90%，改 limit 时要同步修改。

---

## 10. 设计取舍

| 决定 | 理由 | 代价 |
|---|---|---|
| Chi + 标准库 `net/http` | 中间件和 handler 都是标准接口，生态可直接复用，迁移成本低 | 参数绑定和校验要自己写 |
| pgx 手写 SQL，暂不用 sqlc / ORM | 依赖少，SQL 一目了然 | 查询变多后维护成本上升；那时再引入 sqlc |
| 事务通过 ctx 传递 | 仓储方法签名不变，嵌套调用自动并入外层事务 | 事务边界不在函数签名上，要靠约定：fn 里必须用传入的 ctx |
| Keyset 分页 | 深翻页不变慢，并发写入不漏不重 | 不能跳到第 N 页，也不能直接给出总数 |
| Transactional outbox | 数据和事件一致，消息系统故障不影响写入 | 多一个进程；只保证至少一次，不保证全局顺序 |
| 限流 fail open | 限流是保护手段，它自己故障不应该让整个 API 不可用 | Redis 故障期间限流失效（有告警） |
| liveness 不检查数据库 | 避免数据库抖动时所有 Pod 被一起重启 | 进程活着但功能异常时只摘流量、不重启 |
| 不设 CPU limit | 避免 CFS 节流造成的延迟毛刺 | 依赖准确的 CPU requests 和节点容量规划 |
| 入口用 Gateway API + Envoy Gateway | ingress-nginx 已停止维护、不再有安全补丁；Gateway API 是 Kubernetes 官方的后继标准 | 部分能力（客户端 IP 识别、请求体限制）要用 Envoy Gateway 自己的扩展资源，换实现时要重写这部分 |
| OpenAPI 手写，加契约测试 | 文档和代码不一致时测试直接失败；比注释生成更可靠 | 改接口要同时改两处 |
| 迁移用单独的 Job，应用不在启动时迁移 | 多副本不会并发迁移；迁移失败不会更新 Deployment | 发布流程多一步；迁移必须向后兼容 |

---

## 11. 已知限制和后续工作

- 代码、工作流和清单都还没有在真实环境里验证过（见文首）。
- 授权只到角色级，没有资源级（例如"只能修改自己创建的数据"）的检查。
- 只实现了 NATS 发布器。示例消费者的去重表、死信表和业务表和 API 在同一个数据库里；真实场景下消费方通常是独立服务，有自己的数据库和迁移。
- 压测脚本还没有实际跑过，HPA 阈值（CPU 70%）、资源 requests 和延迟阈值都需要用真实的压测结果来校准。
- 拉不到公钥时的 503 判断依据是"这次校验期间向身份提供方的请求失败了"。极少数情况下，并发的另一个请求恰好拉取失败，会让一次普通的 401 被报成 503。
- 没有配置 `CURSOR_SECRET` 时每次启动随机生成密钥：重启后旧游标失效，多副本之间互不认可。所以生产环境（`APP_ENV=prod`）强制要求配置。
- 网关没有限制请求体大小，超大请求体由应用拒绝（400，上限 1 MiB）。需要在入口挡住时，用 Envoy Gateway 的 BackendTrafficPolicy。
- 从私有网段直接访问网关的请求，Envoy 会当成内部请求，可能不写 `X-Envoy-External-Address`；这些请求退回用 Envoy Pod 的 IP，共用一份 IP 限流额度。
- 还没有带 token 的冒烟测试（不同身份提供方获取 token 的方式不同）。
- 若干第三方镜像和工具使用 `latest`（Jaeger、Prometheus、Grafana、kubeconform、oasdiff、Redocly、golangci-lint），CI 和生产环境应固定版本。
- 占位值需要替换：`your-org`、`example.com` 域名、`OIDC_ISSUER_URL`、RUNBOOK 链接里的仓库地址。
