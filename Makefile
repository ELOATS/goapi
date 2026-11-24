TMPDIR ?= /tmp
PROMETHEUS_IMAGE ?= prom/prometheus:v3.5.0
ALERTMANAGER_IMAGE ?= prom/alertmanager:v0.28.1

.PHONY: db-test-create loadtest-smoke loadtest-load loadtest-stress loadtest-ratelimit loadtest-cleanup openapi-lint openapi-diff run run-relay run-consumer test lint build vuln docker db-up db-down test-integration run-pg check-rules test-rules check-alertmanager alert-test ci k8s-render k8s-migrate k8s-rollout k8s-deploy k8s-undo

run:
	APP_ENV=dev AUTH_DISABLED=true LOG_LEVEL=debug go run ./cmd/api

test:
	go test -race -count=1 -cover ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o bin/api ./cmd/api

docker:
	docker build -t goapi:local .

# 本地 compose 映射到宿主机的端口，和 docker-compose.yml 的默认值一致
GOAPI_DB_PORT ?= 15432
GOAPI_REDIS_PORT ?= 16379
GOAPI_NATS_PORT ?= 14222
DB_URL ?= postgres://app:app@localhost:$(GOAPI_DB_PORT)/app?sslmode=disable
# 集成测试用的库，和本地环境的 app 分开
DB_TEST_URL ?= postgres://app:app@localhost:$(GOAPI_DB_PORT)/app_test?sslmode=disable
REDIS_TEST_URL ?= redis://localhost:$(GOAPI_REDIS_PORT)/15
NATS_TEST_URL ?= nats://localhost:$(GOAPI_NATS_PORT)

db-up:
	docker compose up -d db redis nats

db-down:
	docker compose down

run-relay:
	APP_ENV=dev LOG_LEVEL=debug DATABASE_URL=$(DB_URL) NATS_URL=$(NATS_TEST_URL) go run ./cmd/relay

run-consumer:
	APP_ENV=dev LOG_LEVEL=debug DATABASE_URL=$(DB_URL) NATS_URL=$(NATS_TEST_URL) go run ./cmd/consumer

run-pg:
	APP_ENV=dev AUTH_DISABLED=true LOG_LEVEL=debug DATABASE_URL=$(DB_URL) go run ./cmd/api

test-integration: db-test-create
	TEST_DATABASE_URL=$(DB_TEST_URL) TEST_REDIS_URL=$(REDIS_TEST_URL) TEST_NATS_URL=$(NATS_TEST_URL) \
		go test -race -count=1 -p 1 ./internal/store/... ./internal/ratelimit/... ./internal/outbox/... ./internal/consumer/...

# 校验 Prometheus 配置和告警/记录规则语法
check-rules:
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" --entrypoint promtool \
		$(PROMETHEUS_IMAGE) check config /etc/prometheus/prometheus.yml

# 校验 Alertmanager 配置（路由、抑制、时间段）
check-alertmanager:
	docker run --rm -v "$(CURDIR)/deploy/alertmanager:/etc/alertmanager:ro" --entrypoint amtool \
		$(ALERTMANAGER_IMAGE) check-config /etc/alertmanager/alertmanager.yml

# 向本地 Alertmanager 发一条测试告警（critical，约 10 秒后推送）
alert-test:
	curl -sS -X POST -H "Content-Type: application/json" \
		--data @deploy/alertmanager/test-alert.json http://localhost:9093/api/v2/alerts

# 告警规则单元测试（promtool test rules）
test-rules:
	docker run --rm -v "$(CURDIR)/deploy/prometheus:/etc/prometheus:ro" -w /etc/prometheus \
		--entrypoint promtool $(PROMETHEUS_IMAGE) test rules tests/alerts_test.yml

# 本地跑一遍和 CI 基本一致的检查（集成测试需要先 make db-up）
ci: lint openapi-lint test vuln check-rules test-rules check-alertmanager
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

K8S_OVERLAY ?= deploy/k8s/overlays/production
K8S_NS ?= goapi

# 渲染出最终 YAML，检查无误再部署
k8s-render:
	kubectl kustomize $(K8S_OVERLAY)

