// overview.js — 总览：引擎健康、账号池、24h 趋势、近期请求与模型锁。

import { api, isForbidden, describeError } from '../core/api.js';
import { authState, isAuthenticated, openLoginModal } from '../core/auth.js';
import {
  el, svgEl, card, badge, statusDot, emptyState, loadingBlock, icon, copyText,
} from '../core/dom.js';
import {
  fmtCompact, fmtCredits, fmtDuration, fmtInt, fmtPercent, fmtRelative, fmtTime,
  fmtUptime, isKeyBasedProvider, outcomeLabel, parseDate, providerLabel,
} from '../core/format.js';
import { toastError, toastSuccess } from '../components/toast.js';
import { createTable } from '../components/table.js';

let abort = null;
let timer = 0;
let inFlight = false;
let localState = { data: null, accounts: null, usage: null, error: '' };

export async function mount(root, ctx) {
  abort = new AbortController();
  const signal = abort.signal;
  render(root, ctx, { loading: true });

  async function load({ silent = false } = {}) {
    if (inFlight) return;
    inFlight = true;
    try {
      const [overviewRes, accountsRes, usageRes] = await Promise.allSettled([
        api.overview.get({ signal }),
        api.accounts.list({ signal }),
        api.usage.get({ hours: 24, signal }),
      ]);
      if (signal.aborted) return;
      const errors = [];
      let data = null;
      let accounts = null;
      let usage = null;
      if (overviewRes.status === 'fulfilled') {
        data = overviewRes.value;
        if (data?.ok === false) errors.push(data.error || '概览接口返回失败');
      } else errors.push(describeError(overviewRes.reason));
      if (accountsRes.status === 'fulfilled') accounts = accountsRes.value;
      if (usageRes.status === 'fulfilled') usage = usageRes.value;
      localState = { data, accounts, usage, error: errors[0] || '' };
      render(root, ctx, {});
      if (!silent) {
        // 首次加载不弹提示，避免只读接口 401 噪声。
      }
    } catch (error) {
      if (signal.aborted || error?.name === 'AbortError') return;
      localState = { ...localState, error: describeError(error) };
      render(root, ctx, {});
    } finally {
      inFlight = false;
    }
  }

  await load();
  timer = window.setInterval(() => load({ silent: true }), 15000);
  root.__overviewLoaded = true;
}

export function unmount() {
  if (abort) abort.abort();
  abort = null;
  clearInterval(timer);
  timer = 0;
  inFlight = false;
  localState = { data: null, accounts: null, usage: null, error: '' };
}

