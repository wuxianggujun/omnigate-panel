// requests.js — 请求归档列表 + 详情抽屉 + 运行日志页签。

import { api, describeError } from '../core/api.js';
import { authState, isAuthenticated, openLoginModal } from '../core/auth.js';
import {
  el, badge, statusDot, loadingBlock, icon, copyText, debounce, isAbortError,
} from '../core/dom.js';
import {
  fmtCompact, fmtDateTime, fmtDuration, fmtInt, fmtRelative, fmtTime, outcomeLabel, prettyJSON,
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
const AUTO_REFRESH_OPTIONS = [
  { value: 0, label: '关闭' },
  { value: 10, label: '10 秒' },
  { value: 30, label: '30 秒' },
  { value: 60, label: '1 分钟' },
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
    userAgent: '',
    advancedFiltersOpen: false,
    autoRefreshSec: 0,
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
  if (ui.autoRefreshSec > 0) {
    timer = window.setInterval(() => {
      if (!document.hidden) load({ silent: true });
    }, ui.autoRefreshSec * 1000);
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
  if (ui.userAgent) query.user_agent = ui.userAgent.trim();
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
        ui.outcome = ''; ui.provider = ''; ui.account = ''; ui.model = ''; ui.clientIp = ''; ui.userAgent = ''; ui.hours = '24';
        ui.advancedFiltersOpen = false;
        render();
        load();
      },
    },
  }, '重置');
  const advancedCount = [ui.provider, ui.account, ui.model, ui.userAgent, ui.clientIp].filter(Boolean).length;
  const advancedFilters = el('div', {
    class: `request-filter-advanced ${ui.advancedFiltersOpen ? 'is-open' : ''}`.trim(),
  },
    textFilter('供应商', ui.provider, (value) => { ui.provider = value; }, filterRefresh),
    textFilter('账号', ui.account, (value) => { ui.account = value; }, filterRefresh),
    textFilter('模型', ui.model, (value) => { ui.model = value; }, filterRefresh),
    textFilter('客户端 UA', ui.userAgent, (value) => { ui.userAgent = value; }, filterRefresh),
    textFilter('来源 IP', ui.clientIp, (value) => { ui.clientIp = value; }, filterRefresh),
  );
  const moreFilters = el('button', {
    class: 'btn btn-ghost btn-sm request-filter-more', type: 'button',
    attrs: { 'aria-expanded': ui.advancedFiltersOpen ? 'true' : 'false' },
    on: {
      click: () => {
        ui.advancedFiltersOpen = advancedFilters.classList.toggle('is-open');
        moreFilters.setAttribute('aria-expanded', ui.advancedFiltersOpen ? 'true' : 'false');
        moreFilters.replaceChildren(icon('filter', 14), ui.advancedFiltersOpen ? '收起筛选' : `更多筛选${advancedCount ? ` · ${advancedCount}` : ''}`);
      },
    },
  }, icon('filter', 14), ui.advancedFiltersOpen ? '收起筛选' : `更多筛选${advancedCount ? ` · ${advancedCount}` : ''}`);

  // 页面标题与范围说明都不单独占一行：标题由顶栏常驻，范围并入下方列表工具条
  // 的摘要行，避免「范围条 + 筛选条 + 工具条」三条横条上下叠。
  //
  // 匿名访客的归档恒为空，下面那套筛选器、自动刷新与表格无论怎么点都只会
  // 得到同一句空提示。与其摆一屏无效控件，不如只说清「为什么看不到」。
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
    rootEl.replaceChildren(page);
    return;
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
    el('select', {
      class: 'input select select-sm',
      on: { change: (event) => { ui.limit = Number(event.target.value); load(); } },
    }, [100, 200, 500, 1000].map((limit) => el('option', { value: String(limit), text: `最多 ${limit} 条`, selected: ui.limit === limit }))),
    advancedFilters,
    moreFilters,
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

  const totals = tokenTotals(ui.entries);
  const liveDot = el('span', {
    class: `live-dot request-live-dot ${ui.autoRefreshSec ? 'is-active' : ''}`,
    attrs: { 'aria-hidden': 'true' },
  });
  const autoSelect = el('select', {
    class: 'input select select-sm request-auto-select',
    attrs: { 'aria-label': '自动刷新间隔' },
    on: {
      change: (event) => {
        ui.autoRefreshSec = Number(event.target.value) || 0;
        toggleTimer();
        liveDot.classList.toggle('is-active', ui.autoRefreshSec > 0);
        toast(ui.autoRefreshSec ? `已开启每 ${ui.autoRefreshSec} 秒自动刷新` : '已关闭自动刷新', { type: 'info', duration: 2000 });
      },
    },
  }, AUTO_REFRESH_OPTIONS.map((option) => el('option', {
    value: String(option.value),
    text: option.label,
    selected: ui.autoRefreshSec === option.value,
  })));
  const summaryText = ui.entries.length
    ? `入 ${fmtCompact(totals.prompt)} · 出 ${fmtCompact(totals.completion)} · 共 ${fmtCompact(totals.total)} tokens${totals.cacheTotal ? ` · 缓存 ${fmtCacheRate(totals.cacheRate)}` : ''}`
    // 表格自身有 loadingBlock/骨架提示；加载中不再在工具栏重复写状态文案。
    : (ui.loading ? '' : '暂无记录');
  const summaryTitle = ui.entries.length
    ? `Prompt ${fmtInt(totals.prompt)} · Completion ${fmtInt(totals.completion)} · Total ${fmtInt(totals.total)}${totals.cacheTotal ? ` · 缓存命中 ${fmtInt(totals.cacheHit)} / ${fmtInt(totals.cacheTotal)}（${fmtCacheRate(totals.cacheRate)}）` : ' · 缓存明细未上报'}`
    : '当前列表合计';

  page.appendChild(el('div', { class: 'request-toolbar' },
    el('div', { class: 'request-toolbar-summary' },
      el('span', { class: 'request-toolbar-label', text: ui.entries.length ? `当前列表 ${fmtInt(ui.entries.length)} 条` : '当前列表' }),
      el('span', { class: 'request-summary-tokens mono', text: summaryText, title: summaryTitle }),
      // 普通用户的范围说明放在这里：它解释的正是「当前列表」为什么只有这些记录。
      auth.role === 'user'
        ? el('span', { class: 'request-scope-note', title: '按账号归属过滤：只看归属你的账号服务的请求' },
          badge('个人范围', 'info'),
          el('span', { text: 'provider/label 归属你' }),
        )
        : null,
      ui.entries.length ? el('a', { class: 'request-summary-link', href: '#/usage' }, '完整用量 →') : null,
    ),
    el('div', { class: 'request-toolbar-actions' },
      el('label', { class: 'request-auto-control', title: '自动刷新只更新当前筛选结果，不会改变筛选条件' },
        liveDot,
        el('span', { class: 'request-auto-label', text: '自动刷新' }),
        autoSelect,
      ),
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        title: '立即刷新当前筛选结果',
        on: { click: () => load() },
      }, icon('refresh', 15), '刷新'),
    ),
  ));

  const tableHost = el('div', { class: 'table-card request-table-card request-table-desktop' });
  if (ui.loading && !ui.entries.length) {
    tableHost.appendChild(loadingBlock('加载请求归档'));
  } else {
    const table = createTable({
      caption: '请求归档',
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
          width: '118px',
          render: (row) => el('div', { class: 'result-cell' },
            badge(row.status || '—', row.ok ? 'ok' : (row.status === 429 ? 'warn' : 'danger')),
            // 失败原因挂在结果列的 title 上：列表里不必展开，但一眼能把「客户端
            // 主动取消」和「被读超时掐断」区分开。
            el('span', { class: 'cell-sub', text: outcomeLabel(row.outcome), title: row.failure || '' }),
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
          label: '模型 / 重定向',
          render: (row) => {
            const requested = String(row.model || '');
            const info = redirectInfo(row);
            // 「请求名 → 实际上游模型」是这一列的核心信息。只有模型本体真的被改写
            // 才显示第二行（仅补 provider 前缀不算改写）。
            if (!info) {
              return el('span', { class: 'mono truncate', text: requested || '—', title: requested || '' });
            }
            return el('div', { class: 'cell-title' },
              el('span', { class: 'mono truncate', text: requested || '—', title: `客户端请求：${requested}` }),
              el('span', {
                class: 'redirect-line mono truncate',
                title: `实际发往上游：${info.upstream}`,
                text: `↳ ${info.upstream}`,
              }),
            );
          },
        },
        {
          key: 'source',
          label: '来源 / 终端',
          width: '142px',
          render: (row) => {
            const client = clientInfo(row.user_agent);
            return el('div', { class: 'cell-title source-cell', title: client.full || '未记录 User-Agent' },
              el('span', { class: 'source-client', text: client.label }),
              el('span', { class: 'cell-sub mono', text: row.client_ip || '—' }),
            );
          },
        },
        {
          key: 'duration_ms',
          label: '耗时',
          align: 'right',
          width: '82px',
          render: (row) => fmtDuration(row.duration_ms),
        },
        {
          key: 'tokens',
          label: 'Tokens',
          align: 'right',
          width: '138px',
          render: (row) => {
            const tokens = tokenBreakdown(row);
            if (!tokens.total) return el('span', { class: 'muted small', text: '—' });
            return el('div', { class: 'token-cell', title: rowTokenTitle(row) },
              el('span', { class: 'token-total mono', text: fmtInt(tokens.total) }),
              tokens.hasCache
                ? el('span', { class: `cache-pill cache-${cacheTone(tokens.cacheRate)}`, text: `缓存 ${fmtCacheRate(tokens.cacheRate)}` })
                : el('span', { class: 'cache-pill cache-muted', text: '缓存 —' }),
            );
          },
        },
        {
          key: 'path',
          label: '路径',
          render: (row) => el('span', { class: 'mono small muted', text: row.path || '—' }),
        },
        {
          key: 'open',
          label: '',
          width: '34px',
          align: 'right',
          render: () => el('span', { class: 'row-open', title: '查看详情', attrs: { 'aria-hidden': 'true' } }, icon('chevron', 14)),
        },
      ],
      rows: ui.entries,
      empty: isAuthenticated() ? '没有符合筛选条件的请求' : '登录后查看请求归档',
      onRowClick: (row) => openRequestDrawer(row),
    });
    tableHost.appendChild(table.el);
  }
  page.appendChild(tableHost);

  const cardsHost = el('div', { class: 'request-card-list request-table-mobile' });
  if (ui.loading && !ui.entries.length) {
    cardsHost.appendChild(el('div', { class: 'table-card request-card-state' }, loadingBlock('加载请求归档')));
  } else if (!ui.entries.length) {
    cardsHost.appendChild(el('div', { class: 'table-card request-card-state' },
      el('div', { class: 'table-empty' }, icon('info', 16), isAuthenticated() ? '没有符合筛选条件的请求' : '登录后查看请求归档'),
    ));
  } else {
    for (const row of ui.entries) cardsHost.appendChild(renderRequestCard(row));
  }
  page.appendChild(cardsHost);

  page.appendChild(el('div', { class: 'page-footer-note' },
    // 加载中不报条数：0 条是结论，「还没读完」不是。表格区已有加载动画，
    // 这里直接省略那条，不再叠一行「正在读取」。
    (ui.loading && !ui.entries.length)
      ? null
      : el('span', { text: `共 ${fmtInt(ui.entries.length)} 条可见记录` }),
    ui.lastLoaded ? el('span', { text: `更新于 ${fmtTime(ui.lastLoaded)}` }) : null,
    el('span', { text: '列表最多 1000 条/次，完整日用量见「用量」页；点击记录可查看终端与缓存明细。' }),
  ));
  rootEl.replaceChildren(page);
}

