// models.js — 全局模型目录：供应商前缀、价格/积分、能力标签与搜索筛选。

import { api, describeError } from '../core/api.js';
import {
  el, card, badge, emptyState, loadingBlock, icon, copyText, debounce, pageBar,
} from '../core/dom.js';
import {
  BUILTIN_PROVIDER_TYPES, capsOfModel, capabilityLabel, fmtCompact, fmtNum, fmtTokenPrice,
  normalizeProviderTypes, providerLabel,
} from '../core/format.js';
import { createTable } from '../components/table.js';
import { toastError, toastSuccess, toastWarn } from '../components/toast.js';

let pageSignal = null;
let rootEl = null;
let tableHost = null;
let countNode = null;
let ui = initialState();

function initialState() {
  return {
    loading: true,
    loaded: false,
    error: '',
    models: [],
    issues: [],
    types: BUILTIN_PROVIDER_TYPES,
    query: '',
    provider: '',
    capability: '',
    sort: 'name',
    freeOnly: false,
  };
}

export async function mount(root) {
  pageSignal = new AbortController();
  rootEl = root;
  ui = initialState();
  renderShell();
  await load();
}

export function unmount() {
  if (pageSignal) pageSignal.abort();
  pageSignal = null;
  rootEl = null;
  tableHost = null;
  countNode = null;
  ui = initialState();
}

async function load() {
  ui.loading = true;
  ui.error = '';
  const host = document.getElementById('models-table-host');
  if (host) host.replaceChildren(loadingBlock('加载模型目录'));
  const [modelsRes, typesRes] = await Promise.allSettled([
    api.models.get({ signal: pageSignal.signal }),
    api.providerTypes.get({ signal: pageSignal.signal }),
  ]);
  if (!pageSignal || pageSignal.signal.aborted) return;
  ui.loading = false;
  ui.loaded = true;
  if (modelsRes.status === 'fulfilled') {
    const payload = modelsRes.value || {};
    ui.models = Array.isArray(payload.data) ? payload.data : (Array.isArray(payload) ? payload : []);
    ui.issues = Array.isArray(payload.issues) ? payload.issues : [];
  } else {
    ui.error = describeError(modelsRes.reason);
    ui.models = [];
    ui.issues = [];
  }
  ui.types = typesRes.status === 'fulfilled'
    ? (normalizeProviderTypes(typesRes.value) || BUILTIN_PROVIDER_TYPES)
    : BUILTIN_PROVIDER_TYPES;
  renderShell();
  applyFilters();
}

