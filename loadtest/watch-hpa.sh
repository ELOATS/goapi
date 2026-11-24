#!/usr/bin/env bash
# 压测时记录 HPA 和 Pod 数量的变化，输出 CSV。
#   NS=goapi-staging ./loadtest/watch-hpa.sh | tee hpa.csv
set -euo pipefail
NS="${NS:-goapi-staging}"
INTERVAL="${INTERVAL:-10}"

echo "time,current_replicas,desired_replicas,cpu_utilization_percent,ready_pods"
while true; do
  hpa=$(kubectl -n "$NS" get hpa goapi-api \
    -o jsonpath='{.status.currentReplicas},{.status.desiredReplicas},{.status.currentMetrics[0].resource.current.averageUtilization}' 2>/dev/null || echo ",,")
  ready=$(kubectl -n "$NS" get deployment goapi-api -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
  echo "$(date -u +%Y-%m-%dT%H:%M:%SZ),${hpa},${ready:-0}"
  sleep "$INTERVAL"
done
