// mock/api.js — 本地预览用的 fetch 拦截层。
//
// 只拦截 /panel/api/*；静态资源仍走真实 fetch。mock 数据形状与真实后端一致，
// 并且模拟角色（admin/viewer/user/匿名）、归属过滤、掩码、401/403、冷启动延迟
// 与 in-flight 取消。启用方式见页脚：?mock=1（可选 &mock_role=admin）。

import {
  MOCK_PROVIDER_TYPES,
  MOCK_USERS,
  buildAccountsPayload,
  buildModelCatalog,
  buildKeyUsage,
  buildOverview,
  buildRequestLogs,
  buildUsageSnapshot,
  createMockState,
  findAccount,
  isMask,
  maskSecret,
  visibleLogsFor,
  visibleRequestsFor,
} from './fixtures.js';

const SESSION_KEY = 'og-mock-user';
const SECRET_KEYS = new Set([
  'api_key', 'api_keys', 'zen_keys', 'go_keys', 'cookie', 'password',
  'refresh_token', 'session_cookie', 'access_token', 'secret',
]);
const ACCOUNT_FIELDS = ['cookie', 'email', 'password', 'refresh_token', 'uid', 'machine_id', 'device_id', 'api_host', 'api_key', 'session_cookie'];

let state = null;
let installed = false;
let originalFetch = null;

function nowISO() {
  return new Date().toISOString();
}

function clone(value) {
  return value == null ? value : JSON.parse(JSON.stringify(value));
}

function json(payload, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      'Cache-Control': 'no-store',
    },
  });
}

function ok(payload = {}) {
  return json({ ok: true, ...payload });
}

function fail(status, message, code = '') {
  return json({ ok: false, error: message, code: code || message }, status);
}

function storedUsername() {
  try { return localStorage.getItem(SESSION_KEY) || ''; } catch { return ''; }
}

function setStoredUsername(username) {
  try {
    if (username) localStorage.setItem(SESSION_KEY, username);
    else localStorage.removeItem(SESSION_KEY);
  } catch { /* 隐私模式忽略 */ }
}

function currentUser() {
  const name = storedUsername();
  if (!name) return null;
  return state.users.find((u) => u.username === name) || null;
}

function sessionPayload(user) {
  const base = {
    ok: true,
    authenticated: !!user,
    configured: true,
    allow_registration: state.allowRegistration,
    build: 'mock-2026-10-19',
    mode: user ? 'session' : '',
  };
  if (user) {
    base.username = user.username;
    base.role = user.role;
  }
  return base;
}

function requireSession() {
  const user = currentUser();
  if (!user) return { error: fail(401, 'unauthorized', 'unauthorized') };
  return { user };
}

function requirePrivileged() {
  const auth = requireSession();
  if (auth.error) return auth;
  if (auth.user.role !== 'admin' && auth.user.role !== 'viewer') {
    return { error: fail(403, 'forbidden', 'forbidden') };
  }
  return auth;
}

function requireAdmin() {
  const auth = requireSession();
  if (auth.error) return auth;
  if (auth.user.role !== 'admin') return { error: fail(403, '需要管理员权限', 'admin_required') };
  return auth;
}

function requireTenant() {
  const auth = requireSession();
  if (auth.error) return auth;
  if (auth.user.role !== 'admin' && auth.user.role !== 'user') {
    return { error: fail(403, '只读角色不能操作账号', 'tenant_required') };
  }
  return auth;
}

function ownAccount(user, provider, label) {
  if (!user) return false;
  if (user.role === 'admin') return true;
  if (user.role !== 'user') return false;
  return state.ownership[`${provider}/${label}`] === user.username;
}

function pushLog(text, channel = 'sys') {
  state.logs.push({ time: nowISO(), channel, text });
  if (state.logs.length > 600) state.logs.splice(0, state.logs.length - 600);
}

function flatAccounts(role, username) {
  const payload = buildAccountsPayload(state, { role, username, withBalance: true });
  const out = [];
  for (const provider of payload.providers) {
    for (const account of provider.accounts) {
      out.push({ ...account, provider: provider.name, type: provider.type });
    }
  }
  return out;
}

// ---------------------------------------------------------------------------
// 脱敏 / 掩码还原
// ---------------------------------------------------------------------------

function maskSecretValue(value) {
  if (typeof value === 'string') return maskSecret(value);
  if (Array.isArray(value)) {
    return value.map((item) => {
      if (typeof item === 'string') return maskSecret(item);
      if (item && typeof item === 'object' && typeof item.key === 'string') {
        return { ...item, key: maskSecret(item.key) };
      }
      return item;
    });
  }
  if (value && typeof value === 'object' && typeof value.key === 'string') {
    return { ...value, key: maskSecret(value.key) };
  }
  return value;
}

