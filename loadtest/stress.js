// 压力测试：逐步加压，找出服务的极限，并观察 HPA 扩容。
//   START_RPS   起始速率，默认 20
//   TARGET_RPS  最高速率，默认 300
//   RAMP        从起始加到最高用多久，默认 10m
//   HOLD        在最高速率保持多久，默认 5m
//   WRITE_RATIO 写请求占比，默认 0.02
//
// 同时运行 loadtest/watch-hpa.sh（或 .ps1）记录副本数变化。错误率超过 1% 持续 1 分钟会自动停止。
import * as api from './lib/api.js';
import { mixedIteration } from './lib/workload.js';

const START = api.envNum('START_RPS', 20);
const TARGET = api.envNum('TARGET_RPS', 300);
const RAMP = __ENV.RAMP || '10m';
const HOLD = __ENV.HOLD || '5m';
const WRITE_RATIO = api.envNum('WRITE_RATIO', 0.02);

export const options = {
  scenarios: {
    ramp: {
      executor: 'ramping-arrival-rate',
      startRate: START,
      timeUnit: '1s',
      preAllocatedVUs: Math.max(20, Math.ceil(TARGET / 5)),
      maxVUs: Math.max(100, TARGET * 2),
      stages: [
        { target: START, duration: '2m' }, // 预热：让连接池、JIT 编译后的路径都稳定下来
        { target: TARGET, duration: RAMP },
        { target: TARGET, duration: HOLD },
        { target: 0, duration: '2m' },
      ],
    },
  },
  thresholds: {
    http_req_failed: [{ threshold: 'rate<0.01', abortOnFail: true, delayAbortEval: '1m' }],
    http_req_duration: ['p(99)<1000'],
    rate_limited: ['count==0'],
  },
};

export function setup() {
  api.preflight(WRITE_RATIO > 0);
  return { ids: api.seedUserIDs(50, WRITE_RATIO > 0) };
}

export default function (data) {
  mixedIteration(data, WRITE_RATIO);
}
