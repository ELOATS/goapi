# 在 Docker 里跑 Go 测试（Linux 环境，和 CI 一致）。
# 用来绕开 Windows 应用控制策略（Smart App Control / App Control for Business）
# 拦截 %TEMP% 里新编译出来的测试程序：
#   fork/exec ...\go-build...\xxx.test.exe: An Application Control policy has blocked this file.
#
# 用法（在项目根目录或任意目录执行都可以）：
#   .\scripts\test-in-docker.ps1                  # 单元测试（集成测试自动跳过）
#   .\scripts\test-in-docker.ps1 -Integration     # 先启动 db / redis / nats，再跑全部测试（含集成测试）
#   .\scripts\test-in-docker.ps1 -Packages ./internal/consumer/...
#
# 依赖下载和编译缓存放在 Docker 卷 goapi-gomod、goapi-gocache 里，第二次运行会快很多。
param(
  [switch]$Integration,
  # 和 docker-compose.yml 一样，可以用环境变量 GOAPI_DB_PORT 等覆盖
  [int]$DbPort = $(if ($env:GOAPI_DB_PORT) { [int]$env:GOAPI_DB_PORT } else { 15432 }),
  [int]$RedisPort = $(if ($env:GOAPI_REDIS_PORT) { [int]$env:GOAPI_REDIS_PORT } else { 16379 }),
  [int]$NatsPort = $(if ($env:GOAPI_NATS_PORT) { [int]$env:GOAPI_NATS_PORT } else { 14222 }),
  [string]$Packages = "./...",
  [string]$GoImage = ""
)
$ErrorActionPreference = "Stop"

$root = Split-Path -Parent $PSScriptRoot
Set-Location $root

# 镜像版本跟着 go.mod 的 go 版本走
if (-not $GoImage) {
  $m = Select-String -Path go.mod -Pattern '^go (\d+\.\d+)' | Select-Object -First 1
  if (-not $m) { throw "go.mod 里没有找到 go 版本" }
  $GoImage = "golang:" + $m.Matches[0].Groups[1].Value
}

$dockerArgs = @(
  "run", "--rm",
  "-v", "${root}:/src", "-w", "/src",
  "-v", "goapi-gomod:/go/pkg/mod",
  "-v", "goapi-gocache:/root/.cache/go-build"
)

if ($Integration) {
  Write-Host "启动 db / redis / nats ..."
  docker compose up -d --wait db redis nats
  if ($LASTEXITCODE -ne 0) { throw "docker compose up 失败" }

  # 集成测试用独立的 app_test 库，不碰本地环境的 app 库（测试会清空表、留下测试数据）。
  # 新数据卷由 deploy/postgres-init 建好；已有的数据卷在这里补建。
  $exists = docker compose exec -T db psql -U app -d app -tAc "SELECT 1 FROM pg_database WHERE datname='app_test'"
  if ("$exists".Trim() -ne "1") {
    Write-Host "创建测试库 app_test ..."
    docker compose exec -T db psql -U app -d app -c "CREATE DATABASE app_test OWNER app"
    if ($LASTEXITCODE -ne 0) { throw "创建 app_test 失败" }
  }

  # 不加入 compose 的网络，而是通过宿主机上映射出来的端口（默认 15432 / 16379 / 14222）连接：
  # compose 网络被删除重建过时，已经在运行的容器可能还连着旧网络，按服务名或网络 IP 都连不上。
  # 走宿主机端口不受这个影响。
  $dockerArgs += @(
    "--add-host=host.docker.internal:host-gateway",
    "-e", "TEST_DATABASE_URL=postgres://app:app@host.docker.internal:${DbPort}/app_test?sslmode=disable",
    "-e", "TEST_REDIS_URL=redis://host.docker.internal:${RedisPort}/15",
    "-e", "TEST_NATS_URL=nats://host.docker.internal:${NatsPort}"
  )
  # -p 1：store、outbox、consumer 的集成测试共用同一个数据库，必须串行
  $testArgs = @("go", "test", "-race", "-count=1", "-p", "1", $Packages)
} else {
  $testArgs = @("go", "test", "-race", $Packages)
}

Write-Host "$GoImage> $($testArgs -join ' ')"
& docker @dockerArgs $GoImage @testArgs
exit $LASTEXITCODE
