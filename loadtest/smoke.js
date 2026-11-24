// 冒烟：1 个 VU 把每个接口走一遍，确认环境可用。正式压测前先跑它。
//   make loadtest-smoke
import { check } from 'k6';
import http from 'k6/http';
import * as api from './lib/api.js';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: {
    checks: ['rate==1'],
  },
};

export function setup() {
  return { roles: api.preflight(false) };
}

export default function (data) {
  const tok = api.tokenFor(1);

  const me = api.me(tok);
  check(me, {
    'getMe 返回 200': (r) => r.status === 200,
    'getMe 带 sub': (r) => !!(api.json(r) || {}).sub,
  });

  let id;
  if (data.roles.indexOf('goapi-admin') >= 0) {
    const created = api.createUser(tok);
    check(created, {
      'createUser 返回 201': (r) => r.status === 201,
      'createUser 带 Location': (r) => /^\/api\/v1\/users\/.+/.test(api.header(r, 'Location') || ''),
    });
    id = (api.json(created) || {}).id;
  }

  const list = api.listUsers(tok, 5);
  check(list, {
    'listUsers 返回 200': (r) => r.status === 200,
    'listUsers 的 items 是数组': (r) => Array.isArray((api.json(r) || {}).items),
  });
  if (!id) {
    const items = (api.json(list) || {}).items || [];
    if (items.length > 0) id = items[0].id;
  }

  if (id) {
    check(api.getUser(tok, id), { 'getUser 返回 200': (r) => r.status === 200 });
  }
  check(api.getUser(tok, '00000000-0000-0000-0000-000000000000'), {
    '不存在的用户返回 404': (r) => r.status === 404,
  });

  // 认证开着时，不带 token 必须是 401
  if (api.HAS_TOKENS) {
    const anon = http.get(`${api.BASE_URL}/api/v1/users`, { tags: { op: 'listUsers', name: 'listUsers' }, responseCallback: http.expectedStatuses(401) });
    check(anon, { '不带 token 返回 401': (r) => r.status === 401 });
  }
}
