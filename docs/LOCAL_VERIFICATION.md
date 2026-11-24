# 本地验证手册

在自己电脑上确认 goapi 的每个功能都能正常工作：先跑自动化检查，再起完整的本地环境，逐项手动验证。
每一步都写了命令和预期结果。结果对不上时，先看 [README「本地开发常见问题」](../README.md#本地开发常见问题)，大多数情况是环境问题，不是代码问题。

命令都是 Windows PowerShell 写法，在项目根目录执行。Linux / macOS 上把 `curl.exe` 换成 `curl`，`Invoke-RestMethod` 换成 `curl` 加 `-X` / `-d` 即可。

适用场景：
- 第一次拿到模板时，确认一切正常
- 改了代码、准备推到 GitHub 之前
- 改了认证相关代码之后（第 11 节）
- 改了 Kubernetes 清单之后（第 12 节）
- 升级依赖或基础镜像之后

上线前的完整流程（包括 staging、production）见 [LAUNCH_CHECKLIST.md](LAUNCH_CHECKLIST.md)，本文对应其中的阶段 1。

---

## 0. 准备

| 需要 | 说明 |
|---|---|
| Docker Desktop | 测试、lint、构建、本地环境都在容器里跑，本机不装 Go 也可以 |
| Git | 改动前先提交，出问题可以回退 |
| `go.mod` / `go.sum` 已整理 | 第一次或新增依赖后执行一次，见下面 |

```powershell
# 第一次，或者代码引入了新依赖
docker run --rm -v "${PWD}:/src" -w /src -v goapi-gomod:/go/pkg/mod golang:1.26 sh -c "go mod tidy && gofmt -w ."

# 本机配置了 HTTP 代理时（不设的话 curl.exe 访问 localhost 会返回代理的 502）
$env:NO_PROXY = "localhost,127.0.0.1"
```

> Dockerfile 的 `ARG GO_VERSION` 要和 `go.mod` 里的 go 版本一致，否则镜像构建会失败。

---

## 1. 自动化检查

这一步不需要起完整环境，大约 3–5 分钟。

### 1.1 lint

```powershell
docker run --rm -v "${PWD}:/app" -w /app -v goapi-gomod:/go/pkg/mod golangci/golangci-lint:latest golangci-lint run ./...
```

**预期**：`0 issues.`

规则在 `.golangci.yml`：internal 包不要求导出符号写文档注释；测试文件不检查 errcheck、noctx、bodyclose、gosec。

### 1.2 漏洞扫描

```powershell
docker run --rm -v "${PWD}:/src" -w /src -v goapi-gomod:/go/pkg/mod golang:1.26 go run golang.org/x/vuln/cmd/govulncheck@latest ./...
```

**预期**：`Your code is affected by 0 vulnerabilities.`

依赖的模块里有漏洞、但代码没有调用到时，会提示 "found N vulnerabilities in modules you require"。这不阻塞，用 `-show verbose` 看是哪个模块，有空时升级。

### 1.3 单元测试

```powershell
.\scripts\test-in-docker.ps1
```

**预期**：所有包 `ok`，集成测试因为没有设置连接地址而自动跳过。

> Windows 上直接跑 `go test` 可能被应用控制策略拦截（`An Application Control policy has blocked this file`），所以用这个脚本在 Linux 容器里跑。

### 1.4 集成测试

```powershell
.\scripts\test-in-docker.ps1 -Integration
```

脚本会自动启动 db、redis、nats，建好测试库 `app_test`，然后串行（`-p 1`）跑全部测试。

**预期**：所有包 `ok`。日志里会出现一次 `goose: successfully migrated database to version: N`，之后每个测试开始时都打印一次 `no migrations to run`，这是正常的。

集成测试覆盖的内容：

| 包 | 验证什么 |
|---|---|
| `internal/store` | 事务回滚和提交；keyset 分页（包括 `created_at` 相同的情况）；邮箱大小写不敏感的唯一约束 |
| `internal/outbox` | 按顺序投递并标记；暂时性错误停在当前那条、永久性错误进死信；**3 个 relay 并发处理，每条事件恰好投递一次**；LISTEN 唤醒；清理过期事件；NATS 发布去重和消息头；链路上下文从写入传到发布 |
| `internal/consumer` | 去重；事务回滚后重试；重试用完进死信；永久错误直接进死信；panic 后重试；处理慢时发心跳；链路上下文传到 handler；**端到端**（outbox 发布 → JetStream → 消费 → 死信重放） |
| `internal/ratelimit` | Redis 限流的额度、并发（100 个并发恰好放行 20 个）、键的过期时间 |
| `internal/auth` | token 校验的 12 种情况；角色解析；**拉不到公钥时返回 503、过期 token 仍是 401**（用 httptest 模拟身份提供方） |
| `internal/httpserver` | OpenAPI 契约测试（18 个请求逐个按规范校验）；路由和规范一一对应；限流、写接口限流、响应压缩 |
| `internal/handler` | 游标签名：篡改、伪造、换密钥、密钥轮换、只改填充位 |

> 集成测试用独立的库 `app_test`，不会碰本地环境的 `app` 库。

---

## 2. 启动本地环境

```powershell
docker compose up -d --build
docker compose ps
```

**预期**：11 个服务都是 `Up`。用 `docker compose ps -a` 再看一次，确认没有 `Exited` 的（`ps` 默认不显示已退出的容器）。

| 服务 | 地址 | 用途 |
|---|---|---|
| api | http://localhost:8080 | 业务接口；本地关闭了认证，所有请求视为管理员 `local-dev` |
| api 指标 | http://localhost:9090/metrics | |
| relay 指标 | http://localhost:9092/metrics | |
| consumer 指标 | http://localhost:9094/metrics | |
| Jaeger | http://localhost:16686 | 链路 |
| Prometheus | http://localhost:9091 | 指标、告警规则 |
| Alertmanager | http://localhost:9093 | 告警路由 |
| Grafana | http://localhost:3000 | 面板（免登录，首页就是 goapi 面板） |
| db / redis / nats | 宿主机端口 15432 / 16379 / 14222 | 容器之间用 `db:5432` 等 |

relay 和 consumer 刚启动的几秒里，日志可能报流或表还不存在，它们会自动重试。

检查启动日志：

```powershell
docker compose logs api --tail 20
```

**预期**能看到：
- `CURSOR_SECRET not set: using a random key ...`：本地没配游标密钥，属于正常情况，重启后旧游标失效
- `rate limiting enabled`，带 `backend=redis`、`write_rps=2`、`write_burst=10`
- `http server starting`

> 验证限流（第 5、6 节）时不要叠加 `docker-compose.loadtest.yml`，它会关掉限流。

---

## 3. 核心链路：创建用户到事件被消费

### 3.1 健康检查和创建用户

```powershell
curl.exe --noproxy "*" -s http://localhost:8080/readyz
Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/v1/users -ContentType "application/json" -Body '{"name":"Alice","email":"alice@example.com"}'
Invoke-RestMethod http://localhost:8080/api/v1/users
```

**预期**：
- `readyz` 返回 `{"status":"ready"}`
- 创建返回用户 JSON（`id`、`name`、`email`、`created_at`），状态码 201
- 列表里有 Alice

### 3.2 事件投递和消费

```powershell
Start-Sleep 2
docker compose exec db psql -U app -d app -c "select id, event_type, published_at is not null as published, actor from outbox_events order by id desc limit 5;" -c "select * from welcome_notifications;"
docker compose logs relay consumer --tail 20
```

**预期**：
- Alice 对应的事件 `published = t`，`actor = local-dev`
- `welcome_notifications` 里有 Alice
- consumer 日志里有 `welcome notification queued`，带 `user_id` 和 `trace_id`

### 3.3 完整链路

打开 http://localhost:16686，服务选 `goapi`，操作选 `POST /api/v1/users`，打开最近的一条。
也可以复制上一步 consumer 日志里的 `trace_id`，直接打开 `http://localhost:16686/trace/<trace_id>`。

**预期**：同一条 trace 里依次出现

```
POST /api/v1/users                    （goapi）
├── evalsha × 3                       （按 IP、按用户、写接口限流）
├── SQL：INSERT users / INSERT outbox_events
└── outbox publish user.created       （goapi-relay）
    └── consume user.created          （goapi-consumer）
```

最后一段能出现，说明 NATS 消息头里的链路上下文（`traceparent`）传递正常。

---

## 4. 监控和告警

### 4.1 抓取目标

```powershell
(Invoke-RestMethod http://localhost:9091/api/v1/targets).data.activeTargets | Select-Object @{n='job';e={$_.labels.job}}, health, lastError
```

**预期**：`goapi`、`goapi-relay`、`goapi-consumer` 三行，`health` 都是 `up`。

### 4.2 告警规则

打开 http://localhost:9091/alerts。

**预期**：
- 规则都加载出来了（Prometheus 能启动就说明规则文件语法正确）
- `Watchdog` 一直是 **Firing**，这是设计如此
- 如果 `consumer_dead_letters` 表里有记录，`GoapiConsumerDeadLetters` 也会触发，说明这条告警可用

### 4.3 告警通知

```powershell
Invoke-RestMethod -Method Post -Uri http://localhost:9093/api/v2/alerts -ContentType "application/json; charset=utf-8" -InFile deploy/alertmanager/test-alert.json
Start-Sleep 15
docker compose logs alertbridge --tail 20
```

**预期**：日志里出现 `dry-run`，后面是格式化好的告警内容（标题、摘要、处理手册和静默链接）。没有配置飞书或钉钉的 webhook 时只打印到日志，不会真的推送。约 5 分钟后还会出现一条恢复通知。

### 4.4 Grafana

打开 http://localhost:3000。

**预期**：HTTP、数据库连接池、限流、事件投递、事件消费、Go 运行时这几行都有数据。数据太少时先跑一次第 7 节的冒烟测试。

---

## 5. 响应压缩

```powershell
curl.exe --noproxy "*" -s -D - -o NUL -H "Accept-Encoding: gzip" http://localhost:8080/api/v1/users
curl.exe --noproxy "*" -s -D - -o NUL http://localhost:8080/api/v1/users
```

**预期**：
- 第一条：`HTTP/1.1 200 OK`，响应头里有 `Content-Encoding: gzip`
- 第二条：`200 OK`，没有 `Content-Encoding`

> 如果看到 `502 Bad Gateway` 和 `Proxy-Connection` 响应头，是请求走了本机代理，见第 0 节。

---

## 6. 限流

### 6.1 写接口限流

连续创建 15 个用户：

```powershell
foreach ($i in 1..15) {
  $body = '{"name":"w' + $i + '","email":"w' + $i + '-' + (Get-Random) + '@example.com"}'
  try { Invoke-RestMethod -Method Post -Uri http://localhost:8080/api/v1/users -ContentType application/json -Body $body | Out-Null; "$i 201" }
  catch { "$i " + [int]$_.Exception.Response.StatusCode }
}
Invoke-RestMethod "http://localhost:8080/api/v1/users?limit=1" | Out-Null; "读接口仍然可用"
```

**预期**：
- 前 10–11 个返回 `201`，之后返回 `429`（写接口突发额度是 10，循环期间每秒补 2 个，所以可能多 1 个）
- 最后一行正常输出：读接口不受写额度影响

### 6.2 429 响应格式和 IP 限流

用 k6 按设定的倍数超额请求，统计放行次数是否和额度一致：

```powershell
docker run --rm -i --add-host=host.docker.internal:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=http://host.docker.internal:8080 grafana/k6 run /loadtest/ratelimit.js
```

**预期**：所有阈值通过。同一用户 60 秒内放行约 620 次（突发 20 + 每秒 10 × 60），其余返回 429，每个 429 都带 `Retry-After` 和 `RateLimit-*` 响应头。

本地没有传 token，所以"无效 token 先被 IP 限流挡住"这个场景会自动跳过，要到 staging 上带 `TOKENS` 才能测。

> 6.1 用掉的额度要等几秒才恢复。6.2 失败时先等 10 秒再跑一次。

---

## 7. 游标签名

```powershell
$c = (Invoke-RestMethod "http://localhost:8080/api/v1/users?limit=1").next_cursor
$c                                                                              # 形如 xxxxx.yyyyy
(Invoke-RestMethod "http://localhost:8080/api/v1/users?limit=1&cursor=$c").items.Count   # 正常翻页

$p, $s = $c.Split('.')
$alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_'
$mid = [int]($s.Length / 2)
$flipMid = $s.Substring(0, $mid) + $(if ($s[$mid] -eq 'A') { 'B' } else { 'A' }) + $s.Substring($mid + 1)
$padBit  = $s.Substring(0, $s.Length - 1) + $alphabet[$alphabet.IndexOf($s[-1]) -bor 1]

foreach ($x in @("$p.$flipMid", "$p.$padBit", $p)) {
  curl.exe --noproxy "*" -s -w "  -> %{http_code}`n" "http://localhost:8080/api/v1/users?limit=1&cursor=$x"
}
```

**预期**：
- `$c` 是两段用 `.` 连起来的字符串（前一段是数据，后一段是签名）
- 正常翻页返回 `1`
- 三条篡改请求都返回 `{"error":"invalid cursor",...}  -> 400`，依次是：改了签名中间的字符、只改了签名最后一个字符的填充位、去掉签名

`$c` 为空时说明库里只有一页数据，先做完 6.1 多建几个用户再试。

> 只改填充位那一条，是专门防一个已修复的 bug：base64 默认解码会忽略填充位，同一个签名会有几种写法都能通过。

---

## 8. 冒烟测试

```powershell
docker run --rm -i --add-host=host.docker.internal:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=http://host.docker.internal:8080 grafana/k6 run /loadtest/smoke.js
```

**预期**：`checks` 为 100%。每个接口走一遍：`/me`、创建、列表、按 ID 查询、查询不存在的用户返回 404。没有传 token 时，"不带 token 返回 401"这一项自动跳过。

---

## 9. 本地测不到的内容

认证（OIDC、角色、503）可以在本地用 Keycloak 验证，见第 11 节；Kubernetes 部署可以用 kind 验证，见第 12 节。

| 功能 | 原因 | 在哪里验证 |
|---|---|---|
| 网关：真实客户端 IP、云负载均衡、Let's Encrypt 证书 | kind 里请求都来自私有网段，证书是自签的 | staging（阶段 5）。TLS、HTTP 跳转、路由、超时在 kind 里验证（第 12 节） |
| 托管数据库、真实流量下的 HPA 扩缩、跨可用区分布 | kind 里的依赖是单副本、数据在 emptyDir，节点都在一台机器上 | staging（阶段 6）。滚动更新、优雅关闭、PDB、Pod Security、NetworkPolicy 在 kind 里验证（第 12 节） |
| 负载和压力测试 | 本机结果不代表集群上的容量 | staging，见 README「压测」 |
| 告警真正推送到飞书 / 钉钉 | 本地没配 webhook | 在 `.env` 里填 webhook 后重跑 4.3 |

---

## 10. 清理

删除验证时创建的用户：

```powershell
docker compose exec db psql -U app -d app -c "delete from welcome_notifications where email like 'w%@example.com' or email like 'lt-%@loadtest.example.com'; delete from users where email like 'w%@example.com' or email like 'lt-%@loadtest.example.com';"
```

停止环境：

```powershell
docker compose down        # 只停容器，数据保留
docker compose down -v     # 同时删掉本模板的数据卷（goapi-template_*），下次从空库开始
```

`-v` 只影响 `goapi-template_` 开头的数据卷，不会碰其它 compose 项目的数据。

---

## 11. 认证（本地 Keycloak）

默认的本地环境关闭了认证（`AUTH_DISABLED=true`）。叠加 `docker-compose.auth.yml` 后，会多一个 Keycloak，api 开启认证并用它校验 token。
Keycloak 的配置在 `deploy/keycloak/goapi-realm.json`，里面预置了 3 个测试用户：

| 用户 | 密码 | 角色 |
|---|---|---|
| alice | alice | goapi-admin（读写） |
| bob | bob | goapi-viewer（只读） |
| carol | carol | 无 |

### 11.1 启动

```powershell
$env:COMPOSE_FILE = "docker-compose.yml;docker-compose.auth.yml"   # 当前窗口之后的 docker compose 命令都带上两个文件
docker compose up -d --build
docker compose ps keycloak api
docker compose logs api --tail 10
```

**预期**：
- keycloak 是 `healthy`（第一次启动要拉镜像、导入 realm，大约 30–60 秒）
- api 日志里有 `oidc authentication enabled`，`issuer=http://localhost:8180/realms/goapi`，`discovery_url=http://keycloak:8080/realms/goapi`
- 没有 `AUTH_DISABLED=true` 的警告

