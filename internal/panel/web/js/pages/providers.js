// providers.js — 供应商工作台：左侧常显列表 + 右侧详情（概览/账号/上游密钥/模型/高级）。
//
// 所有编辑都在详情内完成，不弹供应商编辑框。provider 类型的字段、账号字段和
// 可用操作全部来自 /panel/api/omni/provider-types（失败时回退内置 7 种元数据），
// 未来新增 provider 只要后端给 schema，本页面无需改动。

import {
  api, describeError, isForbidden,
} from '../core/api.js';
import {
  authState, canOperateAccounts, canWrite, isAdmin, isAuthenticated,
  openLoginModal, requireAccountOperator, requireAdmin,
} from '../core/auth.js';
import {
  el, card, badge, statusDot, emptyState, loadingBlock, icon, copyText, pageBar,
} from '../core/dom.js';
import {
  BUILTIN_PROVIDER_TYPES, accountBalance, accountError, capsOfModel, capabilityLabel,
  deepClone, fmtChartTime, fmtCompact, fmtCredits, fmtDateTime, fmtDuration, fmtInt, fmtNum, fmtPercent,
  fmtRelative, fmtRemaining, fmtTime, fmtTokenPrice, fmtUptime, healthOfAccount,
  isKeyBasedProvider, normalizeProviderTypes, parseDate, prettyJSON, providerLabel, truncate, uniqueBy,
} from '../core/format.js';
import { createTabs } from '../components/tabs.js';
import { createTable } from '../components/table.js';
import { createSchemaForm } from '../components/schema-form.js';
import { openModal, confirmDialog } from '../components/modal.js';
import { openLoginWizard } from '../components/login-wizard.js';
import { toast, toastError, toastSuccess, toastWarn } from '../components/toast.js';

let pageSignal = null;
let rootEl = null;
let ctxRef = null;
let ui = initialState();
let balanceTimer = 0;

function initialState() {
  return {
    loading: true,
    loadError: '',
    accountsPayload: null,
    accountsError: '',
    config: null,
    configPath: '',
    configError: '',
    types: BUILTIN_PROVIDER_TYPES,
    models: [],
    modelsError: '',
    // modelsLoaded 区分「目录还没回来」与「目录真的为空」。聚合目录要访问各家
    // 上游，冷启动时比配置慢；没有这个标记，模型页会先说该供应商没有模型目录。
    modelsLoaded: false,
    // mappings 用于判断「改默认供应商到底有没有用」：命中别名的模型一律走别名
    // 目标链，默认供应商对它们完全无效。拿不到时按空表处理，只是少一条提示。
    mappings: {},
    mappingsError: '',
    keyHealth: null,
    keyHealthError: '',
    quota: {},
    quotaError: {},
    quotaLoading: {},
    revealedSecrets: {},
    balanceLoading: false,
    balanceError: '',
    // modelRefresh / modelRefreshError 按供应商名记录「刷新上游目录」的进行态与
    // 错误，避免整页重渲染时丢状态。
    modelRefresh: {},
    modelRefreshError: {},
    selected: '',
    tab: 'overview',
    addOpen: false,
    addValues: null,
    lastUpdated: 0,
  };
}

export async function mount(root, ctx) {
  pageSignal = new AbortController();
  rootEl = root;
  ctxRef = ctx;
  ui = initialState();
  ui.tab = 'overview';
  ui.selected = String(ctx.route?.query?.name || '');
  render();
  await loadAll({ initial: true });
  // 页面打开期间轻量轮询账号健康（不查余额），保持左侧状态新鲜。
  balanceTimer = window.setInterval(() => {
    if (document.hidden || ui.loading) return;
    refreshAccounts({ balance: false, silent: true });
  }, 45000);
}

export function unmount() {
  if (pageSignal) pageSignal.abort();
  pageSignal = null;
  clearInterval(balanceTimer);
  balanceTimer = 0;
  rootEl = null;
  ctxRef = null;
  ui = initialState();
}

async function loadAll({ initial = false } = {}) {
  if (!pageSignal) return;
  ui.loading = true;
  ui.loadError = '';
  if (initial || (!ui.config && !ui.accountsPayload)) render();

  const [accountsRes, configRes, typesRes, modelsRes, keyHealthRes, mappingRes] = await Promise.allSettled([
    api.accounts.list({ signal: pageSignal.signal }),
    api.config.get({ signal: pageSignal.signal }),
    api.providerTypes.get({ signal: pageSignal.signal }),
    api.models.get({ signal: pageSignal.signal }),
    api.keyHealth.get({ signal: pageSignal.signal }),
    api.mapping.get({ signal: pageSignal.signal }),
  ]);
  if (!pageSignal || pageSignal.signal.aborted) return;

  ui.loading = false;
  if (accountsRes.status === 'fulfilled') {
    ui.accountsPayload = accountsRes.value?.data || accountsRes.value || null;
    ui.accountsError = '';
  } else {
    ui.accountsError = describeError(accountsRes.reason);
    if (initial) ui.loadError = ui.accountsError;
  }

  if (configRes.status === 'fulfilled') {
    const payload = configRes.value || {};
    ui.config = payload.config || (Array.isArray(payload.providers) ? payload : null);
    ui.configPath = payload.path || '';
    ui.configError = '';
  } else {
    ui.config = null;
    ui.configPath = '';
    ui.configError = isForbidden(configRes.reason) ? '当前角色无法读取平台配置' : describeError(configRes.reason);
  }

  if (typesRes.status === 'fulfilled') {
    ui.types = normalizeProviderTypes(typesRes.value) || BUILTIN_PROVIDER_TYPES;
  } else {
    ui.types = BUILTIN_PROVIDER_TYPES;
  }

  if (modelsRes.status === 'fulfilled') {
    const payload = modelsRes.value || {};
    ui.models = Array.isArray(payload.data) ? payload.data : (Array.isArray(payload) ? payload : []);
    ui.modelsError = '';
    ui.modelsLoaded = true;
  } else {
    ui.models = [];
    ui.modelsError = describeError(modelsRes.reason);
    ui.modelsLoaded = false;
  }

  if (keyHealthRes.status === 'fulfilled') {
    ui.keyHealth = keyHealthRes.value;
    ui.keyHealthError = '';
  } else {
    ui.keyHealth = null;
    ui.keyHealthError = isForbidden(keyHealthRes.reason) ? '当前角色无法查看密钥健康' : describeError(keyHealthRes.reason);
  }

  if (mappingRes.status === 'fulfilled') {
    const payload = mappingRes.value || {};
    const raw = payload?.mappings ?? payload?.data?.mappings ?? payload ?? {};
    const out = {};
    for (const [alias, targets] of Object.entries(raw || {})) {
      out[String(alias)] = Array.isArray(targets) ? targets.map(String) : [String(targets)];
    }
    ui.mappings = out;
    ui.mappingsError = '';
  } else {
    ui.mappings = {};
    ui.mappingsError = isForbidden(mappingRes.reason) ? '当前角色无法查看别名表' : describeError(mappingRes.reason);
  }

  const list = providerList();
  if (!list.some((p) => p.name === ui.selected)) {
    ui.selected = list[0]?.name || '';
  }
  ui.lastUpdated = Date.now();
  render();

  if (isAuthenticated()) {
    refreshAccounts({ balance: true, silent: true });
  }
}

async function refreshAccounts({ balance = false, silent = false, announce = false } = {}) {
  if (!pageSignal) return;
  if (balance) {
    ui.balanceLoading = true;
    render();
  }
  try {
    const res = await api.accounts.list({ balance, signal: pageSignal.signal });
    if (!pageSignal || pageSignal.signal.aborted) return;
    ui.accountsPayload = res?.data || res || null;
    ui.accountsError = '';
    ui.balanceError = '';
    ui.lastUpdated = Date.now();
    if (announce) toastSuccess(balance ? '账号余额已刷新' : '账号状态已刷新');
  } catch (error) {
    if (pageSignal?.signal.aborted || error?.name === 'AbortError') return;
    ui.balanceError = describeError(error);
    if (!silent) toastError(error);
  } finally {
    ui.balanceLoading = false;
    // 后台静默刷新时不要打断正在填写的内联账号表单。
    if (!(silent && ui.addOpen)) render();
  }
}

function providerList() {
  const rows = [];
  const byName = new Map();
  const configProviders = Array.isArray(ui.config?.providers) ? ui.config.providers : [];
  for (const provider of configProviders) {
    const row = {
      name: provider.name,
      type: provider.type || '',
      config: provider,
      accounts: [],
      fromConfig: true,
      keys: null,
    };
    byName.set(provider.name, row);
    rows.push(row);
  }
  const payloadProviders = Array.isArray(ui.accountsPayload?.providers) ? ui.accountsPayload.providers : [];
  for (const provider of payloadProviders) {
    if (byName.has(provider.name)) {
      const row = byName.get(provider.name);
      row.accounts = Array.isArray(provider.accounts) ? provider.accounts : [];
      row.type = row.type || provider.type || '';
      row.keys = Number.isFinite(Number(provider.keys)) ? Number(provider.keys) : row.keys;
      continue;
    }
    const row = {
      name: provider.name,
      type: provider.type || '',
      config: null,
      accounts: Array.isArray(provider.accounts) ? provider.accounts : [],
      fromConfig: false,
      keys: Number.isFinite(Number(provider.keys)) ? Number(provider.keys) : null,
    };
    byName.set(provider.name, row);
    rows.push(row);
  }
  return rows;
}

function selectedProvider() {
  const list = providerList();
  return list.find((p) => p.name === ui.selected) || list[0] || null;
}

function typeMeta(type) {
  return (ui.types || []).find((t) => t.type === type)
    || BUILTIN_PROVIDER_TYPES.find((t) => t.type === type)
    || {
      type,
      label: providerLabel(type),
      account_mode: 'mixed',
      capabilities: [],
      actions: [],
      config_schema: [],
      account_schema: [],
    };
}

