# goapi 上线前检查清单

按顺序做，前一阶段没通过不要进入下一阶段。每一项后面的路径是需要改或需要看的文件。架构和设计理由见 [ARCHITECTURE.md](ARCHITECTURE.md)，告警处理见 [RUNBOOK](../deploy/RUNBOOK.md)。

> 这个模板是在没有 Go 工具链、没有集群的环境里写出来的：**所有 Go 代码都没有编译过，CI/CD、k8s 清单、压测脚本都没有实际运行过**。下面第 1、2 阶段就是用来把这些补上的，不能跳过。

图例：🔴 不做就上不了线　🟡 上线前应该做　⚪ 可以上线后再做

---

## 阶段 1：本地能编译、测试全部通过

逐项的命令和预期结果见 [LOCAL_VERIFICATION.md](LOCAL_VERIFICATION.md)。

本地环境问题（测试程序被拦截、代理导致 502、`go.sum` 丢失、端口冲突、镜像架构不对等）的症状和解决办法见 README「本地开发常见问题」。

- [x] 🔴 `go mod tidy`，生成并提交 `go.sum`（CI 的 Lint 和 Dockerfile 的 `COPY go.sum` 都依赖它）
- [x] 🔴 `gofmt -w . && go vet ./...` 没有输出
- [ ] 🔴 `go test -race ./...` 通过（不需要数据库）。Windows 上测试程序被应用控制策略拦截时，用 `.\scripts\test-in-docker.ps1`
- [ ] 🔴 `golangci-lint run ./...` 没有问题，`govulncheck ./...` 没有会被调用到的漏洞（命令见 README「代码检查」）
- [ ] 🟡 加了 `.gitattributes` 之后执行一次 `git add --renormalize .` 并提交，统一已有文件的换行符
- [ ] 🔴 `make db-up && make test-integration` 通过（Windows：`.\scripts\test-in-docker.ps1 -Integration`）（Postgres、Redis、NATS 集成测试，包括 outbox → JetStream → consumer 的端到端测试）
- [ ] 🔴 修掉编译错误。以下 API 是凭记忆写的，没有核对文档，**最可能出错**：
  | 包 | 写法 | 文件 |
  | --- | --- | --- |
  | nats.go/jetstream | `CreateOrUpdateStream`、`CreateOrUpdateConsumer`、`WithMsgID`、`Stream.GetMsg`、`Consumer.Fetch`、`FetchMaxWait`、`MsgMetadata.NumDelivered`、`ConsumerInfo.NumPending/NumAckPending/NumRedelivered`、`ConsumerConfig.FilterSubjects` | `internal/outbox/nats.go`、`internal/consumer/*.go` |
  | nats.go/jetstream | `jetstream.Msg` 能满足自定义的 `consumer.Message` 接口 | `internal/consumer/processor.go` |
  | kin-openapi | `doc.Paths.Map()`（新版写法）；去掉 `servers` 后路由匹配接受任意 host | `internal/httpserver/contract_test.go`、`api/api_test.go` |
  | go-oidc | `oidc.StaticKeySet`、`oidc.NewVerifier` | `internal/auth/verifier.go` |
  | otelpgx | `otelpgx.NewTracer()` | `cmd/api/main.go` |
- [ ] 🔴 `docker compose up --build` 能起来，11 个服务都正常
- [ ] 🟡 `make check-rules && make test-rules && make check-alertmanager`（promtool、amtool 第一次实际校验）
- [ ] 🟡 `make loadtest-smoke` 全部通过
- [ ] 🟡 手动走一遍：创建用户 → `welcome_notifications` 里出现记录 → Jaeger 里同一条 trace 从 HTTP 请求一直到 `consume user.created`
- [ ] 🟡 `make alert-test`，在 `docker compose logs alertbridge` 里看到 dry-run 输出