> 为什么要两个地址：命令行从 `localhost:8180` 拿 token，token 里的 `iss` 就是这个地址；容器里的 api 访问不到宿主机的 localhost，只能通过 `keycloak:8080` 取 discovery 文档和公钥。`OIDC_DISCOVERY_URL` 只改变"从哪里取"，`iss` 仍然严格按 `OIDC_ISSUER_URL` 校验。

### 11.2 拿 token，看身份和角色

```powershell
$alice = .\scripts\get-token.ps1 alice
$bob   = .\scripts\get-token.ps1 bob
$carol = .\scripts\get-token.ps1 carol
Invoke-RestMethod http://localhost:8080/api/v1/me -Headers @{ Authorization = "Bearer $alice" }
```

**预期**：`sub` 是 alice 在 Keycloak 里的用户 ID（UUID），`email` 是 `alice@goapi.local`，`roles` 里有 `goapi-admin`（还会有 Keycloak 自带的 `default-roles-goapi` 等，不影响）。

### 11.3 401 / 403 / 200

```powershell
function Try-Api([string]$method, [string]$path, [string]$token, [string]$body) {
  $h = @("-s", "-o", "NUL", "-w", "%{http_code}", "--noproxy", "*", "-X", $method)
  if ($token) { $h += @("-H", "Authorization: Bearer $token") }
  if ($body)  { $h += @("-H", "Content-Type: application/json", "-d", $body) }
  curl.exe @h "http://localhost:8080$path"
}
$new = '{"name":"KC","email":"kc-' + (Get-Random) + '@example.com"}'

"不带 token        GET  users  -> " + (Try-Api GET  /api/v1/users "")
"无效 token        GET  users  -> " + (Try-Api GET  /api/v1/users "not-a-jwt")
"carol（无角色）  GET  me     -> " + (Try-Api GET  /api/v1/me $carol)
"carol（无角色）  GET  users  -> " + (Try-Api GET  /api/v1/users $carol)
"bob（viewer）    GET  users  -> " + (Try-Api GET  /api/v1/users $bob)
"bob（viewer）    POST users  -> " + (Try-Api POST /api/v1/users $bob $new)
"alice（admin）   POST users  -> " + (Try-Api POST /api/v1/users $alice $new)
```

