// requests.js — 请求归档列表 + 详情抽屉 + 运行日志页签。

import { api, describeError } from '../core/api.js';
import { authState, isAuthenticated, openLoginModal } from '../core/auth.js';
import {
  el, card, badge, statusDot, emptyState, loadingBlock, icon, copyText, debounce, isAbortError,
} from '../core/dom.js';
import {
  fmtCompact, fmtDateTime, fmtDuration, fmtInt, fmtRelative, fmtTime, outcomeLabel, prettyJSON, truncate,
} from '../core/format.js';
import { createTable } from '../components/table.js';
import { openDrawer } from '../components/drawer.js';
import { createTabs } from '../components/tabs.js';
import { toast, toastError, toastSuccess, toastWarn } from '../components/toast.js';

const TIME_RANGES = [
  { value: '1', label: '近 1 小时' },
  { value: '6', label: '近 6 小时' },
  { value: '24', label: '近 24 小时' },
  { value: '168', label: '近 7 天' },
  { value: '0', label: '全部归档' },
];
const OUTCOMES = [
  { value: '', label: '全部结果' },
  { value: 'success', label: '成功' },
  { value: 'http_error', label: '上游错误' },
  { value: 'stream_error', label: '流错误' },
  { value: 'interrupted', label: '客户端中断' },
  { value: 'rate_limited', label: '限流' },
  { value: 'model_error', label: '模型错误' },
  { value: 'no_account', label: '无可用账号' },
];

let pageSignal = null;
let rootEl = null;
let fetchSeq = 0;
let timer = 0;
let drawer = null;
let drawerState = null;
let ui = initialState();

function initialState() {
  return {
    loading: true,
    error: '',
    entries: [],
    limit: 200,
    hours: '24',
    outcome: '',
    provider: '',
    account: '',
    model: '',
    clientIp: '',
    autoRefresh: false,
    lastLoaded: 0,
    fetchedAt: 0,
  };
}

export async function mount(root, ctx) {
  pageSignal = new AbortController();
  rootEl = root;
  ui = initialState();
  render();
  await load();
}

export function unmount() {
  if (pageSignal) pageSignal.abort();
  pageSignal = null;
  clearInterval(timer);
  timer = 0;
  if (drawer) drawclose();
  drawer = null;
  drawerState = null;
  rootEl = null;
  ui = initialState();
}

function drawclose() {
  try { drawer?.close(); } catch { /* ignore */ }
}

function toggleTimer() {
  clearInterval(timer);
  timer = 0;
  if (ui.autoRefresh) {
    timer = window.setInterval(() => {
      if (!document.hidden) load({ silent: true });
    }, 10000);
  }
}

async function load({ silent = false } = {}) {
  if (!pageSignal) return;
  const seq = ++fetchSeq;
  ui.loading = !silent;
  if (!silent) render();
  const query = { limit: ui.limit };
  if (ui.hours && ui.hours !== '0') {
    query.from = Math.floor(Date.now() / 1000) - Number(ui.hours) * 3600;
  }
  if (ui.outcome) query.outcome = ui.outcome;
  if (ui.provider) query.provider = ui.provider.trim();
  if (ui.account) query.account = ui.account.trim();
  if (ui.model) query.model = ui.model.trim();
  if (ui.clientIp) query.client_ip = ui.clientIp.trim();
  try {
    const res = await api.requestLogs.get(query, { signal: pageSignal.signal });
    if (!pageSignal || pageSignal.signal.aborted || seq !== fetchSeq) return;
    ui.entries = Array.isArray(res?.entries) ? res.entries : (Array.isArray(res) ? res : []);
    ui.limit = Number(res?.limit) || ui.limit;
    ui.error = '';
    ui.lastLoaded = Date.now();
    ui.fetchedAt = Date.now();
  } catch (error) {
    if (isAbortError(error) || pageSignal?.signal.aborted) return;
    ui.error = describeError(error);
    if (!silent) toastError(error);
  } finally {
    if (seq === fetchSeq) {
      ui.loading = false;
      render();
    }
  }
}