function actionIds(meta) {
  return new Set((meta.actions || []).map((a) => (typeof a === 'string' ? a : a.id)));
}

/** 类型声明的登录向导元数据（kind + transport + fields），无则返回 null。 */
function loginSpec(meta) {
  return meta && meta.login && meta.login.kind ? meta.login : null;
}

function routeRank(provider) {
  const config = ui.config || {};
  const prefer = Array.isArray(config.routing?.prefer) ? config.routing.prefer : [];
  const idx = prefer.indexOf(provider.name);
  if (idx >= 0) return idx + 1;
  if (!prefer.length && config.default_provider === provider.name) return 1;
  return null;
}

// routingKnown：本角色是否真的拿到了平台配置。
//
// 匿名/普通用户的 /panel/api/config 是 401，ui.config 为 null。此时 routeRank
// 恒为 null，界面若直接据此写「未参与路由」，等于把「看不到」说成「不参与」——
// 实际该供应商可能在优先级里排第 3。凡是拿路由身份下结论的地方都要先问这里。
function routingKnown() {
  return !!ui.config;
}

// bareModelNames：该供应商自有模型去掉供应商前缀后的裸名集合。别名表以裸名为键，
// 用它才能判断「这个供应商的模型是不是被别名接管了」。
function bareModelNames(provider, models) {
  const names = new Set();
  for (const model of models || []) {
    const id = String(model?.id || '');
    if (id) names.add(id.startsWith(`${provider.name}/`) ? id.slice(provider.name.length + 1) : id);
  }
  for (const name of provider.config?.models || []) names.add(String(name));
  return names;
}

// aliasHeadProvider：一条别名实际首选落到哪个供应商（目标链首项）。
function aliasHeadProvider(alias) {
  const chain = ui.mappings[alias] || [];
  if (!chain.length) return '';
  const value = String(chain[0] || '').trim();
  const names = providerList().map((p) => p.name).sort((a, b) => b.length - a.length);
  for (const name of names) {
    if (value === name || value.startsWith(`${name}/`)) return name;
  }
  return '';
}

// shadowingAliases：返回「模型名相同、但实际首选不是本供应商」的别名。
// 这些别名会让「把本供应商设为默认」对同名裸模型完全无效——这是路由里最反直觉
// 的一条规则，必须在供应商页说清楚，否则会一直以为是配置没生效。
function shadowingAliases(provider, models) {
  const out = [];
  for (const bare of bareModelNames(provider, models)) {
    const alias = ui.mappings[bare];
    if (!alias || !alias.length) continue;
    const head = aliasHeadProvider(bare);
    if (head && head !== provider.name) out.push({ alias: bare, head, chain: alias.join(' → ') });
  }
  return out;
}

function providerStatus(provider) {
  const accounts = provider.accounts || [];
  const auth = authState();
  const role = auth.role;
  if (!accounts.length) {
    const meta = typeMeta(provider.type);
    if (isKeyBasedProvider(meta)) {
      // 密钥型供应商的「形态」已经在类型行显示为「密钥型」，状态点必须说别的事，
      // 否则同一行里「密钥型 · 密钥型」是纯噪音。这里只回答凭证是否就绪。
      if (!keysKnown(provider)) return { tone: 'muted', label: '凭证不可见' };
      return upstreamKeyCount(provider) > 0
        ? { tone: 'ok', label: '凭证就绪' }
        : { tone: 'warn', label: '缺凭证' };
    }
    // 匿名访客的 accounts 被后端按归属过滤成空数组，这是「看不到」而不是
    // 「没有账号」：把空列表说成「空壳」会让访客以为供应商没配好。
    if (!auth.authenticated) return { tone: 'muted', label: '账号不可见' };
    if (role === 'user') return { tone: 'muted', label: '无归属账号' };
    return { tone: 'warn', label: '空壳' };
  }
  let failed = 0;
  let cooling = 0;
  let healthy = 0;
  let authorized = 0;
  for (const account of accounts) {
    const health = healthOfAccount(account);
    if (health.tone === 'danger') failed += 1;
    else if (health.tone === 'warn' && health.label === '冷却中') cooling += 1;
    else if (health.tone === 'ok') healthy += 1;
    else if (account.has_token !== false) authorized += 1;
  }
  if (failed) return { tone: 'danger', label: `异常 ${failed}` };
  if (cooling) return { tone: 'warn', label: `冷却 ${cooling}` };
  if (healthy) return { tone: 'ok', label: `${healthy} 正常` };
  if (authorized) return { tone: 'info', label: `${authorized} 已授权` };
  return { tone: 'muted', label: '待配置' };
}

function providerCredits(provider) {
  const nums = (provider.accounts || []).map(accountBalance).filter((n) => n != null);
  return nums.length ? nums.reduce((s, n) => s + n, 0) : null;
}

function keyFieldValues(configProvider) {
  if (!configProvider) return [];
  const out = [];
  for (const name of ['api_key', 'api_keys', 'zen_keys', 'go_keys']) {
    const value = configProvider[name];
    if (Array.isArray(value) && value.length) out.push({ name, values: value, kind: 'pool' });
    else if (typeof value === 'string' && value) out.push({ name, values: [value], kind: 'single' });
  }
  return out;
}

// upstreamKeyCount：该供应商到底配了几组上游密钥。
//
// 配置可读时按字段数（api_key / api_keys / zen_keys / go_keys）算；配置不可读
// （匿名、普通用户）时 config 为 null，但账号接口仍会给出该供应商的 keys 计数，
// 这是匿名可见的元信息。少了这个回退，atria 明明有 2 组密钥也会被写成
// 「凭证 未配置」，而状态点又同时显示「凭证就绪」，自相矛盾。
function upstreamKeyCount(provider) {
  if (provider.config) return keyFieldValues(provider.config).length;
  const n = Number(provider.keys);
  return Number.isFinite(n) && n > 0 ? n : 0;
}

// keysKnown：是否知道密钥数量。配置读不到且账号接口没给计数时为 false，
// 此时只能说「不可见」，不能说「没有」。
function keysKnown(provider) {
  if (provider.config) return true;
  return provider.keys != null && Number.isFinite(Number(provider.keys));
}

function providerModels(provider) {
  // 别名（alias_of）是路由层的转发名，不是供应商真实存在的上游模型；
  // 供应商页只展示自有模型，别名统一在「全局模型目录」和路由页查看。
  const catalog = (ui.models || []).filter((m) =>
    !m.alias_of &&
    (m.owned_by === provider.name || String(m.id || '').startsWith(`${provider.name}/`)));
  const out = uniqueBy(catalog, (m) => m.id);
  const knownSuffixes = new Set(out.map((m) => {
    const id = String(m.id || '');
    return id.startsWith(`${provider.name}/`) ? id.slice(provider.name.length + 1) : id;
  }));
  for (const name of provider.config?.models || []) {
    const id = String(name).startsWith(`${provider.name}/`) ? String(name) : `${provider.name}/${name}`;
    if (!knownSuffixes.has(String(name))) {
      out.push({ id, object: 'model', owned_by: provider.name, capabilities: [] });
      knownSuffixes.add(String(name));
    }
  }
  return out;
}

function render() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-providers' });
  const list = providerList();
  const selected = selectedProvider();
  const current = authState();

  const refreshBtn = el('button', {
    class: 'btn btn-ghost btn-sm', type: 'button',
    on: { click: () => { loadAll(); } },
  }, icon('refresh', 15), '刷新');

  page.appendChild(pageBar({
    context: ui.lastUpdated
      ? el('span', { class: 'auto-refresh-note', text: `更新于 ${fmtTime(ui.lastUpdated)}` })
      // 首次加载由列表区的 loadingBlock 提示；这里只在「已有内容、正在刷新」
      // 时写一行刷新状态，避免手机窄屏上两个加载提示上下叠着。
      : (ui.loading && list.length ? el('span', { class: 'auto-refresh-note', text: '刷新中…' }) : null),
    actions: [
      refreshBtn,
      (isAdmin() && ui.config)
        ? el('button', { class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'add-provider' } }, icon('plus', 15), '添加供应商')
        : null,
    ],
  }));

  if (ui.balanceError) {
    page.appendChild(notice('warn', '余额刷新失败', ui.balanceError));
  }
  if (ui.loadError && !list.length) {
    page.appendChild(card({
      title: '供应商数据加载失败',
      body: el('div', { class: 'error-panel' },
        icon('alert', 20),
        el('div', {},
          el('p', { text: ui.loadError }),
          el('button', { class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'retry-load' } }, '重试'),
        ),
      ),
    }));
  }

  const layout = el('div', { class: 'master-detail' });
  layout.appendChild(providerPane(list, selected, current));
  // 首次加载时 list 必为空。此时说「还没有供应商」是把「还没读到」当成「没有」，
  // 而真实情况往往有 5 家只是还没回来。左栏的 loadingBlock 已经在提示，右栏
  // 不再放第二个加载块（手机窄屏上会上下叠成两条），留一块安静的空位。
  const firstLoad = ui.loading && !list.length;
  layout.appendChild(selected ? providerDetail(selected, current) : el('section', { class: 'provider-detail empty-detail' },
    firstLoad
      ? null
      : emptyState('还没有供应商', '添加第一个供应商后即可配置账号与上游密钥。', isAdmin() ? el('button', {
        class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'add-provider' },
      }, '添加供应商') : null),
  ));
  page.appendChild(layout);

  page.appendChild(el('div', { class: 'page-footer-note' },
    // 「配置存储不可见」在加载完成前同样是假结论：还没问到，不等于没有权限。
    el('span', { text: ui.configPath ? `配置存储 ${ui.configPath}` : (firstLoad ? '配置存储读取中' : '配置存储不可见') }),
    // 匿名的 configError 是 api.js 的统一文案「需要登录后才能执行此操作」，
    // 和左边那句说的是同一件事。同一行里并排两遍只是噪音，条数由左句表达。
    (ui.configError && current.authenticated) ? el('span', { class: 'text-warn', text: ui.configError }) : null,
    el('span', { text: '账号余额来自上游实时查询，可能因网络或风控延迟。' }),
  ));

  page.addEventListener('click', onClick);
  rootEl.replaceChildren(page);
}

