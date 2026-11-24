# 在本地 kind 集群里部署并验证 goapi 的 Kubernetes 配置。
#
#   .\scripts\kind.ps1 up          创建集群，安装 Envoy Gateway、cert-manager、metrics-server，部署 Postgres / Redis / NATS
#   .\scripts\kind.ps1 build       构建 4 个镜像并导入集群
#   .\scripts\kind.ps1 deploy      跑迁移 Job，部署 overlays/kind，等滚动更新完成
#   .\scripts\kind.ps1 check       自动检查网关、证书、事件链路、NetworkPolicy、HPA、滚动更新不丢请求
#   .\scripts\kind.ps1 check -Disruption   另外驱逐一个工作节点，验证 PDB 和服务不中断
#   .\scripts\kind.ps1 forward     把网关转发到本机 18080 / 18443，手动用 curl 访问（Ctrl+C 结束）
#   .\scripts\kind.ps1 down        删除集群
#
# 需要：Docker Desktop、kind、kubectl、helm（winget install Kubernetes.kind Kubernetes.kubectl Helm.Helm）。
# 所有 kubectl / helm 命令都显式指定 --context kind-<集群名>，不会碰到你的其它集群。
param(
  [Parameter(Mandatory = $true, Position = 0)]
  [ValidateSet("up", "build", "deploy", "check", "forward", "down")]
  [string]$Action,
  [string]$Cluster = "goapi",
  [string]$Namespace = "goapi-kind",
  [string]$EnvoyGatewayVersion = "v1.9.0",
  [int]$HttpPort = 18080,
  [int]$HttpsPort = 18443,
  [switch]$Disruption
)
$ErrorActionPreference = "Stop"
$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

$ctx = "kind-$Cluster"
$overlay = "deploy/k8s/overlays/kind"
$hostName = "api.goapi.test"
$deployments = @("goapi-api", "goapi-relay", "goapi-consumer", "goapi-alertbridge")
$images = @("api", "relay", "consumer", "alertbridge")

function Need([string]$cmd, [string]$hint) {
  if (-not (Get-Command $cmd -ErrorAction SilentlyContinue)) { throw "找不到 $cmd。$hint" }
}

# 运行外部命令，失败时抛异常
function Run {
  $exe = $args[0]
  $rest = @()
  if ($args.Count -gt 1) { $rest = $args[1..($args.Count - 1)] }
  & $exe @rest
  if ($LASTEXITCODE -ne 0) { throw "命令失败（退出码 $LASTEXITCODE）：$exe $($rest -join ' ')" }
}

# 运行外部命令，忽略 stderr 和退出码（Windows PowerShell 5.1 在 Stop 模式下重定向 stderr 会直接抛异常，这里临时改成 Continue）
function Quiet {
  $ErrorActionPreference = "Continue"
  $exe = $args[0]
  $rest = @()
  if ($args.Count -gt 1) { $rest = $args[1..($args.Count - 1)] }
  & $exe @rest 2>$null
}

# 运行外部命令，stderr 合并进输出，忽略退出码
function Capture {
  $ErrorActionPreference = "Continue"
  $exe = $args[0]
  $rest = @()
  if ($args.Count -gt 1) { $rest = $args[1..($args.Count - 1)] }
  (& $exe @rest 2>&1 | ForEach-Object { "$_" }) -join "`n"
}

function K { Run kubectl --context $ctx @args }
function KQuiet { Quiet kubectl --context $ctx @args }
function KJson { (Run kubectl --context $ctx @args -o json | Out-String) | ConvertFrom-Json }

function Assert-Cluster {
  $clusters = Quiet kind get clusters
  if (-not ($clusters -contains $Cluster)) { throw "kind 集群 $Cluster 不存在，先运行 .\scripts\kind.ps1 up" }
}