function render() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-requests' });
  const auth = authState();
  const filterRefresh = el('button', {
    class: 'btn btn-primary btn-sm', type: 'button',
    on: { click: () => load() },
  }, icon('search', 14), '查询');
  const reset = el('button', {
    class: 'btn btn-ghost btn-sm', type: 'button',
    on: {
      click: () => {
        ui.outcome = ''; ui.provider = ''; ui.account = ''; ui.model = ''; ui.clientIp = ''; ui.hours = '24';
        render();
        load();
      },
    },
  }, '重置');

  page.appendChild(el('div', { class: 'page-head' },
    el('div', {},
      el('div', { class: 'eyebrow', text: 'REQUEST ARCHIVE' }),
      el('h1', { class: 'page-title', text: '请求' }),
      el('p', { class: 'page-subtitle', text: '归档请求的筛选、详情与运行日志；点击任意行打开抽屉。' }),
    ),
    el('div', { class: 'page-actions' },
      el('label', { class: 'check-inline' },
        el('input', {
          type: 'checkbox',
          checked: ui.autoRefresh,
          on: {
            change: (event) => {
              ui.autoRefresh = event.target.checked;
              toggleTimer();
              toast(ui.autoRefresh ? '已开启每 10 秒自动刷新' : '已关闭自动刷新', { type: 'info', duration: 2000 });
            },
          },
        }),
        el('span', { text: '自动刷新' }),
      ),
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => load() },
      }, icon('refresh', 15), '刷新'),
    ),
  ));

  if (!isAuthenticated()) {
    page.appendChild(el('div', { class: 'notice notice-info' },
      icon('info', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '匿名访客没有请求记录' }),
        el('span', { text: '请求归档按账号归属过滤；登录后查看全部或自己的请求。' }),
      ),
      el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openLoginModal({ message: '登录后查看请求与运行日志。' }) },
      }, '登录'),
    ));
  } else if (auth.role === 'user') {
    page.appendChild(el('div', { class: 'context-strip' },
      badge('个人范围', 'info'),
      el('span', { class: 'context-note', text: '仅显示归属你的账号（provider/label）服务的请求。' }),
    ));
  }

  const filters = el('div', { class: 'filter-bar filter-bar-wrap' },
    el('select', {
      class: 'input select select-sm',
      on: { change: (event) => { ui.hours = event.target.value; load(); } },
    }, TIME_RANGES.map((range) => el('option', { value: range.value, text: range.label, selected: ui.hours === range.value }))),
    el('select', {
      class: 'input select select-sm',
      on: { change: (event) => { ui.outcome = event.target.value; load(); } },
    }, OUTCOMES.map((out) => el('option', { value: out.value, text: out.label, selected: ui.outcome === out.value }))),
    textFilter('供应商', ui.provider, (value) => { ui.provider = value; }, filterRefresh),
    textFilter('账号', ui.account, (value) => { ui.account = value; }, filterRefresh),
    textFilter('模型', ui.model, (value) => { ui.model = value; }, filterRefresh),
    textFilter('客户端 IP', ui.clientIp, (value) => { ui.clientIp = value; }, filterRefresh),
    el('select', {
      class: 'input select select-sm',
      on: { change: (event) => { ui.limit = Number(event.target.value); load(); } },
    }, [100, 200, 500, 1000].map((limit) => el('option', { value: String(limit), text: `最多 ${limit} 条`, selected: ui.limit === limit }))),
    filterRefresh,
    reset,
  );
  page.appendChild(filters);

  if (ui.error) {
    page.appendChild(el('div', { class: 'notice notice-warn' },
      icon('alert', 16),
      el('div', { class: 'notice-content' }, el('strong', { text: '请求列表加载失败' }), el('span', { text: ui.error })),
      el('button', { class: 'btn btn-ghost btn-sm', type: 'button', on: { click: () => load() } }, '重试'),
    ));
  }

  if (ui.entries.length) {
    const totals = tokenTotals(ui.entries);
    page.appendChild(el('div', { class: 'request-summary' },
      el('span', { class: 'request-summary-label', text: `当前列表 ${fmtInt(ui.entries.length)} 条` }),
      el('span', {
        class: 'request-summary-tokens mono',
        text: `入 ${fmtCompact(totals.prompt)} · 出 ${fmtCompact(totals.completion)} · 共 ${fmtCompact(totals.total)} tokens`,
        title: `Prompt ${fmtInt(totals.prompt)} · Completion ${fmtInt(totals.completion)} · Total ${fmtInt(totals.total)}`,
      }),
      el('a', { class: 'request-summary-link', href: '#/usage' }, '完整用量 →'),
    ));
  }

  const tableHost = el('div', { class: 'table-card request-table-card' });
  if (ui.loading && !ui.entries.length) {
    tableHost.appendChild(loadingBlock('加载请求归档'));
  } else {
    const table = createTable({
      columns: [
        {
          key: 'time',
          label: '时间',
          width: '160px',
          render: (row) => el('div', { class: 'cell-title' },
            el('span', { class: 'mono small', text: fmtTime(row.time) }),
            el('span', { class: 'cell-sub', text: fmtRelative(row.time) }),
          ),
        },
        {
          key: 'result',
          label: '结果',
          width: '132px',
          render: (row) => el('div', { class: 'result-cell' },
            badge(row.status || '—', row.ok ? 'ok' : (row.status === 429 ? 'warn' : 'danger')),
            el('span', { class: 'cell-sub', text: outcomeLabel(row.outcome) }),
            rowTokenSummary(row)
              ? el('span', {
                class: 'cell-sub token-sub mobile-only',
                text: rowTokenSummary(row),
                title: rowTokenTitle(row),
              })
              : null,
          ),
        },
        {
          key: 'provider',
          label: '供应商 / 账号',
          render: (row) => el('div', { class: 'cell-title' },
            el('span', { class: 'mono', text: row.provider || '—' }),
            el('span', { class: 'cell-sub', text: row.account || '供应商密钥' }),
          ),
        },
        {
          key: 'model',
          label: '模型',
          render: (row) => el('span', { class: 'mono truncate', text: row.model || '—', title: row.model || '' }),
        },
        {
          key: 'duration_ms',
          label: '耗时',
          align: 'right',
          hideOnMobile: true,
          render: (row) => fmtDuration(row.duration_ms),
        },
        {
          key: 'tokens',
          label: 'Tokens',
          align: 'right',
          hideOnMobile: true,
          render: (row) => (row.total_tokens
            ? el('span', { text: fmtInt(row.total_tokens), title: rowTokenTitle(row) })
            : '—'),
        },
        {
          key: 'client_ip',
          label: '来源 IP',
          align: 'right',
          hideOnMobile: true,
          render: (row) => el('span', { class: 'mono small', text: row.client_ip || '—' }),
        },
        {
          key: 'path',
          label: '路径',
          hideOnMobile: true,
          render: (row) => el('span', { class: 'mono small muted', text: row.path || '—' }),
        },
      ],
      rows: ui.entries,
      empty: isAuthenticated() ? '没有符合筛选条件的请求' : '登录后查看请求归档',
      onRowClick: (row) => openRequestDrawer(row),
    });
    tableHost.appendChild(table.el);
  }
  page.appendChild(tableHost);
  page.appendChild(el('div', { class: 'page-footer-note' },
    el('span', { text: `共 ${fmtInt(ui.entries.length)} 条可见记录` }),
    ui.lastLoaded ? el('span', { text: `更新于 ${fmtTime(ui.lastLoaded)}` }) : null,
    el('span', { text: '归档上限 1000 条/次；筛选为包含匹配。' }),
  ));
  rootEl.replaceChildren(page);
}

