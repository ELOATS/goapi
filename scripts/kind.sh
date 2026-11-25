#!/usr/bin/env bash
# 在本地 kind 集群里部署并验证 goapi 的 Kubernetes 配置。Linux / macOS / CI 用；Windows 用 scripts/kind.ps1。
# 两个脚本做的事情一样，检查项一一对应，改一个时记得同步另一个。
#
#   scripts/kind.sh up          创建集群，预先导入依赖镜像，安装 Envoy Gateway、cert-manager、metrics-server，部署 Postgres / Redis / NATS
#   scripts/kind.sh build       构建 4 个镜像并导入集群
#   scripts/kind.sh deploy      跑迁移 Job，部署 overlays/kind，等滚动更新、网关和证书就绪
#   scripts/kind.sh check       自动检查（加 --disruption 另外驱逐一个工作节点）
#   scripts/kind.sh all         依次执行 up、build、deploy、check（同样可以加 --disruption）
#   scripts/kind.sh diag        打印排查用的信息（CI 失败时调用）
#   scripts/kind.sh down        删除集群
#
# 需要：docker、kind、kubectl、helm、curl。所有 kubectl / helm 命令都显式指定 --context kind-<集群名>。
# 可以用环境变量覆盖：KIND_CLUSTER、KIND_NAMESPACE、ENVOY_GATEWAY_VERSION、KIND_HTTP_PORT、KIND_HTTPS_PORT。
#
# 注意：脚本里刻意不用"美元符号加数字"的位置参数写法（从聊天或网页复制时容易被加上反斜杠），
# 函数参数一律先放进数组再按下标取。
set -euo pipefail

CLUSTER="${KIND_CLUSTER:-goapi}"
NS="${KIND_NAMESPACE:-goapi-kind}"
EG_VERSION="${ENVOY_GATEWAY_VERSION:-v1.9.0}"
HTTP_PORT="${KIND_HTTP_PORT:-18080}"
HTTPS_PORT="${KIND_HTTPS_PORT:-18443}"
CTX="kind-${CLUSTER}"
OVERLAY="deploy/k8s/overlays/kind"
HOST="api.goapi.test"
SEL_PREDEPLOY="goapi.example.com/phase=pre-deploy"
ENVOY_SEL="gateway.envoyproxy.io/owning-gateway-name=goapi"
DEPLOYMENTS=(goapi-api goapi-relay goapi-consumer goapi-alertbridge)
IMAGES=(api relay consumer alertbridge)
# 依赖和测试用的镜像先在宿主机拉好再导入，节点在线拉镜像慢时不会把等待拖到超时
DEPS_IMAGES=(postgres:17 redis:7-alpine nats:2-alpine curlimages/curl:latest)

cd "$(dirname "${BASH_SOURCE[0]}")/.."