- [ ] 🟡 用 kind 在本地部署一遍（`scripts/kind.ps1`，见 LOCAL_VERIFICATION 第 12 节）：`check` 和 `check -Disruption` 全部通过。阶段 5、6 里大部分 K8s 层面的问题能在这里提前发现

## 阶段 2：替换占位值

### 必须改（🔴）

| 占位值 | 改成 | 位置 |
| --- | --- | --- |
| 模块路径 `example.com/goapi` | 你们的仓库路径，例如 `github.com/<org>/goapi` | `go.mod`，以及所有 `.go` 文件的 import（全局替换） |
| 镜像地址 `ghcr.io/your-org/goapi/...` | 实际仓库（GHCR 路径必须小写） | `deploy/k8s/base/kustomization.yaml`、`overlays/staging/kustomization.yaml`、`overlays/production/kustomization.yaml` |
| production 镜像版本 `v0.1.0` | 第一次发布的版本 | `overlays/production/kustomization.yaml` |
| `OIDC_ISSUER_URL=https://idp.example.com/realms/goapi` | 身份提供方的 issuer（必须和 discovery 文档完全一致，包括末尾斜杠） | `deploy/k8s/base/config.env` |
| `OIDC_AUDIENCE`、`OIDC_ROLES_CLAIM` | 按身份提供方实际情况 | `deploy/k8s/base/config.env` |
| 域名 `api.example.com`、`staging-api.example.com` | 实际域名（Gateway 的两个监听器和两个 HTTPRoute 里各有一处） | `overlays/*/gateway.yaml` |
| 处理手册链接 `github.com/your-org/goapi/...` | 实际仓库地址 | `deploy/prometheus/alerts.yml`（改完重新生成 `tests/alerts_test.yml` 里的预期值，或者同步修改，否则告警规则测试失败）、`deploy/alertmanager/test-alert.json` |

### 按环境确认（🟡）

| 配置 | 默认值 | 位置 |
| --- | --- | --- |
| GatewayClass | `eg`（Envoy Gateway 快速安装的默认名） | `overlays/*/gateway.yaml` |
| Envoy 代理所在命名空间 | `envoy-gateway-system`（API 的 NetworkPolicy 只放行这里） | `deploy/k8s/base/api-networkpolicy.yaml` |
| cert-manager issuer | `letsencrypt-prod` | `overlays/*/gateway.yaml` |
| 监控所在的命名空间 | `monitoring`（NetworkPolicy 只放行这个命名空间） | `deploy/k8s/base/` 下 4 个 NetworkPolicy |
| ServiceMonitor 的 `release` 标签 | `kube-prometheus-stack` | `overlays/production/servicemonitor.yaml` |
| OTLP Collector 地址 | `http://otel-collector.monitoring.svc:4318` | `overlays/*/kustomization.yaml` |
| 告警时区和工作时间 | Asia/Taipei，工作日 09:00–19:00 | `deploy/alertmanager/alertmanager.yml`、`deploy/k8s/base/alertbridge.yaml` |
| JetStream 流副本数 | 1（3 节点 NATS 集群改为 3） | `deploy/k8s/base/config.env` |
| PgBouncer 事务池模式 | 如果有，设 `RELAY_LISTEN=false` | `deploy/k8s/base/config.env` |
| 前面还有云负载均衡 | 让 Envoy 拿到真实客户端 IP（TCP 层保留源地址、X-Forwarded-For 或 PROXY protocol），否则所有人共用一份 IP 限流额度 | `overlays/*/gateway.yaml` 里的 ClientTrafficPolicy |
| CI 里校验 k8s 清单用的版本 | `-kubernetes-version 1.33.0` | `.github/workflows/ci.yml` |

### 固定版本（🟡）

以下工具和镜像还在用 `latest`，新版本发布可能让 CI 突然变红或本地环境行为变化：