function maskDeep(value) {
  if (Array.isArray(value)) return value.map(maskDeep);
  if (!value || typeof value !== 'object') return value;
  const out = {};
  for (const [key, val] of Object.entries(value)) {
    out[key] = SECRET_KEYS.has(key) ? maskSecretValue(val) : maskDeep(val);
  }
  return out;
}

function collectSecretMasks(value, map) {
  if (Array.isArray(value)) {
    for (const item of value) collectSecretMasks(item, map);
    return;
  }
  if (!value || typeof value !== 'object') return;
  for (const [key, val] of Object.entries(value)) {
    if (SECRET_KEYS.has(key)) {
      addSecretMask(val, map);
    } else {
      collectSecretMasks(val, map);
    }
  }
}

function addSecretMask(value, map) {
  if (typeof value === 'string' && value) {
    map.set(maskSecret(value), value);
    return;
  }
  if (Array.isArray(value)) {
    for (const item of value) {
      if (typeof item === 'string' && item) map.set(maskSecret(item), item);
      else if (item && typeof item === 'object' && typeof item.key === 'string' && item.key) {
        map.set(maskSecret(item.key), item.key);
      }
    }
    return;
  }
  if (value && typeof value === 'object' && typeof value.key === 'string' && value.key) {
    map.set(maskSecret(value.key), value.key);
  }
}

function restoreSecretValue(value, current, maskMap) {
  if (typeof value === 'string') {
    if (isMask(value)) return maskMap.get(value) ?? (typeof current === 'string' && !isMask(current) ? current : value);
    return value;
  }
  if (Array.isArray(value)) {
    const curArr = Array.isArray(current) ? current : [];
    return value.map((item, index) => {
      if (typeof item === 'string') {
        if (!isMask(item)) return item;
        return maskMap.get(item) ?? (typeof curArr[index] === 'string' ? curArr[index] : item);
      }
      if (item && typeof item === 'object' && typeof item.key === 'string' && isMask(item.key)) {
        const cur = curArr.find((x) => x && typeof x === 'object' && x.providers === item.providers) || curArr[index];
        return { ...item, key: maskMap.get(item.key) ?? (cur && typeof cur.key === 'string' ? cur.key : item.key) };
      }
      return item;
    });
  }
  return value;
}

function restoreDeep(incoming, current, maskMap) {
  if (Array.isArray(incoming)) {
    const currentArr = Array.isArray(current) ? current : [];
    return incoming.map((item, index) => restoreDeep(item, currentArr[index], maskMap));
  }
  if (!incoming || typeof incoming !== 'object') return incoming;
  const out = {};
  for (const [key, val] of Object.entries(incoming)) {
    if (SECRET_KEYS.has(key)) {
      out[key] = restoreSecretValue(val, current && typeof current === 'object' ? current[key] : undefined, maskMap);
    } else {
      out[key] = restoreDeep(val, current && typeof current === 'object' ? current[key] : undefined, maskMap);
    }
  }
  return out;
}

function maskConfigForRole(config, role) {
  const masked = maskDeep(clone(config));
  if (role !== 'admin') {
    delete masked.api_keys;
  }
  return masked;
}

// ---------------------------------------------------------------------------
// 出站代理脱敏
// ---------------------------------------------------------------------------

function maskUserinfo(url) {
  const s = String(url || '');
  return s.replace(/\/\/([^/@]+)@/, '//***@');
}

function maskOutbound(out) {
  const copy = clone(out) || { proxies: [], routes: {} };
  copy.proxies = (copy.proxies || []).map((proxy) => ({
    ...proxy,
    ...(proxy.username ? { username: maskSecret(proxy.username) } : {}),
    ...(proxy.password ? { password: maskSecret(proxy.password) } : {}),
    ...(proxy.url ? { url: maskUserinfo(proxy.url) } : {}),
  }));
  return copy;
}

function restoreOutbound(incoming, current) {
  const currentByName = new Map((current?.proxies || []).map((p) => [p.name, p]));
  const out = clone(incoming) || {};
  out.proxies = (out.proxies || []).map((proxy) => {
    const prev = currentByName.get(proxy.name) || {};
    const next = { ...proxy };
    if (isMask(next.username) && prev.username) next.username = prev.username;
    if (isMask(next.password) && prev.password) next.password = prev.password;
    if ((isMask(next.url) || String(next.url || '').includes('***@')) && prev.url) next.url = prev.url;
    return next;
  });
  out.routes = out.routes && typeof out.routes === 'object' ? out.routes : {};
  return out;
}