usage() {
  sed -n '2,13p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

die() { echo "错误：$*" >&2; exit 1; }
need() { local a=("$@"); command -v "${a[0]}" >/dev/null 2>&1 || die "找不到 ${a[0]}"; }
k() { kubectl --context "$CTX" "$@"; }
h() { helm --kube-context "$CTX" "$@"; }

cluster_exists() { kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; }
assert_cluster() { cluster_exists || die "kind 集群 $CLUSTER 不存在，先运行 scripts/kind.sh up"; }

PF_PID=""
TMPD=$(mktemp -d)
cleanup() {
  [[ -n "$PF_PID" ]] && kill "$PF_PID" >/dev/null 2>&1 || true
  rm -rf "$TMPD"
}
trap cleanup EXIT

# ---------------------------------------------------------------- up
# 把宿主机上的镜像导入 kind 节点。
# Docker 用 containerd 镜像存储时（GitHub runner 和新版 Docker Desktop 的默认设置），
# 多架构镜像在本地只有当前架构的层，但 kind load docker-image 会按 --all-platforms 导入整个清单，
# 报 "ctr: content digest sha256:...: not found"。这时改成只导出当前架构再导入。
# 用法：load_image <镜像> <required|optional>。optional 导入失败只警告：节点会自己去拉
NODE_ARCH=""
load_image() {
  local a=("$@")
  local img="${a[0]}" mode="${a[1]}" tar
  kind load docker-image --name "$CLUSTER" "$img" && return 0
  [[ -n "$NODE_ARCH" ]] || NODE_ARCH=$(docker version --format '{{.Server.Arch}}')
  echo "kind load docker-image 失败，改为只导出 linux/${NODE_ARCH} 再导入：$img"
  tar="$TMPD/image.tar"
  if docker save --platform "linux/${NODE_ARCH}" -o "$tar" "$img" && kind load image-archive --name "$CLUSTER" "$tar"; then
    rm -f "$tar"; return 0
  fi
  rm -f "$tar"
  if [[ "$mode" == "optional" ]]; then
    echo "::warning::没能把 $img 导入集群，节点会在部署时自己拉取"
    return 0
  fi
  die "没能把 $img 导入集群"
}

preload_images() {
  local img
  for img in "$@"; do
    echo "== 导入镜像 $img"
    docker image inspect "$img" >/dev/null 2>&1 || docker pull "$img"
    load_image "$img" optional
  done
}

do_up() {
  need docker; need kind; need kubectl; need helm
  if cluster_exists; then
    echo "集群 $CLUSTER 已存在，跳过创建"
  else
    kind create cluster --name "$CLUSTER" --config deploy/kind/cluster.yaml --wait 3m
  fi
  k get nodes -L topology.kubernetes.io/zone

  preload_images "${DEPS_IMAGES[@]}"

  echo; echo "== Envoy Gateway $EG_VERSION（同时安装 Gateway API CRD）"
  h upgrade --install eg oci://docker.io/envoyproxy/gateway-helm \
    --version "$EG_VERSION" -n envoy-gateway-system --create-namespace --wait --timeout 5m
  k apply -f deploy/kind/gatewayclass.yaml

  # cert-manager 要在 Gateway API CRD 之后安装，否则它不会监听 Gateway
  echo; echo "== cert-manager（开启 Gateway API 支持）"
  helm repo add jetstack https://charts.jetstack.io --force-update
  h upgrade --install cert-manager jetstack/cert-manager \
    -n cert-manager --create-namespace --wait --timeout 5m \
    --set crds.enabled=true \
    --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
    --set config.kind=ControllerConfiguration \
    --set config.enableGatewayAPI=true

  echo; echo "== metrics-server（HPA 需要）"
  helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update
  h upgrade --install metrics-server metrics-server/metrics-server \
    -n kube-system --wait --timeout 5m --set "args={--kubelet-insecure-tls}"

  echo; echo "== Postgres / Redis / NATS"
  k apply -f deploy/kind/deps.yaml
  k -n goapi-deps rollout status statefulset/postgres --timeout=5m
  k -n goapi-deps rollout status deployment/redis --timeout=5m
  k -n goapi-deps rollout status deployment/nats --timeout=5m

  echo; echo "集群准备好了。下一步：scripts/kind.sh build，然后 scripts/kind.sh deploy"
}

# ---------------------------------------------------------------- build
do_build() {
  assert_cluster
  local c
  for c in "${IMAGES[@]}"; do
    echo; echo "== 构建 goapi/${c}:kind"
    # 不生成 provenance / SBOM 证明：它们会让本地镜像变成多清单的索引，kind 导入时更容易出 digest not found
    docker build --provenance=false --sbom=false -t "goapi/${c}:kind" --build-arg "CMD=${c}" .
  done
  for c in "${IMAGES[@]}"; do load_image "goapi/${c}:kind" required; done
}

# ---------------------------------------------------------------- deploy
ensure_secret_files() {
  # base 的 secretGenerator 需要这两个文件存在；kind overlay 会整个替换 goapi-secrets 的内容
  local f
  for f in secrets.env alertbridge-secrets.env; do
    [[ -f "deploy/k8s/base/$f" ]] || { cp "deploy/k8s/base/$f.example" "deploy/k8s/base/$f"; echo "已从示例创建 deploy/k8s/base/$f"; }
  done
}

assert_kind_images() {
  # 渲染结果里的镜像必须都是本地构建的 goapi/*:kind，否则节点会去公网拉
  local imgs wrong
  imgs=$(kubectl kustomize "$OVERLAY" | sed -n -E 's/^[[:space:]]*image:[[:space:]]*//p' | tr -d '"' | sort -u)
  wrong=$(grep -vE '^goapi/[a-z]+:kind$' <<<"$imgs" || true)
  [[ -z "$wrong" ]] || die "overlays/kind 渲染出的镜像不是本地构建的：$(tr '\n' ' ' <<<"$wrong")。检查 deploy/k8s/base/kustomization.yaml 里是否还有 images 段"
  echo "镜像：$(tr '\n' ' ' <<<"$imgs")"
}

# 等迁移 Job 完成。Job 失败、或 Pod 起不来（拉不到镜像、配置引用错误）时立刻报错并打印原因
wait_migrate() {
  local deadline=$((SECONDS + 300)) succeeded failed waiting
  local fatal='ErrImagePull|ImagePullBackOff|ErrImageNeverPull|InvalidImageName|CreateContainerConfigError|CreateContainerError'
  while (( SECONDS < deadline )); do
    succeeded=$(k -n "$NS" get job goapi-migrate -o jsonpath='{.status.succeeded}' 2>/dev/null || true)
    if [[ "${succeeded:-0}" -ge 1 ]]; then echo "迁移完成"; return 0; fi
    failed=$(k -n "$NS" get job goapi-migrate -o jsonpath='{.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null || true)
    waiting=$(k -n "$NS" get pods -l app.kubernetes.io/name=goapi-migrate \
      -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.state.waiting.reason}{" "}{end}{end}' 2>/dev/null || true)
    if [[ "$failed" == "True" ]] || grep -qE "$fatal" <<<"$waiting"; then
      echo; echo "---- 迁移 Pod 日志 ----"
      k -n "$NS" logs -l app.kubernetes.io/name=goapi-migrate --tail 50 --prefix 2>&1 || true
      echo; echo "---- 迁移 Pod 事件 ----"
      k -n "$NS" describe pod -l app.kubernetes.io/name=goapi-migrate 2>&1 | tail -n 15 || true
      die "迁移 Job 没有成功（Failed=${failed:-False}，容器等待原因：${waiting:-无}）"
    fi
    sleep 3
  done
  k -n "$NS" describe pod -l app.kubernetes.io/name=goapi-migrate 2>&1 | tail -n 15 || true
  die "等待迁移 Job 超时（300 秒）"
}

wait_gateway() {
  if ! k -n "$NS" wait --for=condition=Programmed gateway/goapi --timeout=6m; then
    echo; echo "---- Gateway 状态 ----"
    k -n "$NS" get gateway goapi -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}' || true
    echo; echo "---- Envoy Service / Pod ----"
    k -n envoy-gateway-system get svc,pods -l "$ENVOY_SEL" -o wide || true
    k -n envoy-gateway-system describe pod -l "$ENVOY_SEL" 2>&1 | tail -n 15 || true
    die "等待 Gateway Programmed 超时"
  fi
}

do_deploy() {
  assert_cluster
  ensure_secret_files
  assert_kind_images

  echo; echo "== 第 1 步：配置、密钥、迁移 Job"
  k -n "$NS" delete job goapi-migrate --ignore-not-found >/dev/null 2>&1 || true
  k apply -k "$OVERLAY" -l "$SEL_PREDEPLOY"
  wait_migrate

  echo; echo "== 第 2 步：全部资源"
  k apply -k "$OVERLAY"
  # 镜像标签固定是 kind，重新 build 之后 Deployment 的内容没变，不会自动滚动；这里强制滚动一次
  local d
  for d in "${DEPLOYMENTS[@]}"; do k -n "$NS" rollout restart "deployment/$d" >/dev/null; done
  for d in "${DEPLOYMENTS[@]}"; do k -n "$NS" rollout status "deployment/$d" --timeout=5m; done

  echo; echo "== 网关和证书"
  wait_gateway
  k -n "$NS" wait --for=condition=Ready certificate/goapi-api-tls --timeout=3m
  echo; echo "部署完成。下一步：scripts/kind.sh check"
}

# ---------------------------------------------------------------- check 辅助

# 通过网关发请求，输出 "状态码 跳转地址"。用法：gw <http|https> <路径> [额外的 curl 参数...]
gw() {
  local a=("$@")
  local scheme="${a[0]}" path="${a[1]}" port="$HTTPS_PORT"
  [[ "$scheme" == "http" ]] && port="$HTTP_PORT"
  curl --noproxy '*' -s -k -o /dev/null -w '%{http_code} %{redirect_url}' --max-time 10 \
    --resolve "${HOST}:${port}:127.0.0.1" "${a[@]:2}" "${scheme}://${HOST}:${port}${path}" || true
}

start_forward() {
  local svc i code
  svc=$(k -n envoy-gateway-system get svc -l "$ENVOY_SEL,gateway.envoyproxy.io/owning-gateway-namespace=$NS" -o jsonpath='{.items[0].metadata.name}')
  [[ -n "$svc" ]] || die "找不到网关对应的 Envoy Service（先运行 deploy）"
  k -n envoy-gateway-system port-forward "svc/$svc" "${HTTP_PORT}:80" "${HTTPS_PORT}:443" >"$TMPD/pf.log" 2>&1 &
  PF_PID=$!
  for i in $(seq 1 30); do
    sleep 0.5
    code=$(gw https "/api/v1/users?limit=1")
    [[ "$code" =~ ^[1-5][0-9][0-9] ]] && return 0
  done
  cat "$TMPD/pf.log" >&2
  die "port-forward 没有起来（端口 $HTTP_PORT / $HTTPS_PORT 被占用？可以用 KIND_HTTP_PORT / KIND_HTTPS_PORT 换）"
}

# 后台持续请求，直到 stop 文件出现；每行记一个状态码。000 时附上 curl 退出码：7 连不上，28 超时，52 空响应，56 连接被重置
start_traffic() {
  rm -f "$TMPD/stop" "$TMPD/traffic"
  : >"$TMPD/traffic"
  (
    set +e
    while [[ ! -f "$TMPD/stop" ]]; do
      c=$(curl --noproxy '*' -s -k -o /dev/null -w '%{http_code}' --max-time 5 \
        --resolve "${HOST}:${HTTPS_PORT}:127.0.0.1" "https://${HOST}:${HTTPS_PORT}/api/v1/users?limit=1")
      rc=$?
      [[ "$c" == "000" ]] && c="000(curl $rc)"
      echo "$c" >>"$TMPD/traffic"
      sleep 0.05
    done
  ) &
  TRAFFIC_PID=$!
}

# 停止后台请求，设置 TRAFFIC_TOTAL、TRAFFIC_BAD、TRAFFIC_SUMMARY
stop_traffic() {
  touch "$TMPD/stop"
  wait "$TRAFFIC_PID" 2>/dev/null || true
  TRAFFIC_TOTAL=$(wc -l <"$TMPD/traffic" | tr -d ' ')
  TRAFFIC_BAD=$(grep -vc '^200$' "$TMPD/traffic" || true)
  TRAFFIC_SUMMARY=$(sort "$TMPD/traffic" | uniq -c | sed -E 's/^ *([0-9]+) (.*)$/\2×\1/' | paste -sd ' ' -)
}

RESULTS=()
FAILED=0
# 用法：run_check <名称> <函数名>。函数成功时输出说明（以 INFO: 开头表示只展示信息），失败时输出原因并返回非零
run_check() {
  local a=("$@")
  local name="${a[0]}" fn="${a[1]}" out status
  echo "检查：$name ..."
  if out=$("$fn" 2>&1); then
    status="PASS"
    if [[ "$out" == INFO:* ]]; then status="INFO"; out="${out#INFO:}"; fi
  else
    status="FAIL"; FAILED=$((FAILED + 1))
  fi
  out=$(tr '\n' ' ' <<<"$out" | sed -E 's/ +/ /g; s/^ //; s/ $//')
  RESULTS+=("${status}|${name}|${out}")
}

print_results() {
  local r status name detail
  echo
  printf '%-5s %s\n' "结果" "检查 / 说明"
  printf '%-5s %s\n' "----" "-----------"
  for r in "${RESULTS[@]}"; do
    IFS='|' read -r status name detail <<<"$r"
    printf '%-5s %s\n      %s\n' "$status" "$name" "$detail"
  done
  # GitHub Actions 里把结果表写进任务摘要
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    {
      echo "### kind 端到端检查"
      echo
      echo "| 结果 | 检查 | 说明 |"
      echo "| --- | --- | --- |"
      for r in "${RESULTS[@]}"; do
        IFS='|' read -r status name detail <<<"$r"
        case "$status" in PASS) status="✅ PASS" ;; FAIL) status="❌ FAIL" ;; INFO) status="ℹ️ INFO" ;; esac
        echo "| $status | $name | ${detail//|//} |"
      done
    } >>"$GITHUB_STEP_SUMMARY"
  fi
}