function providerPane(list, selected, current) {
  const pane = el('aside', { class: 'provider-pane' });
  pane.appendChild(el('div', { class: 'pane-head' },
    el('div', {},
      el('h2', { class: 'pane-title', text: '全部供应商' }),
      // 加载完成前不报数：0 个是结论，不是状态。左栏正文已有加载动画，这里留空，
      // 避免同一块区域上下叠两行「加载中」。
      ui.loading && !list.length ? null : el('span', { class: 'pane-sub', text: `${list.length} 个` }),
    ),
    (isAdmin() && ui.config)
      ? el('button', {
        class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '添加供应商',
        dataset: { action: 'add-provider' }, attrs: { 'aria-label': '添加供应商' },
      }, icon('plus', 15))
      : null,
  ));
  const items = el('div', { class: 'provider-list', attrs: { role: 'list' } });
  if (!list.length) {
    items.appendChild(ui.loading
      ? loadingBlock('加载供应商')
      : emptyState('暂无供应商', '点击右上角添加第一个供应商。'));
  }
  for (const provider of list) {
    const meta = typeMeta(provider.type);
    const status = providerStatus(provider);
    const rank = routeRank(provider);
    const credits = providerCredits(provider);
    const isDefault = ui.config?.default_provider === provider.name;
    // 列表项只保留三行：名称 + 路由身份、类型 + 账号形态、健康 + 额度。
    // 路由身份用徽标一次性表达（默认 / #顺位），不再在底部用一句话重复一遍
    // 「默认 · 优先级 #1」，那是同一件事说两遍。
    const item = el('button', {
      class: `provider-item ${provider.name === selected?.name ? 'active' : ''}`.trim(),
      type: 'button',
      dataset: { action: 'select-provider', provider: provider.name },
      attrs: { role: 'listitem' },
    },
      el('div', { class: 'provider-item-top' },
        el('span', { class: 'provider-name-block' },
          el('span', { class: `provider-dot dot-${status.tone}`, 'aria-hidden': 'true' }),
          el('span', { class: 'provider-name mono', text: provider.name }),
        ),
        el('span', { class: 'provider-badges' },
          isDefault ? badge('默认', 'accent', { title: 'default_provider：仅兜住没有别名命中的裸模型' }) : null,
          rank ? badge(`#${rank}`, 'info', { title: `裸模型优先级第 ${rank} 顺位` }) : null,
        ),
      ),
      el('div', { class: 'provider-item-type' },
        el('span', { text: meta.label || providerLabel(provider.type) }),
        el('span', { class: 'dot-sep', text: '·' }),
        // 这一行只回答「账号形态」：密钥型还是账号池。数量与可见性交给状态点，
        // 否则匿名视角下「账号不可见」会在同一行里出现两次。
        el('span', {
          text: provider.accounts.length
            ? `${provider.accounts.length} 账号`
            : (isKeyBasedProvider(meta) ? '密钥型' : '账号池'),
        }),
      ),
      el('div', { class: 'provider-item-meta' },
        statusDot(status.tone, status.label),
        el('span', { class: 'provider-quota' },
          credits != null ? `${fmtCredits(credits)} 额度` : (provider.accounts.length ? '额度未知' : '—'),
        ),
        el('span', { class: 'provider-arrow', 'aria-hidden': 'true' }, icon('chevron', 14)),
      ),
    );
    items.appendChild(item);
  }
  pane.appendChild(items);
  pane.appendChild(el('div', { class: 'pane-foot' },
    current.authenticated
      ? el('span', { class: 'small muted', text: current.role === 'user' ? '仅显示归属你的账号' : '账号按供应商实时聚合' })
      : el('button', {
        class: 'btn btn-ghost btn-sm btn-block', type: 'button', dataset: { action: 'login' },
      }, icon('login', 14), '登录查看账号'),
  ));
  return pane;
}

function providerDetail(provider, current) {
  const meta = typeMeta(provider.type);
  const status = providerStatus(provider);
  const rank = routeRank(provider);
  const models = providerModels(provider);
  // 密钥型供应商（account_mode=none/apikey）的凭证在供应商级，没有账号记录：
  // 不显示「账号」Tab，避免空壳 / 0 账号的误导。账号型供应商始终保留 Tab——第一
  // 个账号的添加入口就在里面，隐藏了就没地方加。
  const showAccounts = !isKeyBasedProvider(meta);
  if (!showAccounts && ui.tab === 'accounts') ui.tab = 'overview';
  const tabItems = [
    { id: 'overview', label: '概览' },
    // 匿名访客的账号列表被后端过滤为空，把 0 写成 Tab 计数等于断言「没有账号」。
    showAccounts
      ? { id: 'accounts', label: '账号', count: isAuthenticated() ? provider.accounts.length : null }
      : null,
    { id: 'keys', label: '上游密钥', count: upstreamKeyCount(provider) },
    { id: 'models', label: '模型', count: models.length },
    { id: 'advanced', label: '高级' },
  ].filter(Boolean);
  const tabs = createTabs({
    items: tabItems,
    value: ui.tab,
    ariaLabel: `${provider.name} 详情分区`,
    onChange: (value) => { ui.tab = value; ui.addOpen = false; render(); },
  });

  const detail = el('section', { class: 'provider-detail' });
  detail.appendChild(el('div', { class: 'provider-detail-head' },
    el('div', { class: 'provider-title-block' },
      el('div', { class: 'provider-title-row' },
        el('h2', { class: 'provider-title mono', text: provider.name }),
        badge(meta.label || providerLabel(provider.type), 'muted'),
        statusDot(status.tone, status.label),
        ui.config?.default_provider === provider.name ? badge('默认供应商', 'accent') : null,
        !routingKnown()
          ? badge('路由未知', 'muted', { title: '当前角色无法读取平台配置，无法判断它的默认/优先级身份' })
          : (rank ? badge(`路由 #${rank}`, 'info') : badge('未参与路由', 'muted')),
      ),
      el('p', { class: 'provider-subtitle', text: addressLabel(provider) }),
    ),
    el('div', { class: 'provider-head-actions' },
      current.authenticated
        ? el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'refresh-balance' },
          disabled: ui.balanceLoading,
        }, icon('refresh', 15), ui.balanceLoading ? '查询中…' : '刷新余额')
        : el('button', { class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'login' } }, icon('login', 14), '登录'),
    ),
  ));
  detail.appendChild(tabs.el);

  const body = el('div', { class: 'provider-tab-body' });
  try {
    if (ui.tab === 'accounts') body.appendChild(accountsTab(provider, meta, current));
    else if (ui.tab === 'keys') body.appendChild(keysTab(provider, meta, current));
    else if (ui.tab === 'models') body.appendChild(modelsTab(provider, models, current));
    else if (ui.tab === 'advanced') body.appendChild(advancedTab(provider, meta, current));
    else body.appendChild(overviewTab(provider, meta, current, models));
  } catch (error) {
    console.error('provider tab render failed', error);
    body.appendChild(card({ title: '分区渲染失败', body: el('p', { class: 'text-danger', text: describeError(error) }) }));
  }
  detail.appendChild(body);
  return detail;
}

function providerAddress(provider) {
  const c = provider.config || {};
  return c.base_url || c.main_origin || c.agent_host || c.ug_host || c.oauth_host || '';
}

// addressLabel：详情头那一行地址副标题。
// 地址来自平台配置，匿名与普通用户读不到 config，此时 providerAddress 返回空串。
// 写「地址未配置」等于断言「管理员没配」——实际只是「看不到」。
function addressLabel(provider) {
  return providerAddress(provider) || (provider.config ? '地址未配置' : '地址不可见');
}