function render(root, ctx, { loading = false } = {}) {
  root.replaceChildren();
  const page = el('div', { class: 'page page-overview' });
  const state = authState();
  const data = localState.data || {};
  const accountsPayload = localState.accounts?.data || localState.accounts || {};
  const usage = localState.usage || {};
  const providerRows = providerRowsOf(data, accountsPayload);
  const totals = computeTotals(data, usage, providerRows);
  const noAccounts = totals.accounts === 0;
  const supplierBits = [];
  if (totals.keyReady) supplierBits.push(`${fmtInt(totals.keyReady)} 个密钥直连`);
  if (totals.keyEmpty) supplierBits.push(`${fmtInt(totals.keyEmpty)} 个密钥待配置`);
  if (totals.accountProviders) supplierBits.push(`${fmtInt(totals.accountProviders)} 个账号型`);
  const supplierMeta = supplierBits.join(' · ') || '暂无可用凭证';
  const recent = Array.isArray(data.recent) ? data.recent : [];
  const series = Array.isArray(data.series) && data.series.length ? data.series : (Array.isArray(usage.series) ? usage.series : []);

  const refreshBtn = el('button', {
    class: 'btn btn-ghost btn-sm',
    type: 'button',
    title: '立即刷新',
    on: { click: async (event) => {
      const btn = event.currentTarget;
      btn.disabled = true;
      await loadAndRerender(root, ctx);
      btn.disabled = false;
    } },
  }, icon('refresh', 15), '刷新');

  page.appendChild(el('div', { class: 'page-head' },
    el('div', {},
      el('div', { class: 'eyebrow' },
        el('span', { class: 'live-dot', 'aria-hidden': 'true' }),
        'ENGINE OVERVIEW',
      ),
      el('h1', { class: 'page-title', text: '总览' }),
      el('p', { class: 'page-subtitle', text: '内置 OmniGate 引擎的实时健康、供应商与近 24 小时负载。' }),
    ),
    el('div', { class: 'page-actions' },
      el('span', { class: 'auto-refresh-note', text: '每 15 秒自动刷新' }),
      refreshBtn,
    ),
  ));

  // 身份信息（用户名 + 角色）已由外壳常驻展示：右上角 #session-chip 与侧边栏底部
  // #sidebar-session（见 main.js renderShellSession）。总览这里再放一条同样的
  // context-strip 属于重复，已移除；仅保留 Mock 数据提示。
  if (data.mock) {
    page.appendChild(el('div', { class: 'notice notice-warn' },
      icon('alert', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: 'Mock 数据' }),
        el('span', { text: '当前展示的是本地示例数据，未连接真实引擎。' }),
      ),
    ));
  }
  if (!state.authenticated) {
    page.appendChild(el('div', { class: 'notice notice-info' },
      icon('info', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '当前为匿名只读浏览' }),
        el('span', { text: '登录后可查看归属账号、实时额度与运行细节。' }),
      ),
      el('button', {
        class: 'btn btn-primary btn-sm',
        type: 'button',
        // 登录成功后 main.js 会因会话变化自动重载当前页，这里不再手动刷新，
        // 避免旧页面把路由刚挂载的新页面覆盖掉。
        on: { click: () => openLoginModal({ message: '登录后查看账号详情与平台配置。' }) },
      }, '登录'),
    ));
  }

  if (localState.error && !data.version) {
    page.appendChild(card({
      title: '概览加载失败',
      body: el('div', { class: 'error-panel' },
        icon('alert', 20),
        el('div', {},
          el('p', { text: localState.error }),
          el('button', {
            class: 'btn btn-ghost btn-sm',
            type: 'button',
            on: { click: () => loadAndRerender(root, ctx) },
          }, '重试'),
        ),
      ),
    }));
    root.appendChild(page);
    return;
  }

  page.appendChild(el('div', { class: 'kpi-grid' },
    kpiCard('服务状态', data.version ? '运行中' : (loading ? '连接中' : '未知'), data.version ? `版本 ${data.version}` : '', data.version ? 'ok' : 'muted', { iconName: 'power', tone: data.version ? 'ok' : 'muted' }),
    kpiCard(noAccounts ? '供应商' : '账号健康',
      noAccounts ? fmtInt(totals.providers) : `${fmtInt(totals.healthy)} / ${fmtInt(totals.accounts)}`,
      noAccounts ? supplierMeta : healthMeta(totals),
      noAccounts ? 'muted' : (totals.failed ? 'warn' : 'ok'),
      { iconName: noAccounts ? 'key' : 'shield' }),
    kpiCard('近 24h 请求', fmtCompact(totals.requests), totals.requests ? `成功率 ${fmtPercent(totals.successRate)}` : '暂无流量', 'accent', { iconName: 'activity' }),
    kpiCard('近 24h Tokens', fmtCompact(totals.tokens), `平均延迟 ${fmtDuration(totals.avgLatency)}`, 'muted', { iconName: 'models' }),
    kpiCard('近 24h 积分', fmtCredits(totals.credits), '按上游回传的 credit 统计', 'muted', { iconName: 'usage' }),
  ));

  page.appendChild(el('div', { class: 'overview-main-grid' },
    engineCard(data, totals, contextOf(data)),
    healthCard(totals, providerRows),
  ));

  page.appendChild(el('div', { class: 'overview-main-grid' },
    trendCard(series, ctx),
    locksCard(data.model_locks || [], totals),
  ));

  page.appendChild(providerCard(providerRows, ctx));
  page.appendChild(recentCard(recent, ctx, state));

  const footer = el('div', { class: 'page-footer-note' },
    el('span', { text: `运行 ${fmtUptime(data.uptime_sec)}` }),
    data.build ? el('span', { class: 'mono', text: `build ${data.build}` }) : null,
    el('button', {
      class: 'link-btn',
      type: 'button',
      on: { click: async () => {
        const ok = await copyText(JSON.stringify({ version: data.version, build: data.build, generated: data.generated }, null, 2));
        if (ok) toastSuccess('已复制版本信息');
      } },
    }, '复制版本信息'),
  );
  page.appendChild(footer);
  root.appendChild(page);
}