# ---------------------------------------------------------------- 各项检查
chk_deployments() {
  local bad="" name ready want
  while IFS='=' read -r name ready; do
    [[ -z "$name" ]] && continue
    want="${ready#*/}"; ready="${ready%/*}"
    if [[ -z "$want" || "$want" == "0" || "${ready:-0}" != "$want" ]]; then bad+="$name ${ready:-0}/${want:-0} "; fi
  done < <(k -n "$NS" get deployments -o jsonpath='{range .items[*]}{.metadata.name}={.status.readyReplicas}/{.status.replicas}{"\n"}{end}')
  [[ -z "$bad" ]] || { echo "未就绪：$bad"; return 1; }
  echo "${#DEPLOYMENTS[@]} 个 Deployment 全部就绪"
}

chk_pod_security() {
  local level
  level=$(k get namespace "$NS" -o jsonpath='{.metadata.labels.pod-security\.kubernetes\.io/enforce}')
  [[ "$level" == "restricted" ]] || { echo "命名空间的 enforce 级别是 '$level'"; return 1; }
  echo "enforce=restricted，所有 Pod 都通过了检查（否则上一项不会就绪）"
}

chk_migrate() {
  local s
  s=$(k -n "$NS" get job goapi-migrate -o jsonpath='{.status.succeeded}')
  [[ "${s:-0}" -ge 1 ]] || { echo "Job 没有成功：$(k -n "$NS" get job goapi-migrate -o jsonpath='{.status}')"; return 1; }
  echo "succeeded=$s"
}