function overviewTab(provider, meta, current, models) {
  const wrap = el('div', { class: 'tab-stack' });
  const credits = providerCredits(provider);
  const keyBased = isKeyBasedProvider(meta);
  const keyCount = upstreamKeyCount(provider);
  const keyKnown = keysKnown(provider);
  const fields = [
    // 供应商类型 / 运行状态 / 默认供应商 / 路由顺位 已由详情头部的徽标表达，
    // 这里再列一遍只是把同一句话写两次。事实区只放顶部徽标放不下的量化信息。
    keyBased
      ? ['凭证', !keyKnown ? '不可见' : (keyCount ? `上游密钥 ${keyCount} 组` : '未配置')]
      : ['账号数', authState().authenticated ? String(provider.accounts.length) : '不可见'],
    ['额度合计', credits != null ? `${fmtCredits(credits)} 积分` : '未知'],
    ['模型数量', String(models.length)],
    // 密钥型供应商上面已有「凭证」一行，不再重复。账号型供应商是账号池，凭据在
    // 账号里；这里写「上游密钥 未配置」会被读成该供应商没配好。只在真的配了
    // 密钥、或读不到配置时才列这一行。
    keyBased ? null : ((!keyKnown || keyCount > 0)
      ? ['上游密钥', !keyKnown ? '不可见' : `${keyCount} 组`]
      : null),
  ].filter(Boolean);
  const facts = el('div', { class: 'facts-grid' });
  for (const [label, value] of fields) {
    facts.appendChild(el('div', { class: 'fact' },
      el('span', { class: 'fact-label', text: label }),
      el('span', { class: 'fact-value', text: value }),
    ));
  }

  wrap.appendChild(card({
    title: '供应商概览',
    subtitle: '平台级配置与实时状态',
    actions: current.authenticated ? el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'refresh-balance' },
    }, '刷新余额') : null,
    body: el('div', {},
      facts,
      el('div', { class: 'cap-row' },
        el('span', { class: 'cap-label', text: '能力' }),
        (meta.capabilities || []).length
          ? (meta.capabilities || []).map((cap) => badge(capabilityLabel(cap), 'info'))
          : el('span', { class: 'muted small', text: '元数据未声明' }),
      ),
      providerAddress(provider)
        ? el('div', { class: 'address-row' },
          el('span', { class: 'fact-label', text: '地址' }),
          el('span', { class: 'mono small', text: providerAddress(provider) }),
        )
        : null,
    ),
  }));

  if (provider.config?.system_prompt) {
    wrap.appendChild(card({
      title: '供应商系统提示词',
      subtitle: '每个经该上游的请求都会注入',
      body: el('pre', { class: 'code-block prompt-block', text: provider.config.system_prompt }),
    }));
  }

  const errorAccounts = (provider.accounts || []).filter((a) => accountError(a));
  if (errorAccounts.length) {
    wrap.appendChild(card({
      title: '需要关注的账号',
      subtitle: `${errorAccounts.length} 个账号存在最近错误`,
      body: el('div', { class: 'attention-list' }, errorAccounts.slice(0, 5).map((account) => el('div', { class: 'attention-row' },
        el('div', { class: 'attention-main' },
          el('span', { class: 'mono', text: account.label }),
          statusDot(healthOfAccount(account).tone, healthOfAccount(account).label),
        ),
        el('p', { class: 'inline-error', text: truncate(accountError(account), 180) }),
      ))),
      actions: el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => { ui.tab = 'accounts'; render(); } },
      }, '前往账号'),
    }));
  }

  // 额度卡片由描述符声明：哪个类型声明了 quota 动作，概览页就渲染额度卡片。
  if (actionIds(meta).has('quota')) {
    wrap.appendChild(quotaCard(provider, current));
  }

  // 密钥型供应商没有账号记录：不渲染「账号一览」，改为一句说明，避免空表与
  // 指向已隐藏 Tab 的「管理账号」按钮。
  if (!keyBased) {
    const accounts = provider.accounts || [];
    const table = createTable({
      columns: [
        {
          key: 'label', label: '账号',
          render: (row) => el('div', { class: 'cell-title' },
            el('span', { class: 'mono', text: row.label }),
            el('span', { class: 'cell-sub', text: row.name || row.email || '—' }),
          ),
        },
        { key: 'status', label: '状态', render: (row) => statusDot(healthOfAccount(row).tone, healthOfAccount(row).label) },
        { key: 'balance', label: '额度', align: 'right', render: (row) => (accountBalance(row) != null ? fmtCredits(accountBalance(row)) : '—') },
        { key: 'expires', label: '到期', render: (row) => (row.expires_at ? fmtRelative(row.expires_at) : '—') },
        { key: 'error', label: '最近错误', render: (row) => (accountError(row) ? el('span', { class: 'text-warn truncate', text: truncate(accountError(row), 48) }) : '—') },
      ],
      rows: accounts,
      empty: current.authenticated ? '该供应商还没有账号' : '登录后查看账号明细',
    });
    wrap.appendChild(card({
      title: '账号一览',
      subtitle: '余额为最近一次上游查询结果',
      body: table.el,
      actions: el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => { ui.tab = 'accounts'; ui.addOpen = !current.authenticated ? false : ui.addOpen; render(); } },
      }, '管理账号'),
    }));
  } else {
    wrap.appendChild(notice(
      'muted',
      '密钥型供应商',
      '该类型的凭证配置在「上游密钥」，没有账号记录，也无需签到。',
    ));
  }

  // 「添加后怎么让供应商被使用」——很多供应商加完却始终不生效，原因是要么没配
  // 模型目录、要么没进路由。这里把它当前是否参与路由讲清楚，并给一键接入。
  wrap.appendChild(adoptionCard(provider, models));
  return wrap;
}

/**
 * 新供应商接入提示：说明它当前如何被路由命中，未参与路由时给出快捷操作。
 * 路由规则（与后端 routeResolved 同口径）：
 *   - 带 `provider/` 前缀的请求直接命中该供应商；
 *   - 裸模型名按 routing.prefer 顺序选第一个「目录里有该模型」的供应商；
 *   - 都没命中时回落到 default_provider。
 */
function adoptionCard(provider, models) {
  const config = ui.config || {};
  const isDefault = config.default_provider === provider.name;
  const rank = routeRank(provider);
  const canEdit = isAdmin() && canWrite();
  const keyBased = isKeyBasedProvider(typeMeta(provider.type));

  // 默认 / 优先级 / 模型目录三项已在上面的「供应商概览」里展示，这里不再重复，
  // 只保留本卡独有的价值：能做什么（按钮）+ 为什么走不到它（提示）。
  const actions = [];
  if (canEdit) {
    if (!isDefault) {
      actions.push(el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        dataset: { action: 'set-default-provider', provider: provider.name },
      }, '设为默认供应商'));
    }
    if (!rank) {
      actions.push(el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        dataset: { action: 'add-to-prefer', provider: provider.name },
      }, '加入路由优先级'));
    }
  }
  actions.push(el('a', { class: 'btn btn-ghost btn-sm', href: '#/routing' }, '前往路由设置'));

  const hints = [];
  const shadowed = shadowingAliases(provider, models);
  hints.push(`带 ${provider.name}/ 前缀的模型请求会直接命中该供应商，无需额外配置。`);
  if (!routingKnown()) {
    // 配置读不到时不要替用户下「走不到它」的结论：那只是看不到，不是不参与。
    hints.push('当前角色无法读取平台配置，无法判断裸模型名是否会路由到它；需要管理员或只读角色查看。');
  } else if (!isDefault && !rank) {
    hints.push('裸模型名目前不会路由到它：需要把它设为默认供应商，或加入路由优先级。');
  } else if (!isDefault && rank) {
    hints.push('裸模型名会按路由优先级顺序尝试；命中前若前序供应商已提供同名模型，会优先用前序供应商。');
  } else if (isDefault && shadowed.length) {
    hints.push(`它已是默认供应商，但下列模型的同名别名会先命中其它供应商，默认设置对它们无效。`);
  } else {
    hints.push('它是默认供应商：未命中其它规则时，请求会回落到这里。');
  }
  if (!models.length) {
    hints.push(keyBased
      ? '当前没有模型目录，请在「模型」页刷新上游目录或手动填写模型列表。'
      : '当前没有模型目录，请先添加账号，再在「模型」页刷新上游目录。');
  }

  return card({
    title: '接入路由',
    subtitle: '新供应商如何被请求命中',
    actions: actions.length ? actions : null,
    body: el('div', {},
      shadowed.length
        ? notice(
          'warn',
          `${shadowed.length} 个模型被别名接管`,
          `${shadowed.map((s) => `${s.alias} → ${s.head}`).join('；')}。`
          + '别名优先于默认供应商与路由优先级，这些模型不会走到本供应商；'
          + '要改首选，请到「路由 · 模型别名映射」调整目标链顺序。',
        )
        : null,
      el('ul', { class: 'hint-list' }, hints.map((t) => el('li', { text: t }))),
    ),
  });
}

function accountsTab(provider, meta, current) {
  const wrap = el('div', { class: 'tab-stack' });
  const schema = Array.isArray(meta.account_schema) ? meta.account_schema : [];
  const actions = actionIds(meta);
  const canOperate = canOperateAccounts();

  const toolbar = el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '账号与凭证' }),
      // 匿名时账号列表被后端过滤为空，写「0 个可见账号」会被读成「没有账号」，
      // 实际是「看不到」。这里只对已登录角色给数量。
      isAuthenticated()
        ? el('span', { class: 'muted small', text: `${provider.accounts.length} 个可见账号` })
        : null,
    ),
    el('div', { class: 'toolbar-actions' },
      current.authenticated ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'refresh-balance' },
        disabled: ui.balanceLoading,
      }, icon('refresh', 14), ui.balanceLoading ? '查询中…' : '刷新余额') : null,
      (isAdmin() && actions.has('checkin') && provider.accounts.length) ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'checkin-all', provider: provider.name },
      }, icon('check', 14), '全部签到') : null,
      (canOperate && loginSpec(meta)) ? el('button', {
        class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'login-wizard', provider: provider.name },
      }, icon('login', 14), '登录向导') : null,
      (canOperate && schema.length) ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'toggle-add-account', provider: provider.name },
      }, icon('plus', 14), ui.addOpen ? '收起表单' : '手动添加') : null,
    ),
  );
  wrap.appendChild(toolbar);

  if (!isAuthenticated()) {
    // 匿名访客看不到任何账号：一条提示说清「为什么空、怎么才能看到」就够，
    // 再叠一张「登录后查看账号」的空状态卡只是把同一句话说两遍。
    wrap.appendChild(notice('info', '账号明细需要登录', '账号按 provider/label 归属过滤；登录后按角色返回可见账号。', {
      label: '登录',
      action: 'login',
    }));
    return wrap;
  }
  if (!canOperate) {
    wrap.appendChild(notice('info', '只读角色', 'viewer 可以查看账号、余额与错误，但不能签到、刷新或移除。'));
  }

  if (ui.addOpen && canOperate) {
    wrap.appendChild(addAccountPanel(provider, meta, current));
  } else if (!schema.length) {
    wrap.appendChild(notice('muted', '该供应商使用上游密钥', '账号模式下无需添加账号；请到「上游密钥」或「高级」分区配置密钥。'));
  }

  if (!provider.accounts.length) {
    const spec = loginSpec(meta);
    wrap.appendChild(card({
      body: emptyState(
        current.authenticated ? '没有可见账号' : '登录后查看账号',
        current.authenticated
          ? (schema.length ? '用登录向导或手动添加账号后，即可签到、查看额度与最近错误。' : '请在上游密钥中配置密钥。')
          : '账号按 provider/label 归属过滤，普通用户只能看到自己的账号。',
        canOperate && spec
          ? el('button', {
            class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'login-wizard', provider: provider.name },
          }, icon('login', 14), '登录向导')
          : ((canOperate && schema.length) ? el('button', {
            class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'toggle-add-account', provider: provider.name },
          }, '添加账号') : null),
      ),
    }));
    return wrap;
  }

  const list = el('div', { class: 'account-list' });
  for (const account of provider.accounts) list.appendChild(accountCard(provider, account, meta, actions, current));
  wrap.appendChild(list);

  wrap.appendChild(el('div', { class: 'section-note' },
    el('span', { text: '操作范围：' }),
    isAdmin() ? el('span', { text: '管理员可操作全部账号' }) : (current.role === 'user' ? el('span', { text: '普通用户仅能操作归属自己的账号（后端校验）' }) : el('span', { text: '只读' })),
    el('span', { class: 'muted', text: ' · 余额与冷却由上游实时返回，失败时会保留最近的错误。' }),
  ));
  return wrap;
}