**预期**：

| 请求 | 状态码 |
|---|---|
| 不带 token | 401 |
| 无效 token | 401 |
| carol 调 `/me` | 200 |
| carol 读列表 | 403 |
| bob 读列表 | 200 |
| bob 创建用户 | 403 |
| alice 创建用户 | 201 |

再看一眼 alice 创建用户产生的事件，`actor` 应该是 alice 的 `sub`，而不是 `local-dev`：

```powershell
docker compose exec db psql -U app -d app -c "select id, actor from outbox_events order by id desc limit 1;"
```

### 11.4 拉不到公钥时返回 503

`scripts/forge-token.ps1` 生成一个 iss、aud、exp 都正确，但签名是假的、kid 也不认识的 token。api 校验它时会去 Keycloak 重新拉公钥：

```powershell
$forged = .\scripts\forge-token.ps1

"Keycloak 正常  伪造 token -> " + (Try-Api GET /api/v1/users $forged)     # 401：拉到了公钥，签名确实不对
docker compose stop keycloak
"Keycloak 停掉  伪造 token -> " + (Try-Api GET /api/v1/users $forged)     # 503：拉不到公钥，无法判断
"Keycloak 停掉  bob 的 token -> " + (Try-Api GET /api/v1/users $bob)       # 200：缓存里的公钥照常可用
curl.exe --noproxy "*" -s -D - -o NUL -H "Authorization: Bearer $forged" http://localhost:8080/api/v1/users | Select-String "HTTP/|Retry-After"
docker compose start keycloak
```

