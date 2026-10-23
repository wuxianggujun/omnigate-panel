// users.js — 按用户统计：以「入站密钥」为身份，回答「谁用了多少」。
//
// 与用量页刻意分开：用量页的维度是上游资源（供应商 / 上游账号 / 模型），
// 用户页的维度是我们发出去的凭证（api_keys[].name）。两者不能混成一张表，
// 因为一个用户可能落到多个上游账号，混在一起会把同一个人拆成好几行。
//
// 数据口径全部来自请求归档，受保留期限制；页面必须把这一点说清楚，否则
// 「查不到」会被误读成「没用过」。

import { api, describeError } from '../core/api.js';
import { authState, isAuthenticated, openLoginModal } from '../core/auth.js';
import {
  el, card, emptyState, loadingBlock, icon,
} from '../core/dom.js';
import {
  fmtCompact, fmtDateTime, fmtDuration, fmtInt, fmtPercent, fmtRelative,
} from '../core/format.js';
import { createTable } from '../components/table.js';
import { openDrawer } from '../components/drawer.js';
import { toastError } from '../components/toast.js';

const WINDOWS = [
  { hours: 24, label: '近 24 小时' },
  { hours: 168, label: '近 7 天' },
];

let pageSignal = null;
let rootEl = null;
let loadSeq = 0;
let ui = initialState();
let drawer = null;

function initialState() {
  return {
    hours: 168,
    loading: true,
    error: '',
    data: null,
  };
}

export async function mount(root) {
  pageSignal = new AbortController();
  rootEl = root;
  ui = initialState();
  render();
  await load(ui.hours);
}

export function unmount() {
  if (pageSignal) pageSignal.abort();
  pageSignal = null;
  rootEl = null;
  ui = initialState();
  if (drawer) {
    drawer.close();
    drawer = null;
  }
}

function windowRange(hours) {
  const to = new Date();
  const from = new Date(to.getTime() - hours * 3600 * 1000);
  return { from: from.toISOString(), to: to.toISOString() };
}

async function load(hours) {
  if (!pageSignal) return;
  const seq = ++loadSeq;
  ui.hours = hours;
  ui.loading = true;
  render();
  const { from, to } = windowRange(hours);
  try {
    const data = await api.usage.byKey({ from, to, signal: pageSignal.signal });
    if (!pageSignal || pageSignal.signal.aborted || seq !== loadSeq) return;
    ui.data = normalize(data);
    ui.error = '';
  } catch (error) {
    if (pageSignal?.signal.aborted || error?.name === 'AbortError') return;
    ui.error = describeError(error);
    toastError(error);
  } finally {
    if (seq === loadSeq) {
      ui.loading = false;
      render();
    }
  }
}

function normalize(raw) {
  const data = raw && typeof raw === 'object' ? raw : {};
  const rows = Array.isArray(data.rows) ? data.rows : [];
  const configured = new Set(Array.isArray(data.configured) ? data.configured : []);
  return {
    ...data,
    rows: rows.map((row) => ({
      ...row,
      key: String(row.key || ''),
      requests: Number(row.requests || 0),
      errors: Number(row.errors || 0),
      prompt_tokens: Number(row.prompt_tokens || 0),
      completion_tokens: Number(row.completion_tokens || 0),
      total_tokens: Number(row.total_tokens || 0),
      avg_duration_ms: Number(row.avg_duration_ms || 0),
      models: Array.isArray(row.models) ? row.models : [],
      configured: configured.has(row.key),
      // 零流量行来自「配置里存在但本窗口没人用」，与「历史归档里有但已删配置」
      // 是两种完全不同的状态，页面上要能一眼分开。
      active: Number(row.requests || 0) > 0,
    })),
    total: data.total || {},
    retention_days: Number(data.retention_days || 0),
  };
}