function accountCard(provider, account, meta, actions, current) {
  const health = healthOfAccount(account);
  const balance = accountBalance(account);
  // 缺数字用户 ID 是「可修的配置缺失」，不是授权失效：账号还能聊天，只是签到与
  // 额度用不了。提示语已经写清怎么修（重新抓取），所以优先展示它，避免用户先看到
  // 一条语义模糊的「未知」。
  const error = account.uid_error || accountError(account);
  const coolingMs = account.cooling_until ? parseDate(account.cooling_until)?.getTime() - Date.now() : 0;
  const canOperate = canOperateAccounts();
  const walletList = Array.isArray(account.wallets) ? account.wallets : [];
  const canCheckin = canOperate && actions.has('checkin');
  const canRefresh = canOperate && actions.has('refresh');
  const canRemove = canOperate && actions.has('remove');
  const canLogin = canOperate && actions.has('login');

  const actionButtons = [];
  if (canCheckin) {
    actionButtons.push(el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      dataset: { action: 'account-checkin', provider: provider.name, label: account.label },
    }, icon('check', 14), '签到'));
  }
  if (canRefresh) {
    actionButtons.push(el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      dataset: { action: 'account-refresh', provider: provider.name, label: account.label },
    }, icon('refresh', 14), '刷新会话'));
  }
  if (canLogin && (account.auth_invalid || account.has_token === false)) {
    actionButtons.push(el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      dataset: { action: 'login-wizard', provider: provider.name, label: account.label },
    }, icon('key', 14), '重新登录'));
  }
  if (current.authenticated) {
    actionButtons.push(el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      dataset: { action: 'refresh-balance' },
    }, '查额度'));
  }
  if (canRemove) {
    actionButtons.push(el('button', {
      class: 'btn btn-danger-ghost btn-sm', type: 'button',
      dataset: { action: 'account-remove', provider: provider.name, label: account.label },
    }, icon('trash', 14), '移除'));
  }

  const metrics = el('div', { class: 'account-metrics' },
    metric('额度', balance != null ? fmtCredits(balance) : '未知', account.balance_error ? 'text-warn' : ''),
    metric('到期', account.expires_at ? fmtRelative(account.expires_at) : '未知', ''),
    metric('签到', account.last_checkin ? fmtRelative(account.last_checkin) : (account.checkin_done ? '今日已签' : '未签到'), ''),
    metric('冷却', coolingMs > 0 ? fmtRemaining(coolingMs / 1000) : '无', coolingMs > 0 ? 'text-warn' : ''),
  );

  return el('article', { class: 'account-card' },
    el('div', { class: 'account-head' },
      el('div', { class: 'account-identity' },
        el('span', { class: 'mono account-label', text: account.label }),
        account.name ? el('span', { class: 'account-name', text: account.name }) : null,
        statusDot(health.tone, health.label),
        account.checkin_done ? badge('今日已签', 'ok') : null,
        account.credits_expiring ? badge(`快过期 ${fmtCredits(account.credits_expiring)}`, 'warn') : null,
      ),
      el('div', { class: 'account-actions' }, actionButtons.length ? actionButtons : badge('只读', 'muted')),
    ),
    metrics,
    walletList.length
      ? el('div', { class: 'wallet-row' }, walletList.map((wallet) => el('span', { class: 'wallet-chip' },
        el('span', { text: wallet.displayName || wallet.type || '额度' }),
        el('strong', { text: fmtCredits(wallet.balance) }),
      )))
      : null,
    (account.credits_limit != null)
      ? el('div', { class: 'quota-line' },
        el('span', { text: '套餐额度' }),
        el('span', { text: `${fmtCredits(account.credits_used ?? 0)} / ${fmtCredits(account.credits_limit)}` }),
      )
      : null,
    error ? el('div', { class: 'account-error' },
      icon('alert', 14),
      el('span', { text: truncate(error, 220) }),
    ) : null,
    (account.user_id || account.org_name || account.group)
      ? el('div', { class: 'account-meta-line' },
        account.user_id ? el('span', { text: `UID ${account.user_id}` }) : null,
        account.org_name ? el('span', { text: account.org_name }) : null,
        account.group ? el('span', { text: `分组 ${account.group}` }) : null,
      )
      : null,
  );
}

function metric(label, value, className = '') {
  return el('div', { class: `account-metric ${className}`.trim() },
    el('span', { class: 'metric-label', text: label }),
    el('strong', { class: 'metric-value', text: value }),
  );
}

function addAccountPanel(provider, meta, current) {
  const fields = Array.isArray(meta.account_schema) ? meta.account_schema : [];
  const form = createSchemaForm({
    fields,
    values: ui.addValues || {},
    disabled: false,
    idPrefix: `account-${provider.name}-${Date.now()}`,
  });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const addBtn = el('button', { class: 'btn btn-primary btn-sm', type: 'button' }, '添加账号');
  const cancelBtn = el('button', { class: 'btn btn-ghost btn-sm', type: 'button' }, '取消');

  async function submit() {
    errorBox.textContent = '';
    const validation = form.validate();
    if (!validation.valid) return;
    const values = form.getValues();
    if (!String(values.label || '').trim()) {
      errorBox.textContent = '账号标签不能为空';
      return;
    }
    if (!(await requireAccountOperator())) return;
    addBtn.disabled = true;
    try {
      await api.account.add({ provider: provider.name, ...values });
      toastSuccess('账号已添加');
      ui.addOpen = false;
      ui.addValues = null;
      await reloadAfterMutation();
    } catch (error) {
      errorBox.textContent = describeError(error);
      toastError(error);
    } finally {
      addBtn.disabled = false;
    }
  }

  addBtn.addEventListener('click', () => submit());
  cancelBtn.addEventListener('click', () => { ui.addOpen = false; ui.addValues = null; render(); });

  const spec = loginSpec(meta);
  return card({
    class: 'inline-form-card',
    title: '手动添加账号',
    subtitle: '凭证只提交给本地服务端；页面不会把明文写入 URL 或 localStorage',
    actions: spec
      ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'login-wizard', provider: provider.name },
      }, icon('login', 14), '改用登录向导')
      : badge(meta.login?.kind === 'paste' ? 'Cookie 账号' : (meta.login?.kind === 'oauth' ? 'OAuth 账号' : '登录账号'), 'info'),
    body: el('div', {},
      form.el,
      errorBox,
      el('div', { class: 'form-actions' }, addBtn, cancelBtn),
    ),
  });
}

async function reloadAfterMutation() {
  if (!pageSignal) return;
  const results = await Promise.allSettled([
    api.accounts.list({ balance: true, signal: pageSignal.signal }),
    ui.config ? api.config.get({ signal: pageSignal.signal }) : Promise.resolve(null),
  ]);
  if (!pageSignal || pageSignal.signal.aborted) return;
  const [accountsRes, configRes] = results;
  if (accountsRes.status === 'fulfilled') {
    ui.accountsPayload = accountsRes.value?.data || accountsRes.value || null;
    ui.accountsError = '';
  } else {
    ui.accountsError = describeError(accountsRes.reason);
  }
  if (configRes.status === 'fulfilled' && configRes.value) {
    ui.config = configRes.value.config
      || (Array.isArray(configRes.value.providers) ? configRes.value : ui.config);
    ui.configPath = configRes.value.path || ui.configPath;
  }
  ui.balanceLoading = false;
  ui.lastUpdated = Date.now();
  render();
}

/**
 * 刷新某供应商的上游模型目录（绕过网关 10 分钟缓存）。成功后就地重取模型目录
 * 并重渲染；失败时保留已有目录，只提示错误。
 */
async function refreshProviderModels(providerName) {
  if (!providerName) return;
  if (!(await requireAdmin({ message: '刷新上游模型目录需要管理员权限' }))) return;
  ui.modelRefresh[providerName] = true;
  delete ui.modelRefreshError[providerName];
  render();
  try {
    const res = await api.providerModels.refresh(providerName, { signal: pageSignal?.signal });
    const payload = res?.data || res || {};
    const warning = payload.warning;
    if (warning) toastWarn(`上游目录已刷新，但上游返回告警：${warning}`);
    else toastSuccess(`已刷新 ${providerName} 的上游目录`);
    // 重新拉取全局模型目录（含积分价），页面随之更新。
    const modelsRes = await api.models.get({ signal: pageSignal?.signal });
    const p = modelsRes || {};
    ui.models = Array.isArray(p.data) ? p.data : (Array.isArray(p) ? p : []);
    ui.modelsError = '';
    ui.modelsLoaded = true;
  } catch (error) {
    ui.modelRefreshError[providerName] = describeError(error);
    toastError(error);
  } finally {
    delete ui.modelRefresh[providerName];
    if (pageSignal && !pageSignal.signal.aborted) render();
  }
}