function tokenBreakdown(row) {
  const prompt = Number(row.prompt_tokens || 0);
  const completion = Number(row.completion_tokens || 0);
  const total = Number(row.total_tokens || 0) || (prompt + completion);
  const cacheHit = Number(row.cache_hit_tokens || 0);
  const cacheMiss = Number(row.cache_miss_tokens || 0);
  const cacheTotal = cacheHit + cacheMiss;
  return {
    prompt,
    completion,
    total,
    cacheHit,
    cacheMiss,
    cacheTotal,
    cacheRate: cacheTotal > 0 ? (cacheHit / cacheTotal) * 100 : 0,
    hasCache: cacheTotal > 0,
  };
}

function tokenTotals(entries) {
  const totals = { prompt: 0, completion: 0, total: 0, cacheHit: 0, cacheMiss: 0, cacheTotal: 0, cacheRate: 0 };
  for (const row of entries) {
    const tokens = tokenBreakdown(row);
    totals.prompt += tokens.prompt;
    totals.completion += tokens.completion;
    totals.total += tokens.total;
    totals.cacheHit += tokens.cacheHit;
    totals.cacheMiss += tokens.cacheMiss;
  }
  totals.cacheTotal = totals.cacheHit + totals.cacheMiss;
  totals.cacheRate = totals.cacheTotal > 0 ? (totals.cacheHit / totals.cacheTotal) * 100 : 0;
  return totals;
}