function Ensure-SecretFiles {
  # base 的 secretGenerator 需要这两个文件存在；kind overlay 会整个替换 goapi-secrets 的内容，
  # 所以这里只在文件不存在时从示例复制，不会改动你已有的 secrets.env
  foreach ($f in @("secrets.env", "alertbridge-secrets.env")) {
    $p = "deploy/k8s/base/$f"
    if (-not (Test-Path $p)) { Copy-Item "$p.example" $p; Write-Host "已从示例创建 $p" }
  }
}

function Get-EnvoyService {
  $sel = "gateway.envoyproxy.io/owning-gateway-name=goapi,gateway.envoyproxy.io/owning-gateway-namespace=$Namespace"
  $svc = KJson -n envoy-gateway-system get svc -l $sel
  if (-not $svc.items -or $svc.items.Count -eq 0) { throw "找不到网关对应的 Envoy Service（Gateway 还没 Programmed？先运行 deploy）" }
  return $svc.items[0].metadata.name
}

function Start-Forward {
  $svc = Get-EnvoyService
  $p = Start-Process kubectl -PassThru -WindowStyle Hidden -ArgumentList @(
    "--context", $ctx, "-n", "envoy-gateway-system", "port-forward", "svc/$svc", "${HttpPort}:80", "${HttpsPort}:443")
  for ($i = 0; $i -lt 20; $i++) {
    Start-Sleep -Milliseconds 500
    $c = Gw -Scheme https -Path "/api/v1/users?limit=1"
    if ($c -match '^\d{3}' -and $c -notmatch '^000') { return $p }
  }
  Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
  throw "port-forward 没有起来（本机端口 $HttpPort / $HttpsPort 被占用？可以用 -HttpPort / -HttpsPort 换）"
}

# 通过网关发请求，返回 "状态码 跳转地址"
function Gw {
  param([string]$Scheme = "https", [string]$Path = "/", [string[]]$Extra = @())
  $port = $HttpsPort
  if ($Scheme -eq "http") { $port = $HttpPort }
  $a = @("--noproxy", "*", "-s", "-k", "-o", "NUL", "-w", "%{http_code} %{redirect_url}", "--max-time", "10",
    "--resolve", "${hostName}:${port}:127.0.0.1", "-H", "Host: $hostName") + $Extra + @("${Scheme}://${hostName}:${port}$Path")
  return (& curl.exe @a | Out-String).Trim()
}

function Gw-Headers([string]$Path, [string[]]$Extra = @()) {
  $a = @("--noproxy", "*", "-s", "-k", "-D", "-", "-o", "NUL", "--max-time", "10",
    "--resolve", "${hostName}:${HttpsPort}:127.0.0.1", "-H", "Host: $hostName") + $Extra + @("https://${hostName}:${HttpsPort}$Path")
  return (& curl.exe @a | Out-String)
}

# 后台不停地请求，直到 $stopFile 出现；返回各状态码的次数
function Start-Traffic([string]$stopFile) {
  return Start-Job -ArgumentList $HttpsPort, $hostName, $stopFile -ScriptBlock {
    param($port, $h, $stop)
    $codes = @{}
    while (-not (Test-Path $stop)) {
      $c = & curl.exe --noproxy "*" -s -k -o NUL -w "%{http_code}" --max-time 5 --resolve "${h}:${port}:127.0.0.1" -H "Host: $h" "https://${h}:${port}/api/v1/users?limit=1"
      $c = "$c"
      if ($codes.ContainsKey($c)) { $codes[$c] = $codes[$c] + 1 } else { $codes[$c] = 1 }
      Start-Sleep -Milliseconds 50
    }
    $codes
  }
}

function Stop-Traffic($job, [string]$stopFile) {
  New-Item -ItemType File -Path $stopFile -Force | Out-Null
  $codes = Receive-Job -Job $job -Wait -AutoRemoveJob
  Remove-Item $stopFile -ErrorAction SilentlyContinue
  $total = 0; $bad = 0; $parts = @()
  foreach ($k in ($codes.Keys | Sort-Object)) {
    $n = [int]$codes[$k]; $total += $n; $parts += "$k×$n"
    if ($k -ne "200") { $bad += $n }
  }
  return [pscustomobject]@{ Total = $total; Bad = $bad; Summary = ($parts -join "，") }
}