function tokenTotals(entries) {
  const totals = { prompt: 0, completion: 0, total: 0 };
  for (const row of entries) {
    const prompt = Number(row.prompt_tokens || 0);
    const completion = Number(row.completion_tokens || 0);
    const total = Number(row.total_tokens || 0) || (prompt + completion);
    totals.prompt += prompt;
    totals.completion += completion;
    totals.total += total;
  }
  return totals;
}

function rowTokenSummary(row) {
  const prompt = Number(row.prompt_tokens || 0);
  const completion = Number(row.completion_tokens || 0);
  const total = Number(row.total_tokens || 0) || (prompt + completion);
  if (!total) return '';
  return `↑${fmtCompact(prompt)} ↓${fmtCompact(completion)}`;
}

function rowTokenTitle(row) {
  const prompt = Number(row.prompt_tokens || 0);
  const completion = Number(row.completion_tokens || 0);
  const total = Number(row.total_tokens || 0) || (prompt + completion);
  return `Prompt ${fmtInt(prompt)} · Completion ${fmtInt(completion)} · Total ${fmtInt(total)}`;
}

function detailTokenStat(label, value) {
  return el('div', { class: 'detail-token-stat' },
    el('span', { class: 'detail-token-label', text: label }),
    el('span', { class: 'detail-token-value mono', text: fmtInt(value || 0) }),
  );
}

