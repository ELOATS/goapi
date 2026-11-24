// 限流验证：确认限流按配置生效、429 响应格式正确、按 IP 限流在认证之前执行。
// 需要服务开着限流（本地 docker compose 默认就是），并且有 Redis 或只有一个 API 副本，
// 否则实际额度是"额度 × 副本数"。
//
// 场景一 per_client：同一个用户、同一个 IP，以 OVER_FACTOR 倍于 EXPECT_RPS 的速率请求 60 秒。
//   预期放行 ≈ EXPECT_BURST + EXPECT_RPS × 60，其余 429。
//   EXPECT_RPS / EXPECT_BURST 填两条规则里更严的那条，默认是按用户的 10 / 20。
// 场景二 unauthenticated（只在传了 TOKENS 时运行）：带无效 token，以 OVER_FACTOR 倍于 IP_RPS 的速率请求 30 秒。
//   按 IP 限流在认证之前，所以 401 的数量 ≈ IP_BURST + IP_RPS × 30，其余在验签之前就被 429 挡住。
//   不传 TOKENS 时服务应是 AUTH_DISABLED，无效 token 也会通过认证，这个场景没有意义，自动跳过。
//
// 两个场景之间隔 15 秒，让额度恢复。TOLERANCE 是允许的偏差，默认 15%。
import http from 'k6/http';
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import * as api from './lib/api.js';

const EXPECT_RPS = api.envNum('EXPECT_RPS', 10);
const EXPECT_BURST = api.envNum('EXPECT_BURST', 20);
const IP_RPS = api.envNum('IP_RPS', 20);
const IP_BURST = api.envNum('IP_BURST', 40);
const OVER = api.envNum('OVER_FACTOR', 3);
const TOL = api.envNum('TOLERANCE', 0.15);
const A_SECONDS = 60;
const B_SECONDS = 30;

const authedAllowed = new Counter('authed_allowed');
const authedLimited = new Counter('authed_limited');
const unauthPassed = new Counter('unauth_passed_ip_limit');
const unauthLimited = new Counter('unauth_limited');

const expectedA = EXPECT_BURST + EXPECT_RPS * A_SECONDS;
const expectedB = IP_BURST + IP_RPS * B_SECONDS;
const band = (n) => [`count>=${Math.floor(n * (1 - TOL))}`, `count<=${Math.ceil(n * (1 + TOL))}`];

// 这几个状态码是预期内的，不算失败；出现其它状态码（例如 5xx）才算
http.setResponseCallback(http.expectedStatuses(200, 401, 429));

const scenarios = {
  per_client: {
    executor: 'constant-arrival-rate',
    exec: 'perClient',
    rate: Math.ceil(EXPECT_RPS * OVER),
    timeUnit: '1s',
    duration: `${A_SECONDS}s`,
    preAllocatedVUs: 20,
    maxVUs: 100,
  },
};
const thresholds = {
  authed_allowed: band(expectedA),
  authed_limited: ['count>0'],
  http_req_failed: ['rate==0'],
  checks: ['rate==1'],
};

if (api.HAS_TOKENS) {
  scenarios.unauthenticated = {
    executor: 'constant-arrival-rate',
    exec: 'unauthenticated',
    rate: Math.ceil(IP_RPS * OVER),
    timeUnit: '1s',
    duration: `${B_SECONDS}s`,
    startTime: `${A_SECONDS + 15}s`,
    preAllocatedVUs: 20,
    maxVUs: 100,
  };
  thresholds.unauth_passed_ip_limit = band(expectedB);
  thresholds.unauth_limited = ['count>0'];
}

export const options = { scenarios, thresholds };

export function setup() {
  api.preflight(false);
  console.log(`预期：同一用户放行约 ${expectedA} 次` + (api.HAS_TOKENS ? `；无效 token 通过 IP 限流约 ${expectedB} 次` : '（没有 TOKENS，跳过无效 token 场景）'));
}

function check429(res) {
  check(res, {
    '429 带 Retry-After（正整数秒）': (r) => /^[1-9][0-9]*$/.test(api.header(r, 'Retry-After') || ''),
    '429 的 RateLimit-Remaining 为 0': (r) => api.header(r, 'RateLimit-Remaining') === '0',
    '429 带 RateLimit-Limit': (r) => api.header(r, 'RateLimit-Limit') !== undefined,
    '429 响应体有 error 和 request_id': (r) => {
      const b = api.json(r);
      return !!(b && b.error && b.request_id);
    },
  });
}

export function perClient() {
  const res = api.listUsers(api.tokenFor(1), 1);
  if (res.status === 200) {
    authedAllowed.add(1);
    check(res, { '200 带 RateLimit-Remaining': (r) => api.header(r, 'RateLimit-Remaining') !== undefined });
  } else if (res.status === 429) {
    authedLimited.add(1);
    check429(res);
  } else {
    check(res, { [`同一用户只应返回 200 或 429（实际 ${res.status}）`]: () => false });
  }
}

export function unauthenticated() {
  const res = http.get(`${api.BASE_URL}/api/v1/users`, {
    headers: { Authorization: 'Bearer invalid-token-for-rate-limit-test' },
    tags: { op: 'listUsers', name: 'listUsers' },
  });
  if (res.status === 401) {
    unauthPassed.add(1);
  } else if (res.status === 429) {
    unauthLimited.add(1);
    check429(res);
  } else {
    check(res, { [`无效 token 只应返回 401 或 429（实际 ${res.status}）`]: () => false });
  }
}