# 第 1 步：只应用 pre-deploy 资源并等迁移完成（失败则停止，Deployment 不会被更新）
k8s-migrate:
	kubectl -n $(K8S_NS) delete job goapi-migrate --ignore-not-found
	kubectl apply -k $(K8S_OVERLAY) -l goapi.example.com/phase=pre-deploy
	kubectl -n $(K8S_NS) wait --for=condition=complete job/goapi-migrate --timeout=10m

# 第 2 步：应用全部资源并等待滚动完成
k8s-rollout:
	kubectl apply -k $(K8S_OVERLAY)
	kubectl -n $(K8S_NS) rollout status deployment/goapi-api --timeout=5m
	kubectl -n $(K8S_NS) rollout status deployment/goapi-alertbridge --timeout=5m
	kubectl -n $(K8S_NS) rollout status deployment/goapi-relay --timeout=5m
	kubectl -n $(K8S_NS) rollout status deployment/goapi-consumer --timeout=5m

# 完整发布（本地手动用；CI 里由 deploy 工作流分步执行）
k8s-deploy: k8s-migrate k8s-rollout

# 回滚 Deployment 到上一版本（不回滚数据库）
k8s-undo:
	kubectl -n $(K8S_NS) rollout undo deployment/goapi-api
	kubectl -n $(K8S_NS) rollout undo deployment/goapi-alertbridge
	kubectl -n $(K8S_NS) rollout undo deployment/goapi-relay
	kubectl -n $(K8S_NS) rollout undo deployment/goapi-consumer
	kubectl -n $(K8S_NS) rollout status deployment/goapi-api --timeout=5m

# OpenAPI 规范检查（需要 Node.js）
openapi-lint:
	npx --yes @redocly/cli@latest lint --config api/redocly.yaml api/openapi.yaml

# 和 origin/main 对比，找出不兼容的接口变更
openapi-diff:
	git show origin/main:api/openapi.yaml > $(TMPDIR)/base-openapi.yaml
	docker run --rm -v "$(TMPDIR):/base:ro" -v "$(CURDIR)/api:/rev:ro" tufin/oasdiff \
		breaking /base/base-openapi.yaml /rev/openapi.yaml --fail-on ERR

# ---------- 压测（k6，用 Docker 运行，不需要本地安装）----------
# 在容器里访问宿主机上的服务用 host.docker.internal；压 staging 时改成它的地址：
#   make loadtest-load LT_BASE_URL=https://staging-api.example.com TOKENS=...
LT_BASE_URL ?= http://host.docker.internal:8080
K6_IMAGE ?= grafana/k6:latest
K6 = docker run --rm -i --add-host=host.docker.internal:host-gateway \
	-v "$(CURDIR)/loadtest:/loadtest:ro" \
	-e BASE_URL=$(LT_BASE_URL) -e TOKENS -e TOKEN -e RATE -e DURATION -e WRITE_RATIO \
	-e START_RPS -e TARGET_RPS -e RAMP -e HOLD \
	-e EXPECT_RPS -e EXPECT_BURST -e IP_RPS -e IP_BURST -e OVER_FACTOR -e TOLERANCE \
	$(K6_IMAGE) run

loadtest-smoke:
	$(K6) /loadtest/smoke.js

loadtest-load:
	$(K6) /loadtest/load.js

loadtest-stress:
	$(K6) /loadtest/stress.js

loadtest-ratelimit:
	$(K6) /loadtest/ratelimit.js

# 删除压测创建的用户（本地库）。staging 上在数据库里执行同样的 SQL
loadtest-cleanup:
	docker compose exec -T db psql -U app -d app -c \
		"DELETE FROM welcome_notifications WHERE email LIKE 'lt-%@loadtest.example.com'; \
		 DELETE FROM users WHERE email LIKE 'lt-%@loadtest.example.com';"

# 建测试库 app_test（已存在则跳过）
db-test-create:
	docker compose exec -T db psql -U app -d app -tAc "SELECT 1 FROM pg_database WHERE datname='app_test'" | grep -q 1 || \
		docker compose exec -T db psql -U app -d app -c "CREATE DATABASE app_test OWNER app"
