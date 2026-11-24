# 伪造一个"声明都对、但签名是假的、kid 也不认识"的 token，用来验证"拉不到公钥返回 503"：
#   Keycloak 正常：api 重新拉取公钥，仍然找不到这把钥匙 → 401
#   Keycloak 停掉：重新拉取失败，无法判断 → 503
# iss、aud、exp 必须正确，否则 api 在验签之前就拒绝了（401），走不到拉公钥那一步。
param(
  [int]$Port = $(if ($env:GOAPI_KEYCLOAK_PORT) { [int]$env:GOAPI_KEYCLOAK_PORT } else { 8180 })
)
function B64Url([string]$s) {
  [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($s)).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}
$now = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$header = B64Url('{"alg":"RS256","typ":"JWT","kid":"forged-key-id"}')
$payload = B64Url((@{
  iss = "http://localhost:$Port/realms/goapi"
  aud = "goapi"
  sub = "forged-user"
  iat = $now
  exp = $now + 3600
} | ConvertTo-Json -Compress))
$sig = B64Url("this-is-not-a-real-signature-0123456789")
"$header.$payload.$sig"