| 工具 / 镜像 | 位置 |
| --- | --- |
| golangci-lint（`version: latest`） | `.github/workflows/ci.yml` |
| `grafana/k6`、`ghcr.io/yannh/kubeconform`、`tufin/oasdiff`、`@redocly/cli`、`govulncheck` | `.github/workflows/ci.yml`、`Makefile` |
| `jaegertracing/jaeger`、`prom/prometheus`、`grafana/grafana`、`prom/alertmanager` | `docker-compose.yml` |
| `prom/prometheus:v3.5.0`、`prom/alertmanager:v0.28.1` | `.github/workflows/ci.yml`、`Makefile`（**我估计的版本号，没有核实是否存在**） |
| docker 系列 actions、`upload-artifact@v5` | `.github/workflows/ci.yml`（没有核实主版本号；Dependabot 会提升级 PR） |

## 阶段 3：外部依赖准备好

- [ ] 🔴 **PostgreSQL 13+**（托管服务）：建库、建用户；staging 和 production 分开
- [ ] 🔴 **连接数够用**：`HPA maxReplicas (10) × DB_MAX_CONNS (10)` + relay `2 × 4` + consumer `2 × 4` + 迁移 Job，要小于 `max_connections`
- [ ] 🔴 **身份提供方**：
  - [ ] 创建角色 `goapi-viewer`、`goapi-admin`，分配给用户或服务账号
  - [ ] access token 的 `aud` 包含 `OIDC_AUDIENCE`（Keycloak 要加 audience mapper）
  - [ ] 角色出现在 `OIDC_ROLES_CLAIM` 指定的声明里
  - [ ] 集群能访问 `<issuer>/.well-known/openid-configuration`（启动时 discovery 失败 Pod 起不来）
- [ ] 🔴 **NATS 2.10+，开启 JetStream**（consumer 的 `FilterSubjects` 需要 2.10）；production 必须配 `NATS_URL`，否则 relay 拒绝启动
- [ ] 🟡 **Redis**：多副本共享限流计数。不配也能跑，但实际额度是"额度 × 副本数"
- [ ] 🔴 **集群里已安装**：
  - [ ] Gateway API CRD（standard channel）
  - [ ] Envoy Gateway，确认 GatewayClass 名称和代理 Pod 所在命名空间
  - [ ] cert-manager，并**开启 Gateway API 支持**（默认不开，不开不会给 Gateway 签证书）
  - [ ] metrics-server（HPA 需要）
- [ ] 🟡 集群里已有 ingress-nginx 的话：它已停止维护、不再有安全补丁，新服务不要再接入；可以和 Gateway 并存，逐步迁移
- [ ] 🟡 **集群里已安装**：Prometheus（Operator 或基于注解抓取）、Alertmanager、OpenTelemetry Collector 或 Jaeger、Grafana
- [ ] 🟡 CNI 支持 NetworkPolicy（Calico、Cilium 等）。不支持的话策略不会报错，但也不生效
- [ ] 🔴 GHCR 包是私有的话：给集群配 imagePullSecret（当前清单里**没有**），或者把包设为对集群可见

## 阶段 4：GitHub 仓库设置

- [ ] 🔴 Settings → Environments 新建 `staging` 和 `production`，分别配置：
  | 名称 | 类型 | 必填 |
  | --- | --- | --- |
  | `KUBECONFIG_B64` | secret | 是（建议用只能操作对应命名空间的 ServiceAccount） |
  | `DATABASE_URL` | secret | 是 |
  | `CURSOR_SECRET` | secret | 是。分页游标的签名密钥，至少 32 字节，`openssl rand -base64 48` 生成；staging 和 production 各用一个。没有它 Pod 拒绝启动 |
  | `CURSOR_SECRET_PREVIOUS` | secret | 否（只在轮换游标密钥时填旧值） |
  | `NATS_URL` | secret | production 是 |
  | `REDIS_URL` | secret | 否 |
  | `FEISHU_WEBHOOK_URL`、`FEISHU_SECRET`、`DINGTALK_WEBHOOK_URL`、`DINGTALK_SECRET` | secret | 否（不填就不推送告警） |
  | `APP_URL` | variable | 否（冒烟测试用公网地址再测一次） |