/** 把某供应商设为默认供应商（写 config.default_provider）。 */
async function setDefaultProvider(providerName) {
  if (!providerName) return;
  if (!(await requireAdmin({ message: '修改默认供应商需要管理员权限' }))) return;
  if (!ui.config) return;
  const next = deepClone(ui.config);
  next.default_provider = providerName;
  try {
    const res = await api.config.save(next, { signal: pageSignal?.signal });
    toastSuccess(`已把 ${providerName} 设为默认供应商`);
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`, { duration: 7000 });
    }
    await loadAll();
  } catch (error) {
    toastError(error);
  }
}

/** 把某供应商追加到 routing.prefer 末尾（裸模型名的供应商选择顺序）。 */
async function addProviderToPrefer(providerName) {
  if (!providerName) return;
  if (!(await requireAdmin({ message: '修改路由优先级需要管理员权限' }))) return;
  if (!ui.config) return;
  const next = deepClone(ui.config);
  const prefer = Array.isArray(next.routing?.prefer) ? next.routing.prefer.slice() : [];
  if (prefer.includes(providerName)) {
    toastWarn(`${providerName} 已在路由优先级中`);
    return;
  }
  prefer.push(providerName);
  next.routing = { ...(next.routing || {}), prefer };
  try {
    const res = await api.config.save(next, { signal: pageSignal?.signal });
    toastSuccess(`已把 ${providerName} 加入路由优先级（第 ${prefer.length} 顺位）`);
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`, { duration: 7000 });
    }
    await loadAll();
  } catch (error) {
    toastError(error);
  }
}

function keysTab(provider, meta, current) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!provider.config) {
    wrap.appendChild(notice(
      isAuthenticated() ? 'info' : 'muted',
      '平台配置不可读',
      // 匿名时 ui.configError 是 api.js 的统一文案「需要登录后才能执行此操作」，
      // 读起来像一句命令；这里换成说清能做什么的那句。
      !isAuthenticated()
        ? '登录管理员账号后可查看掩码密钥。'
        : (ui.configError || '需要管理员或只读角色才能查看上游密钥。'),
      isAuthenticated() ? null : { label: '登录', action: 'login' },
    ));
    return wrap;
  }

  const keyFields = keyFieldValues(provider.config);
  const revealed = ui.revealedSecrets[provider.name];
  const revealAllowed = isAdmin();
  const keyFieldLabels = {
    api_key: '上游密钥',
    api_keys: '上游密钥池',
    zen_keys: 'OpenCode Zen 密钥池',
    go_keys: 'OpenCode Go 密钥池',
  };

  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '上游密钥' }),
      el('span', { class: 'muted small', text: '默认只显示掩码；管理员可显式查看完整值' }),
    ),
    el('div', { class: 'toolbar-actions' },
      revealAllowed && keyFields.length
        ? (revealed
          ? el('button', { class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'hide-secrets', provider: provider.name } }, icon('eyeOff', 14), '隐藏完整密钥')
          : el('button', { class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'reveal-secrets', provider: provider.name } }, icon('eye', 14), '查看完整密钥'))
        : null,
    ),
  ));

  if (!keyFields.length) {
    wrap.appendChild(card({
      body: emptyState('没有配置上游密钥字段', '账号型供应商在「账号」里维护 Cookie/Token；API Key 型供应商请在「高级」中填写密钥。'),
    }));
  }

  for (const field of keyFields) {
    const rawValues = revealed?.[field.name] ?? field.values;
    const values = Array.isArray(rawValues) ? rawValues : [rawValues];
    wrap.appendChild(card({
      title: keyFieldLabels[field.name] || field.name,
      subtitle: field.kind === 'pool' ? `${field.values.length} 把密钥，按请求轮询` : '单个密钥',
      actions: field.kind === 'pool' ? badge(`${field.values.length} 项`, 'muted') : null,
      body: el('div', { class: 'secret-list' }, values.map((value, index) => {
        const text = typeof value === 'string' ? value : JSON.stringify(value);
        return el('div', { class: 'secret-row' },
          el('span', { class: 'secret-index mono', text: String(index + 1).padStart(2, '0') }),
          el('code', { class: `secret-value ${revealed ? 'revealed' : ''}`.trim(), text }),
          revealed ? el('button', {
            class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '复制密钥',
            attrs: { 'aria-label': '复制密钥' },
            on: { click: async () => {
              const ok = await copyText(text);
              if (ok) toastSuccess('已复制');
              else toastWarn('复制失败');
            } },
          }, icon('copy', 14)) : null,
        );
      })),
      class: revealed ? 'revealed-card' : '',
    }));
  }
  if (revealed) {
    wrap.appendChild(notice('warn', '完整密钥已显示', '响应不应被缓存；离开此页或点击「隐藏完整密钥」即可收起。'));
  }

  // 密钥池健康
  const healthProvider = (ui.keyHealth?.providers || []).find((p) => p.name === provider.name);
  if (ui.keyHealthError) {
    wrap.appendChild(notice('muted', '密钥健康不可用', ui.keyHealthError));
  } else if (healthProvider) {
    const table = createTable({
      columns: [
        { key: 'key', label: '密钥', render: (row) => el('span', { class: 'mono small', text: row.key || '—' }) },
        {
          key: 'status', label: '状态',
          render: (row) => (row.disabled ? badge('已禁用', 'danger') : badge('轮换中', 'ok')),
        },
        { key: 'strikes', label: '连败', align: 'right', render: (row) => fmtInt(row.strikes || 0) },
        { key: 'until', label: '禁用截止', render: (row) => (row.until ? fmtRelative(row.until) : '—') },
        { key: 'cleanup', label: '自动清理', render: (row) => (row.cleanup_at ? fmtRelative(row.cleanup_at) : '—') },
        {
          key: 'last_error', label: '最近错误',
          render: (row) => (row.last_error ? el('span', { class: 'text-warn small', text: truncate(row.last_error, 60) }) : '—'),
        },
      ],
      rows: healthProvider.keys || [],
      empty: '该供应商没有密钥池',
    });
    wrap.appendChild(card({
      title: '密钥池健康',
      subtitle: `连败 ${ui.keyHealth?.cleanup_strikes ?? 5} 次自动清理；清理提前 ${ui.keyHealth?.cleanup_days ?? 3} 天进入倒计时`,
      body: table.el,
    }));
  } else if (keyFields.length) {
    wrap.appendChild(notice('muted', '暂无密钥健康数据', '只有配置了上游密钥池（api_keys / zen_keys / go_keys）的供应商会进入自动禁用与清理。'));
  }

  // 同上：额度卡片只看描述符是否声明了 quota，不再认类型名。
  if (actionIds(meta).has('quota')) {
    wrap.appendChild(quotaCard(provider, current));
  }
  return wrap;
}

function quotaCard(provider, current) {
  const quota = ui.quota[provider.name];
  const loading = ui.quotaLoading[provider.name];
  const error = ui.quotaError[provider.name];
  const body = el('div', { class: 'quota-body' });
  if (error) body.appendChild(notice('warn', '套餐额度查询失败', error));
  if (!quota) {
    body.appendChild(el('p', { class: 'muted small', text: '查询 OpenCode Go 订阅套餐的 5h / 周窗口用量。该操作会访问上游，失败可重试。' }));
  } else {
    for (const key of quota.keys || []) {
      const cardEl = el('div', { class: 'quota-key' },
        el('div', { class: 'quota-key-head' },
          el('span', { class: 'mono', text: key.masked || key.fingerprint || '未知密钥' }),
          badge(key.status === 'ok' ? '正常' : (key.status === 'stale' ? '缓存' : '不可用'), key.status === 'ok' ? 'ok' : (key.status === 'stale' ? 'warn' : 'danger')),
        ),
      );
      if (key.error) cardEl.appendChild(el('p', { class: 'inline-error', text: key.error }));
      const windows = key.windows || {};
      if (!Object.keys(windows).length) {
        cardEl.appendChild(el('p', { class: 'muted small', text: '没有可用窗口数据' }));
      } else {
        for (const [name, win] of Object.entries(windows)) {
          const used = Number(win.used_percent || 0);
          cardEl.appendChild(el('div', { class: 'quota-window' },
            el('div', { class: 'quota-window-head' },
              el('span', { text: name }),
              el('span', { class: 'mono small', text: `${fmtNum(win.used_percent, 1)}% 已用` }),
            ),
            el('div', { class: 'progress' },
              el('span', { class: `progress-fill ${used > 85 ? 'danger' : (used > 60 ? 'warn' : '')}`.trim(), style: { width: `${Math.min(100, Math.max(0, used))}%` } }),
            ),
            el('div', { class: 'quota-window-foot' },
              el('span', { text: `剩余 ${fmtNum(win.remaining_percent, 1)}%` }),
              win.resets_at ? el('span', { text: `重置 ${fmtRelative(win.resets_at)}` }) : null,
            ),
          ));
        }
      }
      body.appendChild(cardEl);
    }
    if (!(quota.keys || []).length) body.appendChild(el('p', { class: 'muted small', text: '该供应商没有 Go 套餐密钥。' }));
  }
  return card({
    title: 'Go 套餐额度',
    subtitle: quota ? `查询于 ${fmtTime(quota.checked_at)}` : '上游实时额度',
    actions: el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button', disabled: loading, dataset: { action: 'load-quota', provider: provider.name },
    }, icon('refresh', 14), loading ? '查询中…' : '查询额度'),
    body,
  });
}

