// users.js — 按用户统计：以「入站密钥」为身份，回答「谁用了多少」。
//
// 与用量页刻意分开：用量页的维度是上游资源（供应商 / 上游账号 / 模型），
// 用户页的维度是我们发出去的凭证（api_keys[].name）。两者不能混成一张表，
// 因为一个用户可能落到多个上游账号，混在一起会把同一个人拆成好几行。
//
// 数据口径全部来自请求归档，受保留期限制；页面必须把这一点说清楚，否则
// 「查不到」会被误读成「没用过」。

import { api, describeError } from '../core/api.js';
import {
  authState, isAdmin, isAuthenticated, openLoginModal, requireAdmin,
} from '../core/auth.js';
import {
  el, card, emptyState, loadingBlock, icon, badge, pageBar,
} from '../core/dom.js';
import {
  fmtCompact, fmtDateTime, fmtDuration, fmtInt, fmtPercent, fmtRelative,
} from '../core/format.js';
import { createTable } from '../components/table.js';
import { openDrawer } from '../components/drawer.js';
import { openModal } from '../components/modal.js';
import { toastError, toastSuccess, toastWarn } from '../components/toast.js';

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
    // 权限分组筛选：空 = 全部；否则是「providers.join(' + ')」或「全量」。
    groupFilter: '',
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
  // configured_keys 是后端按 api_keys[].name 合并后的「用户 → 供应商范围」视图；
  // 同名多把密钥已经并成一行。这里建索引给表格/抽屉/分组卡共用。
  const configuredKeys = (Array.isArray(data.configured_keys) ? data.configured_keys : [])
    .map((item) => ({
      name: String(item?.name || '').trim(),
      providers: Array.isArray(item?.providers) ? item.providers.map(String) : [],
      mixed: !!item?.mixed,
    }))
    .filter((item) => item.name);
  const accessByName = new Map(configuredKeys.map((item) => [item.name, item]));
  return {
    ...data,
    rows: rows.map((row) => {
      const key = String(row.key || '');
      return {
        ...row,
        key,
        requests: Number(row.requests || 0),
        errors: Number(row.errors || 0),
        prompt_tokens: Number(row.prompt_tokens || 0),
        completion_tokens: Number(row.completion_tokens || 0),
        total_tokens: Number(row.total_tokens || 0),
        avg_duration_ms: Number(row.avg_duration_ms || 0),
        models: Array.isArray(row.models) ? row.models : [],
        configured: configured.has(key),
        access: accessByName.get(key) || null,
        // 零流量行来自「配置里存在但本窗口没人用」，与「历史归档里有但已删配置」
        // 是两种完全不同的状态，页面上要能一眼分开。
        active: Number(row.requests || 0) > 0,
      };
    }),
    configured_keys: configuredKeys,
    access_by_name: accessByName,
    total: data.total || {},
    retention_days: Number(data.retention_days || 0),
  };
}

