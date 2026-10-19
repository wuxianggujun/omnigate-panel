// api.js — 面板 JSON API 客户端。
//
// - 统一拼 /panel/api 前缀、JSON 编解码与错误对象
// - GET 请求在 in-flight 期间去重（多个页面/轮询同时拉同一资源时只发一次）
// - 401 派发 og:unauthorized 事件，由 main.js 刷新会话态
// - 所有 fetch 均同源；不接受外部地址

const API_BASE = '/panel/api';
const inflight = new Map();

export class ApiError extends Error {
  constructor(message, { status = 0, code = '', body = null, method = 'GET', url = '' } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.body = body;
    this.method = method;
    this.url = url;
  }
}

const ERROR_HINTS = {
  unauthorized: '需要登录后才能执行此操作',
  forbidden: '当前角色无权执行此操作',
  admin_required: '需要管理员权限',
  tenant_required: '只读角色不能执行此操作',
  not_your_account: '只能操作自己名下的账号',
  invalid_credentials: '用户名或密码不正确',
  too_many_attempts: '尝试次数过多，请稍后再试',
  csrf_origin_mismatch: '请求来源校验失败，请刷新页面重试',
  panel_auth_not_configured: '面板尚未配置账号，请先在服务端初始化',
  registration_disabled: '管理员已关闭自助注册',
  omnigate_unauthorized: '内置网关密钥不匹配，请检查服务端配置',
  not_implemented: '服务端尚未实现该功能',
};

export function describeError(error) {
  if (!error) return '未知错误';
  if (error.name === 'AbortError') return '请求已取消';
  const code = String(error.code || '');
  if (code && ERROR_HINTS[code]) return ERROR_HINTS[code];
  if (error.message) return error.message;
  return '请求失败';
}

function buildURL(path, query) {
  let url = path.startsWith('http') ? path : `${API_BASE}${path.startsWith('/') ? path : `/${path}`}`;
  if (query && typeof query === 'object') {
    const params = new URLSearchParams();
    for (const [key, value] of Object.entries(query)) {
      if (value == null || value === '' || value === false) continue;
      if (Array.isArray(value)) {
        for (const item of value) params.append(key, String(item));
      } else {
        params.append(key, String(value));
      }
    }
    const qs = params.toString();
    if (qs) url += `${url.includes('?') ? '&' : '?'}${qs}`;
  }
  return url;
}

function extractError(payload, fallback) {
  if (!payload) return fallback;
  if (typeof payload === 'string') return payload || fallback;
  if (typeof payload.error === 'string' && payload.error) return payload.error;
  if (typeof payload.message === 'string' && payload.message) return payload.message;
  if (typeof payload.detail === 'string' && payload.detail) return payload.detail;
  if (Array.isArray(payload.errors) && payload.errors.length) {
    return payload.errors.map((e) => (typeof e === 'string' ? e : e?.message || e?.msg)).filter(Boolean).join('；') || fallback;
  }
  return fallback;
}

async function parseResponse(res) {
  const text = await res.text();
  if (!text) return null;
  const contentType = res.headers.get('content-type') || '';
  if (contentType.includes('json') || /^[\[{]/.test(text.trim())) {
    try { return JSON.parse(text); } catch { return text; }
  }
  return text;
}

/**
 * request('/omni/config', { method:'POST', body: config })
 */
export async function request(path, {
  method = 'GET',
  body = undefined,
  query = undefined,
  signal = undefined,
  headers = undefined,
  dedupe = undefined,
} = {}) {
  const upper = String(method).toUpperCase();
  const url = buildURL(path, query);
  const canDedupe = dedupe ?? (upper === 'GET');
  const key = `${upper} ${url}`;

  if (canDedupe && inflight.has(key)) return withCallerAbort(inflight.get(key), signal);

  const init = {
    method: upper,
    credentials: 'same-origin',
    cache: 'no-store',
    headers: {
      Accept: 'application/json',
      ...(headers || {}),
    },
    // 去重请求共用同一次网络请求：某个调用方 abort（如页面卸载）只影响它自己，
    // 不能让其它调用方一起失败。共享 fetch 本身不挂调用方 signal。
    signal: canDedupe ? undefined : signal,
  };
  if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = typeof body === 'string' ? body : JSON.stringify(body);
  }

  const promise = (async () => {
    let res;
    try {
      res = await fetch(url, init);
    } catch (error) {
      if (error?.name === 'AbortError') throw error;
      throw new ApiError(`无法连接面板服务：${error?.message || error}`, {
        method: upper, url,
      });
    }
    const payload = await parseResponse(res);
    if (!res.ok) {
      const message = extractError(payload, `请求失败（HTTP ${res.status}）`);
      const code = payload && typeof payload === 'object' ? String(payload.code || payload.error || '') : '';
      if (res.status === 401 && !(upper === 'POST' && path === '/session')) {
        window.dispatchEvent(new CustomEvent('og:unauthorized'));
      }
      throw new ApiError(message, {
        status: res.status,
        code,
        body: payload,
        method: upper,
        url,
      });
    }
    return payload;
  })();

  if (canDedupe) {
    inflight.set(key, promise);
    promise.finally(() => {
      if (inflight.get(key) === promise) inflight.delete(key);
    }).catch(() => { /* finally 链上的 reject 由调用方处理 */ });
    return withCallerAbort(promise, signal);
  }
  return promise;
}

function withCallerAbort(promise, signal) {
  if (!signal) return promise;
  if (signal.aborted) return Promise.reject(new DOMException('Aborted', 'AbortError'));
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      cleanup();
      reject(new DOMException('Aborted', 'AbortError'));
    };
    const cleanup = () => signal.removeEventListener('abort', onAbort);
    signal.addEventListener('abort', onAbort, { once: true });
    promise.then(
      (value) => { cleanup(); resolve(value); },
      (error) => { cleanup(); reject(error); },
    );
  });
}