// ---------------------------------------------------------------------------
// 请求辅助
// ---------------------------------------------------------------------------

function sleep(ms, signal) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Aborted', 'AbortError'));
      return;
    }
    const timer = setTimeout(() => {
      signal?.removeEventListener('abort', onAbort);
      resolve();
    }, ms);
    function onAbort() {
      clearTimeout(timer);
      reject(new DOMException('Aborted', 'AbortError'));
    }
    signal?.addEventListener('abort', onAbort, { once: true });
  });
}

function delayFor(url, method) {
  const path = url.pathname;
  if (path.endsWith('/provider-types')) return 70 + Math.random() * 60;
  if (path.endsWith('/accounts') && url.searchParams.get('balance') === '1') return 380 + Math.random() * 220;
  if (path.endsWith('/models')) return 220 + Math.random() * 160;
  if (path.endsWith('/usage')) return 200 + Math.random() * 160;
  if (path.endsWith('/request_logs')) return 140 + Math.random() * 120;
  if (path.endsWith('/overview')) return 120 + Math.random() * 100;
  if (method !== 'GET') return 160 + Math.random() * 180;
  return 80 + Math.random() * 120;
}

async function readBody(init) {
  const raw = init?.body;
  if (raw == null || raw === '') return {};
  if (typeof raw === 'string') {
    try { return JSON.parse(raw); } catch { return {}; }
  }
  if (raw instanceof FormData) {
    const out = {};
    for (const [k, v] of raw.entries()) out[k] = v;
    return out;
  }
  return {};
}

function parseTimeParam(value) {
  if (!value) return null;
  if (/^\d+$/.test(String(value))) {
    const n = Number(value);
    return new Date(n > 1e12 ? n : n * 1000);
  }
  const d = new Date(String(value));
  return Number.isNaN(d.getTime()) ? null : d;
}

function matchContains(haystack, needle) {
  if (!needle) return true;
  return String(haystack ?? '').toLowerCase().includes(String(needle).toLowerCase());
}

// ---------------------------------------------------------------------------
// 路由处理
// ---------------------------------------------------------------------------

