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
  el, card, badge, statusDot, emptyState, loadingBlock, icon, copyText,
} from '../core/dom.js';
import {
  BUILTIN_PROVIDER_TYPES, accountBalance, accountError, capsOfModel, capabilityLabel,
  deepClone, fmtChartTime, fmtCredits, fmtDateTime, fmtDuration, fmtInt, fmtNum, fmtPercent,
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
    keyHealth: null,
    keyHealthError: '',
    quota: {},
    quotaError: {},
    quotaLoading: {},
    revealedSecrets: {},
    balanceLoading: false,
    balanceError: '',
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

  const [accountsRes, configRes, typesRes, modelsRes, keyHealthRes] = await Promise.allSettled([
    api.accounts.list({ signal: pageSignal.signal }),
    api.config.get({ signal: pageSignal.signal }),
    api.providerTypes.get({ signal: pageSignal.signal }),
    api.models.get({ signal: pageSignal.signal }),
    api.keyHealth.get({ signal: pageSignal.signal }),
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
  } else {
    ui.models = [];
    ui.modelsError = describeError(modelsRes.reason);
  }

  if (keyHealthRes.status === 'fulfilled') {
    ui.keyHealth = keyHealthRes.value;
    ui.keyHealthError = '';
  } else {
    ui.keyHealth = null;
    ui.keyHealthError = isForbidden(keyHealthRes.reason) ? '当前角色无法查看密钥健康' : describeError(keyHealthRes.reason);
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
      continue;
    }
    const row = {
      name: provider.name,
      type: provider.type || '',
      config: null,
      accounts: Array.isArray(provider.accounts) ? provider.accounts : [],
      fromConfig: false,
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

function providerStatus(provider) {
  const accounts = provider.accounts || [];
  const role = authState().role;
  if (!accounts.length) {
    const meta = typeMeta(provider.type);
    if (isKeyBasedProvider(meta)) {
      if (provider.config && keyFieldValues(provider.config).length === 0) {
        return { tone: 'warn', label: '空壳' };
      }
      return { tone: 'ok', label: '密钥型' };
    }
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

function providerModels(provider) {
  const catalog = (ui.models || []).filter((m) => m.owned_by === provider.name || String(m.id || '').startsWith(`${provider.name}/`));
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

  page.appendChild(el('div', { class: 'page-head' },
    el('div', {},
      el('div', { class: 'eyebrow', text: 'SUPPLY LAYER' }),
      el('h1', { class: 'page-title', text: '供应商' }),
      el('p', { class: 'page-subtitle', text: '供应商、账号、上游密钥与运行参数集中在一处；列表保持常显，空壳也能直接配置。' }),
    ),
    el('div', { class: 'page-actions' },
      el('span', { class: 'auto-refresh-note' },
        ui.lastUpdated ? `更新于 ${fmtTime(ui.lastUpdated)}` : (ui.loading ? '加载中…' : ''),
      ),
      refreshBtn,
      (isAdmin() && ui.config)
        ? el('button', { class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'add-provider' } }, icon('plus', 15), '添加供应商')
        : null,
    ),
  ));

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
  layout.appendChild(selected ? providerDetail(selected, current) : el('section', { class: 'provider-detail empty-detail' },
    emptyState('还没有供应商', '添加第一个供应商后即可配置账号与上游密钥。', isAdmin() ? el('button', {
      class: 'btn btn-primary btn-sm', type: 'button', dataset: { action: 'add-provider' },
    }, '添加供应商') : null),
  ));
  page.appendChild(layout);

  page.appendChild(el('div', { class: 'page-footer-note' },
    el('span', { text: ui.configPath ? `配置文件 ${ui.configPath}` : '配置文件路径不可见' }),
    ui.configError ? el('span', { class: 'text-warn', text: ui.configError }) : null,
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
      el('span', { class: 'pane-sub', text: `${list.length} 个` }),
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
          isDefault ? badge('默认', 'accent') : null,
          rank ? badge(`#${rank}`, 'info') : null,
        ),
      ),
      el('div', { class: 'provider-item-type' },
        el('span', { text: meta.label || providerLabel(provider.type) }),
        el('span', { class: 'dot-sep', text: '·' }),
        el('span', { text: provider.accounts.length ? `${provider.accounts.length} 账号` : (isKeyBasedProvider(meta) ? '密钥型' : '0 账号') }),
      ),
      el('div', { class: 'provider-item-meta' },
        statusDot(status.tone, status.label),
        el('span', { class: 'provider-quota' },
          credits != null ? `${fmtCredits(credits)} 额度` : (provider.accounts.length ? '额度未知' : '—'),
        ),
      ),
      el('div', { class: 'provider-item-foot' },
        el('span', { class: rank ? 'route-rank' : 'route-rank muted', text: rank ? `路由第 ${rank} 顺位` : '未参与路由' }),
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
  const tabItems = [
    { id: 'overview', label: '概览' },
    { id: 'accounts', label: '账号', count: provider.accounts.length },
    { id: 'keys', label: '上游密钥', count: keyFieldValues(provider.config).length },
    { id: 'models', label: '模型', count: models.length },
    { id: 'advanced', label: '高级' },
  ];
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
        rank ? badge(`路由 #${rank}`, 'info') : badge('未参与路由', 'muted'),
      ),
      el('p', { class: 'provider-subtitle', text: providerAddress(provider) || '地址未配置' }),
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

function overviewTab(provider, meta, current, models) {
  const wrap = el('div', { class: 'tab-stack' });
  const status = providerStatus(provider);
  const credits = providerCredits(provider);
  const rank = routeRank(provider);
  const fields = [
    ['供应商类型', meta.label || providerLabel(provider.type)],
    ['运行状态', status.label],
    ['账号数', String(provider.accounts.length)],
    ['额度合计', credits != null ? `${fmtCredits(credits)} 积分` : '未知'],
    ['路由顺位', rank ? `第 ${rank} 顺位` : '未参与路由'],
    ['默认供应商', ui.config?.default_provider === provider.name ? '是' : '否'],
    ['模型数量', String(models.length)],
    ['上游密钥', keyFieldValues(provider.config).length ? `${keyFieldValues(provider.config).length} 组` : '未配置'],
  ];
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
  return wrap;
}

function accountsTab(provider, meta, current) {
  const wrap = el('div', { class: 'tab-stack' });
  const schema = Array.isArray(meta.account_schema) ? meta.account_schema : [];
  const actions = actionIds(meta);
  const canOperate = canOperateAccounts();

  const toolbar = el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '账号与凭证' }),
      el('span', { class: 'muted small', text: `${provider.accounts.length} 个可见账号` }),
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
    wrap.appendChild(notice('info', '账号明细需要登录', '匿名访客只能看到供应商空壳。登录后根据角色返回可见账号。', {
      label: '登录',
      action: 'login',
    }));
  } else if (!canOperate) {
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
  const error = accountError(account);
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

function keysTab(provider, meta, current) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!provider.config) {
    wrap.appendChild(notice(
      isAuthenticated() ? 'info' : 'muted',
      '平台配置不可读',
      ui.configError || (isAuthenticated() ? '需要管理员或只读角色才能查看上游密钥。' : '登录管理员账号后可查看掩码密钥。'),
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
    empty: '该供应商还没有模型目录；可在「高级」配置 models 或刷新模型列表。',
  });
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '模型目录' }),
      el('span', { class: 'muted small', text: `${models.length} 个模型；id 已带供应商前缀` }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/models' }, '全局模型目录'),
    ),
  ));
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
      ui.configError || '当前角色无权读取平台配置。',
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
  let form = null;

  function rebuildSchema() {
    const meta = typeMeta(typeSelect.value);
    form = createSchemaForm({
      fields: (meta.config_schema || []).filter((f) => f.name !== 'name' && f.name !== 'type'),
      values: {},
      idPrefix: `new-provider-${typeSelect.value}`,
    });
    extra.replaceChildren(form.el);
  }
  typeSelect.addEventListener('change', rebuildSchema);
  rebuildSchema();

  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, '保存并热重载');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');

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
      errorBox,
    ),
    footer: [cancelBtn, saveBtn],
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
      ui.tab = 'overview';
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