function rowTokenTitle(row) {
  const tokens = tokenBreakdown(row);
  const parts = [
    `Prompt ${fmtInt(tokens.prompt)}`,
    `Completion ${fmtInt(tokens.completion)}`,
    `Total ${fmtInt(tokens.total)}`,
  ];
  if (tokens.hasCache) {
    parts.push(`缓存命中 ${fmtInt(tokens.cacheHit)}（${fmtCacheRate(tokens.cacheRate)}）`);
    parts.push(`缓存未命中 ${fmtInt(tokens.cacheMiss)}`);
  } else {
    parts.push('缓存明细未上报');
  }
  return parts.join(' · ');
}

function cacheTitle(row) {
  const tokens = tokenBreakdown(row);
  if (!tokens.hasCache) return '上游未上报缓存 token 明细';
  return `缓存命中 ${fmtInt(tokens.cacheHit)} / ${fmtInt(tokens.cacheTotal)} tokens（${fmtCacheRate(tokens.cacheRate)}）· 未命中 ${fmtInt(tokens.cacheMiss)}`;
}

function cacheTone(rate) {
  if (rate >= 80) return 'ok';
  if (rate >= 40) return 'warn';
  return 'danger';
}

// 缓存命中率向下取整到 0.1%，避免 99.95% 被四舍五入成看似完美的 100%。
function fmtCacheRate(rate) {
  if (!Number.isFinite(rate)) return '—';
  return `${Math.floor(rate * 10) / 10}%`;
}

