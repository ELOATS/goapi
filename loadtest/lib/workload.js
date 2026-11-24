// 混合负载：默认 70% 列表（其中 20% 再翻一页）、25% 按 ID 查询、5% 创建。
import { check } from 'k6';
import { Counter } from 'k6/metrics';
import * as api from './api.js';

// 压测期间被限流的请求数。load / stress 要求它为 0：出现 429 说明测的是限流，不是服务能力。
export const rateLimited = new Counter('rate_limited');

function expectStatus(res, want, what) {
  if (res.status === 429) rateLimited.add(1);
  return check(res, { [`${what} 返回 ${want}`]: (r) => r.status === want });
}

export function mixedIteration(data, writeRatio) {
  const tok = api.tokenFor(__VU);
  const r = Math.random();

  if (r < writeRatio) {
    expectStatus(api.createUser(tok), 201, 'createUser');
    return;
  }
  if (r < writeRatio + 0.25) {
    const id = data.ids[Math.floor(Math.random() * data.ids.length)];
    expectStatus(api.getUser(tok, id), 200, 'getUser');
    return;
  }
  const res = api.listUsers(tok, 20);
  if (expectStatus(res, 200, 'listUsers') && Math.random() < 0.2) {
    const body = api.json(res);
    if (body && body.next_cursor) {
      expectStatus(api.listUsers(tok, 20, body.next_cursor), 200, 'listUsers 下一页');
    }
  }
}