function modelsTab(provider, models, current) {
  const wrap = el('div', { class: 'tab-stack' });
  if (ui.modelsError && !models.length) {
    wrap.appendChild(notice('warn', '模型目录加载失败', ui.modelsError, { label: '重试', action: 'retry-load' }));
  }
  // 目录还没回来时不能断言「该供应商没有模型」：聚合目录要访问各家上游，冷启动
  // 时明显晚于配置到达。
  const pending = !ui.modelsLoaded && !models.length;
  const table = createTable({
    columns: [
      {
        key: 'id', label: '模型',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.id || '—' }),
          row.alias_of ? el('span', { class: 'cell-sub', text: `别名 → ${Array.isArray(row.alias_of) ? row.alias_of.join(' → ') : row.alias_of}` }) : null,
        ),
      },
      { key: 'context_window', label: '上下文', align: 'right', render: (row) => (row.context_window ? fmtCompact(row.context_window) : '—') },
      { key: 'max_tokens', label: '最大输出', align: 'right', render: (row) => (row.max_tokens ? fmtCompact(row.max_tokens) : '—') },
      {
        key: 'price', label: '价格 / 积分',
        render: (row) => modelPrice(row),
      },
      {
        key: 'caps', label: '能力',
        render: (row) => el('span', { class: 'tag-row' }, capsOfModel(row, provider.type).slice(0, 6).map((cap) => badge(capabilityLabel(cap), 'muted'))),
      },
    ],
    rows: models,
    empty: pending ? '正在读取模型目录…' : '该供应商还没有模型目录；可在「高级」配置 models 或刷新模型列表。',
  });
  const refreshing = ui.modelRefresh[provider.name];
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '模型目录' }),
      // 同样不为「还没读到」报数：0 个是结论。
      el('span', { class: 'muted small', text: pending ? '' : `${models.length} 个模型；id 形如 供应商/上游模型 ID` }),
    ),
    el('div', { class: 'toolbar-actions' },
      (isAdmin() && canWrite())
        ? el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button',
          dataset: { action: 'refresh-provider-models', provider: provider.name },
          disabled: !!refreshing,
        }, icon('refresh', 15), refreshing ? '拉取中…' : '刷新上游目录')
        : null,
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/models' }, '全局模型目录'),
    ),
  ));
  if (ui.modelRefreshError[provider.name]) {
    wrap.appendChild(notice('warn', '刷新上游目录失败', ui.modelRefreshError[provider.name]));
  }
  wrap.appendChild(card({ body: table.el, class: 'table-card' }));
  return wrap;
}

function modelPrice(model) {
  const bits = [];
  if (model.is_free) bits.push(badge('免费', 'ok'));
  if (model.credits != null) {
    bits.push(el('span', { class: 'mono small', text: model.credit_unit === 'multiplier' ? `×${fmtNum(model.credits, 2)}` : `${fmtNum(model.credits, 2)} 积分 / 1M` }));
  }
  if (model.pricing) {
    const input = model.pricing.input != null ? fmtTokenPrice(model.pricing.input) : null;
    const output = model.pricing.output != null ? fmtTokenPrice(model.pricing.output) : null;
    if (input) bits.push(el('span', { class: 'price-line', text: `入 ${input}` }));
    if (output) bits.push(el('span', { class: 'price-line', text: `出 ${output}` }));
  }
  return bits.length ? el('div', { class: 'price-cell' }, bits) : '—';
}

function advancedTab(provider, meta, current) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!ui.config) {
    wrap.appendChild(notice(
      isAuthenticated() ? 'info' : 'muted',
      '平台配置不可编辑',
      !isAuthenticated()
        ? '高级配置属于平台级设置，登录管理员或只读角色后可查看。'
        : (ui.configError || '当前角色无权读取平台配置。'),
      isAuthenticated() ? null : { label: '登录', action: 'login' },
    ));
    return wrap;
  }
  const fields = (meta.config_schema || []).filter((field) => field.name !== 'name' && field.name !== 'type' && field.name !== 'accounts');
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '高级配置' }),
      el('span', { class: 'muted small', text: canWrite() ? '保存后会热重载引擎；带掩码的密钥保持原值' : '当前角色只读' }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/system' }, '运行参数'),
    ),
  ));

  const basics = el('div', { class: 'facts-grid compact' },
    el('div', { class: 'fact' }, el('span', { class: 'fact-label', text: '名称' }), el('span', { class: 'fact-value mono', text: provider.name })),
    el('div', { class: 'fact' }, el('span', { class: 'fact-label', text: '类型' }), el('span', { class: 'fact-value', text: meta.label || provider.type })),
    el('div', { class: 'fact' }, el('span', { class: 'fact-label', text: '账号' }), el('span', { class: 'fact-value', text: String(provider.accounts.length) })),
  );
  wrap.appendChild(card({ title: '基本信息', body: basics }));

  if (!fields.length) {
    wrap.appendChild(notice('muted', '没有可编辑字段', '该类型的元数据未提供 config_schema。'));
  } else {
    const form = createSchemaForm({
      fields,
      values: provider.config || {},
      disabled: !canWrite(),
      idPrefix: `provider-${provider.name}-${Date.now()}`,
      submitLabel: canWrite() ? '保存供应商配置' : '',
      onSubmit: async (values) => {
        await saveProviderConfig(provider, values);
      },
    });
    wrap.appendChild(card({
      title: '供应商参数',
      subtitle: '字段由 provider-types 元数据动态生成',
      body: form.el,
      class: 'form-card',
    }));
  }

  if (provider.config) {
    const masked = deepClone(provider.config);
    wrap.appendChild(el('details', { class: 'raw-config' },
      el('summary', { text: '查看供应商原始 JSON（密钥为掩码）' }),
      el('pre', { class: 'code-block', text: prettyJSON(masked) }),
    ));
  }
  return wrap;
}

async function saveProviderConfig(provider, values) {
  if (!(await requireAdmin({ message: '修改供应商配置需要管理员权限' }))) return;
  if (!ui.config) return;
  const next = deepClone(ui.config);
  const idx = next.providers.findIndex((p) => p.name === provider.name);
  if (idx < 0) {
    toastError('供应商已不存在，请刷新页面');
    return;
  }
  next.providers[idx] = {
    ...next.providers[idx],
    ...values,
    name: provider.name,
    type: provider.type,
    accounts: next.providers[idx].accounts || [],
  };
  try {
    const res = await api.config.save(next, { signal: pageSignal?.signal });
    toastSuccess('供应商配置已保存');
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`, { duration: 7000 });
    }
    await loadAll();
  } catch (error) {
    toastError(error);
  }
}

function onClick(event) {
  const target = event.target instanceof Element ? event.target.closest('[data-action]') : null;
  if (!target) return;
  const action = target.dataset.action;
  if (!action) return;

  switch (action) {
    case 'select-provider': {
      const name = target.dataset.provider;
      if (!name) return;
      ui.selected = name;
      ui.tab = 'overview';
      ui.addOpen = false;
      ui.addValues = null;
      history.replaceState(null, '', `#/providers?name=${encodeURIComponent(name)}`);
      render();
      break;
    }
    case 'retry-load':
      loadAll();
      break;
    case 'refresh-balance':
      refreshAccounts({ balance: true, announce: true });
      break;
    case 'login':
      openLoginModal({ message: '登录后管理账号与供应商配置。' });
      break;
    case 'toggle-add-account':
      ui.addOpen = !ui.addOpen;
      ui.addValues = null;
      render();
      break;
    case 'add-provider':
      openAddProvider();
      break;
    case 'account-checkin':
      accountAction('checkin', target.dataset.provider, target.dataset.label, target);
      break;
    case 'account-refresh':
      accountAction('refresh', target.dataset.provider, target.dataset.label, target);
      break;
    case 'account-remove':
      removeAccount(target.dataset.provider, target.dataset.label);
      break;
    case 'login-wizard':
      openWizardFor(target.dataset.provider, target.dataset.label || '');
      break;
    case 'checkin-all':
      checkinAll(target.dataset.provider, target);
      break;
    case 'reveal-secrets':
      revealSecrets(target.dataset.provider);
      break;
    case 'hide-secrets':
      delete ui.revealedSecrets[target.dataset.provider];
      render();
      break;
    case 'load-quota':
      loadQuota(target.dataset.provider);
      break;
    case 'refresh-provider-models':
      refreshProviderModels(target.dataset.provider);
      break;
    case 'set-default-provider':
      setDefaultProvider(target.dataset.provider);
      break;
    case 'add-to-prefer':
      addProviderToPrefer(target.dataset.provider);
      break;
    default:
      break;
  }
}

/** 打开某供应商的登录向导（label 非空 = 重新登录某个账号，预填标签）。 */
function openWizardFor(providerName, label = '') {
  const provider = providerList().find((p) => p.name === providerName);
  if (!provider) return;
  const meta = typeMeta(provider.type);
  const spec = loginSpec(meta);
  if (!spec) {
    toastWarn('该供应商没有声明登录向导，请使用「手动添加」。');
    return;
  }
  openLoginWizard({
    provider,
    meta,
    values: label ? { label } : {},
    onDone: () => reloadAfterMutation(),
  });
}

async function accountAction(kind, providerName, label, button) {
  if (!(await requireAccountOperator())) return;
  if (!providerName || !label) return;
  if (button) button.disabled = true;
  try {
    const res = kind === 'checkin'
      ? await api.account.checkin(providerName, label)
      : await api.account.refresh(providerName, label);
    const results = Array.isArray(res) ? res : (res?.results || []);
    const failed = results.filter((r) => r && r.success === false);
    if (failed.length) toastWarn(`${label}：${failed[0].error || failed[0].msg || '操作未成功'}`);
    else toastSuccess(kind === 'checkin' ? '签到完成' : '会话刷新完成');
    await reloadAfterMutation();
  } catch (error) {
    toastError(error);
  } finally {
    if (button) button.disabled = false;
  }
}

async function checkinAll(providerName, button) {
  if (!(await requireAdmin({ message: '全部签到需要管理员权限' }))) return;
  if (!providerName) return;
  if (button) button.disabled = true;
  try {
    const res = await api.account.checkin(providerName, '');
    const results = Array.isArray(res) ? res : (res?.results || []);
    const failed = results.filter((r) => r && r.success === false);
    if (failed.length) toastWarn(`签到完成：成功 ${results.length - failed.length}，失败 ${failed.length}`);
    else toastSuccess(`已签到 ${results.length} 个账号`);
    await reloadAfterMutation();
  } catch (error) {
    toastError(error);
  } finally {
    if (button) button.disabled = false;
  }
}

