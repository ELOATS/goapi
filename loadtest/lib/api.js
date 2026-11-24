// 压测脚本共用的配置和接口封装。
//
// 环境变量：
//   BASE_URL  服务地址，默认 http://localhost:8080（不要带末尾斜杠）
//   TOKENS    逗号分隔的 access token，按 VU 轮流使用；只有一个时也可以用 TOKEN
//             不传时假定服务以 AUTH_DISABLED=true 启动（本地 docker compose 就是这样）
import http from 'k6/http';
import { fail } from 'k6';

export function envNum(name, def) {
  const v = __ENV[name];
  if (v === undefined || v === '') return def;
  const n = Number(v);
  if (Number.isNaN(n)) throw new Error(`${name} 必须是数字，实际是 "${v}"`);
  return n;
}

export const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8080').replace(/\/+$/, '');

const TOKENS = (__ENV.TOKENS || __ENV.TOKEN || '')
  .split(',')
  .map((s) => s.trim())
  .filter((s) => s.length > 0);

export const HAS_TOKENS = TOKENS.length > 0;

// tokenFor 按 VU 编号选 token。按用户限流以 token 的 sub 计数，
// 想模拟多个用户就传多个不同用户的 token。
export function tokenFor(vu) {
  if (!HAS_TOKENS) return '';
  return TOKENS[Math.max(vu - 1, 0) % TOKENS.length];
}

// op 用 OpenAPI 的 operationId，同时设为 name 标签：带 ID 的 URL 会归到同一组，
// 不会每个用户 ID 产生一个指标序列。
function params(op, token) {
  const headers = { Accept: 'application/json' };
  if (token) headers.Authorization = `Bearer ${token}`;
  return { headers, tags: { op: op, name: op } };
}

export function me(token) {
  return http.get(`${BASE_URL}/api/v1/me`, params('getMe', token));
}

export function listUsers(token, limit, cursor) {
  let url = `${BASE_URL}/api/v1/users?limit=${limit}`;
  if (cursor) url += `&cursor=${encodeURIComponent(cursor)}`;
  return http.get(url, params('listUsers', token));
}

export function getUser(token, id) {
  return http.get(`${BASE_URL}/api/v1/users/${id}`, params('getUser', token));
}

// createUser 生成不会重复的邮箱，统一用 @loadtest.example.com，方便压测后清理。
export function createUser(token) {
  const p = params('createUser', token);
  p.headers['Content-Type'] = 'application/json';
  const suffix = `${__VU}-${Date.now()}-${Math.floor(Math.random() * 1e9)}`;
  const body = JSON.stringify({ name: `loadtest ${suffix}`, email: `lt-${suffix}@loadtest.example.com` });
  return http.post(`${BASE_URL}/api/v1/users`, body, p);
}

export function json(res) {
  try {
    return res.json();
  } catch (e) {
    return null;
  }
}

// header 不区分大小写地取响应头（k6 会把头名规范化，例如 RateLimit-Limit 变成 Ratelimit-Limit）。
export function header(res, name) {
  const want = name.toLowerCase();
  for (const k in res.headers) {
    if (k.toLowerCase() === want) return res.headers[k];
  }
  return undefined;
}

// preflight 在 setup 里确认服务可达、token 有效、角色够用，避免压测跑了几分钟才发现全是 401 / 403。
// 只检查第一个 token。
export function preflight(needWrite) {
  const res = me(tokenFor(1));
  if (res.status === 401) {
    fail('GET /api/v1/me 返回 401：请用 TOKENS（或 TOKEN）传入有效的 access token；本地也可以用 AUTH_DISABLED=true 启动 API');
  }
  if (res.status !== 200) {
    fail(`GET /api/v1/me 返回 ${res.status}，请确认 BASE_URL=${BASE_URL} 能访问`);
  }
  const roles = (json(res) || {}).roles || [];
  const isAdmin = roles.indexOf('goapi-admin') >= 0;
  if (!isAdmin && roles.indexOf('goapi-viewer') < 0) {
    fail(`token 没有 goapi-viewer 或 goapi-admin 角色（当前：${roles.join(', ') || '无'}）`);
  }
  if (needWrite && !isAdmin) {
    fail(`压测会创建用户，需要 goapi-admin 角色（当前：${roles.join(', ')}）；只读压测请设 WRITE_RATIO=0`);
  }
  return roles;
}

// seedUserIDs 准备一批用户 ID 供 getUser 使用：能写就新建，只读时从列表里取。
export function seedUserIDs(n, canWrite) {
  const tok = tokenFor(1);
  const ids = [];
  if (canWrite) {
    for (let i = 0; i < n; i++) {
      const res = createUser(tok);
      if (res.status !== 201) fail(`准备数据时创建用户失败：${res.status} ${res.body}`);
      ids.push(json(res).id);
    }
    return ids;
  }
  const res = listUsers(tok, 100);
  if (res.status !== 200) fail(`准备数据时读取用户列表失败：${res.status}`);
  for (const u of (json(res) || {}).items || []) ids.push(u.id);
  if (ids.length === 0) fail('WRITE_RATIO=0 时需要库里已经有用户，先用有写权限的 token 跑一次，或手动创建几个');
  return ids;
}