async function loadAndRerender(root, ctx) {
  try {
    const [overviewRes, accountsRes, usageRes] = await Promise.allSettled([
      api.overview.get(),
      api.accounts.list(),
      api.usage.get({ hours: 24 }),
    ]);
    const data = overviewRes.status === 'fulfilled' ? overviewRes.value : null;
    const accounts = accountsRes.status === 'fulfilled' ? accountsRes.value : null;
    const usage = usageRes.status === 'fulfilled' ? usageRes.value : null;
    localState = { data, accounts, usage, error: overviewRes.status === 'rejected' ? describeError(overviewRes.reason) : '' };
    render(root, ctx, {});
    if (overviewRes.status === 'rejected') toastError(overviewRes.reason);
  } catch (error) {
    toastError(error);
  }
}

function providerRowsOf(data, accountsPayload) {
  const rows = [];
  const seen = new Map();
  if (Array.isArray(data.providers)) {
    for (const item of data.providers) {
      if (!item || !item.name) continue;
      const accounts = Number(item.accounts ?? item.account_count ?? 0);
      const keys = Number(item.keys ?? 0);
      const keyBased = isKeyBasedProvider({ account_mode: item.account_mode }) || (accounts === 0 && keys > 0);
      seen.set(item.name, rows.length);
      rows.push({
        name: item.name,
        type: item.type || '',
        accountMode: item.account_mode || '',
        keyBased,
        keys,
        accounts,
        healthy: Number(item.healthy ?? 0),
        cooling: Number(item.cooling ?? 0),
        failed: Number(item.failed ?? item.disabled ?? 0),
        requests: Number(item.requests_24h ?? 0),
        tokens: Number(item.tokens_24h ?? 0),
      });
    }
  }
  const providers = Array.isArray(accountsPayload?.providers) ? accountsPayload.providers : [];
  for (const provider of providers) {
    let healthy = 0;
    let cooling = 0;
    let failed = 0;
    for (const account of provider.accounts || []) {
      if (account.auth_invalid) failed += 1;
      else if (parseDate(account.cooling_until)?.getTime() > Date.now()) cooling += 1;
      else if (account.has_token !== false) healthy += 1;
    }
    const accounts = (provider.accounts || []).length;
    const keys = Number(provider.keys ?? 0);
    const keyBased = isKeyBasedProvider({ account_mode: provider.account_mode }) || (accounts === 0 && keys > 0);
    if (seen.has(provider.name)) {
      const row = rows[seen.get(provider.name)];
      row.type = row.type || provider.type || '';
      row.accountMode = row.accountMode || provider.account_mode || '';
      row.keyBased = row.keyBased || keyBased;
      row.keys = Math.max(row.keys || 0, keys);
      row.accounts = Math.max(row.accounts, accounts);
      row.healthy = Math.max(row.healthy, healthy);
      row.cooling = Math.max(row.cooling, cooling);
      row.failed = Math.max(row.failed, failed);
      continue;
    }
    rows.push({
      name: provider.name,
      type: provider.type || '',
      accountMode: provider.account_mode || '',
      keyBased,
      keys,
      accounts,
      healthy,
      cooling,
      failed,
      requests: 0,
      tokens: 0,
    });
  }
  return rows.sort((a, b) => (b.accounts - a.accounts) || a.name.localeCompare(b.name));
}

