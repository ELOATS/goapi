# goapi 告警处理手册

阈值是模板默认值，请根据实际流量和 SLO 调整 `deploy/prometheus/alerts.yml`。

## 通用：静默与确认
- 消息里的「静默」链接会打开 Alertmanager 并预填匹配条件，填写时长和原因即可
- 计划内维护提前建静默：
  ```bash
  amtool --alertmanager.url=http://localhost:9093 silence add service=goapi --duration=2h --comment="发布 v1.2"
  ```
- warning 只在工作时间（周一至周五 09:00–19:00）推送，critical 全天候

## Watchdog
这条告警**永远在触发**，用来证明 Prometheus → Alertmanager 链路是活的。
默认路由到 `null` 接收器（丢弃）。上线后把它路由到一个死信监控服务
（例如 healthchecks.io、Grafana OnCall、PagerDuty 的 heartbeat），
设置"超过 5 分钟没收到就报警"：这样 Prometheus 或 Alertmanager 自己挂了也能知道。

## 发布与回滚
- 发布：main 合并后自动上 staging；打 `v*` 标签后上 production（GitHub 上审批）
- 部署失败（滚动超时或冒烟测试失败）时工作流会**自动回滚 Deployment**，但**不会回滚数据库迁移**
- 手动回滚到某个旧版本：Actions → Deploy → Run workflow，environment 选 production，ref 填旧标签（如 `v1.2.2`）
  - 旧版本的迁移文件比数据库少，goose 不会做任何变更；旧代码要能在新表结构上运行
- 紧急回滚到上一版本（不经过 CI）：`make k8s-undo K8S_NS=goapi`
- 迁移的向后兼容规则（滚动期间新旧版本同时运行，回滚后旧版本也要能跑）：
  1. 加列：先加可空列或带默认值的列，新代码再开始写
  2. 删列 / 改名：先发一个不再读写它的版本，下一个版本再删
  3. 大表加索引用 `CREATE INDEX CONCURRENTLY`（goose 文件里加 `-- +goose NO TRANSACTION`）

## 外部访问不了（入口排查）
没有专门的告警：Prometheus 在集群内直接抓 Pod，入口坏了指标看起来仍然正常。通常由用户反馈或 CD 冒烟测试的公网检查发现。
1. 看入口资源状态：
   ```bash
   kubectl -n goapi get gateway,httproute,clienttrafficpolicy
   kubectl -n goapi describe gateway goapi      # Conditions 里看 Accepted / Programmed 的原因
   kubectl -n goapi describe httproute goapi-api
   ```
2. Gateway 没有 `Programmed`：GatewayClass 名称不对，或 Envoy Gateway 没装好（`kubectl -n envoy-gateway-system get pods`）
3. HTTPRoute 没有 `ResolvedRefs`：引用的 Service `goapi-api` 或端口不对
4. 证书问题（浏览器报证书错误、HTTPS 握手失败）：`kubectl -n goapi get certificate,certificaterequest`，
   以及 cert-manager 是否开启了 Gateway API 支持
5. 返回 503 / `no healthy upstream`：后端 Pod 都没就绪（看 `/readyz`），或 NetworkPolicy 挡住了 Envoy：
   确认 Envoy 代理 Pod 所在命名空间和 `api-networkpolicy.yaml` 里放行的一致
6. 返回 504 且响应体没有 `request_id`：网关超时先到了（HTTPRoute 的 `timeouts.request`），应用还没返回

## 认证接口返回 503（authentication temporarily unavailable）
api 拉不到身份提供方的公钥（JWKS），无法判断 token 是否有效。已经缓存的公钥照常可用，所以通常只有一部分请求受影响；
身份提供方刚轮换过签名密钥时，所有新 token 都会受影响。5xx 比例上升会触发错误预算燃烧率告警。
1. 日志里搜 `cannot verify token: signing keys unavailable`，`err` 里有具体原因（超时、DNS、502 等）
2. 从集群里访问身份提供方：
   ```bash
   kubectl -n goapi run jwks-check --rm -i --restart=Never --image=curlimages/curl -- \
     curl -sS -o /dev/null -w '%{http_code}\n' <OIDC_ISSUER_URL>/.well-known/openid-configuration
   ```
   注意 NetworkPolicy 只限制了入站，出站没有限制；如果你们加了 Egress 规则，确认放行了身份提供方