function render() {
  if (!rootEl) return;
  const data = ui.data;
  const rows = data?.rows || [];
  const active = rows.filter((row) => row.active);
  const mutated = rows.reduce((sum, row) => sum + row.errors, 0);
  const totalReq = rows.reduce((sum, row) => sum + row.requests, 0);
  const totalTokens = rows.reduce((sum, row) => sum + row.total_tokens, 0);
  const page = el('div', { class: 'page page-users' });

  const windowButtons = el('div', { class: 'segmented' }, WINDOWS.map((win) => el('button', {
    class: `segment ${ui.hours === win.hours ? 'active' : ''}`.trim(),
    type: 'button',
    on: { click: () => { if (ui.hours !== win.hours) load(win.hours); } },
  }, win.label)));

  page.appendChild(el('div', { class: 'page-head' },
    el('div', {},
      el('div', { class: 'eyebrow', text: 'ANALYTICS' }),
      el('h1', { class: 'page-title', text: '用户' }),
      el('p', { class: 'page-subtitle', text: '按入站密钥统计谁在用、用了多少；点开一行看模型拆分。' }),
    ),
    el('div', { class: 'page-actions' },
      windowButtons,
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => load(ui.hours) },
      }, icon('refresh', 15), '刷新'),
    ),
  ));

  if (!isAuthenticated()) {
    page.appendChild(el('div', { class: 'notice notice-info' },
      icon('info', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '登录后查看按用户统计' }),
        el('span', { text: '该页只对管理员/查看者开放：归属表没有到入站密钥的映射，普通租户看不到全量用户用量。' }),
      ),
      el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openLoginModal({ message: '登录后查看按用户统计。' }) },
      }, '登录'),
    ));
  } else if (authState().role === 'user') {
    page.appendChild(el('div', { class: 'notice notice-warn' },
      icon('alert', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '当前角色不可见' }),
        el('span', { text: '按用户统计需要管理员或查看者角色。' }),
      ),
    ));
  }

  if (ui.error && !data) {
    page.appendChild(card({
      title: '用户统计加载失败',
      body: el('div', { class: 'error-panel' },
        icon('alert', 20),
        el('div', {},
          el('p', { text: ui.error }),
          el('button', {
            class: 'btn btn-ghost btn-sm', type: 'button',
            on: { click: () => load(ui.hours) },
          }, '重试'),
        ),
      ),
    }));
    rootEl.replaceChildren(page);
    return;
  }

  page.appendChild(el('div', { class: 'kpi-grid' },
    kpi('活跃用户', fmtInt(active.length), `已配置 ${fmtInt(rows.length)} 把密钥`, 'accent', 'user'),
    kpi('请求数', fmtCompact(totalReq), `失败 ${fmtInt(mutated)}`, 'muted', 'activity'),
    kpi('成功率', rate(totalReq, mutated), `${fmtInt(totalReq)} 次请求`, mutated ? 'warn' : 'ok', 'shield'),
    kpi('Tokens', fmtCompact(totalTokens), '入 + 出（上游回报口径）', 'muted', 'models'),
  ));

  const host = el('div', { class: 'table-card' });
  if (ui.loading && !data) {
    host.appendChild(loadingBlock('按用户聚合请求记录'));
  } else if (!rows.length) {
    host.appendChild(el('div', { class: 'table-empty' },
      icon('info', 16),
      '当前窗口没有可按用户归属的请求',
    ));
  } else {
    host.appendChild(usersTable(rows));
  }
  page.appendChild(card({
    title: '用户明细',
    subtitle: rangeText(data),
    body: host,
    class: 'users-table-card',
  }));

  page.appendChild(el('div', { class: 'page-footer-note' },
    data?.retention_days
      ? el('span', { text: `统计受请求记录保留期限制：最多回溯 ${fmtInt(data.retention_days)} 天。` })
      : el('span', { text: '统计来自请求归档，受保留期限制。' }),
    // 这条不是客套话：升级前写入的历史请求没有入站密钥字段，无法归属到用户。
    // 不说清楚的话，部署后第一眼看到空表会以为功能没生效。
    el('span', { text: '本次升级前的历史请求没有记录入站密钥，无法归属到用户；新请求会立即计入。' }),
    el('span', { text: '身份取 api_keys[].name；未命名密钥显示为 unnamed-<摘要>。' }),
  ));
  rootEl.replaceChildren(page);
}

function kpi(label, value, meta, tone, iconName) {
  return el('div', { class: `kpi-card kpi-${tone}` },
    el('div', { class: 'kpi-top' },
      el('span', { class: 'kpi-label', text: label }),
      el('span', { class: 'kpi-icon' }, icon(iconName, 16)),
    ),
    el('div', { class: 'kpi-value', text: value }),
    el('div', { class: 'kpi-meta', text: meta || ' ' }),
  );
}

function rate(requests, errors) {
  if (!requests) return '—';
  return fmtPercent(((requests - errors) / requests) * 100);
}

function rangeText(data) {
  if (!data) return '加载中';
  const label = WINDOWS.find((w) => w.hours === ui.hours)?.label || `近 ${ui.hours} 小时`;
  if (data.from && data.to) {
    return `${fmtDateTime(data.from)} 至 ${fmtDateTime(data.to)}（${label}）`;
  }
  return label;
}