function computeTotals(data, usage, providerRows) {
  const t = data.totals || {};
  const u = usage.totals || {};
  const accounts = Number(t.accounts ?? data.total ?? providerRows.reduce((s, p) => s + p.accounts, 0));
  const healthy = Number(t.healthy ?? data.healthy ?? providerRows.reduce((s, p) => s + p.healthy, 0));
  const cooling = Number(t.cooling ?? data.cooling ?? providerRows.reduce((s, p) => s + p.cooling, 0));
  const failed = Number(t.failed ?? data.disabled ?? providerRows.reduce((s, p) => s + p.failed, 0));
  const requests = Number(t.requests_24h ?? u.requests ?? 0);
  const errors = Number(u.errors ?? 0);
  let successRate = Number(t.success_rate ?? NaN);
  if (!Number.isFinite(successRate)) successRate = requests ? ((requests - errors) / requests) * 100 : 0;
  const keyProviders = providerRows.filter((p) => p.keyBased).length;
  const keyReady = providerRows.filter((p) => p.keyBased && p.keys > 0).length;
  return {
    accounts, healthy, cooling, failed,
    providers: providerRows.length,
    keyProviders,
    keyReady,
    keyEmpty: keyProviders - keyReady,
    accountProviders: providerRows.length - keyProviders,
    requests,
    tokens: Number(t.tokens_24h ?? u.total_tokens ?? 0),
    credits: Number(t.credits_24h ?? u.credits ?? 0),
    avgLatency: Number(t.avg_latency_ms ?? u.avg_latency_ms ?? 0),
    successRate: successRate <= 1 && t.success_rate != null ? successRate * 100 : successRate,
  };
}

function contextOf(data) {
  const bits = [];
  if (data.default_provider) bits.push(`默认供应商 ${data.default_provider}`);
  if (data.default_model) bits.push(`默认模型 ${data.default_model}`);
  if (data.redis_mode) bits.push(`会话存储 ${data.redis_mode}`);
  return bits;
}

function kpiCard(label, value, meta, tone = 'muted', { iconName = 'activity', tone: iconTone = tone } = {}) {
  return el('div', { class: `kpi-card kpi-${tone}` },
    el('div', { class: 'kpi-top' },
      el('span', { class: 'kpi-label', text: label }),
      el('span', { class: `kpi-icon kpi-icon-${iconTone}`, 'aria-hidden': 'true' }, icon(iconName, 16)),
    ),
    el('div', { class: 'kpi-value', text: value }),
    el('div', { class: 'kpi-meta', text: meta || ' ' }),
  );
}

function healthMeta(totals) {
  if (!totals.accounts) return '暂无账号';
  const bits = [`正常 ${totals.healthy}`];
  if (totals.cooling) bits.push(`冷却 ${totals.cooling}`);
  if (totals.failed) bits.push(`异常 ${totals.failed}`);
  if (totals.keyProviders) bits.push(`${totals.keyProviders} 个密钥型`);
  return bits.join(' · ');
}

function engineCard(data, totals, context) {
  const body = el('div', { class: 'engine-card-body' },
    el('div', { class: 'engine-version' },
      el('span', { class: 'engine-mark', 'aria-hidden': 'true' }, 'OG'),
      el('div', {},
        el('div', { class: 'engine-name', text: 'OmniGate Engine' }),
        el('div', { class: 'engine-sub', text: data.version ? `${data.version} · 运行 ${fmtUptime(data.uptime_sec)}` : '等待服务端响应' }),
      ),
      el('span', { class: 'engine-state' }, statusDot(data.version ? 'ok' : 'muted', data.version ? '运行中' : '未知')),
    ),
    el('div', { class: 'engine-facts' },
      fact('供应商', fmtInt(totals.providers || context.providers || 0)),
      totals.accounts
        ? fact('健康账号', `${fmtInt(totals.healthy)} / ${fmtInt(totals.accounts)}`)
        : fact('密钥直连', totals.keyReady ? `${fmtInt(totals.keyReady)} 个供应商` : '未配置'),
      fact('成功请求率', fmtPercent(totals.successRate)),
      fact('平均延迟', fmtDuration(totals.avgLatency)),
    ),
    context.length ? el('div', { class: 'fact-tags' }, context.map((text) => badge(text, 'muted'))) : null,
    el('div', { class: 'quick-actions' },
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, icon('server', 14), '供应商'),
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/routing' }, icon('routing', 14), '路由'),
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/usage' }, icon('usage', 14), '用量'),
    ),
  );
  return card({ title: '引擎状态', subtitle: '单进程、内嵌面板、OmniGate 多供应商运行时', body, class: 'engine-card' });
}