export const api = {
  session: {
    get: (opts = {}) => request('/session', { ...opts }),
    login: (username, password) => request('/session', { method: 'POST', body: { username, password }, dedupe: false }),
    logout: (opts = {}) => request('/session', { method: 'DELETE', dedupe: false, ...opts }),
    register: (username, password, opts = {}) => request('/register', { method: 'POST', body: { username, password }, dedupe: false, ...opts }),
    changePassword: (currentPassword, newPassword, opts = {}) => request('/password', {
      method: 'POST',
      body: { current_password: currentPassword, new_password: newPassword },
      dedupe: false,
      ...opts,
    }),
  },

  overview: {
    get: (opts = {}) => request('/overview', { ...opts }),
  },

  config: {
    get: (opts = {}) => request('/omni/config', { ...opts }),
    save: (config, opts = {}) => request('/omni/config', { method: 'POST', body: config, ...opts }),
  },

  accounts: {
    list: ({ balance = false, ...opts } = {}) => request('/omni/accounts', { query: balance ? { balance: 1 } : undefined, ...opts }),
  },

  account: {
    add: (payload, opts = {}) => request('/omni/account/add', { method: 'POST', body: payload, ...opts }),
    remove: (provider, label, opts = {}) => request('/omni/account/remove', { method: 'POST', body: { provider, label }, ...opts }),
    checkin: (provider, label = '', opts = {}) => request('/omni/account/checkin', { method: 'POST', body: { provider, label }, ...opts }),
    refresh: (provider, label = '', opts = {}) => request('/omni/account/refresh', { method: 'POST', body: { provider, label }, ...opts }),
    login: (payload, opts = {}) => request('/omni/account/login', { method: 'POST', body: payload, ...opts }),
  },

  // 登录向导：raccoon / TRAE / geminiweb 走面板代理到进程内网关的管理端点。
  loginFlow: {
    authorize: (kind, query = {}, opts = {}) => request(`/omni/admin/${kind}/authorize`, { query, ...opts }),
    callback: (kind, payload, opts = {}) => request(`/omni/admin/${kind}/callback`, { method: 'POST', body: payload, dedupe: false, ...opts }),
    token: (kind, payload, opts = {}) => request(`/omni/admin/${kind}/token`, { method: 'POST', body: payload, dedupe: false, ...opts }),
  },

  // New API 浏览器会话捕获：发起 → 脚本回传 → 轮询。
  newapi: {
    authorize: (payload, opts = {}) => request('/omni/newapi/authorize', { method: 'POST', body: payload, dedupe: false, ...opts }),
    status: (state, opts = {}) => request('/omni/newapi/status', { query: { state }, ...opts }),
    setKey: (payload, opts = {}) => request('/omni/newapi/setkey', { method: 'POST', body: payload, dedupe: false, ...opts }),
  },

  providerSecret: {
    get: (provider, opts = {}) => request('/omni/provider-secret', { query: { provider }, ...opts }),
  },
  providerQuota: {
    get: (provider, opts = {}) => request('/omni/provider-quota', { query: { provider }, ...opts }),
  },
  keyHealth: {
    get: (opts = {}) => request('/omni/key-health', { ...opts }),
  },
  providerTypes: {
    get: (opts = {}) => request('/omni/provider-types', { ...opts }),
  },

  models: {
    get: (opts = {}) => request('/omni/models', { ...opts }),
  },

  mapping: {
    get: (opts = {}) => request('/omni/model-mapping', { ...opts }),
    set: (payload, opts = {}) => request('/omni/model-mapping', { method: 'POST', body: payload, dedupe: false, ...opts }),
    remove: (alias, opts = {}) => request(`/omni/model-mapping/${encodeURIComponent(alias)}`, { method: 'DELETE', dedupe: false, ...opts }),
  },

  outbound: {
    get: (opts = {}) => request('/omni/outbound', { ...opts }),
    save: (outbound, opts = {}) => request('/omni/outbound', { method: 'POST', body: outbound, ...opts }),
  },

  usage: {
    get: ({ hours = 24, from, to, ...opts } = {}) => request('/usage', {
      query: { hours, from, to },
      ...opts,
    }),
  },

  requestLogs: {
    get: (query = {}, opts = {}) => request('/request_logs', { query, ...opts }),
  },

  logs: {
    get: (query = {}, opts = {}) => request('/logs', { query, ...opts }),
  },

  admin: {
    users: {
      list: (opts = {}) => request('/admin/users', { ...opts }),
      save: (payload, opts = {}) => request('/admin/users', { method: 'POST', body: payload, dedupe: false, ...opts }),
      resetPassword: (username, password, opts = {}) => request('/admin/users/password', {
        method: 'POST', body: { username, password }, dedupe: false, ...opts,
      }),
      remove: (username, opts = {}) => request('/admin/users/delete', {
        method: 'POST', body: { username }, dedupe: false, ...opts,
      }),
      setRegistration: (allow, opts = {}) => request('/admin/registration', {
        method: 'POST', body: { allow }, dedupe: false, ...opts,
      }),
    },
  },
};

export function isUnauthorized(error) {
  return error instanceof ApiError && error.status === 401;
}

export function isForbidden(error) {
  return error instanceof ApiError && error.status === 403;
}

export function isMissing(error) {
  return error instanceof ApiError && (error.status === 404 || error.status === 501);
}
