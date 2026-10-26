// usage.js — 用量分析：请求/token/积分趋势，按供应商、账号与模型分组。

import { api, describeError } from '../core/api.js';
import { authState, isAuthenticated, openLoginModal } from '../core/auth.js';
import {
  el, svgEl, card, badge, emptyState, loadingBlock, icon, pageBar,
} from '../core/dom.js';
import {
  fmtBytes, fmtChartTime, fmtCompact, fmtCredits, fmtDateTime, fmtDuration, fmtInt, fmtNum, fmtPercent,
  fmtTime,
} from '../core/format.js';
import { createTabs } from '../components/tabs.js';
import { createTable } from '../components/table.js';
import { toastError } from '../components/toast.js';

const WINDOWS = [
  { hours: 24, label: '近 24 小时' },
  { hours: 168, label: '近 7 天' },
  { hours: 720, label: '近 30 天' },
];

let pageSignal = null;
let rootEl = null;
let loadSeq = 0;
let ui = initialState();

function initialState() {
  return {
    hours: 24,
    loading: true,
    error: '',
    data: null,
    metric: 'requests',
    group: 'provider',
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
}

async function load(hours) {
  if (!pageSignal) return;
  const seq = ++loadSeq;
  ui.hours = hours;
  ui.loading = true;
  render();
  try {
    const data = await api.usage.get({ hours, signal: pageSignal.signal });
    if (!pageSignal || pageSignal.signal.aborted || seq !== loadSeq) return;
    ui.data = normalizeUsage(data);
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

function normalizeUsage(raw) {
  const data = raw && typeof raw === 'object' ? raw : {};
  const by = (value) => {
    if (Array.isArray(value)) return value;
    if (value && typeof value === 'object') {
      return Object.entries(value).map(([key, item]) => ({ key, ...(typeof item === 'object' ? item : { value: item }) }));
    }
    return [];
  };
  let byProvider = by(data.by_provider ?? data.byProvider);
  if (!byProvider.length) byProvider = by(data.by_realm ?? data.byRealm);
  return {
    ...data,
    totals: data.totals || {},
    series: Array.isArray(data.series) ? data.series : [],
    byProvider,
    byAccount: by(data.by_account ?? data.byAccount),
    byModel: by(data.by_model ?? data.byModel),
  };
}

function render() {
  if (!rootEl) return;
  const data = ui.data;
  const totals = data?.totals || {};
  const auth = authState();
  const page = el('div', { class: 'page page-usage' });

  const windowButtons = el('div', { class: 'segmented' }, WINDOWS.map((win) => el('button', {
    class: `segment ${ui.hours === win.hours ? 'active' : ''}`.trim(),
    type: 'button',
    dataset: { hours: String(win.hours) },
    on: { click: () => { if (ui.hours !== win.hours) load(win.hours); } },
  }, win.label)));

  // 普通用户的范围说明与时间窗口/刷新放进同一条工具条：分成两条会上下叠着，
  // 而它们其实是同一件事——这条工具条在说什么范围、以什么粒度看。
  page.appendChild(pageBar({
    context: !isAuthenticated()
      ? null
      : (auth.role === 'user'
        ? [
          badge('个人范围', 'info'),
          el('span', { text: '仅统计归属你的账号（provider/label）；平台密钥型供应商的共享流量不在个人范围内。' }),
        ]
        : null),
    actions: [
      windowButtons,
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => load(ui.hours) },
      }, icon('refresh', 15), '刷新'),
    ],
  }));

  if (!isAuthenticated()) {
    page.appendChild(el('div', { class: 'notice notice-info' },
      icon('info', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '匿名访客看不到账号级用量' }),
        el('span', { text: '登录后按角色显示全部或归属自己的用量明细。' }),
      ),
      el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openLoginModal({ message: '登录后查看用量明细。' }) },
      }, '登录'),
    ));
  }

  if (ui.error && !data) {
    page.appendChild(card({
      title: '用量加载失败',
      body: el('div', { class: 'error-panel' },
        icon('alert', 20),
        el('div', {}, el('p', { text: ui.error }), el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button', on: { click: () => load(ui.hours) },
        }, '重试')),
      ),
    }));
    rootEl.replaceChildren(page);
    return;
  }

  // 匿名的 totals 是后端按归属过滤后的空集合，服务端确实返回 0——但那句 0 的
  // 含义是「没有你看得到的记录」，不是「平台这段时间没有请求」。直接用 0 渲染，
  // 访客会以为服务真的空转（页脚还写着用量文件几百 KB）。这里改成留空 + 一句
  // 说明，把「不可见」和「真的没有」分开。
  const hidden = !isAuthenticated();
  // loaded：数据真的回来了。首次渲染发生在 await 之前，totals 是空对象，
  // 此时按 0 渲染等于把「还没读到」当成「这段时间没有请求」。
  const loaded = !!data;
  if (hidden) {
    page.appendChild(el('div', { class: 'kpi-grid' },
      kpi('请求数', '—', '登录后统计', 'muted', 'activity'),
      kpi('成功率', '—', '登录后统计', 'muted', 'shield'),
      kpi('Tokens', '—', '登录后统计', 'muted', 'models'),
      kpi('积分 / 成本', '—', '登录后统计', 'muted', 'usage'),
      kpi('平均延迟', '—', '登录后统计', 'muted', 'clock'),
      kpi('缓存命中率', '—', '登录后统计', 'muted', 'server'),
    ));
  } else if (!loaded) {
    // 加载中只留横线，不重复写 6 遍「读取中」：正文已有加载动画在提示状态。
    page.appendChild(el('div', { class: 'kpi-grid' },
      kpi('请求数', '—', '', 'muted', 'activity'),
      kpi('成功率', '—', '', 'muted', 'shield'),
      kpi('Tokens', '—', '', 'muted', 'models'),
      kpi('积分 / 成本', '—', '', 'muted', 'usage'),
      kpi('平均延迟', '—', '', 'muted', 'clock'),
      kpi('缓存命中率', '—', '', 'muted', 'server'),
    ));
  } else {
    page.appendChild(el('div', { class: 'kpi-grid' },
      kpi('请求数', fmtCompact(totals.requests), `失败 ${fmtInt(totals.errors)} · 中断 ${fmtInt(totals.interrupted_requests || 0)}`, 'accent', 'activity'),
      kpi('成功率', successRate(totals), `${fmtInt(totals.requests)} 次请求`, totals.errors ? 'warn' : 'ok', 'shield'),
      kpi('Tokens', fmtCompact(totals.total_tokens), `入 ${fmtCompact(totals.prompt_tokens)} · 出 ${fmtCompact(totals.completion_tokens)}`, 'muted', 'models'),
      kpi('积分 / 成本', fmtCredits(totals.credits), totals.credits_per_1m_tokens ? `${fmtNum(totals.credits_per_1m_tokens, 2)} 积分 / 1M tokens` : '上游未回传 credit 时为空', 'muted', 'usage'),
      kpi('平均延迟', fmtDuration(totals.avg_latency_ms), `平均 ${fmtNum(totals.avg_tokens_per_second, 1)} tokens/s`, 'muted', 'clock'),
      kpi('缓存命中率', fmtPercent(totals.cache_hit_rate), `命中 ${fmtCompact(totals.cache_hit_tokens || 0)} tokens`, 'muted', 'server'),
    ));
  }

  page.appendChild(chartCard(data, totals, { hidden, loaded }));

  const groupTabs = createTabs({
    items: [
      { id: 'provider', label: '按供应商', count: (hidden || !loaded) ? null : (data?.byProvider?.length || 0) },
      { id: 'account', label: '按账号', count: (hidden || !loaded) ? null : (data?.byAccount?.length || 0) },
      { id: 'model', label: '按模型', count: (hidden || !loaded) ? null : (data?.byModel?.length || 0) },
    ],
    value: ui.group,
    ariaLabel: '用量分组维度',
    onChange: (value) => { ui.group = value; render(); },
  });
  page.appendChild(card({
    title: '分组明细',
    subtitle: hidden ? '需要登录' : (loaded ? rangeText(data) : ''),
    actions: groupTabs.el,
    body: hidden
      ? emptyState('用量明细不可见', '按供应商 / 账号 / 模型的用量属于账号级数据，登录后按角色显示。')
      : (loaded ? groupTable(data, totals) : loadingBlock('加载用量')),
    class: 'usage-table-card',
  }));

  page.appendChild(el('div', { class: 'page-footer-note' },
    data?.generated ? el('span', { text: `数据生成于 ${fmtTime(data.generated)}` }) : null,
    data?.file_bytes ? el('span', { text: `用量文件 ${fmtBytes(data.file_bytes)}` }) : null,
    el('span', { text: '积分口径以上游回传的 usage.credit 为准，缺字段的请求不计入积分。' }),
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

function successRate(totals) {
  const req = Number(totals.requests || 0);
  const err = Number(totals.errors || 0);
  if (!req) return '—';
  return fmtPercent(((req - err) / req) * 100);
}

function rangeText(data) {
  // 卡片正文已有 loadingBlock（动画 + 文案），这里不再重复写「加载中」。
  if (!data) return '';
  if (data.window_from || data.window_to) {
    return `${data.window_from ? fmtDateTime(data.window_from) : '最早'} 至 ${data.window_to ? fmtDateTime(data.window_to) : '现在'}（${WINDOWS.find((w) => w.hours === ui.hours)?.label || `${ui.hours} 小时`}）`;
  }
  return WINDOWS.find((w) => w.hours === ui.hours)?.label || `近 ${ui.hours} 小时`;
}

function chartCard(data, totals, { hidden = false, loaded = true } = {}) {
  const series = data?.series || [];
  const metricMeta = {
    requests: { label: '请求数', format: fmtCompact, color: 'var(--accent)' },
    tokens: { label: 'Tokens', format: fmtCompact, color: 'var(--info)' },
    credits: { label: '积分', format: fmtCredits, color: 'var(--warn)' },
  };
  const current = metricMeta[ui.metric] || metricMeta.requests;
  const metricButtons = el('div', { class: 'segmented segmented-sm' },
    Object.entries(metricMeta).map(([key, meta]) => el('button', {
      class: `segment ${ui.metric === key ? 'active' : ''}`.trim(),
      type: 'button',
      on: { click: () => { ui.metric = key; render(); } },
    }, meta.label)),
  );
  const chartHost = el('div', { class: 'chart-host' });
  if (hidden) {
    chartHost.appendChild(emptyState('趋势不可见', '时序桶按账号归属聚合；登录后可见。'));
  } else if (!loaded) {
    chartHost.appendChild(loadingBlock('加载用量'));
  } else if (series.length) {
    chartHost.appendChild(lineChart(series, ui.metric, current));
  } else {
    chartHost.appendChild(emptyState('暂无趋势数据', '当前窗口内没有请求，或服务端未返回时序桶。'));
  }
  // 峰值/时间桶数只在数据真的回来后才有意义；加载期间显示 0 与「暂无」同罪。
  const legend = (hidden || !loaded) ? null : el('div', { class: 'chart-legend' },
    el('span', { class: 'legend-dot-mark', style: { background: current.color } }),
    el('span', { text: current.label }),
    el('span', { class: 'muted', text: `峰值 ${current.format(Math.max(0, ...series.map((p) => metricValue(p, ui.metric))))} · ${series.length} 个时间桶` }),
  );
  return card({
    title: '趋势',
    subtitle: hidden ? '需要登录' : (loaded ? `按时间桶聚合的 ${current.label}；悬停查看明细` : ''),
    actions: metricButtons,
    body: el('div', { class: 'trend-chart' }, chartHost, legend),
    class: 'chart-card',
  });
}

function metricValue(point, metric) {
  if (!point) return 0;
  if (metric === 'tokens') return Number(point.total_tokens ?? ((point.prompt_tokens || 0) + (point.completion_tokens || 0)) ?? 0);
  if (metric === 'credits') return Number(point.credits ?? 0);
  return Number(point.requests ?? 0);
}

function lineChart(series, metric, meta) {
  const width = 960;
  const height = 268;
  const pad = { left: 54, right: 18, top: 18, bottom: 30 };
  const innerW = width - pad.left - pad.right;
  const innerH = height - pad.top - pad.bottom;
  const values = series.map((p) => metricValue(p, metric));
  const max = niceMax(Math.max(0, ...values));
  const stepX = series.length > 1 ? innerW / (series.length - 1) : innerW;
  const xFor = (index) => pad.left + (series.length > 1 ? index * stepX : innerW / 2);
  const yFor = (value) => pad.top + innerH - (value / (max || 1)) * innerH;

  const svg = svgEl('svg', {
    viewBox: `0 0 ${width} ${height}`,
    class: 'trend-svg',
    preserveAspectRatio: 'none',
    role: 'img',
    'aria-label': `${meta.label}趋势图`,
  });

  const gridCount = 4;
  for (let i = 0; i <= gridCount; i += 1) {
    const y = pad.top + (innerH / gridCount) * i;
    svg.appendChild(svgEl('line', {
      x1: pad.left, x2: width - pad.right, y1: y, y2: y, class: 'chart-grid',
    }));
    const value = max - (max / gridCount) * i;
    const text = svgEl('text', { x: pad.left - 8, y: y + 4, class: 'chart-axis-label', 'text-anchor': 'end' });
    text.textContent = meta.format(value);
    svg.appendChild(text);
  }

  const areaPath = [];
  const linePath = [];
  series.forEach((point, index) => {
    const x = xFor(index);
    const y = yFor(metricValue(point, metric));
    if (index === 0) {
      linePath.push(`M ${x.toFixed(2)} ${y.toFixed(2)}`);
      areaPath.push(`M ${x.toFixed(2)} ${(pad.top + innerH).toFixed(2)}`, `L ${x.toFixed(2)} ${y.toFixed(2)}`);
    } else {
      linePath.push(`L ${x.toFixed(2)} ${y.toFixed(2)}`);
      areaPath.push(`L ${x.toFixed(2)} ${y.toFixed(2)}`);
    }
  });
  if (series.length) {
    const lastX = xFor(series.length - 1);
    areaPath.push(`L ${lastX.toFixed(2)} ${(pad.top + innerH).toFixed(2)}`, 'Z');
    svg.appendChild(svgEl('path', { d: areaPath.join(' '), class: 'chart-area' }));
    svg.appendChild(svgEl('path', { d: linePath.join(' '), class: 'chart-line', style: { stroke: meta.color } }));
  }

  series.forEach((point, index) => {
    const x = xFor(index);
    const y = yFor(metricValue(point, metric));
    const dot = svgEl('circle', { cx: x, cy: y, r: 2.6, class: 'chart-dot', style: { fill: meta.color } });
    const title = svgEl('title');
    title.textContent = `${fmtChartTime(point.t, point.scope)} · ${meta.label} ${meta.format(metricValue(point, metric))} · ${fmtInt(point.requests)} 请求`;
    dot.appendChild(title);
    svg.appendChild(dot);
  });

  const labelCount = Math.min(5, series.length);
  for (let i = 0; i < labelCount; i += 1) {
    const index = labelCount === 1 ? 0 : Math.round((series.length - 1) * (i / (labelCount - 1)));
    const point = series[index];
    const text = svgEl('text', {
      x: xFor(index),
      y: height - 8,
      class: 'chart-axis-label',
      'text-anchor': index === 0 ? 'start' : (index === series.length - 1 ? 'end' : 'middle'),
    });
    text.textContent = fmtChartTime(point.t, point.scope);
    svg.appendChild(text);
  }
  return svg;
}

function niceMax(value) {
  if (!Number.isFinite(value) || value <= 0) return 1;
  const exp = Math.floor(Math.log10(value));
  const base = 10 ** exp;
  const normalized = value / base;
  const nice = normalized <= 1 ? 1 : normalized <= 2 ? 2 : normalized <= 5 ? 5 : 10;
  return nice * base;
}

function groupTable(data, totals) {
  if (!data) return loadingBlock('加载用量');
  const rows = ui.group === 'account' ? data.byAccount : (ui.group === 'model' ? data.byModel : data.byProvider);
  const totalRequests = Number(totals.requests || 0) || 1;
  const share = (value) => el('div', { class: 'share-cell' },
    el('div', { class: 'progress progress-sm' },
      el('span', { class: 'progress-fill', style: { width: `${Math.min(100, (Number(value || 0) / totalRequests) * 100)}%` } }),
    ),
    el('span', { class: 'share-label mono', text: fmtPercent((Number(value || 0) / totalRequests) * 100) }),
  );
  const nameLabel = ui.group === 'account' ? '账号' : (ui.group === 'model' ? '模型' : '供应商');
  const table = createTable({
    columns: [
      {
        key: 'key',
        label: nameLabel,
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.key || '—' }),
          row.extra || row.nickname ? el('span', { class: 'cell-sub', text: row.extra || row.nickname }) : null,
          row.realm ? el('span', { class: 'cell-sub', text: `realm ${row.realm}` }) : null,
        ),
      },
      { key: 'requests', label: '请求', align: 'right', render: (row) => fmtInt(row.requests) },
      {
        key: 'errors', label: '失败', align: 'right',
        render: (row) => (row.errors ? el('span', { class: 'text-warn', text: fmtInt(row.errors) }) : '—'),
      },
      {
        key: 'success', label: '成功率', align: 'right',
        render: (row) => (row.requests ? fmtPercent(((row.requests - (row.errors || 0)) / row.requests) * 100) : '—'),
      },
      {
        key: 'tokens', label: 'Tokens', align: 'right',
        render: (row) => fmtCompact(row.total_tokens),
      },
      { key: 'credits', label: '积分', align: 'right', render: (row) => fmtCredits(row.credits) },
      {
        key: 'latency', label: '平均延迟', align: 'right', hideOnMobile: true,
        render: (row) => fmtDuration(row.avg_latency_ms),
      },
      { key: 'share', label: '请求占比', width: '150px', render: (row) => share(row.requests) },
    ],
    rows,
    empty: '当前维度没有数据',
  });
  return table.el;
}