function usersTable(rows) {
  return createTable({
    caption: '按入站密钥聚合的用户用量',
    columns: [
      {
        key: 'key',
        label: '用户 / 密钥',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'user-key-name', text: row.key || '（未命名）' }),
          el('span', { class: 'cell-sub', text: row.active ? rowStatusText(row) : (row.configured ? '已配置 · 窗口内无流量' : '历史归档 · 配置中已不存在') }),
        ),
      },
      {
        key: 'requests',
        label: '请求',
        align: 'right',
        width: '96px',
        render: (row) => el('div', { class: 'token-cell' },
          el('span', { class: 'mono', text: fmtInt(row.requests) }),
          row.errors
            ? el('span', { class: 'cache-pill cache-warn', text: `失败 ${fmtInt(row.errors)}` })
            : null,
        ),
      },
      {
        key: 'rate',
        label: '成功率',
        align: 'right',
        width: '92px',
        render: (row) => (row.active
          ? el('span', { class: rateTone(row) === 'ok' ? 'mono' : 'mono text-warn', text: rate(row.requests, row.errors) })
          : el('span', { class: 'muted small', text: '—' })),
      },
      {
        key: 'tokens',
        label: 'Tokens',
        align: 'right',
        width: '150px',
        render: (row) => (row.total_tokens
          ? el('div', { class: 'token-cell' },
            el('span', { class: 'token-total mono', text: fmtCompact(row.total_tokens) }),
            el('span', { class: 'cell-sub', text: `入 ${fmtCompact(row.prompt_tokens)} · 出 ${fmtCompact(row.completion_tokens)}` }),
          )
          : el('span', { class: 'muted small', text: '—' })),
      },
      {
        key: 'avg_duration_ms',
        label: '平均耗时',
        align: 'right',
        width: '96px',
        render: (row) => (row.active
          ? el('span', { class: 'mono', text: fmtDuration(row.avg_duration_ms) })
          : el('span', { class: 'muted small', text: '—' })),
      },
      {
        key: 'last_seen',
        label: '最后活跃',
        width: '130px',
        render: (row) => (row.last_seen
          ? el('span', { class: 'mono small', text: fmtRelative(row.last_seen), title: fmtDateTime(row.last_seen, { seconds: true }) })
          : el('span', { class: 'muted small', text: '—' })),
      },
      {
        key: 'models',
        label: '主要模型',
        render: (row) => {
          if (!row.models.length) return el('span', { class: 'muted small', text: '—' });
          const top = row.models[0];
          return el('div', { class: 'cell-title' },
            el('span', { class: 'mono truncate', text: top.model || '—', title: (top.model || '') }),
            el('span', { class: 'cell-sub', text: `${fmtInt(top.requests)} 次${row.models.length > 1 ? ` · 共 ${fmtInt(row.models.length)} 个模型` : ''}` }),
          );
        },
      },
      {
        key: 'open',
        label: '',
        width: '34px',
        align: 'right',
        render: () => el('span', { class: 'row-open', title: '查看模型拆分', attrs: { 'aria-hidden': 'true' } }, icon('chevron', 14)),
      },
    ],
    rows,
    empty: '当前窗口没有可按用户归属的请求',
    onRowClick: (row) => openUserDrawer(row),
  }).el;
}

function rowStatusText(row) {
  return row.configured ? '已配置 · 有流量' : '历史归档 · 配置中已不存在';
}

function rateTone(row) {
  if (!row.requests) return 'muted';
  return row.errors / row.requests > 0.2 ? 'warn' : 'ok';
}

// ---------------------------------------------------------------------------
// 用户详情抽屉
// ---------------------------------------------------------------------------

function openUserDrawer(row) {
  if (drawer) drawer.close();
  drawer = openDrawer({
    title: row.key || '（未命名密钥）',
    subtitle: `${fmtInt(row.requests)} 次请求 · ${fmtCompact(row.total_tokens)} tokens`,
    size: 'lg',
    body: el('div', {},
      el('div', { class: 'use-user-summary' },
        summaryItem('成功率', rate(row.requests, row.errors)),
        summaryItem('平均耗时', row.active ? fmtDuration(row.avg_duration_ms) : '—'),
        summaryItem('最后活跃', row.last_seen ? fmtDateTime(row.last_seen, { seconds: true }) : '—'),
        summaryItem('来源', row.configured ? '当前配置中的密钥' : '仅存在于历史归档'),
      ),
      el('h3', { class: 'section-title', text: '模型拆分' }),
      row.models.length
        ? createTable({
          caption: '该用户在各模型上的用量',
          columns: [
            {
              key: 'model',
              label: '模型',
              render: (m) => el('span', { class: 'mono truncate', text: m.model || '—', title: m.model || '' }),
            },
            {
              key: 'requests',
              label: '请求',
              align: 'right',
              width: '100px',
              render: (m) => fmtInt(m.requests),
            },
            {
              key: 'errors',
              label: '失败',
              align: 'right',
              width: '84px',
              render: (m) => (m.errors ? el('span', { class: 'text-warn', text: fmtInt(m.errors) }) : el('span', { class: 'muted', text: '0' })),
            },
            {
              key: 'tokens',
              label: 'Tokens',
              align: 'right',
              width: '120px',
              render: (m) => fmtCompact(m.total_tokens),
            },
          ],
          rows: row.models,
          empty: '暂无模型明细',
        }).el
        : emptyState('没有模型明细', '该用户在当前窗口内没有成功记录，或上游未回报模型。'),
    ),
    onClose: () => { drawer = null; },
  });
}

function summaryItem(label, value) {
  return el('div', { class: 'use-user-summary-item' },
    el('span', { class: 'cell-sub', text: label }),
    el('span', { class: 'mono', text: value }),
  );
}