all_true() {
  # 参数是 jsonpath 取出的一串状态值，必须非空且全是 True
  local v="$*"
  [[ -n "$v" ]] && ! grep -qvE '^True$' < <(tr ' ' '\n' <<<"$v" | grep -v '^$')
}

chk_gateway() {
  local v r t
  v=$(k -n "$NS" get gateway goapi -o jsonpath='{.status.conditions[?(@.type=="Programmed")].status}')
  all_true "$v" || { echo "Gateway 未 Programmed：$(k -n "$NS" get gateway goapi -o jsonpath='{.status.conditions[?(@.type=="Programmed")].message}')"; return 1; }
  for r in goapi-api goapi-http-redirect; do
    for t in Accepted ResolvedRefs; do
      v=$(k -n "$NS" get httproute "$r" -o jsonpath="{.status.parents[*].conditions[?(@.type==\"$t\")].status}")
      all_true "$v" || { echo "HTTPRoute $r 的 $t 不是 True（$v）"; return 1; }
    done
  done
  v=$(k -n "$NS" get certificate goapi-api-tls -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  all_true "$v" || { echo "证书未就绪"; return 1; }
  echo "全部正常"
}

chk_redirect() {
  local r; r=$(gw http "/api/v1/users")
  [[ "$r" == "301 https://api.goapi.test"* ]] || { echo "得到：$r"; return 1; }
  echo "$r"
}

chk_https() {
  local r; r=$(gw https "/api/v1/users?limit=1")
  [[ "$r" == 200* ]] || { echo "得到：$r"; return 1; }
  echo "200"
}

chk_not_exposed() {
  local r; r=$(gw https "/healthz")
  [[ "$r" == 404* ]] || { echo "/healthz 通过网关返回 $r，应该是 404（HTTPRoute 只转发 /api）"; return 1; }
  echo "/healthz 返回 404"
}

chk_gzip() {
  local hdr
  hdr=$(curl --noproxy '*' -s -k -D - -o /dev/null --max-time 10 -H 'Accept-Encoding: gzip' \
    --resolve "${HOST}:${HTTPS_PORT}:127.0.0.1" "https://${HOST}:${HTTPS_PORT}/api/v1/users" || true)
  grep -qiE '^content-encoding:[[:space:]]*gzip' <<<"$hdr" || { echo "响应头里没有 Content-Encoding: gzip"; return 1; }
  echo "Content-Encoding: gzip"
}

chk_event_chain() {
  local email r i n
  email="kind-${RANDOM}${RANDOM}@example.com"
  r=$(gw https "/api/v1/users" -X POST -H 'Content-Type: application/json' --data "{\"name\":\"Kind\",\"email\":\"${email}\"}")
  [[ "$r" == 201* ]] || { echo "创建用户返回 $r"; return 1; }
  for i in $(seq 0 29); do
    n=$(k -n goapi-deps exec postgres-0 -- psql -U app -d app -tAc "select count(*) from welcome_notifications where email = '${email}'" 2>/dev/null | tr -d '[:space:]' || true)
    if [[ "$n" == "1" ]]; then echo "约 ${i} 秒后 welcome_notifications 里出现了 $email"; return 0; fi
    sleep 1
  done
  echo "30 秒内 welcome_notifications 里没有 $email（看 relay、consumer 日志）"; return 1
}

chk_client_ip() {
  local ips
  # 只统计经过网关的业务请求（/api 开头），去掉端口后按地址计数
  ips=$(k -n "$NS" logs deployment/goapi-api --tail 200 2>/dev/null | grep '"path":"/api' |
    grep -o '"remote_ip":"[^"]*"' | cut -d'"' -f4 | sed -E 's/:[0-9]+$//' | sort | uniq -c |
    sed -E 's/^ *([0-9]+) (.*)$/\2 ×\1/' | paste -sd ' ' - || true)
  echo "INFO: api 日志里 /api 请求的客户端地址：${ips:-无}。没有端口号说明地址来自 X-Envoy-External-Address（RemoteAddr 一定带端口），也就是 Envoy 认定的客户端地址；kind 里经过 port-forward，Envoy 看到的来源是 127.0.0.1。经过云负载均衡后的真实公网 IP 要到 staging 验证"
}

chk_network_policy() {
  local out
  out=$(k -n goapi-deps run "np-test-${RANDOM}" --rm -i --restart=Never --quiet \
    --pod-running-timeout=2m --image=curlimages/curl:latest --image-pull-policy=IfNotPresent --command -- \
    curl -s -m 5 -o /dev/null -w '%{http_code}' "http://goapi-api.${NS}.svc/healthz" 2>&1 || true)
  if grep -qE '(^|[^0-9])200([^0-9]|$)' <<<"$out"; then echo "从 goapi-deps 直连 api 返回 200：NetworkPolicy 没有生效"; return 1; fi
  grep -qE '(^|[^0-9])000([^0-9]|$)' <<<"$out" || { echo "无法判断，测试 Pod 的输出：$out"; return 1; }
  echo "连接超时，被策略挡住了"
}

chk_hpa() {
  local i cpu cur min max
  # Pod 刚重建时，metrics-server 要采到至少一个周期才有数据，最多等 2 分钟
  for i in $(seq 1 24); do
    cpu=$(k -n "$NS" get hpa goapi-api -o jsonpath='{.status.currentMetrics[?(@.type=="Resource")].resource.current.averageUtilization}')
    if [[ -n "$cpu" ]]; then
      cur=$(k -n "$NS" get hpa goapi-api -o jsonpath='{.status.currentReplicas}')
      min=$(k -n "$NS" get hpa goapi-api -o jsonpath='{.spec.minReplicas}')
      max=$(k -n "$NS" get hpa goapi-api -o jsonpath='{.spec.maxReplicas}')
      echo "当前 CPU ${cpu}%，副本 ${cur}（${min}–${max}）"; return 0
    fi
    sleep 5
  done
  echo "2 分钟内 HPA 都没读到 CPU 指标（kubectl top pods -n $NS 看 metrics-server 是否正常）"; return 1
}

chk_rolling_update() {
  start_traffic
  sleep 3
  k -n "$NS" rollout restart deployment/goapi-api >/dev/null
  k -n "$NS" rollout status deployment/goapi-api --timeout=5m >/dev/null
  sleep 5
  stop_traffic
  (( TRAFFIC_TOTAL >= 50 )) || { echo "只发出了 $TRAFFIC_TOTAL 个请求，结果不可信：$TRAFFIC_SUMMARY"; return 1; }
  (( TRAFFIC_BAD == 0 )) || { echo "$TRAFFIC_TOTAL 个请求里 $TRAFFIC_BAD 个不是 200：$TRAFFIC_SUMMARY"; return 1; }
  echo "$TRAFFIC_TOTAL 个请求全部 200"
}

# 放在滚动更新之后检查：这时的分布是调度器刚做出的决定。
# 放在最前面的话，看到的可能是上一次驱逐测试后的残留（uncordon 之后 Pod 不会自动挪回来）
chk_spread() {
  local lines nodes detail nozone
  # 只看正在运行、没有在终止的 Pod
  lines=$(k -n "$NS" get pods -l app.kubernetes.io/name=goapi-api \
    -o jsonpath='{range .items[?(@.status.phase=="Running")]}{.metadata.deletionTimestamp}|{.metadata.name}|{.spec.nodeName}{"\n"}{end}' | grep '^|' || true)
  nodes=$(cut -d'|' -f3 <<<"$lines" | sort -u | grep -v '^$' || true)
  detail=$(while IFS='|' read -r _ name node; do [[ -n "$name" ]] && printf '%s@%s ' "${name##*-}" "$node"; done <<<"$lines")
  if (( $(grep -c . <<<"$nodes" || true) < 2 )); then
    nozone=$(k get nodes -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.labels.topology\.kubernetes\.io/zone}{"\n"}{end}' | grep '=$' | tr -d '=' | paste -sd ' ' - || true)
    if [[ -n "$nozone" ]]; then
      echo "api 的 Pod 都在 $(tr '\n' ' ' <<<"$nodes")上（$detail）。这些节点没有 topology.kubernetes.io/zone 标签：$nozone"
    else
      echo "api 的 Pod 都在 $(tr '\n' ' ' <<<"$nodes")上（$detail）。检查 Deployment 是否带了 matchLabelKeys: [pod-template-hash]"
    fi
    return 1
  fi
  echo "分布在：$detail"
}

chk_disruption() {
  local envoy_node ctrl_node target w d drain_ok=true
  envoy_node=$(k -n envoy-gateway-system get pods -l "$ENVOY_SEL" -o jsonpath='{.items[0].spec.nodeName}')
  ctrl_node=$(k -n envoy-gateway-system get pods -l control-plane=envoy-gateway -o jsonpath='{.items[0].spec.nodeName}')
  # 不驱逐 Envoy 所在的节点：port-forward 连着那个 Envoy Pod，它没了转发就断了
  for w in $(k get nodes -l '!node-role.kubernetes.io/control-plane' -o jsonpath='{.items[*].metadata.name}'); do
    if [[ "$w" != "$envoy_node" ]]; then target="$w"; break; fi
  done
  [[ -n "${target:-}" ]] || { echo "找不到可以驱逐的工作节点（Envoy 在 $envoy_node）"; return 1; }

  start_traffic
  sleep 3
  k drain "$target" --ignore-daemonsets --delete-emptydir-data --timeout=5m >"$TMPD/drain.log" 2>&1 || drain_ok=false
  for d in "${DEPLOYMENTS[@]}"; do k -n "$NS" rollout status "deployment/$d" --timeout=5m >/dev/null 2>&1 || true; done
  sleep 5
  stop_traffic
  k uncordon "$target" >/dev/null
  # uncordon 不会把 Pod 挪回来：滚动重启一次恢复分散，免得下次 check 一开始就是单节点
  k -n "$NS" rollout restart deployment/goapi-api >/dev/null
  k -n "$NS" rollout status deployment/goapi-api --timeout=5m >/dev/null || true

  $drain_ok || { echo "drain $target 失败：$(tail -n 5 "$TMPD/drain.log" | tr '\n' ' ')"; return 1; }
  if (( TRAFFIC_BAD > 0 )); then
    echo "驱逐 $target（控制面 $ctrl_node）期间 $TRAFFIC_TOTAL 个请求里 $TRAFFIC_BAD 个不是 200：$TRAFFIC_SUMMARY。"
    echo "Envoy 访问日志里非 200 的请求："
    k -n envoy-gateway-system logs -l "$ENVOY_SEL" -c envoy --since=10m 2>/dev/null |
      grep -E '"response_code":(0|[45][0-9][0-9])[,}]' | tail -n 5 || true
    return 1
  fi
  echo "驱逐 $target（Envoy 在 $envoy_node），$TRAFFIC_TOTAL 个请求全部 200，节点已恢复调度"
}

do_check() {
  local disruption="${DISRUPTION}"
  assert_cluster
  need curl

  run_check "所有 Deployment 就绪" chk_deployments
  run_check "Pod Security restricted 生效且所有 Pod 被接受" chk_pod_security
  run_check "迁移 Job 成功" chk_migrate
  run_check "Gateway Programmed，HTTPRoute 已接受，证书已签发" chk_gateway

  start_forward
  run_check "HTTP 跳转到 HTTPS" chk_redirect
  run_check "HTTPS 访问 API" chk_https
  run_check "探针和文档以外的路径不对外" chk_not_exposed
  run_check "响应压缩经过网关后仍然生效" chk_gzip
  run_check "创建用户后事件被消费（经过网关 → api → outbox → relay → NATS → consumer）" chk_event_chain
  run_check "客户端 IP（kind 里只能看到私有地址）" chk_client_ip
  run_check "NetworkPolicy：其它命名空间直连 api 被拒绝" chk_network_policy
  run_check "HPA 能读到 CPU 指标" chk_hpa
  run_check "滚动更新期间不丢请求（优雅关闭）" chk_rolling_update
  run_check "滚动更新后 api 副本分散在不同节点" chk_spread
  if [[ "$disruption" == "true" ]]; then
    run_check "驱逐一个工作节点：PDB 生效，服务不中断" chk_disruption
  fi

  print_results
  if (( FAILED > 0 )); then echo; echo "$FAILED 项失败"; exit 1; fi
  echo; echo "全部通过"
}

# ---------------------------------------------------------------- diag / down
do_diag() {
  set +e
  echo "==== 节点"; k get nodes -o wide -L topology.kubernetes.io/zone
  echo; echo "==== 所有 Pod"; k get pods -A -o wide
  echo; echo "==== 最近的事件"; k get events -A --sort-by=.lastTimestamp | tail -n 40
  echo; echo "==== Gateway / HTTPRoute / 证书"
  k -n "$NS" get gateway,httproute,certificate -o wide
  k -n "$NS" get gateway goapi -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{"\n"}{end}'
  echo; echo "==== 没有就绪的 Pod"
  k get pods -A --field-selector=status.phase!=Running,status.phase!=Succeeded -o name |
    while read -r p; do echo "-- $p"; done
  local d
  for d in "${DEPLOYMENTS[@]}"; do
    echo; echo "==== $d 日志（最后 40 行）"
    k -n "$NS" logs "deployment/$d" --tail 40 --all-containers
  done
  echo; echo "==== 迁移 Job 日志"; k -n "$NS" logs job/goapi-migrate --tail 40
  return 0
}

do_down() { kind delete cluster --name "$CLUSTER"; }

# ---------------------------------------------------------------- 入口
ACTION=""
DISRUPTION="false"
for arg in "$@"; do
  case "$arg" in
    up|build|deploy|check|all|diag|down) ACTION="$arg" ;;
    --disruption|-Disruption) DISRUPTION="true" ;;
    -h|--help) usage; exit 0 ;;
    *) echo "未知参数：$arg" >&2; usage; exit 2 ;;
  esac
done

case "$ACTION" in
  up) do_up ;;
  build) do_build ;;
  deploy) do_deploy ;;
  check) do_check ;;
  all) do_up; do_build; do_deploy; do_check ;;
  diag) do_diag ;;
  down) do_down ;;
  *) usage; exit 2 ;;
esac
