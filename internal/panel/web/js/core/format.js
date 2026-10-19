// format.js — 展示格式化、领域标签与内置供应商元数据（后端 provider-types 404 时兜底）。

const intFmt = new Intl.NumberFormat('zh-CN');
const compactFmt = new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 });
const oneDecimal = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 1 });

export const PROVIDER_TYPE_LABELS = Object.freeze({
  raccoon: '浣熊',
  runable: 'Runable',
  openai: 'OpenAI 兼容',
  opencode: 'OpenCode',
  trae: 'TRAE',
  newapi: 'New API',
  geminiweb: 'Gemini Web',
  unknown: '未知类型',
});

export const ACCOUNT_MODE_LABELS = Object.freeze({
  apikey: '密钥型',
  oauth: 'OAuth 账号',
  cookie: 'Cookie 账号',
  login: '登录账号',
  none: '无账号',
  mixed: '混合',
});

export const CAPABILITY_LABELS = Object.freeze({
  chat: '对话',
  stream: '流式',
  tools: '工具',
  vision: '视觉',
  reasoning: '推理',
  image: '图像',
  embedding: '向量',
  audio: '音频',
  code: '代码',
});

export const ROLE_LABELS = Object.freeze({
  admin: '管理员',
  viewer: '只读',
  user: '用户',
  '': '访客',
});

export const OUTCOME_LABELS = Object.freeze({
  success: '成功',
  http_error: '上游错误',
  stream_error: '流错误',
  interrupted: '客户端中断',
  model_error: '模型错误',
  no_account: '无可用账号',
  unauthorized: '未授权',
  rate_limited: '限流',
  canceled: '已取消',
});

export function providerLabel(type) {
  return PROVIDER_TYPE_LABELS[String(type || '').toLowerCase()] || type || '未指定类型';
}

export function roleLabel(role) {
  return ROLE_LABELS[role] ?? (role || '访客');
}

export function outcomeLabel(outcome) {
  return OUTCOME_LABELS[outcome] || outcome || '未知';
}

export function fmtInt(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return intFmt.format(Math.round(n));
}

export function fmtNum(value, digits = 1) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return new Intl.NumberFormat('zh-CN', { maximumFractionDigits: digits }).format(n);
}

export function fmtCompact(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return compactFmt.format(n);
}

export function fmtCredits(value) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  if (Math.abs(n) >= 10000) return compactFmt.format(n);
  return oneDecimal.format(n);
}

export function fmtPercent(value, digits = 1) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return `${oneDecimal.format(n)}%`;
}

/** 后端有些比率是 0..1（reqlog success_rate），统一转成 0..100 显示。 */
export function fmtRatio(value, digits = 1) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  return `${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: digits }).format(n * 100)}%`;
}