function renderShell() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-models' });
  const providers = Array.from(new Set(ui.models.map((m) => m.owned_by).filter(Boolean))).sort();
  const capabilities = Array.from(new Set(ui.models.flatMap((m) => capsOfModel(m)))).sort();
  const freeCount = ui.models.filter((m) => m.is_free).length;
  const reasoningCount = ui.models.filter((m) => capsOfModel(m).includes('reasoning')).length;

  page.appendChild(pageBar({
    actions: [el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      on: { click: () => load() },
    }, icon('refresh', 15), '刷新目录')],
  }));

  // 首次渲染发生在数据回来之前，models 还是空数组。此时按 0 渲染四个数字，等于
  // 对「目录里没有模型」下结论——真实情况是还没读到。加载期间统一留横线。
  const loaded = ui.loaded;
  page.appendChild(el('div', { class: 'kpi-grid kpi-grid-4' },
    statCard('模型总数', loaded ? fmtNum(ui.models.length, 0) : '—', loaded ? '来自引擎聚合目录' : '', 'models'),
    statCard('供应商', loaded ? fmtNum(providers.length, 0) : '—', loaded ? (providers.slice(0, 4).join(' · ') || '—') : '', 'providers'),
    statCard('免费模型', loaded ? fmtNum(freeCount, 0) : '—', loaded ? '上游标记免费或匿名通道' : '', 'ok'),
    statCard('支持推理', loaded ? fmtNum(reasoningCount, 0) : '—', loaded ? 'reasoning / think 类模型' : '', 'reasoning'),
  ));

  if (ui.error) {
    page.appendChild(el('div', { class: 'notice notice-warn' },
      icon('alert', 16),
      el('div', { class: 'notice-content' }, el('strong', { text: '模型目录加载失败' }), el('span', { text: ui.error })),
      el('button', { class: 'btn btn-ghost btn-sm', type: 'button', on: { click: () => load() } }, '重试'),
    ));
  }
  // 某家上游目录这次没读到：模型会变少，必须说明「少了谁、是暂缺还是沿用缓存」，
  // 否则用户只会看到一个悄悄缩水的目录。
  const missingProviders = ui.issues.filter((i) => !i.cached).map((i) => i.provider).filter(Boolean);
  const staleProviders = ui.issues.filter((i) => i.cached).map((i) => i.provider).filter(Boolean);
  if (missingProviders.length || staleProviders.length) {
    const bits = [];
    if (missingProviders.length) bits.push(`${missingProviders.join('、')} 的目录这次没读到，这些供应商的模型暂缺`);
    if (staleProviders.length) bits.push(`${staleProviders.join('、')} 正在沿用上次成功的目录（新模型与价格可能滞后）`);
    page.appendChild(el('div', { class: `notice ${missingProviders.length ? 'notice-warn' : 'notice-info'}` },
      icon('alert', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '部分供应商目录未刷新' }),
        el('span', { text: `${bits.join('；')}。` })),
    ));
  }

  const search = el('input', {
    class: 'input search-input', type: 'search', value: ui.query,
    placeholder: '搜索模型、供应商或能力…', autocomplete: 'off',
  });
  const providerSelect = el('select', { class: 'input select select-sm' },
    el('option', { value: '', text: '全部供应商' }),
    providers.map((provider) => el('option', { value: provider, text: provider, selected: ui.provider === provider })),
  );
  const capabilitySelect = el('select', { class: 'input select select-sm' },
    el('option', { value: '', text: '全部能力' }),
    capabilities.map((cap) => el('option', { value: cap, text: capabilityLabel(cap), selected: ui.capability === cap })),
  );
  const sortSelect = el('select', { class: 'input select select-sm' },
    el('option', { value: 'name', text: '按名称', selected: ui.sort === 'name' }),
    el('option', { value: 'provider', text: '按供应商', selected: ui.sort === 'provider' }),
    el('option', { value: 'context', text: '按上下文', selected: ui.sort === 'context' }),
    el('option', { value: 'credits', text: '按积分价', selected: ui.sort === 'credits' }),
  );
  const freeToggle = el('input', { type: 'checkbox', checked: ui.freeOnly });
  const freeLabel = el('label', { class: 'check-inline' }, freeToggle, el('span', { text: '仅看免费' }));

  search.addEventListener('input', debounce(() => { ui.query = search.value.trim(); applyFilters(); }, 120));
  providerSelect.addEventListener('change', () => { ui.provider = providerSelect.value; applyFilters(); });
  capabilitySelect.addEventListener('change', () => { ui.capability = capabilitySelect.value; applyFilters(); });
  sortSelect.addEventListener('change', () => { ui.sort = sortSelect.value; applyFilters(); });
  freeToggle.addEventListener('change', () => { ui.freeOnly = freeToggle.checked; applyFilters(); });

  const filterbar = el('div', { class: 'filter-bar' },
    el('div', { class: 'search-wrap' }, icon('search', 15), search),
    providerSelect,
    capabilitySelect,
    sortSelect,
    freeLabel,
    el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      on: {
        click: () => {
          ui.query = ''; ui.provider = ''; ui.capability = ''; ui.sort = 'name'; ui.freeOnly = false;
          renderShell();
          applyFilters();
        },
      },
    }, '清除筛选'),
  );

  countNode = el('span', { class: 'muted small' });
  tableHost = el('div', { id: 'models-table-host', class: 'table-card' });
  const tableCard = card({
    title: '目录',
    subtitle: '价格与积分为上游回传，未知字段显示为横线',
    actions: countNode,
    body: tableHost,
    class: 'models-card',
  });
  page.appendChild(filterbar);
  page.appendChild(tableCard);
  page.appendChild(el('div', { class: 'page-footer-note' },
    el('span', { text: '模型目录只读；模型别名在「路由」页配置后会自动出现在这里。' }),
    el('a', { class: 'link-btn', href: '#/routing' }, '前往路由'),
  ));
  rootEl.replaceChildren(page);
}

function statCard(label, value, meta, tone) {
  return el('div', { class: `kpi-card kpi-${tone === 'ok' ? 'ok' : (tone === 'reasoning' ? 'accent' : 'muted')}` },
    el('div', { class: 'kpi-top' },
      el('span', { class: 'kpi-label', text: label }),
      el('span', { class: 'kpi-icon' }, icon(tone === 'ok' ? 'check' : (tone === 'reasoning' ? 'activity' : (tone === 'providers' ? 'server' : 'models')), 16)),
    ),
    el('div', { class: 'kpi-value', text: value }),
    el('div', { class: 'kpi-meta', text: meta }),
  );
}