async function handleApi(url, init) {
  const method = String(init?.method || 'GET').toUpperCase();
  const path = url.pathname.replace(/^\/panel\/api/, '') || '/';
  const body = await readBody(init);
  const user = currentUser();
  const role = user?.role || '';

  // ---- 会话 ----
  if (path === '/session' && method === 'GET') {
    return json(sessionPayload(user));
  }
  if (path === '/session' && method === 'POST') {
    const found = state.users.find((u) => u.username === String(body.username || '').trim());
    if (!found || found.password !== String(body.password || '')) {
      return fail(401, 'invalid_credentials', 'invalid_credentials');
    }
    setStoredUsername(found.username);
    return ok({ username: found.username, role: found.role });
  }
  if (path === '/session' && method === 'DELETE') {
    setStoredUsername('');
    return ok();
  }
  if (path === '/register' && method === 'POST') {
    if (!state.allowRegistration) return fail(403, 'registration_disabled', 'registration_disabled');
    const username = String(body.username || '').trim();
    const password = String(body.password || '');
    if (username.length < 2) return fail(400, '用户名至少 2 个字符');
    if (password.length < 6) return fail(400, '密码至少 6 个字符');
    if (state.users.some((u) => u.username === username)) return fail(409, '用户名已存在');
    const newUser = { username, password, role: 'user', display: username };
    state.users.push(newUser);
    setStoredUsername(username);
    pushLog(`[panel] 新用户 ${username} 注册成功`, 'sys');
    return ok({ username, role: 'user' });
  }
  if (path === '/password' && method === 'POST') {
    const auth = requireSession();
    if (auth.error) return auth.error;
    if (String(body.current_password || '') !== auth.user.password) {
      return fail(401, '当前密码不正确', 'invalid_credentials');
    }
    if (String(body.new_password || '').length < 6) return fail(400, '新密码至少 6 个字符');
    auth.user.password = String(body.new_password);
    pushLog(`[panel] ${auth.user.username} 修改了自己的密码`, 'sys');
    return ok();
  }

  // ---- 管理员：面板账号 ----
  if (path === '/admin/users' && method === 'GET') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    return json({
      ok: true,
      users: state.users.map((u) => ({ username: u.username, role: u.role })),
      session_hours: state.sessionHours,
      max_failures: state.maxFailures,
      lock_minutes: state.lockMinutes,
      allow_registration: state.allowRegistration,
    });
  }
  if (path === '/admin/users' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const username = String(body.username || '').trim();
    const nextRole = String(body.role || '');
    const password = String(body.password || '');
    if (!username) return fail(400, '用户名不能为空');
    if (!['admin', 'viewer', 'user'].includes(nextRole)) return fail(400, '角色不合法');
    const existing = state.users.find((u) => u.username === username);
    if (username === auth.user.username && nextRole !== 'admin') {
      return fail(400, '不能修改当前登录账号自己的角色');
    }
    if (!existing && !password) return fail(400, '新增账号必须设置密码');
    if (existing) {
      existing.role = nextRole;
      if (password) existing.password = password;
      existing.display = existing.display || username;
    } else {
      state.users.push({ username, password, role: nextRole, display: username });
    }
    pushLog(`[panel] ${auth.user.username} 保存了面板账号 ${username}（${nextRole}）`, 'sys');
    return ok();
  }
  if (path === '/admin/users/password' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const target = state.users.find((u) => u.username === String(body.username || '').trim());
    if (!target) return fail(404, '账号不存在');
    const password = String(body.password || '');
    if (password.length < 6) return fail(400, '密码至少 6 个字符');
    target.password = password;
    pushLog(`[panel] ${auth.user.username} 重置了 ${target.username} 的密码`, 'sys');
    return ok();
  }
  if (path === '/admin/users/delete' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const username = String(body.username || '').trim();
    if (username === auth.user.username) return fail(400, '不能删除当前登录账号自己');
    const idx = state.users.findIndex((u) => u.username === username);
    if (idx < 0) return fail(404, '账号不存在');
    state.users.splice(idx, 1);
    for (const [key, owner] of Object.entries(state.ownership)) {
      if (owner === username) delete state.ownership[key];
    }
    pushLog(`[panel] ${auth.user.username} 删除了面板账号 ${username}`, 'sys');
    return ok();
  }
  if (path === '/admin/registration' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    state.allowRegistration = !!body.allow;
    pushLog(`[panel] 自助注册已${state.allowRegistration ? '开启' : '关闭'}`, 'sys');
    return ok({ allow_registration: state.allowRegistration });
  }

  // ---- 概览 ----
  if (path === '/overview' && method === 'GET') {
    return json(buildOverview(state, role, user?.username || ''));
  }

  // ---- 引擎配置 ----
  if (path === '/omni/config' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    return json({
      ok: true,
      path: 'data/omnigate/omnigate.json',
      config: maskConfigForRole(state.config, auth.user.role),
    });
  }
  if (path === '/omni/config' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const maskMap = new Map();
    collectSecretMasks(state.config, maskMap);
    const restored = restoreDeep(body, state.config, maskMap);
    if (!Array.isArray(restored.providers)) return fail(400, 'providers 必须是数组');
    restored.data_dir = restored.data_dir || state.config.data_dir;
    state.config = restored;
    pushLog(`[panel] ${auth.user.username} 保存了引擎配置（${restored.providers.length} 个供应商）`, 'sys');
    return ok({ restart_required: [] });
  }
  if (path === '/access/scope' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const name = String(body.name || '').trim();
    if (!name) return fail(400, 'name 不能为空');
    const providers = Array.isArray(body.providers)
      ? body.providers.map((p) => String(p || '').trim()).filter(Boolean)
      : [];
    const valid = new Set((state.config.providers || []).map((p) => p.name));
    for (const p of providers) {
      if (!valid.has(p)) return fail(400, `未知供应商: ${p}`);
    }
    let updated = 0;
    for (const key of state.config.api_keys || []) {
      if (!key || typeof key !== 'object') continue;
      if (String(key.name || '').trim() !== name) continue;
      if (providers.length) key.providers = providers.slice();
      else delete key.providers;
      updated += 1;
    }
    if (!updated) return fail(404, `配置里没有名为 ${name} 的入站密钥`);
    pushLog(`[panel] ${auth.user.username} 更新用户 ${name} 的供应商范围：${providers.length ? providers.join(' / ') : '全量'}`, 'sys');
    return ok({ name, providers, updated, restart_required: [] });
  }

  // ---- 账号 ----
  if (path === '/omni/accounts' && method === 'GET') {
    const data = buildAccountsPayload(state, {
      role,
      username: user?.username || '',
      withBalance: url.searchParams.get('balance') === '1',
    });
    return json({ ok: true, data });
  }
  if (path === '/omni/account/add' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!providerName || !label) return fail(400, 'provider/label 不能为空');
    const provider = state.config.providers.find((p) => p.name === providerName);
    if (!provider) return fail(404, '供应商不存在');
    const meta = MOCK_PROVIDER_TYPES.find((t) => t.type === provider.type);
    if (meta && meta.account_mode === 'apikey') return fail(400, '该供应商使用上游密钥，不需要账号');
    if ((provider.accounts || []).some((a) => a.label === label)) return fail(409, '账号标签已存在');
    const key = `${providerName}/${label}`;
    const owner = state.ownership[key];
    if (owner && owner !== auth.user.username && auth.user.role !== 'admin') {
      return fail(409, '账号名已被其他用户占用，请换一个');
    }
    const account = { label };
    for (const field of ACCOUNT_FIELDS) {
      if (body[field] != null && body[field] !== '') account[field] = body[field];
    }
    if (auth.user.role === 'user') state.ownership[key] = auth.user.username;
    provider.accounts = provider.accounts || [];
    provider.accounts.push(account);
    state.runtime[key] = {
      available: 0,
      checkin_done: false,
      ...(String(account.cookie || '') ? {} : { auth_invalid: true, auth_error: '尚未配置凭证' }),
    };
    pushLog(`[panel] ${auth.user.username} 添加账号 ${key}`, 'task');
    return ok();
  }
  if (path === '/omni/account/remove' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!providerName || !label) return fail(400, 'provider/label 不能为空');
    if (!ownAccount(auth.user, providerName, label)) return fail(403, '只能移除自己名下的账号', 'not_your_account');
    const provider = state.config.providers.find((p) => p.name === providerName);
    if (!provider) return fail(404, '供应商不存在');
    const idx = (provider.accounts || []).findIndex((a) => a.label === label);
    if (idx < 0) return fail(404, '账号不存在');
    provider.accounts.splice(idx, 1);
    delete state.runtime[`${providerName}/${label}`];
    delete state.ownership[`${providerName}/${label}`];
    pushLog(`[panel] ${auth.user.username} 移除账号 ${providerName}/${label}`, 'task');
    return ok({ removed: true });
  }
  if (path === '/omni/account/checkin' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!providerName) return fail(400, 'provider 不能为空');
    if (!label && auth.user.role !== 'admin') return fail(403, '整供应商签到仅管理员可用', 'admin_required');
    if (label && !ownAccount(auth.user, providerName, label)) return fail(403, '只能签到自己的账号', 'not_your_account');
    const provider = state.config.providers.find((p) => p.name === providerName);
    if (!provider) return fail(404, '供应商不存在');
    const targets = (provider.accounts || []).filter((a) => !label || a.label === label);
    if (!targets.length) return fail(404, '账号不存在');
    const results = targets.map((account) => {
      const key = `${providerName}/${account.label}`;
      const rt = state.runtime[key] || (state.runtime[key] = {});
      if (!account.cookie) {
        return { label: account.label, success: false, error: '没有可用凭证，请先登录' };
      }
      rt.checkin_done = true;
      rt.last_checkin = nowISO();
      if (typeof rt.available === 'number') rt.available = Number((rt.available + 5).toFixed(1));
      rt.auth_invalid = false;
      delete rt.auth_error;
      delete rt.last_error;
      pushLog(`[checkin] ${providerName}/${account.label} 签到成功，获得 5 积分`, 'task');
      return { label: account.label, success: true, msg: '签到成功，+5 积分' };
    });
    return ok({ results });
  }
  if (path === '/omni/account/refresh' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!providerName) return fail(400, 'provider 不能为空');
    if (!label && auth.user.role !== 'admin') return fail(403, '整供应商刷新仅管理员可用', 'admin_required');
    if (label && !ownAccount(auth.user, providerName, label)) return fail(403, '只能刷新自己的账号', 'not_your_account');
    const provider = state.config.providers.find((p) => p.name === providerName);
    if (!provider) return fail(404, '供应商不存在');
    const targets = (provider.accounts || []).filter((a) => !label || a.label === label);
    if (!targets.length) return fail(404, '账号不存在');
    const results = targets.map((account) => {
      const key = `${providerName}/${account.label}`;
      const rt = state.runtime[key] || (state.runtime[key] = {});
      if (!account.cookie) return { label: account.label, success: false, error: '没有可用凭证' };
      rt.expires_at = new Date(Date.now() + 7 * 86400 * 1000).toISOString();
      rt.auth_invalid = false;
      delete rt.auth_error;
      delete rt.balance_error;
      delete rt.session_error;
      delete rt.session_expired_at;
      rt.session_verified_at = nowISO();
      pushLog(`[refresh] ${providerName}/${account.label} 会话已刷新`, 'task');
      return { label: account.label, success: true, msg: '会话已刷新' };
    });
    return ok({ results });
  }
  if (path === '/omni/account/login' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!providerName) return fail(400, 'provider 不能为空');
    if (!label) return fail(400, 'label 不能为空');
    if (!body.cookie && !body.email && !body.password) return fail(400, '请提供 Cookie 或账号密码');
    const provider = state.config.providers.find((p) => p.name === providerName);
    if (!provider) return fail(404, '供应商不存在');
    let account = (provider.accounts || []).find((a) => a.label === label);
    const key = `${providerName}/${label}`;
    if (!account) {
      const owner = state.ownership[key];
      if (owner && owner !== auth.user.username && auth.user.role !== 'admin') {
        return fail(409, '账号名已被其他用户占用，请换一个');
      }
      account = { label };
      provider.accounts = provider.accounts || [];
      provider.accounts.push(account);
      if (auth.user.role === 'user') state.ownership[key] = auth.user.username;
    }
    for (const field of ACCOUNT_FIELDS) {
      if (body[field] != null && body[field] !== '') account[field] = body[field];
    }
    const rt = state.runtime[key] || (state.runtime[key] = {});
    rt.auth_invalid = false;
    delete rt.auth_error;
    delete rt.session_error;
    rt.session_verified_at = nowISO();
    rt.last_checkin = nowISO();
    if (rt.available == null) rt.available = 0;
    pushLog(`[login] ${providerName}/${label} 登录校验通过`, 'task');
    return ok({ account: { label, name: account.name || label } });
  }

  // ---- 登录向导：面板代理到网关的管理端点 ----
  if (path.startsWith('/omni/admin/') && method === 'GET' && path.endsWith('/authorize')) {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const kind = path.slice('/omni/admin/'.length, -'/authorize'.length);
    const providerName = String(url.searchParams.get('provider') || '');
    const label = String(url.searchParams.get('label') || '').trim() || `账号 ${(state.config.providers.find((p) => p.name === providerName)?.accounts?.length || 0) + 1}`;
    const base = `${location.origin}/panel/api/omni/admin/${kind}/callback`;
    return ok({
      provider: providerName,
      label,
      state: `mock-state-${Date.now()}`,
      mode: url.searchParams.get('redirect_base') ? 'auto' : 'manual',
      authorize_url: `https://example.com/oauth/authorize?client_id=mock&state=mock-state`,
      redirect_url: url.searchParams.get('redirect_base') || '',
      callback_hint: kind === 'trae'
        ? 'http://redirect.local/authorize?refreshToken=..&userInfo=..'
        : 'raccoon://callback?code=..&state=..',
    });
  }
  if (path.startsWith('/omni/admin/') && method === 'POST' && path.endsWith('/callback')) {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim() || `账号 ${(state.config.providers.find((p) => p.name === providerName)?.accounts?.length || 0) + 1}`;
    if (!String(body.callback_url || body.callback || '').trim()) return fail(400, '回调地址不能为空');
    return ok({ account: { label, name: label } });
  }
  if (path.startsWith('/omni/admin/') && method === 'POST' && path.endsWith('/token')) {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    const label = String(body.label || '').trim();
    if (!label) return fail(400, '账号标签不能为空');
    if (!String(body.cookie || body.token || '').trim()) return fail(400, 'Cookie 不能为空');
    return ok({ account: { label, name: label, valid: true } });
  }

  // ---- New API 浏览器会话捕获 ----
  if (path === '/omni/newapi/authorize' && method === 'POST') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const providerName = String(body.provider || '').trim();
    if (!providerName) return fail(400, 'provider 不能为空');
    const stateID = `mock-capture-${Date.now()}`;
    return ok({
      state: stateID,
      label: String(body.label || ''),
      signin_url: 'https://example.com/signin',
      snippet: `(async () => {\n  // mock 抓取脚本（真实环境由服务端生成）\n  await fetch('${location.origin}/panel/api/omni/newapi/capture?state=${stateID}', { method: 'POST', body: '{}' });\n})();`,
    });
  }
  if (path === '/omni/newapi/status' && method === 'GET') {
    const auth = requireTenant();
    if (auth.error) return auth.error;
    const sid = String(url.searchParams.get('state') || '');
    if (!sid) return fail(400, '缺少 state');
    // 首次轮询仍等待，第二次起报告完成——让向导的轮询分支可被看到。
    state.__newapiPolls = state.__newapiPolls || {};
    state.__newapiPolls[sid] = (state.__newapiPolls[sid] || 0) + 1;
    return ok({ pending: state.__newapiPolls[sid] < 2 });
  }

  // ---- 供应商密钥 / 额度 / 健康 ----
  if (path === '/omni/provider-secret' && method === 'GET') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const name = String(url.searchParams.get('provider') || '');
    const provider = state.config.providers.find((p) => p.name === name);
    if (!provider) return fail(404, '供应商不存在');
    const secrets = {};
    for (const field of ['api_key', 'api_keys', 'zen_keys', 'go_keys']) {
      if (provider[field] != null && (!Array.isArray(provider[field]) || provider[field].length)) {
        secrets[field] = provider[field];
      }
    }
    if (!Object.keys(secrets).length) return fail(404, '该供应商没有配置密钥字段');
    return ok({ name, secrets });
  }
  if (path === '/omni/provider-quota' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    const name = String(url.searchParams.get('provider') || '');
    const provider = state.config.providers.find((p) => p.name === name);
    if (!provider) return fail(404, '供应商不存在');
    if (provider.type !== 'opencode') return ok({ provider: name, keys: [] });
    return json(state.quota);
  }
  if (path === '/omni/key-health' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    return json(state.keyHealth);
  }

  // ---- 类型元数据 / 模型 ----
  if (path === '/omni/provider-types' && method === 'GET') {
    return json({ ok: true, types: MOCK_PROVIDER_TYPES });
  }
  if (path === '/omni/models' && method === 'GET') {
    const data = buildModelCatalog(state.config);
    const seen = new Set(data.map((m) => m.id));
    const whitelist = Array.isArray(state.config.model_whitelist) ? state.config.model_whitelist : [];
    for (const [alias, targets] of Object.entries(state.mappings)) {
      if (state.disabledMappings[alias] || seen.has(alias)) continue;
      if (whitelist.length && !whitelist.includes(alias)) continue;
      const first = Array.isArray(targets) ? targets[0] : targets;
      const primary = String(first || '').split('/')[0];
      const targetModel = data.find((m) => m.id === first);
      data.push({
        id: alias,
        object: 'model',
        owned_by: primary,
        context_window: targetModel?.context_window || 128000,
        max_tokens: targetModel?.max_tokens || 8192,
        capabilities: targetModel?.capabilities || ['chat', 'stream'],
        alias_of: Array.isArray(targets) ? targets : [targets],
      });
      seen.add(alias);
    }
    const filtered = whitelist.length ? data.filter((m) => whitelist.includes(m.id)) : data;
    return json({ ok: true, object: 'list', data: filtered });
  }

  // ---- 模型映射 ----
  if (path === '/omni/model-mapping' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    return json({ mappings: state.mappings, disabled: state.disabledMappings });
  }
  if (path === '/omni/model-mapping' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const alias = String(body.alias || '').trim();
    if (!alias) return fail(400, 'alias 不能为空');
    const hasEnabledOnly = body.enabled != null && body.target == null && (!Array.isArray(body.targets) || body.targets.length === 0);
    if (hasEnabledOnly) {
      if (!state.mappings[alias]) return fail(404, '映射不存在');
      if (body.enabled) delete state.disabledMappings[alias];
      else state.disabledMappings[alias] = true;
      return ok({ mappings: state.mappings, disabled: state.disabledMappings });
    }
    const rawTargets = Array.isArray(body.targets) ? body.targets : (body.target ? [body.target] : []);
    const targets = rawTargets.map((t) => String(t || '').trim()).filter(Boolean).filter((t) => t !== alias);
    if (!targets.length) return fail(400, 'alias 与 target 均不能为空');
    state.mappings[alias] = Array.from(new Set(targets));
    if (body.enabled === false) state.disabledMappings[alias] = true;
    else delete state.disabledMappings[alias];
    pushLog(`[modelmap] ${auth.user.username} 保存映射 ${alias} → ${state.mappings[alias].join(' → ')}`, 'sys');
    return ok({ mappings: state.mappings, disabled: state.disabledMappings });
  }
  if (path.startsWith('/omni/model-mapping/') && method === 'DELETE') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    const alias = decodeURIComponent(path.slice('/omni/model-mapping/'.length));
    if (!alias || !state.mappings[alias]) return fail(404, '映射不存在');
    delete state.mappings[alias];
    delete state.disabledMappings[alias];
    pushLog(`[modelmap] ${auth.user.username} 删除映射 ${alias}`, 'sys');
    return ok({ mappings: state.mappings, disabled: state.disabledMappings });
  }

  // ---- 出站代理 ----
  if (path === '/omni/outbound' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    return json({ ok: true, outbound: maskOutbound(state.outbound) });
  }
  if (path === '/omni/outbound' && method === 'POST') {
    const auth = requireAdmin();
    if (auth.error) return auth.error;
    state.outbound = restoreOutbound(body, state.outbound);
    pushLog(`[panel] ${auth.user.username} 保存了出站代理配置`, 'sys');
    return ok({ restart_required: [] });
  }

  // ---- 用量 / 请求 / 日志 ----
  if (path === '/usage' && method === 'GET') {
    const hours = Math.max(0, Math.min(1440, Number(url.searchParams.get('hours')) || 24));
    if (!user) return json(buildUsageSnapshot(state, { hours, accounts: [], providers: [] }));
    const accounts = flatAccounts(role, user.username);
    const providers = role === 'admin' || role === 'viewer'
      ? state.config.providers.map((p) => p.name)
      : Array.from(new Set(accounts.map((a) => a.provider)));
    return json(buildUsageSnapshot(state, { hours, accounts, providers }));
  }
  if (path === '/usage/by-key' && method === 'GET') {
    const auth = requirePrivileged();
    if (auth.error) return auth.error;
    return json(buildKeyUsage(state, visibleRequestsFor(state, auth.user.role, auth.user.username)));
  }
  if (path === '/request_logs' && method === 'GET') {
    let rows = visibleRequestsFor(state, role, user?.username || '');
    const filters = {
      outcome: url.searchParams.get('outcome'),
      account: url.searchParams.get('account'),
      model: url.searchParams.get('model'),
      provider: url.searchParams.get('provider'),
      client_ip: url.searchParams.get('client_ip'),
      user_agent: url.searchParams.get('user_agent'),
    };
    const from = parseTimeParam(url.searchParams.get('from'));
    const to = parseTimeParam(url.searchParams.get('to'));
    rows = rows.filter((row) => {
      for (const [key, value] of Object.entries(filters)) {
        if (!matchContains(row[key], value)) return false;
      }
      const t = new Date(row.time);
      if (from && t < from) return false;
      if (to && t > to) return false;
      return true;
    });
    rows = rows.slice().sort((a, b) => new Date(b.time) - new Date(a.time));
    const limit = Math.max(1, Math.min(1000, Number(url.searchParams.get('limit')) || 200));
    return json({ entries: rows.slice(0, limit), limit });
  }
  if (path === '/logs' && method === 'GET') {
    let entries = visibleLogsFor(state, role, user?.username || '');
    const channel = url.searchParams.get('channel');
    const q = url.searchParams.get('q');
    entries = entries.filter((entry) => {
      if (channel && entry.channel !== channel) return false;
      if (q && !matchContains(entry.text, q)) return false;
      return true;
    });
    return json({ entries });
  }

  return fail(404, `mock 未实现该接口：${method} ${path}`, 'not_found');
}

