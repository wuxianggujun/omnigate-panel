// format.js — 展示格式化、领域标签与内置供应商元数据（生成产物，接口失败时兜底）。

import { BUILTIN_DESCRIPTORS } from './builtin-types.generated.js';

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

export const LOGIN_KIND_LABELS = Object.freeze({
  oauth: '浏览器授权',
  snippet: '浏览器脚本捕获',
  paste: '粘贴凭证',
  manual: '手动填写',
});

// 描述符的 group 是稳定的英文常量；表单直接把它当分组标题，这里译成中文。
const GROUP_LABELS = Object.freeze({
  basic: '基础',
  connection: '连接',
  credentials: '凭据',
  models: '模型',
  behavior: '行为',
  advanced: '高级',
  fingerprint: '指纹',
});

export const CAPABILITY_LABELS = Object.freeze({
  // 模型能力（模型目录条目）
  chat: '对话',
  stream: '流式',
  tools: '工具',
  vision: '视觉',
  reasoning: '推理',
  image: '图像',
  embedding: '向量',
  audio: '音频',
  code: '代码',
  // 供应商能力（插件描述符 capabilities）
  accounts: '账号池',
  upstream_keys: '密钥池',
  balance: '余额',
  checkin: '签到',
  quota: '额度',
  login: '登录',
  fingerprint: '指纹',
  models: '模型目录',
  refresh: '刷新',
  attachments: '附件',
  cookie_rotation: 'Cookie 轮换',
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

export function loginKindLabel(kind) {
  return LOGIN_KIND_LABELS[kind] || kind || '登录';
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
// 兜底供应商元数据：由 catalog 生成（builtin-types.generated.js），与后端
// provider-types 接口同形；两条路径共用 normalizeProviderTypes 解析。
// ---------------------------------------------------------------------------

export const BUILTIN_PROVIDER_TYPES = normalizeProviderTypes({ types: BUILTIN_DESCRIPTORS }) || [];

export function findProviderType(types, type) {
  const list = Array.isArray(types) && types.length ? types : BUILTIN_PROVIDER_TYPES;
  return list.find((item) => item.type === type) || null;
}

// 密钥型供应商（凭证在供应商级，没有账号记录）。
//
// account_mode 有两套历史词汇：后端插件描述符用 SPI 的 none/pool/dynamic，
// 内置兜底元数据用 apikey/oauth/cookie/login。判断入口只此一处，两种都认，
// 避免新增密钥型插件被界面误判成「空壳 / 无账号」。
export function isKeyBasedProvider(meta) {
  const mode = String(meta?.account_mode || '').toLowerCase();
  return mode === 'none' || mode === 'apikey';
}

function normalizeCondition(condition) {
  if (Array.isArray(condition)) return condition.map(normalizeCondition);
  if (!condition || typeof condition !== 'object') return condition;
  // 描述符用 { key, equals }；表单求值器用 { field, equals }。
  if (condition.key && !condition.field) return { ...condition, field: condition.key };
  return condition;
}

function normalizeSchemaField(field) {
  if (!field || typeof field !== 'object') return null;
  const name = field.name || field.key || field.id;
  if (!name) return null;
  // 描述符用 key / kind / enum / show_if / item_kind；表单用
  // name / type / options / visible_if / item_type。
  const kind = field.type || field.kind || 'text';
  return {
    ...field,
    name: String(name),
    label: String(field.label || field.title || name),
    type: String(kind).toLowerCase(),
    options: field.options || field.enum || undefined,
    visible_if: normalizeCondition(field.visible_if ?? field.show_if),
    item_type: field.item_type || field.item_kind,
    group: GROUP_LABELS[field.group] || field.group,
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
      label: String(item.label || item.display_name || providerLabel(item.type)),
      account_mode: modeMap[mode] || mode,
      capabilities: Array.isArray(item.capabilities) ? item.capabilities.map(String) : [],
      actions: Array.isArray(item.actions)
        ? item.actions.map((a) => {
          if (typeof a === 'string') return { id: a, label: a };
          const id = a.id || a.name || '';
          return {
            id: String(id),
            label: String(a.label || id),
            scope: a.scope,
            confirm: !!a.confirm,
            help: a.help,
          };
        }).filter((a) => a.id)
        : [],
      config_schema: (Array.isArray(item.config_schema) ? item.config_schema : (Array.isArray(item.configSchema) ? item.configSchema : []))
        .map(normalizeSchemaField).filter(Boolean),
      account_schema: (Array.isArray(item.account_schema) ? item.account_schema : (Array.isArray(item.accountSchema) ? item.accountSchema : []))
        .map(normalizeSchemaField).filter(Boolean),
      login: normalizeLoginSpec(item.login),
    });
  }
  return out.length ? out : null;
}

function normalizeLoginSpec(raw) {
  if (!raw || typeof raw !== 'object') return null;
  const kind = String(raw.kind || '').toLowerCase();
  const transport = String(raw.transport || '').toLowerCase();
  if (!['oauth', 'snippet', 'paste', 'manual'].includes(kind)) return null;
  if (!['admin', 'account', 'newapi'].includes(transport)) return null;
  return {
    kind,
    transport,
    label: String(raw.label || ''),
    help: String(raw.help || ''),
    fields: (Array.isArray(raw.fields) ? raw.fields : []).map(normalizeSchemaField).filter(Boolean),
  };
}