export function fmtBytes(value) {
  const n = Number(value);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return `${Math.round(n)} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i += 1; }
  return `${oneDecimal.format(v)} ${units[i]}`;
}

export function fmtDuration(ms) {
  const n = Number(ms);
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1000) return `${Math.round(n)} ms`;
  if (n < 60000) return `${oneDecimal.format(n / 1000)} s`;
  const m = Math.floor(n / 60000);
  const s = Math.round((n % 60000) / 1000);
  return `${m} 分 ${s} 秒`;
}

export function fmtUptime(seconds) {
  const n = Number(seconds);
  if (!Number.isFinite(n) || n < 0) return '—';
  const d = Math.floor(n / 86400);
  const h = Math.floor((n % 86400) / 3600);
  const m = Math.floor((n % 3600) / 60);
  if (d > 0) return `${d} 天 ${h} 小时`;
  if (h > 0) return `${h} 小时 ${m} 分`;
  if (m > 0) return `${m} 分`;
  return `${Math.floor(n)} 秒`;
}

export function parseDate(value) {
  if (value == null || value === '') return null;
  if (value instanceof Date) return Number.isNaN(value.getTime()) ? null : value;
  if (typeof value === 'number') {
    const ms = value > 1e12 ? value : value * 1000;
    const d = new Date(ms);
    return Number.isNaN(d.getTime()) ? null : d;
  }
  const s = String(value);
  if (/^\d+$/.test(s)) {
    const n = Number(s);
    return parseDate(n);
  }
  const d = new Date(s);
  return Number.isNaN(d.getTime()) ? null : d;
}

export function fmtDateTime(value, { seconds = false } = {}) {
  const d = parseDate(value);
  if (!d) return '—';
  return new Intl.DateTimeFormat('zh-CN', {
    year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit',
    ...(seconds ? { second: '2-digit' } : {}),
    hour12: false,
  }).format(d);
}

export function fmtTime(value) {
  const d = parseDate(value);
  if (!d) return '—';
  return new Intl.DateTimeFormat('zh-CN', {
    hour: '2-digit', minute: '2-digit', second: '2-digit', hour12: false,
  }).format(d);
}

export function fmtChartTime(value, scope = 'hour') {
  const d = parseDate(value);
  if (!d) return '—';
  if (scope === 'day') {
    return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit' }).format(d);
  }
  return new Intl.DateTimeFormat('zh-CN', { month: '2-digit', day: '2-digit', hour: '2-digit', hour12: false }).format(d);
}

export function fmtRelative(value) {
  const d = parseDate(value);
  if (!d) return '—';
  const diff = d.getTime() - Date.now();
  const abs = Math.abs(diff);
  const past = diff < 0;
  let text;
  if (abs < 45 * 1000) return '刚刚';
  if (abs < 60 * 60 * 1000) text = `${Math.round(abs / 60000)} 分钟`;
  else if (abs < 24 * 60 * 60 * 1000) text = `${Math.round(abs / 3600000)} 小时`;
  else if (abs < 30 * 24 * 60 * 60 * 1000) text = `${Math.round(abs / 86400000)} 天`;
  else text = `${Math.round(abs / (30 * 86400000))} 个月`;
  return past ? `${text}前` : `${text}后`;
}

/** 将“还剩 N 秒”也友好展示。 */
export function fmtRemaining(seconds) {
  const n = Number(seconds);
  if (!Number.isFinite(n) || n <= 0) return '已结束';
  if (n < 60) return `${Math.ceil(n)} 秒`;
  if (n < 3600) return `${Math.ceil(n / 60)} 分钟`;
  if (n < 86400) return `${oneDecimal.format(n / 3600)} 小时`;
  return `${oneDecimal.format(n / 86400)} 天`;
}

/** 美元/token → 可读的 $/百万 token。 */
export function fmtTokenPrice(value, { perMillion = true } = {}) {
  const n = Number(value);
  if (!Number.isFinite(n)) return '—';
  if (perMillion && n < 0.01) {
    const scaled = n * 1e6;
    return `$${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 }).format(scaled)} / 1M`;
  }
  return `$${new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 6 }).format(n)} / tok`;
}

export function fmtCost(credits) {
  const n = Number(credits);
  if (!Number.isFinite(n)) return '—';
  return `${fmtCredits(n)} 积分`;
}

export function maskKey(value) {
  const s = String(value ?? '');
  if (!s) return '';
  if (isMasked(s)) return s;
  if (s.length <= 10) return `${s.slice(0, 2)}…(len=${s.length})`;
  return `${s.slice(0, 6)}…${s.slice(-4)}…(len=${s.length})`;
}

export function isMasked(value) {
  const s = String(value ?? '');
  return s.includes('…') || /\*{3,}/.test(s) || /\(len=\d+\)/.test(s);
}

export function truncate(value, max = 80) {
  const s = String(value ?? '');
  if (s.length <= max) return s;
  return `${s.slice(0, Math.max(0, max - 1))}…`;
}

export function healthOfAccount(account) {
  if (!account) return { tone: 'muted', label: '未知' };
  if (account.auth_invalid) return { tone: 'danger', label: '授权失效' };
  const coolingUntil = parseDate(account.cooling_until);
  if (coolingUntil && coolingUntil.getTime() > Date.now()) return { tone: 'warn', label: '冷却中' };
  if (account.session_expired_at && !account.session_verified_at) return { tone: 'warn', label: '会话过期' };
  if (account.has_token === false && (account.cookie_required !== false)) return { tone: 'muted', label: '未配置凭证' };
  if (account.balance_error && account.available == null) return { tone: 'warn', label: '额度未知' };
  return { tone: 'ok', label: '正常' };
}

export function accountBalance(account) {
  if (!account) return null;
  const candidates = [account.available, account.credits, account.balance];
  for (const v of candidates) {
    if (v != null && v !== '') {
      const n = Number(v);
      if (Number.isFinite(n)) return n;
    }
  }
  return null;
}

export function accountError(account) {
  if (!account) return '';
  return account.auth_error || account.session_error || account.balance_error || account.last_error || '';
}

export function capsOfModel(model, providerType) {
  const set = new Set();
  const explicit = model?.capabilities || model?.capability || model?.tags;
  if (Array.isArray(explicit)) for (const c of explicit) set.add(String(c));
  if (model?.reasoning) set.add('reasoning');
  if (model?.supports_tools || model?.tools) set.add('tools');
  if (model?.vision) set.add('vision');
  if (model?.is_free) set.add('free');
  const id = String(model?.id || '').toLowerCase();
  if (/vision|vl|omni/.test(id)) set.add('vision');
  if (/reason|think|o[1-9]|r1/.test(id)) set.add('reasoning');
  if (/coder|cod(e|ing)/.test(id)) set.add('code');
  if (!set.size && providerType) {
    const p = BUILTIN_PROVIDER_TYPES.find((x) => x.type === providerType);
    for (const c of p?.capabilities || []) set.add(c);
  }
  set.delete('free');
  return Array.from(set);
}

export function capabilityLabel(cap) {
  return CAPABILITY_LABELS[cap] || cap;
}

export function deepClone(value) {
  if (value == null) return value;
  try { return JSON.parse(JSON.stringify(value)); } catch { return value; }
}

export function getPath(object, path) {
  if (!path) return object;
  return String(path).split('.').reduce((cur, key) => (cur == null ? undefined : cur[key]), object);
}

export function setPath(object, path, value) {
  const keys = String(path).split('.');
  let cur = object;
  for (let i = 0; i < keys.length - 1; i += 1) {
    const key = keys[i];
    if (cur[key] == null || typeof cur[key] !== 'object') cur[key] = {};
    cur = cur[key];
  }
  cur[keys[keys.length - 1]] = value;
  return object;
}

export function flattenObject(object, prefix = '', out = {}) {
  if (object == null || typeof object !== 'object' || Array.isArray(object)) {
    if (prefix) out[prefix] = object;
    return out;
  }
  for (const [key, value] of Object.entries(object)) {
    const path = prefix ? `${prefix}.${key}` : key;
    if (value != null && typeof value === 'object' && !Array.isArray(value)) {
      flattenObject(value, path, out);
    } else {
      out[path] = value;
    }
  }
  return out;
}

export function unflattenObject(flat) {
  const out = {};
  for (const [path, value] of Object.entries(flat || {})) setPath(out, path, value);
  return out;
}

export function prettyJSON(value) {
  try { return JSON.stringify(value, null, 2); } catch { return String(value); }
}

export function uniqueBy(list, keyFn) {
  const seen = new Set();
  const out = [];
  for (const item of list || []) {
    const key = keyFn(item);
    if (seen.has(key)) continue;
    seen.add(key);
    out.push(item);
  }
  return out;
}

export function normalizeKeyList(raw) {
  if (!Array.isArray(raw)) return [];
  return raw.map((item) => {
    if (typeof item === 'string') return { key: item, name: '', providers: [] };
    if (item && typeof item === 'object') {
      return {
        key: String(item.key ?? ''),
        name: String(item.name ?? ''),
        providers: Array.isArray(item.providers) ? item.providers.map(String) : [],
      };
    }
    return { key: String(item ?? ''), name: '', providers: [] };
  });
}

export function sum(list, getter) {
  let total = 0;
  for (const item of list || []) {
    const v = Number(getter(item));
    if (Number.isFinite(v)) total += v;
  }
  return total;
}

// ---------------------------------------------------------------------------
// 内置供应商元数据：7 种类型。后端 GET /panel/api/omni/provider-types 可用时
// 以服务端为准；404 / 失败 / 结构不符时退回这份定义（页面完全由 schema 驱动）。
// ---------------------------------------------------------------------------

const group = {
  conn: '连接',
  cred: '凭据',
  model: '模型',
  behavior: '行为',
};

/** @typedef {{name:string,label:string,type:string,group?:string,help?:string,placeholder?:string,required?:boolean,default?:any,advanced?:boolean,options?:Array<{value:string,label:string}|string>,item_type?:string,visible_if?:any,min?:number,max?:number,step?:number,rows?:number}} SchemaField */

/** @type {Array<any>} */
export const BUILTIN_PROVIDER_TYPES = Object.freeze([
  {
    type: 'raccoon',
    label: '浣熊',
    account_mode: 'oauth',
    capabilities: ['chat', 'stream', 'tools', 'vision'],
    actions: [
      { id: 'checkin', label: '签到' },
      { id: 'refresh', label: '刷新' },
      { id: 'login', label: '登录' },
      { id: 'remove', label: '移除' },
    ],
    config_schema: [
      { name: 'main_origin', label: '主站地址', type: 'url', group: group.conn, placeholder: 'https://xiaohuanxiong.com', default: 'https://xiaohuanxiong.com' },
      { name: 'llm_base', label: '模型接口', type: 'url', group: group.conn, help: '留空则使用主站默认路径' },
      { name: 'auth_base', label: '鉴权接口', type: 'url', group: group.conn, help: '留空则使用主站默认路径' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3, help: '非空时注入发给该上游的每个请求' },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true, help: '0 使用类型默认值，负数关闭限制' },
    ],
    account_schema: [
      { name: 'label', label: '账号标签', type: 'text', required: true, placeholder: '例如 main' },
      { name: 'cookie', label: 'Access Token', type: 'secret', required: true, help: '粘贴登录后的 access_token；也可先添加空壳再补' },
      { name: 'refresh_token', label: 'Refresh Token', type: 'secret', advanced: true, help: '可选，用于自动续期' },
      { name: 'email', label: '邮箱', type: 'text', advanced: true },
      { name: 'password', label: '密码', type: 'secret', advanced: true },
    ],
  },
  {
    type: 'runable',
    label: 'Runable',
    account_mode: 'cookie',
    capabilities: ['chat', 'stream', 'tools'],
    actions: [
      { id: 'checkin', label: '签到' },
      { id: 'refresh', label: '刷新' },
      { id: 'login', label: '登录' },
      { id: 'remove', label: '移除' },
    ],
    config_schema: [
      { name: 'base_url', label: '上游地址', type: 'url', group: group.conn, placeholder: 'https://api.runable.com', default: 'https://api.runable.com' },
      { name: 'device_header', label: 'Device Header', type: 'text', group: group.conn, advanced: true, help: '上游客户端指纹，通常无需修改' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3 },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [
      { name: 'label', label: '账号标签', type: 'text', required: true, placeholder: '例如 team-alpha' },
      { name: 'cookie', label: 'Session Token', type: 'secret', required: true, help: '浏览器登录 runable.com 后复制 session_token' },
      { name: 'email', label: '邮箱', type: 'text', advanced: true },
      { name: 'password', label: '密码', type: 'secret', advanced: true, help: '上游已关闭邮箱密码登录时仅留作备注' },
    ],
  },
  {
    type: 'openai',
    label: 'OpenAI 兼容',
    account_mode: 'apikey',
    capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    actions: [],
    config_schema: [
      { name: 'base_url', label: 'Base URL', type: 'url', group: group.conn, required: true, placeholder: 'https://api.openai.com/v1', default: 'https://api.openai.com/v1' },
      { name: 'api_key', label: '上游密钥', type: 'secret', group: group.cred, help: '单个密钥；留空也可仅使用密钥池' },
      { name: 'api_keys', label: '上游密钥池', type: 'list', item_type: 'secret', group: group.cred, help: '按请求轮询，401/403/429 自动冷却切下一个' },
      { name: 'models', label: '模型列表', type: 'list', group: group.model, help: '留空时自动从上游 /models 发现' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3 },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [],
  },
  {
    type: 'opencode',
    label: 'OpenCode',
    account_mode: 'apikey',
    capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    actions: [],
    config_schema: [
      { name: 'base_url', label: 'Zen Base URL', type: 'url', group: group.conn, placeholder: 'https://opencode.ai/zen/v1', default: 'https://opencode.ai/zen/v1' },
      { name: 'tier_prefer', label: '套餐优先', type: 'enum', group: group.conn, default: 'go', options: [{ value: 'go', label: 'Go 订阅优先' }, { value: 'zen', label: 'Zen 按量优先' }] },
      { name: 'go_base_url', label: 'Go Base URL', type: 'url', group: group.conn, advanced: true, help: '留空使用官方默认地址' },
      { name: 'zen_keys', label: 'Zen 密钥池', type: 'list', item_type: 'secret', group: group.cred, help: '按量付费 tier，池内轮询与冷却' },
      { name: 'go_keys', label: 'Go 密钥池', type: 'list', item_type: 'secret', group: group.cred, help: '订阅套餐 tier，可在「上游密钥」查询套餐额度' },
      { name: 'anonymous', label: '匿名免费通道', type: 'bool', group: group.behavior, default: true, help: '零密钥时按官方客户端形态使用公共免费层' },
      { name: 'models', label: '模型列表', type: 'list', group: group.model, help: '留空时自动发现' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3, advanced: true },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [],
  },
  {
    type: 'trae',
    label: 'TRAE',
    account_mode: 'login',
    capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    actions: [
      { id: 'checkin', label: '签到' },
      { id: 'refresh', label: '刷新' },
      { id: 'login', label: '登录' },
      { id: 'remove', label: '移除' },
    ],
    config_schema: [
      { name: 'agent_host', label: 'Agent Host', type: 'url', group: group.conn, placeholder: 'https://trae-api-cn.mchost.guru', default: 'https://trae-api-cn.mchost.guru' },
      { name: 'ug_host', label: 'UG Host', type: 'url', group: group.conn, default: 'https://api.trae.cn' },
      { name: 'oauth_host', label: 'OAuth Host', type: 'url', group: group.conn, default: 'https://api.trae.com.cn' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3 },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [
      { name: 'label', label: '账号标签', type: 'text', required: true, placeholder: '例如 solo-1' },
      { name: 'cookie', label: 'Access Token', type: 'secret', required: true },
      { name: 'refresh_token', label: 'Refresh Token', type: 'secret', advanced: true },
      { name: 'uid', label: 'UID', type: 'text', advanced: true },
      { name: 'machine_id', label: 'Machine ID', type: 'text', advanced: true, help: '每个账号必须唯一，签到依赖该指纹' },
      { name: 'device_id', label: 'Device ID', type: 'text', advanced: true },
      { name: 'api_host', label: 'API Host', type: 'url', advanced: true },
    ],
  },
  {
    type: 'newapi',
    label: 'New API',
    account_mode: 'login',
    capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    actions: [
      { id: 'checkin', label: '签到' },
      { id: 'refresh', label: '刷新' },
      { id: 'login', label: '登录' },
      { id: 'remove', label: '移除' },
    ],
    config_schema: [
      { name: 'base_url', label: '站点地址', type: 'url', group: group.conn, required: true, placeholder: 'https://your-newapi.example.com' },
      { name: 'api_key', label: '默认聊天密钥', type: 'secret', group: group.cred, help: '供应商级 sk- 密钥，账号未单独配置时使用' },
      { name: 'api_keys', label: '密钥池', type: 'list', item_type: 'secret', group: group.cred },
      { name: 'models', label: '模型列表', type: 'list', group: group.model },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3, advanced: true },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [
      { name: 'label', label: '账号标签', type: 'text', required: true },
      { name: 'cookie', label: '会话 ID', type: 'secret', required: true, help: 'New API 的 X-Auth-Session / access token' },
      { name: 'api_key', label: '聊天 API Key', type: 'secret', help: 'sk- 密钥，用于实际对话' },
      { name: 'refresh_token', label: 'Refresh Token', type: 'secret', advanced: true },
      { name: 'session_cookie', label: 'Session Cookie', type: 'secret', advanced: true },
    ],
  },
  {
    type: 'geminiweb',
    label: 'Gemini Web',
    account_mode: 'cookie',
    capabilities: ['chat', 'stream', 'vision'],
    actions: [
      { id: 'checkin', label: '签到' },
      { id: 'refresh', label: '刷新' },
      { id: 'login', label: '登录' },
      { id: 'remove', label: '移除' },
    ],
    config_schema: [
      { name: 'base_url', label: '网页端地址', type: 'url', group: group.conn, advanced: true, default: 'https://gemini.google.com' },
      { name: 'system_prompt', label: '供应商系统提示词', type: 'textarea', group: group.behavior, rows: 3 },
      { name: 'max_prompt_chars', label: '单条提示词上限', type: 'number', group: group.behavior, advanced: true },
    ],
    account_schema: [
      { name: 'label', label: '账号标签', type: 'text', required: true },
      { name: 'cookie', label: 'Cookie', type: 'secret', required: true, help: '包含 __Secure-1PSID 等 Gemini 网页端 Cookie' },
    ],
  },
]);

export function findProviderType(types, type) {
  const list = Array.isArray(types) && types.length ? types : BUILTIN_PROVIDER_TYPES;
  return list.find((item) => item.type === type) || null;
}

function normalizeSchemaField(field) {
  if (!field || typeof field !== 'object') return null;
  const name = field.name || field.key || field.id;
  if (!name) return null;
  return {
    ...field,
    name: String(name),
    label: String(field.label || field.title || name),
    type: String(field.type || 'text').toLowerCase(),
  };
}

export function normalizeProviderTypes(payload) {
  const raw = payload?.types || payload?.data?.types || payload?.data || payload;
  if (!Array.isArray(raw) || !raw.length) return null;
  const out = [];
  for (const item of raw) {
    if (!item || !item.type) continue;
    const modeMap = { api_key: 'apikey', api: 'apikey', none: 'none' };
    const mode = String(item.account_mode || item.accountMode || 'mixed').toLowerCase();
    out.push({
      type: String(item.type),
      label: String(item.label || providerLabel(item.type)),
      account_mode: modeMap[mode] || mode,
      capabilities: Array.isArray(item.capabilities) ? item.capabilities.map(String) : [],
      actions: Array.isArray(item.actions)
        ? item.actions.map((a) => (typeof a === 'string' ? { id: a, label: a } : { id: String(a.id || ''), label: String(a.label || a.id || '') }))
        : [],
      config_schema: (Array.isArray(item.config_schema) ? item.config_schema : (Array.isArray(item.configSchema) ? item.configSchema : []))
        .map(normalizeSchemaField).filter(Boolean),
      account_schema: (Array.isArray(item.account_schema) ? item.account_schema : (Array.isArray(item.accountSchema) ? item.accountSchema : []))
        .map(normalizeSchemaField).filter(Boolean),
    });
  }
  return out.length ? out : null;
}