function render() {
  if (!rootEl) return;
  const data = ui.data;
  const allRows = data?.rows || [];
  const groups = accessGroups(data);
  // 当前筛选分组在配置变更后可能已不存在；继续用它过滤会得到一张空表，
  // 看起来像「用户都没了」。自动回到「全部」。
  if (ui.groupFilter && !groups.some((g) => g.key === ui.groupFilter)) {
    ui.groupFilter = '';
  }
  // 分组只统计「配置里存在的用户」；历史归档行与 panel 行可能也在表里，但
  // 它们没有可分配的 scope，不应把「已配置 N 个用户」和分组计数算大。
  const configuredUsers = groups.reduce((sum, g) => sum + g.count, 0);
  const rows = ui.groupFilter
    ? allRows.filter((row) => groupKeyFor(row.key, data?.access_by_name) === ui.groupFilter)
    : allRows;
  const active = allRows.filter((row) => row.active);
  const mutated = allRows.reduce((sum, row) => sum + row.errors, 0);
  const totalReq = allRows.reduce((sum, row) => sum + row.requests, 0);
  const totalTokens = allRows.reduce((sum, row) => sum + row.total_tokens, 0);
  const page = el('div', { class: 'page page-users' });

  // 该页数据只有管理员/查看者能读。普通用户与匿名访客连时间窗口和刷新都不该
  // 看到：那些操作只会重复触发 403。先把「看不到」讲清楚并直接返回。
  const blocked = !isAuthenticated() || authState().role === 'user';

  if (!blocked) {
    const windowButtons = el('div', { class: 'segmented' }, WINDOWS.map((win) => el('button', {
      class: `segment ${ui.hours === win.hours ? 'active' : ''}`.trim(),
      type: 'button',
      on: { click: () => { if (ui.hours !== win.hours) load(win.hours); } },
    }, win.label)));

    page.appendChild(pageBar({
      actions: [
        windowButtons,
        el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button',
          on: { click: () => load(ui.hours) },
        }, icon('refresh', 15), '刷新'),
      ],
    }));
  }

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

  // 角色不足是「权限决定的结果」，不是「请求失败」：重试按钮点了必然还是 403，
  // 所以只在真正可访问时才渲染错误卡，其余情况由上面的提示条承担说明。
  if (ui.error && !data && !blocked) {
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

  if (blocked) {
    rootEl.replaceChildren(page);
    return;
  }

  // 首次渲染发生在 await 之前，data 仍为 null。此时按空集合渲染四个数字，
  // 等于先断言「活跃用户 0 / 请求 0 / Tokens 0」，再被真实数据改写。
  // 加载中只留横线：表格区已有加载动画，不必再写四遍「读取中」。
  const loaded = !!data;
  page.appendChild(el('div', { class: 'kpi-grid' },
    kpi('活跃用户', loaded ? fmtInt(active.length) : '—', loaded ? `已配置 ${fmtInt(configuredUsers)} 个用户` : '', 'accent', 'user'),
    kpi('请求数', loaded ? fmtCompact(totalReq) : '—', loaded ? `失败 ${fmtInt(mutated)}` : '', 'muted', 'activity'),
    kpi('成功率', loaded ? rate(totalReq, mutated) : '—', loaded ? `${fmtInt(totalReq)} 次请求` : '', loaded && mutated ? 'warn' : 'ok', 'shield'),
    kpi('Tokens', loaded ? fmtCompact(totalTokens) : '—', loaded ? '入 + 出（上游回报口径）' : '', 'muted', 'models'),
  ));

  // 分组卡只在拿到数据后出现；加载中由下方 loadingBlock 独占提示，避免
  // 手机窄屏上「顶部一行加载中 + 中间一个加载动画」重复占位。
  if (data && groups.length) {
    page.appendChild(accessGroupsCard(groups, configuredUsers));
  }

  const host = el('div', { class: 'table-card' });
  if (ui.loading && !data) {
    host.appendChild(loadingBlock('按用户聚合请求记录'));
  } else if (!rows.length) {
    host.appendChild(el('div', { class: 'table-empty' },
      icon('info', 16),
      ui.groupFilter ? '该权限分组下没有用户' : '当前窗口没有请求',
      el('span', { class: 'muted small', text: '切换时间窗口，或确认客户端已经在用面板签发的密钥。' }),
    ));
  } else {
    host.appendChild(usersTable(rows));
  }
  page.appendChild(card({
    title: '用户明细',
    subtitle: [ui.groupFilter ? `权限分组：${ui.groupFilter}` : '', rangeText(data)]
      .filter(Boolean).join(' · '),
    body: host,
    class: 'users-table-card',
  }));

  page.appendChild(el('div', { class: 'page-footer-note' },
    data?.retention_days
      ? el('span', { text: `统计受请求记录保留期限制：最多回溯 ${fmtInt(data.retention_days)} 天。` })
      : el('span', { text: '统计来自请求归档，受保留期限制。' }),
    el('span', { text: '身份取 api_keys[].name；未命名密钥显示为 unnamed-<摘要>。' }),
    // 这条不是客套话：升级前写入的历史请求没有入站密钥字段，无法归属到用户。
    // 不说清楚的话，部署后第一眼看到空表会以为功能没生效。但它只在部署后头几天
    // 有用，所以折进 details，别长期占着页脚。
    el('details', { class: 'inline-details' },
      el('summary', { text: '历史请求为什么不在统计里？' }),
      el('p', { class: 'section-note', text: '升级前归档的请求没有记录入站密钥，无法归属到用户；升级后的新请求会立即计入。' }),
    ),
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
  // 加载中不在这里写「加载中」：卡片正文已经有 loadingBlock（旋转动画 + 文案），
  // 手机窄屏上两者叠在一起会显得顶部多出一块无意义的状态文字。
  if (!data) return '';
  const label = WINDOWS.find((w) => w.hours === ui.hours)?.label || `近 ${ui.hours} 小时`;
  if (data.from && data.to) {
    return `${fmtDateTime(data.from)} 至 ${fmtDateTime(data.to)}（${label}）`;
  }
  return label;
}

// ---------------------------------------------------------------------------
// 权限分组
// ---------------------------------------------------------------------------

// accessGroups 把 configured_keys 按 providers 集合归类。分组只是 UI 视图：
// 真正的权限仍然是每条 api_keys[].providers，不额外落一份分组配置。
function accessGroups(data) {
  const byGroup = new Map();
  for (const item of data?.configured_keys || []) {
    const key = item.providers.length ? item.providers.join(' + ') : '全量';
    const entry = byGroup.get(key) || {
      key,
      providers: item.providers,
      count: 0,
      mixed: false,
    };
    entry.count += 1;
    entry.mixed = entry.mixed || item.mixed;
    byGroup.set(key, entry);
  }
  return Array.from(byGroup.values()).sort((a, b) => (
    b.count - a.count || a.key.localeCompare(b.key, 'zh-CN')
  ));
}

function groupKeyFor(name, accessByName) {
  const access = accessByName?.get?.(name);
  if (!access) return '';
  return access.providers.length ? access.providers.join(' + ') : '全量';
}

function accessGroupsCard(groups, totalUsers) {
  // 「全部」的计数用总用户数，其余用分组计数；不把分组 key 当显示文案，
  // 避免供应商名恰好叫「全部」时筛选语义错位。
  const all = el('button', {
    class: `access-group ${ui.groupFilter === '' ? 'active' : ''}`.trim(),
    type: 'button',
    on: { click: () => { ui.groupFilter = ''; render(); } },
  },
    el('span', { class: 'access-group-name' }, badge('全部', 'muted')),
    el('span', { class: 'access-group-count', text: `${fmtInt(totalUsers)} 个已配置用户` }),
  );
  const rest = groups.map((g) => el('button', {
    class: `access-group ${ui.groupFilter === g.key ? 'active' : ''}`.trim(),
    type: 'button',
    on: {
      click: () => {
        ui.groupFilter = ui.groupFilter === g.key ? '' : g.key;
        render();
      },
    },
  },
    el('span', { class: 'access-group-name' },
      g.providers.length
        ? g.providers.map((name) => badge(name, 'info'))
        : badge('全量供应商', 'ok'),
      g.mixed ? badge('同名范围不一致', 'warn') : null,
    ),
    el('span', { class: 'access-group-count', text: `${fmtInt(g.count)} 个用户` }),
  ));
  return card({
    title: '权限分组',
    subtitle: '按 api_keys[].providers 自动归类；点分组筛选下方用户',
    body: el('div', { class: 'access-groups' }, all, rest),
    class: 'access-groups-card',
  });
}

function accessCell(row) {
  if (row.key === 'panel') {
    return badge('面板内部', 'muted');
  }
  const access = row.access;
  if (!access) {
    return el('span', { class: 'muted small', text: '—' });
  }
  if (!access.providers.length) {
    return badge('全量', 'ok');
  }
  return el('div', { class: 'tag-row' },
    access.providers.slice(0, 3).map((name) => badge(name, 'info', { title: name })),
    access.providers.length > 3 ? badge(`+${access.providers.length - 3}`, 'muted', {
      title: access.providers.slice(3).join('、'),
    }) : null,
    access.mixed ? badge('混合', 'warn', { title: '同名密钥的供应商范围不一致，保存后会一起改成同一组' }) : null,
  );
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
        key: 'access',
        label: '访问范围',
        width: '178px',
        render: (row) => accessCell(row),
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
      accessSection(row),
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

// ---------------------------------------------------------------------------
// 访问权限编辑
// ---------------------------------------------------------------------------

function accessSection(row) {
  const wrap = el('div', { class: 'use-user-access' },
    el('h3', { class: 'section-title', text: '访问权限' }),
  );
  if (row.key === 'panel') {
    wrap.appendChild(el('p', { class: 'section-note', text: '面板内置密钥恒为全量，不参与按用户分配。' }));
    return wrap;
  }
  if (!row.access) {
    wrap.appendChild(el('p', { class: 'section-note', text: '这把密钥已不在当前 api_keys 配置里（仅历史归档），无法分配权限。' }));
    return wrap;
  }
  wrap.appendChild(el('div', { class: 'tag-row' },
    row.access.providers.length
      ? row.access.providers.map((name) => badge(name, 'info', { title: name }))
      : badge('全量供应商', 'ok'),
    row.access.mixed ? badge('同名范围不一致', 'warn', {
      title: '该用户名下多把密钥的供应商范围不同；编辑并保存后会合并成同一组',
    }) : null,
  ));
  wrap.appendChild(el('p', { class: 'section-note', text: row.access.providers.length
    ? '该用户只能看到并调用上述供应商的模型；/v1/models 会同步收窄，越权请求返回 403。'
    : '未限制供应商 = 可以访问全部供应商。' }));
  if (isAdmin()) {
    wrap.appendChild(el('div', { class: 'use-user-access-actions' },
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => openScopeModal(row.key, row.access) },
      }, icon('shield', 14), '编辑访问权限'),
    ));
  } else {
    wrap.appendChild(el('p', { class: 'section-note', text: '只有管理员可以修改访问权限。' }));
  }
  return wrap;
}

