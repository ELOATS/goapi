# 压测时记录 HPA 和 Pod 数量的变化，输出 CSV（Windows PowerShell 版）。
#   .\loadtest\watch-hpa.ps1 -Namespace goapi-staging | Tee-Object hpa.csv
param(
  [string]$Namespace = "goapi-staging",
  [int]$Interval = 10
)

"time,current_replicas,desired_replicas,cpu_utilization_percent,ready_pods"
while ($true) {
  $hpa = kubectl -n $Namespace get hpa goapi-api `
    -o "jsonpath={.status.currentReplicas},{.status.desiredReplicas},{.status.currentMetrics[0].resource.current.averageUtilization}" 2>$null
  if (-not $hpa) { $hpa = ",," }
  $ready = kubectl -n $Namespace get deployment goapi-api -o "jsonpath={.status.readyReplicas}" 2>$null
  if (-not $ready) { $ready = 0 }
  "{0},{1},{2}" -f (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ"), $hpa, $ready
  Start-Sleep -Seconds $Interval
}