function textFilter(placeholder, value, onChange) {
  const input = el('input', {
    class: 'input input-sm filter-input',
    type: 'search',
    placeholder,
    value,
    autocomplete: 'off',
  });
  // 输入时只更新状态，不立刻重渲染（避免输入焦点在防抖后丢失）。
  input.addEventListener('input', () => onChange(input.value.trim()));
  input.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      onChange(input.value.trim());
      load();
    }
  });
  return input;
}

// ---------------------------------------------------------------------------
// 详情抽屉
// ---------------------------------------------------------------------------

function openRequestDrawer(row) {
  drawerState = {
    row,
    tab: 'detail',
    logs: [],
    logsLoading: false,
    logsError: '',
    logsLoaded: false,
    relatedOnly: true,
    logQuery: '',
  };
  const tabs = createTabs({
    items: [
      { id: 'detail', label: '详情' },
      { id: 'logs', label: '运行日志' },
    ],
    value: 'detail',
    ariaLabel: '请求详情页签',
    onChange: (value) => {
      if (!drawerState) return;
      drawerState.tab = value;
      renderDrawerBody();
      if (value === 'logs' && !drawerState.logsLoaded) loadLogs();
    },
  });
  drawer = openDrawer({
    title: '请求详情',
    subtitle: `${row.request_id || '无 request id'} · ${fmtDateTime(row.time, { seconds: true })}`,
    size: 'lg',
    body: el('div', { class: 'drawer-tab-body' }, tabs.el, el('div', { class: 'drawer-tab-content' })),
    onClose: () => {
      drawer = null;
      drawerState = null;
    },
  });
  renderDrawerBody();
}

function renderDrawerBody() {
  if (!drawer || !drawerState) return;
  const host = drawer.body.querySelector('.drawer-tab-content');
  if (!host) return;
  host.replaceChildren(drawerState.tab === 'logs' ? renderLogsPanel() : renderDetailPanel());
}

function renderDetailPanel() {
  const row = drawerState.row;
  const facts = [
    ['请求 ID', row.request_id || '—'],
    ['时间', fmtDateTime(row.time, { seconds: true })],
    ['路径', row.path || '—'],
    ['供应商', row.provider || '—'],
    ['账号', row.account || '供应商密钥'],
    ['模型', row.model || '—'],
    ['调度域', row.realm || '—'],
    ['HTTP 状态', row.status != null ? String(row.status) : '—'],
    ['结果', outcomeLabel(row.outcome)],
    ['耗时', fmtDuration(row.duration_ms)],
    ['TTFB', row.ttfb_ms != null ? fmtDuration(row.ttfb_ms) : '—'],
    ['尝试次数', row.attempts != null ? fmtInt(row.attempts) : '—'],
    ['Prompt tokens', fmtInt(row.prompt_tokens || 0)],
    ['Completion tokens', fmtInt(row.completion_tokens || 0)],
    ['Total tokens', fmtInt(row.total_tokens || 0)],
    ['积分', row.credit_known ? `${row.credit ?? 0}` : '未知'],
    ['来源 IP', row.client_ip || '—'],
    ['User-Agent', row.user_agent || '—'],
    ['推理档位', row.reasoning_effort || (row.reasoning ? '已启用' : '未启用')],
    ['推理 tokens', row.reasoning_tokens ? fmtInt(row.reasoning_tokens) : '—'],
  ];
  const panel = el('div', { class: 'drawer-detail' },
    el('div', { class: 'drawer-status-row' },
      badge(row.status || '—', row.ok ? 'ok' : (row.status === 429 ? 'warn' : 'danger')),
      statusDot(row.ok ? 'ok' : 'danger', row.ok ? '请求成功' : '请求失败'),
      el('span', { class: 'muted small', text: row.account ? `由 ${row.provider}/${row.account} 服务` : `由 ${row.provider || '未知供应商'} 的供应商密钥服务` }),
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: async () => {
          const ok = await copyText(row.request_id || '');
          if (ok) toastSuccess('Request ID 已复制');
          else toastWarn('复制失败');
        } },
      }, icon('copy', 14), '复制 ID'),
    ),
    row.error ? el('div', { class: 'drawer-error' }, icon('alert', 15), el('span', { text: row.error })) : null,
    el('div', { class: 'detail-token-stats' },
      detailTokenStat('输入 Prompt', row.prompt_tokens),
      detailTokenStat('输出 Completion', row.completion_tokens),
      detailTokenStat('合计 Total', row.total_tokens || (Number(row.prompt_tokens || 0) + Number(row.completion_tokens || 0))),
    ),
    el('div', { class: 'detail-facts' }, facts.map(([label, value]) => el('div', { class: 'detail-fact' },
      el('span', { class: 'fact-label', text: label }),
      el('span', { class: 'fact-value mono', text: value, title: String(value) }),
    ))),
    el('details', { class: 'raw-config', open: true },
      el('summary', { text: '原始事件 JSON' }),
      el('pre', { class: 'code-block', text: prettyJSON(row) }),
    ),
  );
  return panel;
}