- [ ] 🔴 production：勾选 **Required reviewers**；Deployment branches and tags 只允许 `v*` 标签
- [ ] 🟡 main 分支保护：要求 CI 全部通过才能合并
- [ ] 🟡 Actions 的 workflow 权限允许写 packages（推送 GHCR 镜像）
- [ ] ⚪ 用云厂商 OIDC 代替 `KUBECONFIG_B64`（`deploy.yml` 里有注释好的 EKS 写法）

## 阶段 5：第一次部署到 staging

- [ ] 🔴 CI 在 GitHub 上完整跑通一次，修掉第一次运行暴露的问题。容易出问题的地方：
  - `oasdiff` 的 `--fail-on ERR` 参数
  - Lint 里 `go mod tidy` 的差异检查（`go.sum` 没提交时必然失败）
  - kubeconform 校验自定义资源：schema 从 Envoy Gateway（`ENVOY_GATEWAY_VERSION`，要和集群里装的版本一致）、Gateway API、Prometheus Operator 的官方 CRD 现场生成（`.github/scripts/crd2schema.py`）。下载地址或 CRD 格式有变化时这一步会失败，把日志发出来排查
- [ ] 🔴 推到 main 后 Deploy 工作流自动部署 staging，**迁移 Job → 滚动更新 → 冒烟测试**全部成功
- [ ] 🔴 `kubectl -n goapi-staging get gateway,httproute`：Gateway `PROGRAMMED=True`，HTTPRoute 的 `Accepted` 和 `ResolvedRefs` 都为 True；证书 Secret `goapi-api-tls` 已签发
- [ ] 🔴 客户端 IP 正确：从外网请求一次，应用访问日志里的 `remote_ip` 是你的公网 IP，不是 Envoy 或负载均衡的地址
- [ ] 🟡 `http://` 访问返回 301 跳到 `https://`；超过 20s 的请求由应用返回 504（不是 Envoy 先断开）
- [ ] 🔴 `kubectl -n goapi-staging get pods`：api、relay、consumer、alertbridge 都 Running
- [ ] 🔴 用真实 token 调用：`/api/v1/me` 返回正确的 sub 和角色；viewer 不能创建用户（403）；admin 能创建（201）
- [ ] 🔴 创建一个用户后，`outbox_events` 里的事件变成已投递，`welcome_notifications` 里出现记录
- [ ] 🟡 Jaeger 里能看到完整链路：HTTP 请求 → SQL → relay 发布 → consumer 处理
- [ ] 🟡 Prometheus 里能抓到 `goapi`、`goapi-relay`、`goapi-consumer` 三个 job，`job` 标签值和告警规则里一致
- [ ] 🟡 Grafana 面板各行都有数据（连接池、限流、事件投递、事件消费）
- [ ] 🟡 Alertmanager 收到 `Watchdog`；手动发一条测试告警，飞书 / 钉钉群里收到消息，静默链接能打开
- [ ] 🟡 `/api/docs` 在 staging 能打开（production 默认关闭）

## 阶段 6：在 staging 上验证运行行为

- [ ] 🔴 **压测**（见 README「压测」）：
  - [ ] `ratelimit.js`：限流额度和 429 格式符合预期（限流开着跑）
  - [ ] `load.js`：目标流量下错误率 < 0.1%、各接口 P99 < 1s（先临时调高限流）
  - [ ] `stress.js`：找到极限，同时用 `watch-hpa.sh` 记录 HPA 扩容过程
  - [ ] 根据结果校准：Pod 的 CPU / 内存 requests、HPA 的 CPU 70% 阈值、`maxReplicas`、`DB_MAX_CONNS`、`GOMEMLIMIT`、限流额度
  - [ ] 压测后恢复限流配置，清理压测数据