function fact(label, value) {
  return el('div', { class: 'fact' },
    el('span', { class: 'fact-label', text: label }),
    el('span', { class: 'fact-value', text: value }),
  );
}

function healthCard(totals, providerRows) {
  if (!totals.accounts) return supplierCard(totals, providerRows);
  const total = Math.max(1, totals.accounts);
  const segments = [
    { key: 'healthy', value: totals.healthy, tone: 'ok', label: '正常' },
    { key: 'cooling', value: totals.cooling, tone: 'warn', label: '冷却' },
    { key: 'failed', value: totals.failed, tone: 'danger', label: '异常' },
  ];
  const bar = el('div', { class: 'health-bar', attrs: { role: 'img', 'aria-label': `账号健康分布：正常 ${totals.healthy}，冷却 ${totals.cooling}，异常 ${totals.failed}` } });
  for (const seg of segments) {
    if (!seg.value) continue;
    bar.appendChild(el('span', { class: `health-seg health-${seg.tone}`, style: { width: `${(seg.value / total) * 100}%` } }));
  }
  if (!totals.accounts) bar.appendChild(el('span', { class: 'health-seg health-empty', style: { width: '100%' } }));

  const rows = providerRows.slice(0, 6);
  return card({
    title: '账号健康',
    subtitle: '按供应商聚合，异常优先',
    body: el('div', {},
      el('div', { class: 'health-summary' },
        el('div', { class: 'health-number' },
          el('span', { class: 'health-big', text: totals.accounts ? fmtPercent((totals.healthy / totals.accounts) * 100) : '—' }),
          el('span', { class: 'health-caption', text: '账号可用率' }),
        ),
        el('div', { class: 'health-legend' }, segments.map((s) => el('div', { class: 'legend-row' },
          el('span', { class: `legend-dot health-${s.tone}`, 'aria-hidden': 'true' }),
          el('span', { class: 'legend-label', text: s.label }),
          el('span', { class: 'legend-value', text: String(s.value) }),
        ))),
      ),
      bar,
      rows.length
        ? el('div', { class: 'health-list' }, rows.map((row) => {
          const parts = [];
          if (row.healthy) parts.push(`${row.healthy} 正常`);
          if (row.cooling) parts.push(`${row.cooling} 冷却`);
          if (row.failed) parts.push(`${row.failed} 异常`);
          return el('a', { class: 'health-row', href: `#/providers?name=${encodeURIComponent(row.name)}` },
            el('span', { class: 'health-provider', text: row.name }),
            el('span', { class: 'health-type', text: providerLabel(row.type) }),
            statusDot(row.failed ? 'danger' : (row.cooling ? 'warn' : (row.accounts ? 'ok' : 'muted')),
              parts.join(' · ') || '空壳'),
          );
        }))
        : emptyState('暂无供应商', '配置供应商后会在这里显示健康状态。'),
    ),
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, '查看全部'),
  });
}