3. 身份提供方本身故障：等它恢复，api 不需要重启，下一次请求会自动重新拉取
4. 只有这个集群访问不了：检查出口网络、代理、DNS

## GoapiInstanceDown
Prometheus 抓不到实例的 `/metrics`（端口 9090）。
1. 进程是否存活：`kubectl get pods` / `docker compose ps`
2. 进程活着但抓不到：检查 `METRICS_ADDR`、网络策略、Service 端口
3. 进程挂了：看上一次退出日志（`kubectl logs --previous`），常见原因是数据库连不上或迁移失败

## GoapiTargetMissing
Prometheus 里一个 goapi 目标都没有，**其他告警全部失效**。检查抓取配置和服务发现（k8s 的 ServiceMonitor / annotations）。

## GoapiCrashLooping
30 分钟内重启超过 2 次。
1. `kubectl describe pod` 看是否 `OOMKilled` → 调高内存 limit，或用 pprof heap 查内存
2. 日志里搜 `panic recovered` 之外的 panic（未被中间件捕获的 goroutine panic 会直接让进程退出）
3. 启动阶段失败：`connect postgres`、`migrate` 相关错误

## GoapiErrorBudgetFastBurn / GoapiErrorBudgetSlowBurn
5xx 比例消耗 99.9% SLO 错误预算的速度过快。
1. Grafana「5xx 错误率（按路由）」定位是哪个路由
2. 日志过滤 `level=ERROR`，用 `trace_id` 到 Jaeger 看具体链路
3. 是否刚发布 → 优先回滚
4. 是否数据库问题 → 看「数据库连接池」一行

## GoapiHighLatencyP99
某路由 P99 持续超过 1s。
1. Jaeger 按服务 goapi + 该操作名筛选，按耗时排序
2. 时间主要花在 SQL span → `EXPLAIN ANALYZE`、检查索引
3. 时间主要花在等连接 → 见下面连接池告警
4. CPU 高 → `go tool pprof http://<host>:9090/debug/pprof/profile`（需 `PPROF_ENABLED=true`）

## GoapiDBPoolSaturated / GoapiDBPoolWaiting
连接池接近用满或请求在排队等连接。
1. Postgres 侧看谁占着连接：
   ```sql
   SELECT pid, state, now() - xact_start AS xact_age, query
   FROM pg_stat_activity WHERE datname = current_database()
   ORDER BY xact_age DESC NULLS LAST LIMIT 20;
   ```
2. 长事务 / `idle in transaction` → 找到代码里没结束的事务
3. 确实是流量增长 → 调大 `DB_MAX_CONNS`，但要保证 `DB_MAX_CONNS × 副本数 < max_connections`

## GoapiDBAcquireCanceled
请求在等连接期间超时，用户已经收到错误。按 GoapiDBPoolSaturated 处理，优先级更高。

## GoapiGoroutineLeak
goroutine 数持续超过 10000。
```bash
curl -s "http://<host>:9090/debug/pprof/goroutine?debug=1" | head -50
```
常见原因：没有 ctx 取消的后台 goroutine、未关闭的 `resp.Body`、阻塞的 channel 发送。

## GoapiRateLimiterErrors
限流器判定失败，按设计**放行所有请求**（fail open），期间限流不生效。
1. 检查 Redis：`redis-cli -u "$REDIS_URL" ping`、内存是否打满（`INFO memory`）、是否在主从切换
2. 应用日志搜 `rate limiter failed`，看是连接失败还是超时（单次判定超时 200ms）
3. 如果正在被攻击且 Redis 短时间恢复不了：临时去掉 `REDIS_URL` 并重启，改用每个 Pod 单独计数

## 429 突然增多（没有告警，但用户反馈被限流）
1. Grafana「限流」一行看是 `ip` 还是 `user` 规则在拒绝
2. `ip` 规则：确认 `HTTP_TRUST_PROXY=true`，并且 `HTTP_CLIENT_IP_HEADER`（k8s 里是 `X-Envoy-External-Address`）
   带的是真实客户端地址：看应用访问日志里的 `remote_ip`。如果全是同几个地址（Envoy Pod 或云负载均衡的 IP），
   所有人会共用一份 IP 额度、一起被限流，按 `overlays/*/gateway.yaml` 里 ClientTrafficPolicy 的注释配置客户端 IP 识别