# ---------------------------------------------------------------- up
function Do-Up {
  Need docker "需要 Docker Desktop"
  Need kind "winget install Kubernetes.kind"
  Need kubectl "winget install Kubernetes.kubectl"
  Need helm "winget install Helm.Helm"

  $clusters = Quiet kind get clusters
  if ($clusters -contains $Cluster) {
    Write-Host "集群 $Cluster 已存在，跳过创建"
  } else {
    Run kind create cluster --name $Cluster --config deploy/kind/cluster.yaml --wait 3m
  }

  Write-Host "`n== Envoy Gateway $EnvoyGatewayVersion（同时安装 Gateway API CRD）"
  Run helm --kube-context $ctx upgrade --install eg oci://docker.io/envoyproxy/gateway-helm `
    --version $EnvoyGatewayVersion -n envoy-gateway-system --create-namespace --wait --timeout 5m
  K apply -f deploy/kind/gatewayclass.yaml

  # cert-manager 要在 Gateway API CRD 之后安装，否则它不会监听 Gateway
  Write-Host "`n== cert-manager（开启 Gateway API 支持）"
  Run helm repo add jetstack https://charts.jetstack.io --force-update
  Run helm --kube-context $ctx upgrade --install cert-manager jetstack/cert-manager `
    -n cert-manager --create-namespace --wait --timeout 5m `
    --set crds.enabled=true `
    --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 `
    --set config.kind=ControllerConfiguration `
    --set config.enableGatewayAPI=true

  Write-Host "`n== metrics-server（HPA 需要）"
  Run helm repo add metrics-server https://kubernetes-sigs.github.io/metrics-server/ --force-update
  Run helm --kube-context $ctx upgrade --install metrics-server metrics-server/metrics-server `
    -n kube-system --wait --timeout 5m --set "args={--kubelet-insecure-tls}"

  Write-Host "`n== Postgres / Redis / NATS"
  K apply -f deploy/kind/deps.yaml
  K -n goapi-deps rollout status statefulset/postgres --timeout=3m
  K -n goapi-deps rollout status deployment/redis --timeout=3m
  K -n goapi-deps rollout status deployment/nats --timeout=3m

  Write-Host "`n集群准备好了。下一步：.\scripts\kind.ps1 build，然后 .\scripts\kind.ps1 deploy"
}

# ---------------------------------------------------------------- build
function Do-Build {
  Assert-Cluster
  foreach ($c in $images) {
    Write-Host "`n== 构建 goapi/${c}:kind"
    Run docker build -t "goapi/${c}:kind" --build-arg "CMD=$c" .
  }
  Run kind load docker-image --name $Cluster @($images | ForEach-Object { "goapi/${_}:kind" })
}

# ---------------------------------------------------------------- deploy
function Do-Deploy {
  Assert-Cluster
  Ensure-SecretFiles
  $sel = "goapi.example.com/phase=pre-deploy"

  Write-Host "`n== 第 1 步：配置、密钥、迁移 Job"
  KQuiet -n $Namespace delete job goapi-migrate --ignore-not-found | Out-Null
  K apply -k $overlay -l $sel
  K -n $Namespace wait --for=condition=complete job/goapi-migrate --timeout=5m

  Write-Host "`n== 第 2 步：全部资源"
  K apply -k $overlay
  # 镜像标签固定是 kind，重新 build 之后 Deployment 的内容没变，不会自动滚动；这里强制滚动一次
  foreach ($d in $deployments) { K -n $Namespace rollout restart "deployment/$d" | Out-Null }
  foreach ($d in $deployments) { K -n $Namespace rollout status "deployment/$d" --timeout=5m }

  Write-Host "`n== 网关和证书"
  K -n $Namespace wait --for=condition=Programmed gateway/goapi --timeout=3m
  K -n $Namespace wait --for=condition=Ready certificate/goapi-api-tls --timeout=3m
  Write-Host "`n部署完成。下一步：.\scripts\kind.ps1 check"
}