function clientInfo(userAgent) {
  const ua = String(userAgent || '').trim();
  if (!ua) return { label: '未知终端', platform: '未记录', full: '' };
  let platform = '未知平台';
  if (/Android/i.test(ua)) platform = 'Android';
  else if (/iPhone|iPad|iPod|iOS/i.test(ua)) platform = 'iOS';
  else if (/Windows/i.test(ua)) platform = 'Windows';
  else if (/Macintosh|Mac OS X/i.test(ua)) platform = 'macOS';
  else if (/Linux/i.test(ua)) platform = 'Linux';
  else if (/Go-http-client/i.test(ua)) platform = '服务端';
  else if (/curl/i.test(ua)) platform = '命令行';
  let label = '';
  if (/Go-http-client/i.test(ua)) label = 'Go HTTP';
  else if (/NewAPI|new-api/i.test(ua)) label = 'New API';
  else if (/curl/i.test(ua)) label = 'curl';
  else if (/PostmanRuntime/i.test(ua)) label = 'Postman';
  else if (/openai-python|python-requests|python-httpx|httpx|aiohttp|OpenAI\/Python/i.test(ua)) label = 'Python SDK';
  else if (/openai-node|OpenAI\/Node|node-fetch|undici|axios|Node\.js/i.test(ua)) label = 'Node.js SDK';
  else if (/okhttp|Android/i.test(ua)) label = 'Android App';
  else if (/dart|flutter/i.test(ua)) label = 'Flutter';
  else if (/Java\/|Apache-HttpClient/i.test(ua)) label = 'Java';
  else if (/Mozilla|Chrome|Safari|Firefox|Edg\//i.test(ua)) label = '浏览器';
  else label = (ua.split(/[ /]/)[0] || '其他终端').slice(0, 24);
  return { label, platform, full: ua };
}

function tokenChip(text, title, className = '') {
  return el('span', { class: `token-chip ${className}`.trim(), text, title });
}

function renderRequestCard(row) {
  const tokens = tokenBreakdown(row);
  const client = clientInfo(row.user_agent);
  const statusTone = row.ok ? 'ok' : (row.status === 429 ? 'warn' : 'danger');
  const tokenChips = tokens.total
    ? [
      tokenChip(`↑ ${fmtCompact(tokens.prompt)}`, `Prompt ${fmtInt(tokens.prompt)} tokens`),
      tokenChip(`↓ ${fmtCompact(tokens.completion)}`, `Completion ${fmtInt(tokens.completion)} tokens`),
      tokens.hasCache
        ? tokenChip(`缓存 ${fmtCacheRate(tokens.cacheRate)}`, cacheTitle(row), `cache-${cacheTone(tokens.cacheRate)}`)
        : tokenChip('缓存 —', '上游未上报缓存 token 明细', 'cache-muted'),
    ]
    : el('span', { class: 'request-card-no-token', text: '无 token 记录' });
  return el('button', {
    class: 'request-card',
    type: 'button',
    attrs: { 'aria-label': `查看请求详情：${row.model || '未知模型'}，${fmtTime(row.time)}` },
    on: { click: () => openRequestDrawer(row) },
  },
    el('span', { class: 'request-card-head' },
      el('span', { class: 'request-card-time mono', text: fmtTime(row.time) }),
      el('span', { class: 'request-card-relative', text: fmtRelative(row.time) }),
      el('span', { class: 'request-card-status' },
        badge(row.status || '—', statusTone),
        el('span', { class: 'request-card-outcome', text: outcomeLabel(row.outcome) }),
      ),
    ),
    el('span', { class: 'request-card-model mono', text: row.model || '—', title: row.model || '' }),
    el('span', { class: 'request-card-route' },
      el('span', { text: row.provider || '未知供应商' }),
      el('span', { class: 'request-card-sep', text: '·' }),
      el('span', { text: row.account || '供应商密钥' }),
    ),
    el('span', { class: 'request-card-tokens' }, tokenChips),
    el('span', { class: 'request-card-foot' },
      el('span', { class: 'request-card-source', title: client.full || '未记录 User-Agent' },
        icon('server', 12),
        el('span', { text: client.label }),
        el('span', { class: 'request-card-ip mono', text: row.client_ip || '—' }),
      ),
      el('span', { class: 'request-card-duration mono', text: fmtDuration(row.duration_ms) }),
      el('span', { class: 'request-card-open' }, '详情', icon('chevron', 13)),
    ),
  );
}

function detailTokenStat(label, value, meta = '') {
  const display = typeof value === 'number' ? fmtInt(value) : String(value ?? '—');
  return el('div', { class: 'detail-token-stat' },
    el('span', { class: 'detail-token-label', text: label }),
    el('span', { class: 'detail-token-value mono', text: display }),
    meta ? el('span', { class: 'detail-token-meta', text: meta, title: meta }) : null,
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

// redirectInfo 把「客户端请求的模型」与「真正发往上游的模型」整理成可展示的
// 重定向信息。只有**模型本身**变了才算改写：全局别名、目录补全（短名补成上游
// 真实 ID）、或回退换供应商。
//
// 比较前必须先剥掉 provider 前缀再比，否则会把「仅补全前缀」误报成改写——
// 请求名 "justwoker/o3" 与上游 "o3" 指的是同一个模型，全列标成「已改写」等于
// 噪声淹没信号（实测过）。没有 upstream_model 的旧记录返回 null。
function redirectInfo(row) {
  const requested = String(row?.model || '');
  const upstream = String(row?.upstream_model || '');
  if (!upstream) return null;
  const provider = String(row?.provider || '');
  const stripPrefix = (value) => {
    if (!value) return '';
    if (provider && value.startsWith(`${provider}/`)) return value.slice(provider.length + 1);
    const slash = value.indexOf('/');
    return slash > 0 ? value.slice(slash + 1) : value;
  };
  // 模型本体没变就不是重定向，哪怕前缀补全了。
  if (stripPrefix(upstream) === stripPrefix(requested)) return null;
  const qualified = upstream.includes('/') ? upstream : (provider ? `${provider}/${upstream}` : upstream);
  return { requested, upstream: qualified };
}

// redirectText 是 redirectInfo 的纯文本形态（detail-facts 只放纯文本）。
function redirectText(row) {
  const info = redirectInfo(row);
  if (!info) return String(row?.upstream_model || '—');
  return `${info.requested} → ${info.upstream}`;
}

function redirectBlock(row) {
  const info = redirectInfo(row);
  if (!info) return null;
  return el('div', { class: 'redirect-block' },
    el('div', { class: 'redirect-head' },
      el('span', { class: 'redirect-title' }, icon('arrowRight', 14), '路由重定向'),
      badge('已改写', 'warn'),
    ),
    el('div', { class: 'redirect-flow' },
      el('div', { class: 'redirect-node' },
        el('span', { class: 'redirect-label', text: '客户端请求' }),
        el('span', { class: 'redirect-value mono', text: info.requested, title: info.requested }),
      ),
      el('span', { class: 'redirect-arrow', 'aria-hidden': 'true' }, icon('arrowRight', 16)),
      el('div', { class: 'redirect-node target' },
        el('span', { class: 'redirect-label', text: '实际发往上游' }),
        el('span', { class: 'redirect-value mono', text: info.upstream, title: info.upstream }),
      ),
    ),
    el('p', { class: 'redirect-note', text: '模型名不等于请求名，说明路由层改写过：全局别名、目录补全或回退换供应商各其一。' }),
  );
}

function renderDetailPanel() {
  const row = drawerState.row;
  const tokens = tokenBreakdown(row);
  const client = clientInfo(row.user_agent);
  const facts = [
    ['请求 ID', row.request_id || '—'],
    ['时间', fmtDateTime(row.time, { seconds: true })],
    ['路径', row.path || '—'],
    ['供应商', row.provider || '—'],
    ['账号', row.account || '供应商密钥'],
    // 入站身份：与「账号」是两条正交的账——账号是上游谁在服务，用户是这把密钥
    // 属于谁。新请求才有这一列，旧归档为空。
    ...(row.api_key ? [['用户', row.api_key]] : []),
    ['模型', row.model || '—'],
    // 重定向：把「请求了什么」和「实际打到哪个上游模型」并排显示。两者不同就是
    // 别名 / 目录补全 / 回退换供应商其中之一生效了——以前这条信息完全不可见，
    // 只能靠猜。
    ['实际模型', redirectText(row)],
    ['调度域', row.realm || '—'],
    ['HTTP 状态', row.status != null ? String(row.status) : '—'],
    ['结果', outcomeLabel(row.outcome)],
    // 失败原因：以前只有 outcome，没有原因——「客户端主动取消」和「中间层按读
    // 超时掐断」在记录里长得一样，线上排查只能靠猜。现在由网关回填具体成因。
    ...(row.failure ? [['失败原因', row.failure]] : []),
    ['耗时', fmtDuration(row.duration_ms)],
    ['TTFB', row.ttfb_ms != null ? fmtDuration(row.ttfb_ms) : '—'],
    ['尝试次数', row.attempts != null ? fmtInt(row.attempts) : '—'],
    ['积分', row.credit_known ? `${row.credit ?? 0}` : '未知'],
    ['推理档位', row.reasoning_effort || (row.reasoning ? '已启用' : '未启用')],
    ['推理 tokens', row.reasoning_tokens ? fmtInt(row.reasoning_tokens) : '—'],
  ];
  const cacheMeta = tokens.hasCache
    ? `命中 ${fmtInt(tokens.cacheHit)} · 未命中 ${fmtInt(tokens.cacheMiss)}`
    : '上游未上报';
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
    // 中断/流错误的成因单独一块：它回答「是谁先断的」——客户端主动取消、中间层
    // 按读超时掐断，还是上游真的失败。此前这条信息完全没有落库。
    !row.ok && row.failure
      ? el('div', { class: 'drawer-error' }, icon('alert', 15), el('span', { text: row.failure }))
      : null,
    redirectBlock(row),
    el('div', { class: 'detail-token-stats' },
      detailTokenStat('输入 Prompt', tokens.prompt),
      detailTokenStat('输出 Completion', tokens.completion),
      detailTokenStat('合计 Total', tokens.total, '输入 + 输出'),
      detailTokenStat('缓存命中率', tokens.hasCache ? fmtCacheRate(tokens.cacheRate) : '—', cacheMeta),
    ),
    el('div', { class: 'detail-source' },
      el('div', { class: 'detail-source-head' },
        el('span', { class: 'detail-source-title' }, icon('server', 14), '调用来源'),
        badge(client.label, 'muted'),
      ),
      el('div', { class: 'detail-source-grid' },
        detailSourceItem('终端', `${client.label} · ${client.platform}`),
        detailSourceItem('来源 IP', row.client_ip || '—'),
        detailSourceItem('User-Agent', row.user_agent || '—', true),
      ),
      /Go-http-client/i.test(row.user_agent || '')
        ? el('p', { class: 'detail-source-note', text: 'Go HTTP 常见于服务端 SDK 或 New API 等网关转发；终端 UA 可能已被上游覆盖。' })
        : null,
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

function detailSourceItem(label, value, wide = false) {
  return el('div', { class: `detail-source-item ${wide ? 'is-wide' : ''}`.trim() },
    el('span', { class: 'detail-source-label', text: label }),
    el('span', { class: 'detail-source-value mono', text: value, title: value }),
  );
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