function renderLogsPanel() {
  const state = drawerState;
  const row = state.row;
  const search = el('input', {
    class: 'input input-sm', type: 'search', placeholder: '过滤日志文本…', value: state.logQuery,
  });
  search.addEventListener('input', debounce(() => {
    state.logQuery = search.value.trim();
    refreshLogList();
  }, 140));
  const relatedToggle = el('input', { type: 'checkbox', checked: state.relatedOnly });
  relatedToggle.addEventListener('change', () => {
    state.relatedOnly = relatedToggle.checked;
    refreshLogList();
  });
  const listHost = el('div', { class: 'log-list' });
  const panel = el('div', { class: 'drawer-logs' },
    el('div', { class: 'log-toolbar' },
      el('label', { class: 'check-inline' }, relatedToggle, el('span', { text: '只看相关' })),
      el('div', { class: 'log-search' }, icon('search', 14), search),
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => loadLogs() },
      }, icon('refresh', 14), '刷新日志'),
    ),
    state.logsError ? el('p', { class: 'form-error', text: state.logsError }) : null,
    state.logsLoading ? loadingBlock('加载运行日志') : listHost,
    el('div', { class: 'section-note' },
      el('span', { text: '日志来自服务端环形缓冲；普通用户只保留与自己账号相关的行。' }),
    ),
  );
  const refreshLogList = () => {
    listHost.replaceChildren();
    if (state.logsLoading) return;
    let entries = state.logs.slice();
    if (state.relatedOnly) {
      const needles = [row.provider, row.account, row.model, row.request_id].filter(Boolean);
      entries = entries.filter((entry) => needles.some((needle) => String(entry.text || '').includes(needle)));
    }
    if (state.logQuery) {
      entries = entries.filter((entry) => String(entry.text || '').toLowerCase().includes(state.logQuery.toLowerCase()));
    }
    if (!entries.length) {
      listHost.appendChild(el('div', { class: 'log-empty' },
        icon('info', 16),
        el('span', { text: state.logsLoaded ? '没有匹配的运行日志' : '尚未加载日志' }),
        !state.logsLoaded ? el('button', { class: 'btn btn-ghost btn-sm', type: 'button', on: { click: () => loadLogs() } }, '加载') : null,
      ));
      return;
    }
    let lastChannel = '';
    for (const entry of entries.slice().reverse()) {
      if (entry.channel !== lastChannel) {
        lastChannel = entry.channel;
      }
      listHost.appendChild(el('div', { class: `log-row log-${entry.channel || 'sys'}` },
        el('span', { class: 'log-time mono', text: fmtTime(entry.time) }),
        badge(channelLabel(entry.channel), entry.channel === 'chat' ? 'info' : (entry.channel === 'task' ? 'accent' : 'muted')),
        el('span', { class: 'log-text mono', text: entry.text || '' }),
      ));
    }
  };
  refreshLogList();
  panel.__refreshList = refreshLogList;
  // 首次打开日志页签时加载。
  if (!state.logsLoaded && !state.logsLoading) {
    Promise.resolve().then(() => loadLogs());
  }
  return panel;
}

function channelLabel(channel) {
  return channel === 'chat' ? '请求' : (channel === 'task' ? '任务' : '系统');
}

async function loadLogs() {
  if (!drawerState || !pageSignal) return;
  drawerState.logsLoading = true;
  drawerState.logsError = '';
  renderDrawerBody();
  try {
    const res = await api.logs.get({}, { signal: pageSignal.signal });
    if (!drawerState) return;
    drawerState.logs = Array.isArray(res?.entries) ? res.entries : (Array.isArray(res) ? res : []);
    drawerState.logsLoaded = true;
  } catch (error) {
    if (isAbortError(error)) return;
    if (!drawerState) return;
    drawerState.logsError = describeError(error);
  } finally {
    if (drawerState) {
      // 无论成功失败都标记为已尝试，避免失败时自动重试造成请求风暴。
      drawerState.logsLoaded = true;
      drawerState.logsLoading = false;
      renderDrawerBody();
    }
  }
}