# ---------------------------------------------------------------- check
$results = New-Object System.Collections.Generic.List[object]
function Check([string]$name, [scriptblock]$test) {
  Write-Host "检查：$name ..."
  $status = "PASS"
  try {
    $detail = & $test
    if ($detail -is [array]) { $detail = $detail -join " " }
    if ("$detail".StartsWith("INFO:")) { $status = "INFO"; $detail = "$detail".Substring(5).Trim() }
  } catch {
    $status = "FAIL"; $detail = $_.Exception.Message
  }
  $results.Add([pscustomobject]@{ 结果 = $status; 检查 = $name; 说明 = "$detail" })
}

function Do-Check {
  Assert-Cluster
  Need curl.exe "Windows 10 以上自带 curl.exe"

  Check "所有 Deployment 就绪" {
    $bad = @()
    foreach ($d in (KJson -n $Namespace get deployments).items) {
      $want = [int]$d.status.replicas; $ready = [int]$d.status.readyReplicas
      if ($want -eq 0 -or $ready -ne $want) { $bad += "$($d.metadata.name) $ready/$want" }
    }
    if ($bad.Count -gt 0) { throw "未就绪：$($bad -join '，')" }
    "4 个 Deployment 全部就绪"
  }

  Check "Pod Security restricted 生效且所有 Pod 被接受" {
    $ns = KJson get namespace $Namespace
    $level = $ns.metadata.labels.'pod-security.kubernetes.io/enforce'
    if ($level -ne "restricted") { throw "命名空间的 enforce 级别是 '$level'" }
    "enforce=restricted，所有 Pod 都通过了检查（否则上一项不会就绪）"
  }

  Check "迁移 Job 成功" {
    $j = KJson -n $Namespace get job goapi-migrate
    if ([int]$j.status.succeeded -lt 1) { throw "Job 没有成功：$($j.status | ConvertTo-Json -Compress)" }
    "succeeded=$($j.status.succeeded)"
  }

  Check "api 副本分散在不同节点" {
    $nodes = (KJson -n $Namespace get pods -l app.kubernetes.io/name=goapi-api).items | ForEach-Object { $_.spec.nodeName } | Sort-Object -Unique
    if (@($nodes).Count -lt 2) { throw "api 的 Pod 都在 $($nodes -join ',') 上" }
    "分布在：$($nodes -join '，')"
  }

  Check "Gateway Programmed，HTTPRoute 已接受，证书已签发" {
    $gw = KJson -n $Namespace get gateway goapi
    $prog = $gw.status.conditions | Where-Object { $_.type -eq "Programmed" }
    if ($prog.status -ne "True") { throw "Gateway 未 Programmed：$($prog.message)" }
    foreach ($r in @("goapi-api", "goapi-http-redirect")) {
      $rt = KJson -n $Namespace get httproute $r
      foreach ($t in @("Accepted", "ResolvedRefs")) {
        $c = $rt.status.parents | ForEach-Object { $_.conditions } | Where-Object { $_.type -eq $t }
        if (-not $c -or ($c | Where-Object { $_.status -ne "True" })) { throw "HTTPRoute $r 的 $t 不是 True" }
      }
    }
    $cert = KJson -n $Namespace get certificate goapi-api-tls
    $ready = $cert.status.conditions | Where-Object { $_.type -eq "Ready" }
    if ($ready.status -ne "True") { throw "证书未就绪：$($ready.message)" }
    "全部正常"
  }

  $pf = $null
  try {
    $pf = Start-Forward

    Check "HTTP 跳转到 HTTPS" {
      $r = Gw -Scheme http -Path "/api/v1/users"
      if ($r -notmatch '^301 https://api\.goapi\.test') { throw "得到：$r" }
      $r
    }

    Check "HTTPS 访问 API" {
      $r = Gw -Path "/api/v1/users?limit=1"
      if ($r -notmatch '^200') { throw "得到：$r" }
      "200"
    }

    Check "探针和文档以外的路径不对外" {
      $r = Gw -Path "/healthz"
      if ($r -notmatch '^404') { throw "/healthz 通过网关返回 $r，应该是 404（HTTPRoute 只转发 /api）" }
      "/healthz 返回 404"
    }

    Check "响应压缩经过网关后仍然生效" {
      $h = Gw-Headers "/api/v1/users" @("-H", "Accept-Encoding: gzip")
      if ($h -notmatch '(?im)^content-encoding:\s*gzip') { throw "响应头里没有 Content-Encoding: gzip" }
      "Content-Encoding: gzip"
    }

    Check "创建用户后事件被消费（经过网关 → api → outbox → relay → NATS → consumer）" {
      $email = "kind-$(Get-Random)@example.com"
      $tmp = [IO.Path]::GetTempFileName()
      [IO.File]::WriteAllText($tmp, '{"name":"Kind","email":"' + $email + '"}')
      try {
        $r = Gw -Path "/api/v1/users" -Extra @("-X", "POST", "-H", "Content-Type: application/json", "--data-binary", "@$tmp")
      } finally { Remove-Item $tmp -ErrorAction SilentlyContinue }
      if ($r -notmatch '^201') { throw "创建用户返回 $r" }
      for ($i = 0; $i -lt 30; $i++) {
        $n = (& kubectl --context $ctx -n goapi-deps exec postgres-0 -- psql -U app -d app -tAc "select count(*) from welcome_notifications where email = '$email'" | Out-String).Trim()
        if ($n -eq "1") { return "约 $i 秒后 welcome_notifications 里出现了 $email" }
        Start-Sleep 1
      }
      throw "30 秒内 welcome_notifications 里没有 $email（看 relay、consumer 日志）"
    }

    Check "客户端 IP（kind 里只能看到私有地址）" {
      $lines = KQuiet -n $Namespace logs deployment/goapi-api --tail 200
      $ips = @($lines | ForEach-Object { if ($_ -match '"remote_ip":"([^"]+)"') { $Matches[1] } } | Sort-Object -Unique)
      "INFO: api 日志里的 remote_ip：$($ips -join '，')。kind 里请求来自私有网段，Envoy 不写 X-Envoy-External-Address，这里是 Envoy Pod 的地址；真实客户端 IP 要到 staging 验证"
    }

    Check "NetworkPolicy：其它命名空间直连 api 被拒绝" {
      $out = Capture kubectl --context $ctx -n goapi-deps run "np-test-$(Get-Random)" --rm -i --restart=Never --quiet `
        --pod-running-timeout=2m --image=curlimages/curl --command -- `
        curl -s -m 5 -o /dev/null -w "%{http_code}" "http://goapi-api.$Namespace.svc/healthz"
      if ($out -match '(^|\D)200(\D|$)') { throw "从 goapi-deps 直连 api 返回 200：NetworkPolicy 没有生效（这个集群的 CNI 可能不支持 NetworkPolicy）" }
      if ($out -notmatch '(^|\D)000(\D|$)') { throw "无法判断，测试 Pod 的输出：$out" }
      "连接超时，被策略挡住了"
    }

    Check "HPA 能读到 CPU 指标" {
      $h = KJson -n $Namespace get hpa goapi-api
      $m = $h.status.currentMetrics | Where-Object { $_.type -eq "Resource" } | Select-Object -First 1
      if (-not $m -or $null -eq $m.resource.current.averageUtilization) { throw "还没有指标（metrics-server 刚启动时要 1–2 分钟），稍后重跑" }
      "当前 CPU $($m.resource.current.averageUtilization)%，副本 $($h.status.currentReplicas)（$($h.spec.minReplicas)–$($h.spec.maxReplicas)）"
    }

    Check "滚动更新期间不丢请求（优雅关闭）" {
      $stop = Join-Path $env:TEMP "goapi-kind-traffic-$PID"
      Remove-Item $stop -ErrorAction SilentlyContinue
      $job = Start-Traffic $stop
      Start-Sleep 3
      K -n $Namespace rollout restart deployment/goapi-api | Out-Null
      K -n $Namespace rollout status deployment/goapi-api --timeout=5m | Out-Null
      Start-Sleep 5
      $t = Stop-Traffic $job $stop
      if ($t.Total -lt 50) { throw "只发出了 $($t.Total) 个请求，结果不可信：$($t.Summary)" }
      if ($t.Bad -gt 0) { throw "$($t.Total) 个请求里 $($t.Bad) 个不是 200：$($t.Summary)" }
      "$($t.Total) 个请求全部 200"
    }

    if ($Disruption) {
      Check "驱逐一个工作节点：PDB 生效，服务不中断" {
        $envoyNode = ((KJson -n envoy-gateway-system get pods -l "gateway.envoyproxy.io/owning-gateway-name=goapi").items | Select-Object -First 1).spec.nodeName
        $workers = (KJson get nodes).items | Where-Object { -not $_.metadata.labels.PSObject.Properties["node-role.kubernetes.io/control-plane"] } | ForEach-Object { $_.metadata.name }
        # 不驱逐 Envoy 所在的节点：port-forward 连着那个 Envoy Pod，它没了转发就断了
        $target = $workers | Where-Object { $_ -ne $envoyNode } | Select-Object -First 1
        if (-not $target) { throw "找不到可以驱逐的工作节点（Envoy 在 $envoyNode）" }

        $stop = Join-Path $env:TEMP "goapi-kind-traffic-$PID"
        Remove-Item $stop -ErrorAction SilentlyContinue
        $job = Start-Traffic $stop
        try {
          Start-Sleep 3
          K drain $target --ignore-daemonsets --delete-emptydir-data --timeout=5m | Out-Null
          foreach ($d in $deployments) { K -n $Namespace rollout status "deployment/$d" --timeout=5m | Out-Null }
          Start-Sleep 5
        } finally {
          $t = Stop-Traffic $job $stop
          K uncordon $target | Out-Null
        }
        if ($t.Bad -gt 0) { throw "驱逐 $target 期间 $($t.Total) 个请求里 $($t.Bad) 个不是 200：$($t.Summary)" }
        "驱逐 $target（Envoy 在 $envoyNode），$($t.Total) 个请求全部 200，节点已恢复调度"
      }
    }
  } finally {
    if ($pf) { Stop-Process -Id $pf.Id -Force -ErrorAction SilentlyContinue }
  }

  Write-Host ""
  $results | Format-Table -AutoSize -Wrap
  $failed = @($results | Where-Object { $_.结果 -eq "FAIL" }).Count
  if ($failed -gt 0) { Write-Host "$failed 项失败" -ForegroundColor Red; exit 1 }
  Write-Host "全部通过" -ForegroundColor Green
}

# ---------------------------------------------------------------- forward
function Do-Forward {
  Assert-Cluster
  $svc = Get-EnvoyService
  Write-Host "网关转发到 http://localhost:$HttpPort 和 https://localhost:$HttpsPort（Ctrl+C 结束）。另开一个窗口访问："
  Write-Host "  curl.exe --noproxy `"*`" -k --resolve ${hostName}:${HttpsPort}:127.0.0.1 -H `"Host: $hostName`" https://${hostName}:${HttpsPort}/api/v1/users"
  Write-Host "  curl.exe --noproxy `"*`" -i --resolve ${hostName}:${HttpPort}:127.0.0.1 -H `"Host: $hostName`" http://${hostName}:${HttpPort}/api/v1/users"
  K -n envoy-gateway-system port-forward "svc/$svc" "${HttpPort}:80" "${HttpsPort}:443"
}

# ---------------------------------------------------------------- down
function Do-Down {
  Run kind delete cluster --name $Cluster
}

switch ($Action) {
  "up" { Do-Up }
  "build" { Do-Build }
  "deploy" { Do-Deploy }
  "check" { Do-Check }
  "forward" { Do-Forward }
  "down" { Do-Down }
}