// openScopeModal 只改「这个用户名下所有密钥的 providers」，不动别的配置。
// 走专用接口（/panel/api/access/scope）而不是整份 /omni/config 保存：用户页
// 不该把上游凭证带进浏览器绕一圈，也避免同名多把密钥只能按索引改一条。
async function openScopeModal(name, access) {
  if (!(await requireAdmin({ message: '修改访问权限需要管理员权限' }))) return;
  let payload;
  try {
    payload = await api.config.get({ signal: pageSignal?.signal, dedupe: false });
  } catch (error) {
    toastError(error);
    return;
  }
  const cfg = payload?.config || payload || {};
  const providers = (Array.isArray(cfg.providers) ? cfg.providers : [])
    .map((p) => (typeof p === 'string' ? p : String(p?.name || '')))
    .filter(Boolean);
  const keys = Array.isArray(cfg.api_keys) ? cfg.api_keys : [];
  const sameName = keys.filter((k) => k && typeof k === 'object'
    && String(k.name || '').trim() === name);
  const keyCount = Math.max(1, sameName.length);

  const selected = new Set((access.providers || []).filter((p) => providers.includes(p)));
  // 配置里已经删掉的供应商：不阻塞编辑，但必须提示「保存会移除它们」。
  // 最危险的情况是当前范围里的供应商全被删了，这时空选择 = 全量，等于反向扩权。
  const missing = (access.providers || []).filter((p) => !providers.includes(p));
  const boxes = new Map();
  const summary = el('p', { class: 'scope-summary' });
  const syncSummary = () => {
    const chosen = providers.filter((p) => boxes.get(p)?.checked);
    summary.textContent = chosen.length
      ? `当前选择：${chosen.join(' + ')}`
      : '当前选择：全量（不限制供应商）';
  };

  const presetChips = [];
  const syncPreset = () => {
    const chosen = providers.filter((p) => boxes.get(p)?.checked);
    for (const item of presetChips) {
      item.chip.classList.toggle('active',
        chosen.length === item.providers.length
        && item.providers.every((p) => chosen.includes(p)));
    }
  };
  const applyPreset = (presetProviders) => {
    for (const p of providers) {
      const box = boxes.get(p);
      if (box) box.checked = presetProviders.includes(p);
    }
    syncSummary();
    syncPreset();
  };

  // 快捷分组直接来自当前已有的 scope 集合；不额外维护一份分组表，
  // 免得「分组表改了、api_keys 没改」出现两套事实。
  const seen = new Set(['']);
  const presets = [{ label: '全量', providers: [] }];
  for (const item of ui.data?.configured_keys || []) {
    const key = item.providers.join('|');
    if (seen.has(key)) continue;
    seen.add(key);
    presets.push({
      label: item.providers.length ? item.providers.join(' + ') : '全量',
      providers: item.providers,
    });
  }
  for (const preset of presets) {
    const chip = el('button', {
      class: 'scope-preset',
      type: 'button',
      on: { click: () => applyPreset(preset.providers) },
    }, preset.label);
    presetChips.push({ chip, providers: preset.providers });
  }

  const providerList = el('div', { class: 'scope-options' });
  if (!providers.length) {
    providerList.appendChild(emptyState('还没有供应商', '先在「供应商」页添加供应商，再分配访问范围。'));
  }
  for (const p of providers) {
    const input = el('input', {
      type: 'checkbox',
      checked: selected.has(p),
      on: { change: () => { syncSummary(); syncPreset(); } },
    });
    boxes.set(p, input);
    providerList.appendChild(el('label', { class: 'scope-option' },
      input,
      el('span', { class: 'scope-option-name mono', text: p }),
      el('span', { class: 'scope-option-hint', text: '允许访问' }),
    ));
  }

  const body = el('div', { class: 'scope-editor' },
    el('div', { class: 'notice notice-info notice-inline' },
      icon('info', 15),
      el('span', { text: '不勾选任何供应商 = 全量（该用户可以访问所有供应商）。勾选后，网关会同时收窄 /v1/models 与 /v1/chat/completions 等入口。' }),
    ),
    sameName.length > 1
      ? el('div', { class: 'notice notice-warn notice-inline' },
        icon('alert', 15),
        el('span', { text: `该用户名下有 ${sameName.length} 把密钥，保存后会一起改成同一组范围。` }),
      )
      : null,
    missing.length
      ? el('div', { class: 'notice notice-warn notice-inline' },
        icon('alert', 15),
        el('span', { text: `当前范围里的 ${missing.join('、')} 已不在供应商配置中；保存时会移除这些项。若不勾选任何现存供应商，该用户会变成全量。` }),
      )
      : null,
    el('div', { class: 'scope-preset-row' },
      el('span', { class: 'scope-preset-label', text: '快捷分组' }),
      presetChips.map((item) => item.chip),
    ),
    el('div', { class: 'section-title-row' },
      el('h3', { class: 'section-title', text: '供应商' }),
      el('span', { class: 'section-note', text: `${providers.length} 个可选` }),
    ),
    providerList,
    summary,
  );

  const saveBtn = el('button', {
    class: 'btn btn-primary',
    type: 'button',
    disabled: !providers.length,
    on: {
      click: async () => {
        const chosen = providers.filter((p) => boxes.get(p)?.checked);
        saveBtn.disabled = true;
        try {
          const res = await api.access.setScope({ name, providers: chosen }, {
            signal: pageSignal?.signal,
          });
          toastSuccess(res?.updated > 1
            ? `已更新 ${name} 的访问范围（${res.updated} 把密钥）`
            : `已更新 ${name} 的访问范围`);
          if (Array.isArray(res?.restart_required) && res.restart_required.length) {
            toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`);
          }
          modal.close();
          if (drawer) { drawer.close(); drawer = null; }
          // 改完范围后用户往往换了分组；继续停在旧分组筛选上会让刚编辑的人
          // 从表里消失，看起来像保存失败。回到「全部」，让新分组可见。
          ui.groupFilter = '';
          await load(ui.hours);
        } catch (error) {
          toastError(error);
          saveBtn.disabled = false;
        }
      },
    },
  }, '保存');

  const modal = openModal({
    title: `访问权限 · ${name}`,
    subtitle: keyCount > 1
      ? `该用户名下有 ${keyCount} 把密钥，保存后会一起更新`
      : '按供应商收窄该用户可访问的模型',
    size: 'md',
    body,
    footer: [
      el('button', { class: 'btn btn-ghost', type: 'button', on: { click: () => modal.close() } }, '取消'),
      saveBtn,
    ],
  });
  syncSummary();
  syncPreset();
}