function applyFilters() {
  if (!tableHost) return;
  const query = ui.query.toLowerCase();
  let rows = ui.models.slice();
  if (ui.provider) rows = rows.filter((m) => String(m.owned_by || '') === ui.provider);
  if (ui.freeOnly) rows = rows.filter((m) => m.is_free);
  if (ui.capability) rows = rows.filter((m) => capsOfModel(m).includes(ui.capability));
  if (query) {
    rows = rows.filter((m) => {
      const hay = [
        m.id, m.owned_by,
        ...(capsOfModel(m).map(capabilityLabel)),
        m.alias_of ? (Array.isArray(m.alias_of) ? m.alias_of.join(' ') : String(m.alias_of)) : '',
      ].join(' ').toLowerCase();
      return hay.includes(query);
    });
  }
  const typeName = (providerName) => {
    const meta = ui.types.find((t) => t.type === providerName);
    return meta?.label || providerLabel(providerName);
  };
  rows.sort((a, b) => {
    if (ui.sort === 'provider') return String(a.owned_by || '').localeCompare(String(b.owned_by || '')) || String(a.id || '').localeCompare(String(b.id || ''));
    if (ui.sort === 'context') return Number(b.context_window || 0) - Number(a.context_window || 0);
    if (ui.sort === 'credits') return Number(b.credits || 0) - Number(a.credits || 0);
    return String(a.id || '').localeCompare(String(b.id || ''));
  });
  if (countNode) {
    // 没有筛选时不写「24 / 24」：KPI 已经报了模型总数，这里只回答「筛掉了多少」。
    countNode.textContent = (ui.loaded && rows.length !== ui.models.length)
      ? `${fmtNum(rows.length, 0)} / ${fmtNum(ui.models.length, 0)} 个模型`
      : '';
  }
  const table = createTable({
    columns: [
      {
        key: 'id',
        label: '模型',
        render: (row) => {
          const id = String(row.id || '');
          const slash = id.indexOf('/');
          const prefix = slash > 0 ? id.slice(0, slash + 1) : '';
          const rest = slash > 0 ? id.slice(slash + 1) : id;
          return el('div', { class: 'model-name-cell' },
            el('div', { class: 'model-name-line' },
              prefix ? el('span', { class: 'model-prefix', text: prefix }) : null,
              el('span', { class: 'model-name mono', text: rest }),
              row.alias_of ? badge('别名', 'accent') : null,
              row.is_free ? badge('免费', 'ok') : null,
            ),
            row.alias_of
              ? el('div', { class: 'model-alias', text: `→ ${Array.isArray(row.alias_of) ? row.alias_of.join(' → ') : row.alias_of}` })
              : null,
          );
        },
      },
      {
        key: 'owned_by',
        label: '供应商',
        render: (row) => {
          // owned_by 是网关的供应商身份（供应商名），不是供应商类型。typeName 拿
          // 名字去查类型表必然查不中，最后回落到 providerLabel(name) —— 于是
          // 「aotera / aotera」在同一格里上下写了两遍。只有真的查到不同的类型
          // 标签时才补这一行。
          const name = String(row.owned_by || '');
          const label = typeName(name);
          return el('div', { class: 'cell-title' },
            el('span', { class: 'mono', text: name || '—' }),
            (label && label !== name) ? el('span', { class: 'cell-sub', text: label }) : null,
          );
        },
      },
      {
        key: 'caps',
        label: '能力',
        render: (row) => {
          const caps = capsOfModel(row);
          return caps.length
            ? el('span', { class: 'tag-row' }, caps.slice(0, 5).map((cap) => badge(capabilityLabel(cap), 'muted')))
            : el('span', { class: 'muted', text: '—' });
        },
      },
      {
        key: 'context_window',
        label: '上下文',
        align: 'right',
        hideOnMobile: true,
        render: (row) => (row.context_window ? fmtCompact(row.context_window) : '—'),
      },
      {
        key: 'max_tokens',
        label: '最大输出',
        align: 'right',
        hideOnMobile: true,
        render: (row) => (row.max_tokens ? fmtCompact(row.max_tokens) : '—'),
      },
      {
        key: 'price',
        label: '价格 / 积分',
        render: (row) => modelPrice(row),
      },
      {
        key: 'copy',
        label: '',
        align: 'right',
        width: '44px',
        render: (row) => el('button', {
          class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '复制模型 ID',
          attrs: { 'aria-label': '复制模型 ID' },
          on: {
            click: async (event) => {
              event.stopPropagation();
              const ok = await copyText(row.id || '');
              if (ok) toastSuccess('模型 ID 已复制');
              else toastWarn('复制失败');
            },
          },
        }, icon('copy', 14)),
      },
    ],
    rows,
    // 首次加载由 load() 的 loadingBlock（动画 + 文案）独占提示；表格在
    // ui.loading=false 之后才渲染，这里不需要再放一份「加载中」。
    empty: '没有符合筛选条件的模型',
  });
  tableHost.replaceChildren(table.el);
}

function modelPrice(model) {
  const bits = [];
  if (model.credits != null) {
    bits.push(el('span', { class: 'mono small', text: model.credit_unit === 'multiplier' ? `×${fmtNum(model.credits, 2)}` : `${fmtNum(model.credits, 2)} 积分 / 1M` }));
  }
  if (model.pricing) {
    if (model.pricing.input != null) bits.push(el('span', { class: 'price-line', text: `入 ${fmtTokenPrice(model.pricing.input)}` }));
    if (model.pricing.output != null) bits.push(el('span', { class: 'price-line', text: `出 ${fmtTokenPrice(model.pricing.output)}` }));
  }
  if (!bits.length && model.is_free) bits.push(el('span', { class: 'text-ok small', text: '免费' }));
  return bits.length ? el('div', { class: 'price-cell' }, bits) : el('span', { class: 'muted', text: '—' });
}
