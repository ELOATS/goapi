# 从本地 Keycloak 拿 access token（需要先用 docker-compose.auth.yml 启动）。
#   .\scripts\get-token.ps1 alice              # 输出 token
#   $t = .\scripts\get-token.ps1 bob
#   Invoke-RestMethod http://localhost:8080/api/v1/me -Headers @{ Authorization = "Bearer $t" }
# 测试用户的密码和用户名相同，见 deploy/keycloak/README.md。
param(
  [ValidateSet("alice", "bob", "carol")]
  [string]$User = "alice",
  [int]$Port = $(if ($env:GOAPI_KEYCLOAK_PORT) { [int]$env:GOAPI_KEYCLOAK_PORT } else { 8180 })
)
$ErrorActionPreference = "Stop"

$body = @{
  grant_type = "password"
  client_id  = "goapi-cli"
  username   = $User
  password   = $User
  scope      = "openid"
}
try {
  $r = Invoke-RestMethod -Method Post -Uri "http://localhost:$Port/realms/goapi/protocol/openid-connect/token" -Body $body
} catch {
  throw "拿 token 失败：$($_.Exception.Message)。Keycloak 起来了吗？docker compose -f docker-compose.yml -f docker-compose.auth.yml ps keycloak"
}
$r.access_token