- [ ] 🔴 **优雅关闭**：压测进行中执行 `kubectl rollout restart deployment/goapi-api`，压测结果里不应出现错误
- [ ] 🟡 **回滚**：Actions → Deploy → Run workflow，ref 填上一个版本，确认能回滚，旧版本能在新表结构上运行
- [ ] 🟡 **故障演练**（每项都对照 RUNBOOK 处理一遍）：
  - [ ] 停掉 NATS：relay 退避重试，`GoapiOutboxLagging` 告警；恢复后积压补发完
  - [ ] 停掉 consumer：`GoapiConsumerAllDown` 告警；恢复后继续消费
  - [ ] 停掉 Redis：请求照常放行，`GoapiRateLimiterErrors` 告警
  - [ ] 数据库不可达：`/readyz` 返回 503，Pod 被摘流量但不重启
  - [ ] 构造一条处理失败的消息：进入 `consumer_dead_letters`，修复后用 `replay-dead-letters` 重放成功
- [ ] 🟡 删除一个 Pod、驱逐一个节点：PDB 生效，服务不中断

## 阶段 7：production

- [ ] 🔴 打 `v*` 标签，审批通过，部署成功
- [ ] 🔴 冒烟测试通过（不带 token 返回 401，确认认证开着）
- [ ] 🔴 `AUTH_DISABLED` 没有被设置（`APP_ENV=prod` 时设置了会拒绝启动，但仍然要确认）
- [ ] 🟡 确认 `DOCS_ENABLED=false`，或者确实想公开接口文档
- [ ] 🟡 `Watchdog` 接到死信监控服务（healthchecks.io、Grafana OnCall、PagerDuty heartbeat 等），超过 5 分钟收不到就报警
- [ ] 🟡 观察第一个小时：错误率、P99、连接池、事件投递延迟、事件消费积压

---

## 已知限制（可以上线，但要知道）

| 限制 | 影响 | 后续怎么做 |
| --- | --- | --- |
| 只有角色级授权 | 有 viewer 角色就能看所有用户 | 需要时加资源级检查（例如"只能看自己"） |
| 冒烟测试不带 token | 只验证了认证开着，没验证授权和业务接口 | 用 client credentials 流程拿 token，补带 token 的冒烟测试 |
| 示例 consumer 和 API 共用数据库 | 只是演示方便 | 真实消费方用独立服务和独立数据库 |
| 事件不保证全局顺序 | 消费方不能依赖顺序 | 需要时按 `Event-Id` 或业务版本号判断 |
| 某条事件一直暂时性失败会卡住 relay 队头 | 后面的事件一起延迟 | 由 `GoapiOutboxLagging` 告警发现，按 RUNBOOK 手动转死信 |
| 密钥从文件生成 Kubernetes Secret | 依赖 CD 写入，轮换要重新部署 | 改用 External Secrets / Sealed Secrets |
| Swagger UI 从 unpkg CDN 加载 | 内网或离线环境打不开 | 自己托管 `swagger-ui-dist`，改 `api/api.go` |
| alertbridge 没有鉴权 | 靠 NetworkPolicy 限制来源 | 确认 CNI 支持 NetworkPolicy |
| 网关没有限制请求体大小 | 超大请求体会先到达应用，由应用以 400 拒绝（上限 `HTTP_MAX_BODY_BYTES`，1 MiB） | 需要在入口就挡住时，用 Envoy Gateway 的 BackendTrafficPolicy 配置请求体限制 |
| 来自私有网段的请求可能没有 `X-Envoy-External-Address` | Envoy 把它们当成内部请求，不写这个头；应用退回用 Envoy Pod 的 IP，这些请求共用一份 IP 额度 | 只影响从内网直接访问网关的情况；需要时调整 Envoy 的内部地址判定 |