3. 正常业务确实需要更高额度：调大 `RATE_LIMIT_*`（改 config.env 后重新部署即可滚动生效）

## GoapiRelayAllDown
没有正常运行的 relay，事件只会在 `outbox_events` 里积压，**不会丢**，恢复后会补发。
1. `kubectl -n goapi get pods -l app.kubernetes.io/name=goapi-relay`，看日志里的启动错误
2. 常见原因：`connect nats`（NATS 地址或凭据）、`create or update stream`（流配置与已有的冲突，例如改了副本数）、数据库连不上

## GoapiOutboxLagging
最早的待投递事件等待超过 5 分钟。
1. 看最早几条的失败原因：
   ```sql
   SELECT id, event_type, attempts, last_error, created_at
   FROM outbox_events WHERE published_at IS NULL AND dead_at IS NULL
   ORDER BY id LIMIT 10;
   ```
2. `last_error` 是超时或 `no responders`：NATS / JetStream 不可用，恢复后自动追上
3. 只有一条 `attempts` 一直涨、其他正常：这条事件卡住了队头。确认重试无望后手动转为死信：
   `UPDATE outbox_events SET dead_at = now() WHERE id = <id>;`
4. 都没有错误但积压在涨：投递速度跟不上写入，增加 relay 副本或调大 `RELAY_BATCH_SIZE`

## GoapiOutboxDeadEvents
有事件被判定为无法投递（消息过大、事件类型不能作为主题等），已停止重试。
1. 查看：`SELECT id, event_type, last_error, created_at FROM outbox_events WHERE dead_at IS NOT NULL;`
2. 修复原因（例如调大 NATS 的 max_payload，或修正代码里的事件类型）后重新投递：
   `UPDATE outbox_events SET dead_at = NULL, attempts = 0, last_error = NULL WHERE id IN (...);`
3. 确认不需要了：`DELETE FROM outbox_events WHERE dead_at IS NOT NULL AND id IN (...);`

## GoapiConsumerAllDown
没有正常运行的消费者，消息留在 JetStream 里，**不会丢**（流保留 `NATS_STREAM_MAX_AGE`，默认 7 天），恢复后会继续处理。
1. `kubectl -n goapi get pods -l app.kubernetes.io/name=goapi-consumer`，看日志里的启动错误
2. 常见原因：`create consumer ... on stream`（relay 还没创建流，或 NATS 版本低于 2.10 不支持 FilterSubjects）、
   `invalid config`（例如 `CONSUMER_DEDUP_RETENTION` 不比 `NATS_STREAM_MAX_AGE` 长）、数据库连不上

## GoapiConsumerLagging
积压超过 1000 条并持续 10 分钟。
1. Grafana「事件消费」一行：`retry` 很多 → 处理在失败，看日志里的 `event handling failed`
2. 处理都成功但跟不上：增加 consumer 副本（同一个持久消费者，JetStream 会把消息分给各副本），
   或调大 `CONSUMER_BATCH_SIZE`
3. 单条处理耗时高：看 Jaeger 里 `consume user.created` 的慢 span

## GoapiConsumerDeadLetters
有消息重试 `CONSUMER_MAX_DELIVER` 次（默认 10）仍失败，或被判定为永久错误，已停止重试。
1. 查看原因：
   ```sql
   SELECT id, event_id, event_type, attempts, error, created_at
   FROM consumer_dead_letters WHERE consumer = 'goapi-welcome' ORDER BY id;
   ```
2. 修复原因（代码 bug、下游服务、数据问题），发布修复后的版本
3. 重放：
   ```bash
   kubectl -n goapi exec deploy/goapi-consumer -- /app replay-dead-letters -limit 100
   ```
   重放会重新发布到原主题，同一主题上其它已处理过的消费者会按 Event-Id 去重，不受影响
4. 确认不需要处理：`DELETE FROM consumer_dead_letters WHERE id IN (...);`