// 密钥型供应商（openai/opencode 等 AccountModeNone）没有账号池：总览不再用
// 「账号健康 0/0」和「空壳」来描述它们，改为展示供应商与上游密钥状态。
function supplierCard(totals, providerRows) {
  const keyCount = providerRows.filter((row) => row.keyBased).length;
  const rows = providerRows.slice(0, 6);
  const legend = (tone, label, value) => el('div', { class: 'legend-row' },
    el('span', { class: `legend-dot health-${tone}`, 'aria-hidden': 'true' }),
    el('span', { class: 'legend-label', text: label }),
    el('span', { class: 'legend-value', text: String(value) }),
  );
  return card({
    title: '供应商状态',
    subtitle: '当前使用供应商级密钥直连，不维护账号池',
    body: el('div', {},
      el('div', { class: 'health-summary' },
        el('div', { class: 'health-number' },
          el('span', { class: 'health-big', text: fmtInt(totals.providers) }),
          el('span', { class: 'health-caption', text: '供应商' }),
        ),
        el('div', { class: 'health-legend' },
          legend('ok', '密钥直连', keyCount),
          providerRows.length - keyCount ? legend('empty', '账号型', providerRows.length - keyCount) : null,
        ),
      ),
      rows.length
        ? el('div', { class: 'health-list' }, rows.map((row) => {
          let tone = 'muted';
          let text = '未配置账号';
          if (row.keyBased) {
            tone = row.keys ? 'ok' : 'warn';
            text = row.keys ? `${row.keys} 把上游密钥` : '未配置密钥';
          } else if (row.accounts) {
            tone = row.failed ? 'danger' : (row.cooling ? 'warn' : 'ok');
            text = `${row.healthy} / ${row.accounts} 账号`;
          }
          return el('a', { class: 'health-row', href: `#/providers?name=${encodeURIComponent(row.name)}` },
            el('span', { class: 'health-provider', text: row.name }),
            el('span', { class: 'health-type', text: providerLabel(row.type) }),
            statusDot(tone, text),
          );
        }))
        : emptyState('暂无供应商', '配置供应商后会在这里显示密钥与接入状态。'),
    ),
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, '管理供应商'),
  });
}

function trendCard(series, ctx) {
  const points = series.slice(-32).map((p) => ({
    t: p.t,
    requests: Number(p.requests ?? 0),
    tokens: Number(p.total_tokens ?? ((p.prompt_tokens || 0) + (p.completion_tokens || 0)) ?? 0),
    credits: Number(p.credits ?? 0),
  }));
  const max = Math.max(1, ...points.map((p) => p.requests));
  const svg = svgEl('svg', {
    viewBox: `0 0 ${Math.max(320, points.length * 22)} 74`, class: 'mini-chart', preserveAspectRatio: 'none',
    role: 'img', 'aria-label': '近 24 小时请求趋势',
  });
  const chartWidth = Math.max(320, points.length * 22);
  const step = chartWidth / Math.max(1, points.length);
  points.forEach((p, i) => {
    const h = Math.max(2, (p.requests / max) * 62);
    const x = i * step + 2;
    const rect = svgEl('rect', {
      x, y: 68 - h, width: Math.max(2, step - 4), height: h, rx: 2, class: 'mini-bar',
    });
    const title = svgEl('title');
    title.textContent = `${fmtTime(p.t)} · ${fmtInt(p.requests)} 请求 · ${fmtCompact(p.tokens)} tokens`;
    rect.appendChild(title);
    svg.appendChild(rect);
  });
  const totalReq = points.reduce((s, p) => s + p.requests, 0);
  return card({
    title: '近 24 小时趋势',
    subtitle: `${fmtInt(totalReq)} 次请求 · 峰值 ${fmtInt(max)} 次/桶`,
    body: el('div', { class: 'trend-card-body' },
      points.length ? svg : emptyState('暂无趋势', '有请求后这里会显示每小时请求量。'),
    ),
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/usage' }, '用量详情'),
  });
}

function locksCard(locks, totals) {
  const hasAccounts = totals?.accounts > 0;
  return card({
    title: '模型锁与冷却',
    subtitle: hasAccounts ? '账号与密钥的限流、额度耗尽与恢复时间' : '上游密钥的限流、额度耗尽与恢复时间',
    body: locks.length
      ? el('div', { class: 'lock-list' }, locks.map((lock) => el('div', { class: 'lock-row' },
        el('div', { class: 'lock-main' },
          el('span', { class: 'lock-model mono', text: lock.model || '未知模型' }),
          badge(lock.kind === 'quota' ? '额度' : (lock.kind === 'rate_limit' ? '限流' : (lock.kind || '锁定')), lock.kind === 'quota' ? 'warn' : 'danger'),
        ),
        el('div', { class: 'lock-meta' },
          el('span', { text: lock.accounts != null ? `${lock.accounts} 个账号` : (hasAccounts ? '' : '密钥') }),
          lock.until ? el('span', { text: `恢复 ${fmtRelative(lock.until)}` }) : null,
        ),
        lock.reason ? el('p', { class: 'lock-reason', text: lock.reason }) : null,
      )))
      : emptyState('没有模型锁', hasAccounts ? '当前没有账号或密钥处于限流、额度耗尽状态。' : '当前没有密钥处于限流、额度耗尽状态。'),
  });
}