**预期**：依次是 401、503、200；503 的响应头带 `Retry-After: 5`，没有 `WWW-Authenticate`。api 日志里有 `cannot verify token: signing keys unavailable`。

Keycloak 重新起来之后不用重启 api，下一次请求会自动重新拉取。

### 11.5 带 token 跑 k6

```powershell
$env:TOKENS = "$alice"
docker run --rm -i --add-host=host.docker.internal:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=http://host.docker.internal:8080 -e TOKENS grafana/k6 run /loadtest/smoke.js
docker run --rm -i --add-host=host.docker.internal:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=http://host.docker.internal:8080 -e TOKENS grafana/k6 run /loadtest/ratelimit.js
```

**预期**：
- `smoke.js`：`checks` 100%，这次"不带 token 返回 401"这一项也会跑
- `ratelimit.js`：两个场景都跑，无效 token 的场景里约 640 次返回 401（通过了 IP 限流），其余在验签之前就返回 429

### 11.6 回到不带认证的环境

```powershell
Remove-Item Env:COMPOSE_FILE
docker compose -f docker-compose.yml -f docker-compose.auth.yml stop keycloak
docker compose up -d api        # 用 docker-compose.yml 重建 api，认证关闭
```

---

## 12. Kubernetes 部署（kind）

用 [kind](https://kind.sigs.k8s.io/) 在本机起一个 3 节点的 Kubernetes 集群（1 个控制面 + 2 个工作节点），装上和 staging 一样的集群组件，把 `deploy/k8s/overlays/kind` 部署上去。
上 staging 之前，K8s 清单里的大部分问题都能在这里暴露出来：Pod Security、迁移 Job、网关路由和证书、NetworkPolicy、滚动更新、PDB。

和 staging 的区别在 `overlays/kind/kustomization.yaml` 开头有说明，主要是：依赖（Postgres / Redis / NATS）跑在集群里的 `goapi-deps` 命名空间；关闭认证；证书自签，域名 `api.goapi.test`；限流额度调高。

所有操作都在 `scripts/kind.ps1` 里，命令都显式指定 `--context kind-goapi`，不会碰到你的其它集群。

### 12.1 准备

```powershell
winget install Kubernetes.kind Kubernetes.kubectl Helm.Helm
```

Docker Desktop 建议至少分配 4 CPU、8 GB 内存（Settings → Resources）。

### 12.2 创建集群并安装组件

```powershell
.\scripts\kind.ps1 up
```

第一次大约 5–10 分钟，依次完成：创建集群（`deploy/kind/cluster.yaml`）→ Envoy Gateway（同时装 Gateway API CRD）和 GatewayClass `eg` → cert-manager（开启 Gateway API 支持）→ metrics-server → Postgres / Redis / NATS（`deploy/kind/deps.yaml`）。
可以重复运行，已经装好的会跳过或升级。

### 12.3 构建镜像并部署

```powershell
.\scripts\kind.ps1 build      # 构建 goapi/api、relay、consumer、alertbridge（标签 kind），导入集群
.\scripts\kind.ps1 deploy
```

`deploy` 的顺序和 CD 一样：先只应用带 `pre-deploy` 标签的资源（命名空间、配置、密钥、迁移 Job），等迁移完成，再应用全部资源、等滚动更新完成，最后等 Gateway `Programmed`、证书 `Ready`。

镜像标签固定是 `kind`，重新 `build` 之后 Deployment 的内容没有变化，所以 `deploy` 每次都会强制滚动一次。改了代码就是 `build` + `deploy`。

`base/secrets.env` 不存在时会从示例复制一份（base 需要这个文件才能渲染）；kind overlay 会整个替换密钥的内容，不会用到里面的值。

### 12.4 自动检查

```powershell
.\scripts\kind.ps1 check
```

脚本会在后台把网关转发到本机 18080 / 18443，逐项检查后打印一张表，有失败时退出码为 1：

| 检查 | 预期 |
|---|---|
| 所有 Deployment 就绪 | 4 个都就绪 |
| Pod Security restricted | 命名空间 `enforce=restricted`，所有 Pod 都被接受。这是第一次在真正执行 Pod Security 的集群上验证安全上下文的配置 |
| 迁移 Job | `succeeded=1` |
| api 副本分散 | 2 个 api Pod 在不同的工作节点上（topologySpreadConstraints） |
| Gateway / HTTPRoute / 证书 | Gateway `Programmed`；两个 HTTPRoute 的 `Accepted`、`ResolvedRefs` 都是 True；`goapi-api-tls` 已签发 |
| HTTP 跳转 | `301`，跳到 `https://api.goapi.test/...` |
| HTTPS 访问 API | `200` |
| 只暴露 `/api` | 通过网关访问 `/healthz` 返回 `404` |
| 响应压缩经过网关 | 仍然有 `Content-Encoding: gzip` |
| 事件链路 | 通过网关创建用户，几秒内 `welcome_notifications` 里出现记录 |
| 客户端 IP | **INFO**，不判定通过或失败：显示 api 日志里的 `remote_ip`，kind 里会是 Envoy Pod 的私有地址（原因见 12.7） |
| NetworkPolicy | 从 `goapi-deps` 命名空间直连 api 超时 |
| HPA 指标 | 能读到 CPU 使用率。刚部署完 metrics-server 还没有数据，1–2 分钟后重跑 |
| 滚动更新不丢请求 | 后台持续请求的同时 `rollout restart` api，所有请求都是 `200` |

**驱逐节点**（比较慢，单独开启）：

```powershell
.\scripts\kind.ps1 check -Disruption
```

额外选一个 Envoy 不在的工作节点执行 `kubectl drain`，持续请求的同时等所有 Deployment 恢复，然后 `uncordon`。**预期**：所有请求都是 `200`，api 的 PDB（`maxUnavailable: 1`）让驱逐一个一个进行。

### 12.5 手动访问

```powershell
.\scripts\kind.ps1 forward          # 前台运行，Ctrl+C 结束
```

另开一个窗口：

```powershell
$gw = @("--noproxy", "*", "-k", "--resolve", "api.goapi.test:18443:127.0.0.1", "-H", "Host: api.goapi.test")
curl.exe @gw https://api.goapi.test:18443/api/v1/users
curl.exe @gw -s -D - -o NUL https://api.goapi.test:18443/api/v1/me
```

`--resolve` 让 curl 把 `api.goapi.test` 解析到本机的转发端口，不需要改 hosts 文件；`-k` 是因为证书是自签的。

其它常用命令：

```powershell
kubectl --context kind-goapi -n goapi-kind get pods -o wide
kubectl --context kind-goapi -n goapi-kind get gateway,httproute,certificate,hpa,pdb
kubectl --context kind-goapi -n goapi-kind logs deployment/goapi-api --tail 50
kubectl --context kind-goapi -n goapi-deps exec postgres-0 -- psql -U app -d app -c "select count(*) from users;"
```

### 12.6 观察 HPA 扩容（可选）

`forward` 运行时，用 k6 持续加压，同时在另一个窗口看副本数：

```powershell
docker run --rm -i --add-host=api.goapi.test:host-gateway -v "${PWD}/loadtest:/loadtest:ro" `
  -e BASE_URL=https://api.goapi.test:18443 -e K6_INSECURE_SKIP_TLS_VERIFY=true -e RATE=200 -e DURATION=5m `
  grafana/k6 run /loadtest/load.js
kubectl --context kind-goapi -n goapi-kind get hpa goapi-api -w
```

**预期**：CPU 超过 70% 后副本数从 2 往 4 增加；压力停止 5 分钟后慢慢缩回 2。
`forward` 默认只监听 127.0.0.1，k6 容器连不上时，改用 `kubectl ... port-forward --address 0.0.0.0`。
这里的延迟、吞吐数字只能看个趋势，不代表集群上的真实容量。

### 12.7 kind 里验证不了的

- **客户端 IP**：经过 port-forward 的请求，在 Envoy 看来来自私有网段，它不写 `X-Envoy-External-Address`，api 退回用 Envoy Pod 的地址，所有请求共用一份 IP 限流额度（所以 kind overlay 调高了限流）。真实客户端 IP 只能在 staging 上验证。
- **认证**：kind overlay 关闭了认证，认证用第 11 节的 Keycloak 验证。
- **NetworkPolicy** 依赖集群的 CNI。较新版本 kind 自带的 kindnet 会执行 NetworkPolicy；检查项报"返回 200"时，说明这个版本的 kind 不支持，需要升级 kind，或者创建集群时关掉默认 CNI、换成 Calico。
- 云负载均衡、托管数据库、Let's Encrypt、跨可用区分布。

### 12.8 删除集群

```powershell
.\scripts\kind.ps1 down
```

---

## 验证记录

每次验证可以复制这张表，记在 PR 描述里。

| # | 项目 | 结果 |
|---|---|---|
| 1.1 | lint `0 issues` | ☐ |
| 1.2 | govulncheck 无可调用漏洞 | ☐ |
| 1.3 | 单元测试全部 ok | ☐ |
| 1.4 | 集成测试全部 ok | ☐ |
| 2 | 11 个服务都在运行，没有 Exited | ☐ |
| 3.1 | 健康检查、创建用户 201 | ☐ |
| 3.2 | 事件已投递，`welcome_notifications` 有记录 | ☐ |
| 3.3 | Jaeger 一条 trace 从 HTTP 到 consume | ☐ |
| 4.1 | 三个抓取目标都 up | ☐ |
| 4.2 | 告警规则加载，Watchdog 触发 | ☐ |
| 4.3 | alertbridge 输出 dry-run | ☐ |
| 4.4 | Grafana 各行有数据 | ☐ |
| 5 | gzip 压缩生效 | ☐ |
| 6.1 | 写接口约第 11 个开始 429，读接口不受影响 | ☐ |
| 6.2 | ratelimit.js 阈值全部通过 | ☐ |
| 7 | 三种篡改的游标都返回 400 | ☐ |
| 8 | smoke.js checks 100% | ☐ |
| 11.1 | Keycloak healthy，api 开启 OIDC | ☐ |
| 11.3 | 401 / 403 / 200 / 201 都符合预期，事件 actor 是真实用户 | ☐ |
| 11.4 | 伪造 token：Keycloak 正常 401，停掉 503，真实 token 仍 200 | ☐ |
| 11.5 | 带 token 的 smoke.js、ratelimit.js 通过 | ☐ |
| 12.3 | kind：迁移 Job 成功，4 个 Deployment 就绪，Gateway Programmed，证书 Ready | ☐ |
| 12.4 | kind：`check` 全部 PASS | ☐ |
| 12.4 | kind：`check -Disruption` 驱逐节点时请求全部 200 | ☐ |
