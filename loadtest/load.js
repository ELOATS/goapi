// 负载测试：以固定速率跑混合负载，验证延迟和错误率是否满足 SLO。
//   RATE        每秒请求数（迭代数），默认 50
//   DURATION    持续时间，默认 5m
//   WRITE_RATIO 写请求占比，默认 0.05；只读压测设为 0
//
// 单机 k6 的所有请求来自同一个 IP，用同一批用户：先把限流关掉或调高，否则测到的是限流，
// 见 README「压测」。
import * as api from './lib/api.js';
import { mixedIteration } from './lib/workload.js';

const RATE = api.envNum('RATE', 50);
const DURATION = __ENV.DURATION || '5m';
const WRITE_RATIO = api.envNum('WRITE_RATIO', 0.05);

export const options = {
  scenarios: {
    mixed: {
      executor: 'constant-arrival-rate',
      rate: RATE,
      timeUnit: '1s',
      duration: DURATION,
      preAllocatedVUs: Math.max(10, Math.ceil(RATE / 5)),
      maxVUs: Math.max(50, RATE * 2),
    },
  },
  // 和告警规则对齐：P99 超过 1s 会触发 GoapiHighLatencyP99；99.9% 可用性 SLO 对应错误率 < 0.1%
  thresholds: {
    http_req_failed: ['rate<0.001'],
    checks: ['rate>0.999'],
    rate_limited: ['count==0'],
    dropped_iterations: ['count==0'], // k6 的 VU 不够用，说明压测机本身成了瓶颈
    'http_req_duration{op:listUsers}': ['p(95)<300', 'p(99)<1000'],
    'http_req_duration{op:getUser}': ['p(95)<200', 'p(99)<1000'],
    'http_req_duration{op:createUser}': ['p(95)<500', 'p(99)<1000'],
  },
};

export function setup() {
  api.preflight(WRITE_RATIO > 0);
  return { ids: api.seedUserIDs(20, WRITE_RATIO > 0) };
}

export default function (data) {
  mixedIteration(data, WRITE_RATIO);
}