function providerCard(rows, ctx) {
  const hasAccounts = rows.some((row) => row.accounts > 0);
  const hasKeys = rows.some((row) => row.keyBased);
  const table = createTable({
    columns: [
      {
        key: 'name',
        label: '供应商',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.name }),
          el('span', { class: 'cell-sub', text: providerLabel(row.type) }),
        ),
      },
      {
        key: 'accounts',
        label: hasAccounts ? (hasKeys ? '账号 / 接入' : '账号') : '接入方式',
        align: 'right',
        render: (row) => {
          if (row.keyBased) {
            return el('span', { class: 'mono', text: row.keys ? `${row.keys} 把密钥` : '密钥未配置' });
          }
          if (!row.accounts) return el('span', { class: 'muted', text: '未配置' });
          return `${fmtInt(row.healthy)} / ${fmtInt(row.accounts)}`;
        },
      },
      {
        key: 'health',
        label: '健康',
        render: (row) => {
          if (row.keyBased) {
            return statusDot(row.keys ? 'ok' : 'warn', row.keys ? '密钥直连' : '未配置密钥');
          }
          return statusDot(row.failed ? 'danger' : (row.cooling ? 'warn' : (row.accounts ? 'ok' : 'muted')),
            row.accounts ? `${row.healthy} 正常${row.cooling ? ` · ${row.cooling} 冷却` : ''}` : '空壳');
        },
      },
      {
        key: 'requests',
        label: '24h 请求',
        align: 'right',
        render: (row) => (row.requests ? fmtCompact(row.requests) : '—'),
      },
    ],
    rows,
    empty: '还没有配置供应商',
    onRowClick: (row) => ctx.navigate(`/providers?name=${encodeURIComponent(row.name)}`),
  });
  return card({
    title: '供应商健康',
    subtitle: '点击一行进入供应商详情',
    body: table.el,
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, '管理供应商'),
  });
}

function requestSource(account) {
  const value = String(account || '').trim();
  return (!value || value === 'api-key') ? '密钥直连' : value;
}

function recentCard(recent, ctx, state) {
  if (!Array.isArray(recent) || !recent.length) {
    return card({
      title: '近期请求',
      subtitle: '来自请求归档的最近记录',
      body: emptyState(state.authenticated ? '暂无请求' : '登录后查看请求明细',
        state.authenticated ? '引擎收到请求后会在这里显示。' : '匿名访客没有账号级请求记录。',
        state.authenticated ? null : el('button', {
          class: 'btn btn-primary btn-sm', type: 'button',
          on: { click: () => openLoginModal({ message: '登录后查看请求记录。' }) },
        }, '登录')),
      actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/requests' }, '全部请求'),
    });
  }
  const table = createTable({
    columns: [
      { key: 'time', label: '时间', render: (row) => fmtTime(row.time), width: '96px' },
      {
        key: 'provider',
        label: '供应商 / 来源',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.provider || '—' }),
          el('span', { class: 'cell-sub', text: requestSource(row.account) }),
        ),
      },
      { key: 'model', label: '模型', render: (row) => el('span', { class: 'mono truncate', text: row.model || '—' }) },
      {
        key: 'status',
        label: '结果',
        render: (row) => badge(`${row.status || '—'} · ${outcomeLabel(row.outcome)}`, row.ok ? 'ok' : (row.status === 429 ? 'warn' : 'danger')),
      },
      { key: 'duration_ms', label: '耗时', align: 'right', render: (row) => fmtDuration(row.duration_ms) },
      {
        key: 'tokens',
        label: 'Tokens',
        align: 'right',
        render: (row) => fmtCompact(row.total_tokens || 0),
      },
    ],
    rows: recent,
    onRowClick: () => ctx.navigate('/requests'),
  });
  return card({
    title: '近期请求',
    subtitle: '最近完成的 8 次调用',
    body: table.el,
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/requests' }, '查看全部'),
  });
}