async function removeAccount(providerName, label) {
  if (!(await requireAccountOperator())) return;
  const ok = await confirmDialog({
    title: '移除账号',
    message: `确定从 ${providerName} 移除账号「${label}」吗？该操作会清理运行时 token 与归属关系。`,
    confirmText: '移除',
    danger: true,
  });
  if (!ok) return;
  try {
    await api.account.remove(providerName, label);
    toastSuccess('账号已移除');
    await reloadAfterMutation();
  } catch (error) {
    toastError(error);
  }
}

async function revealSecrets(providerName) {
  if (!(await requireAdmin({ message: '查看完整密钥需要管理员权限' }))) return;
  try {
    const res = await api.providerSecret.get(providerName);
    ui.revealedSecrets[providerName] = res?.secrets || {};
    render();
  } catch (error) {
    toastError(error);
  }
}

async function loadQuota(providerName) {
  if (!isAuthenticated()) {
    openLoginModal({ message: '查看套餐额度需要登录。' });
    return;
  }
  ui.quotaLoading[providerName] = true;
  ui.quotaError[providerName] = '';
  render();
  try {
    const res = await api.providerQuota.get(providerName, { signal: pageSignal?.signal });
    ui.quota[providerName] = res;
  } catch (error) {
    ui.quotaError[providerName] = describeError(error);
    toastError(error);
  } finally {
    ui.quotaLoading[providerName] = false;
    render();
  }
}

// ---------------------------------------------------------------------------
// 添加供应商
// ---------------------------------------------------------------------------

function openAddProvider() {
  if (!ui.config) {
    toastWarn('配置尚未加载，无法添加供应商');
    return;
  }
  const nameInput = el('input', {
    class: 'input mono', type: 'text', placeholder: '例如 my-openai', autocomplete: 'off', spellcheck: 'false',
  });
  const typeSelect = el('select', { class: 'input select' },
    ui.types.map((meta) => el('option', { value: meta.type, text: `${meta.label || meta.type}（${meta.type}）` })),
  );
  const extra = el('div', { class: 'add-provider-extra' });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  // 模型选择区：点「获取模型列表」后在此列出上游返回的模型，勾选后写入 models 字段。
  const modelPicker = el('div', { class: 'add-provider-models' });
  let form = null;

  function rebuildSchema() {
    const meta = typeMeta(typeSelect.value);
    form = createSchemaForm({
      fields: (meta.config_schema || []).filter((f) => f.name !== 'name' && f.name !== 'type'),
      values: {},
      idPrefix: `new-provider-${typeSelect.value}`,
    });
    extra.replaceChildren(form.el);
    modelPicker.replaceChildren();
  }
  typeSelect.addEventListener('change', rebuildSchema);
  rebuildSchema();

  // hasModelsField：该类型的 schema 是否声明了 models 字段（有的类型没有，如
  // raccoon/trae 靠账号自动发现），没有就不显示「获取模型列表」。
  function hasModelsField() {
    const meta = typeMeta(typeSelect.value);
    return (meta.config_schema || []).some((f) => f.name === 'models');
  }

  function currentValues() {
    const values = form ? form.getValues() : {};
    // 探测用的供应商配置：名称缺省时给个占位（探测不落库，名称只用于日志）。
    // 刻意清空 models —— 上游后端在 models 非空时会当成白名单直接返回、不再请求
    // 上游；若不清空，用户「再点一次获取」只会拿回上一次勾选的结果。
    return { name: nameInput.value.trim() || 'probe', type: typeSelect.value, ...values, models: [] };
  }

  async function fetchModels() {
    if (!(await requireAdmin())) return;
    const provider = currentValues();
    fetchBtn.disabled = true;
    const original = fetchBtn.textContent;
    fetchBtn.textContent = '获取中…';
    modelPicker.replaceChildren(el('p', { class: 'field-help', text: '正在向该上游拉取模型目录…' }));
    try {
      const res = await api.providerModels.probe(provider, { signal: pageSignal?.signal });
      const payload = res?.data || res || {};
      const models = Array.isArray(payload.models) ? payload.models : [];
      renderModelPicker(models, payload.warning);
    } catch (error) {
      modelPicker.replaceChildren(el('p', { class: 'form-error', text: describeError(error) }));
    } finally {
      fetchBtn.disabled = false;
      fetchBtn.textContent = original;
    }
  }

  function renderModelPicker(models, warning) {
    modelPicker.replaceChildren();
    if (warning) {
      modelPicker.appendChild(el('p', { class: 'field-help text-warn', text: `上游返回告警：${warning}` }));
    }
    if (!models.length) {
      modelPicker.appendChild(el('p', { class: 'field-help', text: '上游没有返回模型；可留空由运行时自动发现，或手动填写 models。' }));
      return;
    }
    const selected = new Set((form.getValues().models || []).map(String));
    const boxes = [];
    const list = el('div', { class: 'model-pick-list' });
    for (const m of models) {
      const id = String(m.id || '').trim();
      if (!id) continue;
      const checked = selected.size ? selected.has(id) : true;
      const box = el('input', { type: 'checkbox', checked });
      boxes.push({ id, box });
      list.appendChild(el('label', { class: 'model-pick-row' },
        box,
        el('span', { class: 'mono', text: id }),
        m.credits != null ? el('span', { class: 'muted small', text: `${fmtNum(m.credits, 2)} 积分` }) : null,
      ));
    }
    const apply = () => {
      const picked = boxes.filter((b) => b.box.checked).map((b) => b.id);
      form.setValues({ models: picked });
      toastSuccess(`已选择 ${picked.length} 个模型`);
    };
    const allBtn = el('button', { class: 'btn btn-ghost btn-sm', type: 'button' }, '全选');
    const noneBtn = el('button', { class: 'btn btn-ghost btn-sm', type: 'button' }, '全不选');
    const applyBtn = el('button', { class: 'btn btn-primary btn-sm', type: 'button' }, '写入模型字段');
    allBtn.addEventListener('click', () => boxes.forEach((b) => { b.box.checked = true; }));
    noneBtn.addEventListener('click', () => boxes.forEach((b) => { b.box.checked = false; }));
    applyBtn.addEventListener('click', apply);
    modelPicker.appendChild(el('div', { class: 'model-pick-head' },
      el('span', { class: 'muted small', text: `上游返回 ${boxes.length} 个模型` }),
      el('div', { class: 'model-pick-actions' }, allBtn, noneBtn, applyBtn),
    ));
    modelPicker.appendChild(list);
  }

  const fetchBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, icon('models', 15), '获取模型列表');
  fetchBtn.addEventListener('click', fetchModels);

  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, '保存并热重载');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');

  // 类型切换后按 schema 决定是否显示「获取模型列表」。
  typeSelect.addEventListener('change', () => {
    fetchBtn.hidden = !hasModelsField();
    modelPicker.replaceChildren();
  });
  fetchBtn.hidden = !hasModelsField();

  const modal = openModal({
    title: '添加供应商',
    subtitle: '字段来自 provider-types 元数据；保存后立即构建新运行时。',
    size: 'lg',
    body: el('div', { class: 'add-provider-form' },
      el('div', { class: 'form-grid two-col' },
        el('div', { class: 'field' },
          el('label', { class: 'field-label' }, '供应商名称', el('span', { class: 'required-mark', text: '*' })),
          nameInput,
          el('p', { class: 'field-help', text: '全局唯一，建议使用小写字母与短横线。' }),
        ),
        el('div', { class: 'field' },
          el('label', { class: 'field-label' }, '供应商类型'),
          typeSelect,
        ),
      ),
      extra,
      modelPicker,
      errorBox,
    ),
    footer: [fetchBtn, cancelBtn, saveBtn],
    initialFocus: nameInput,
  });
  cancelBtn.addEventListener('click', () => modal.close());

  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    const name = nameInput.value.trim();
    const type = typeSelect.value;
    if (!name) { errorBox.textContent = '供应商名称不能为空'; nameInput.focus(); return; }
    if (!/^[A-Za-z0-9][A-Za-z0-9_.-]*$/.test(name)) { errorBox.textContent = '名称仅允许字母、数字、点、下划线与短横线'; return; }
    if (ui.config.providers?.some((p) => p.name === name)) { errorBox.textContent = '供应商名称已存在'; return; }
    const validation = form.validate();
    if (!validation.valid) { errorBox.textContent = '请先修正表单中的字段'; return; }
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    saveBtn.textContent = '保存中…';
    try {
      const next = deepClone(ui.config);
      next.providers = next.providers || [];
      const values = form.getValues();
      next.providers.push({ name, type, accounts: [], ...values });
      const res = await api.config.save(next, { signal: pageSignal?.signal });
      toastSuccess(`供应商 ${name} 已添加`);
      if (Array.isArray(res?.restart_required) && res.restart_required.length) {
        toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`);
      }
      ui.selected = name;
      ui.tab = Array.isArray(values.models) && values.models.length ? 'models' : 'overview';
      history.replaceState(null, '', `#/providers?name=${encodeURIComponent(name)}`);
      modal.close();
      await loadAll();
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
      saveBtn.textContent = '保存并热重载';
    }
  });
}

// ---------------------------------------------------------------------------
// 通用小部件
// ---------------------------------------------------------------------------

function notice(tone, title, text, action = null) {
  return el('div', { class: `notice notice-${tone}` },
    icon(tone === 'warn' ? 'alert' : (tone === 'danger' ? 'alert' : 'info'), 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: title }),
      el('span', { text }),
    ),
    action && action.action
      ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: action.action },
      }, action.label || '操作')
      : null,
  );
}