/** 安装 mock fetch 拦截。幂等。 */
export function installMock() {
  if (installed) return;
  installed = true;
  state = createMockState();
  state.requestLogs = buildRequestLogs(state);

  const params = new URLSearchParams(location.search);
  const roleParam = params.get('mock_role');
  if (roleParam && !storedUsername()) {
    if (roleParam === 'anon' || roleParam === 'anonymous') {
      setStoredUsername('');
    } else {
      const found = state.users.find((u) => u.username === roleParam)
        || state.users.find((u) => u.role === roleParam)
        || (roleParam === 'user' ? state.users.find((u) => u.username === 'alice') : null);
      if (found) setStoredUsername(found.username);
    }
  }

  originalFetch = window.fetch.bind(window);
  window.fetch = async (input, init = {}) => {
    let url;
    try {
      url = new URL(typeof input === 'string' ? input : input.url, location.origin);
    } catch {
      return originalFetch(input, init);
    }
    if (!url.pathname.startsWith('/panel/api/')) {
      return originalFetch(input, init);
    }
    const method = String(init.method || (typeof input !== 'string' ? input.method : 'GET') || 'GET').toUpperCase();
    const signal = init.signal || (typeof input !== 'string' ? input.signal : null);
    try {
      await sleep(delayFor(url, method), signal);
      return await handleApi(url, { ...init, method });
    } catch (error) {
      if (error?.name === 'AbortError') throw error;
      console.error('mock api error', error);
      return fail(500, `Mock 处理失败：${error?.message || error}`);
    }
  };
  window.__OG_MOCK__ = true;
  console.info('[mock] /panel/api/* 已由本地 fixtures 接管；角色可通过 ?mock_role=admin|viewer|alice|bob 指定');
}

export const MOCK_LOGIN_HINT = MOCK_USERS.map((u) => `${u.username}/${u.password}（${u.role}）`).join('，');
