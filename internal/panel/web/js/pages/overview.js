// overview.js — 总览：引擎健康、账号池、24h 趋势、近期请求与模型锁。

import { api, describeError } from '../core/api.js';
import { authState, isAdmin, isAuthenticated, isViewer, openLoginModal } from '../core/auth.js';
import {
  el, svgEl, card, badge, statusDot, emptyState, loadingBlock, icon, copyText, pageBar,
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
  // loaded：三个只读接口里至少有一个真的回来了。
  //
  // 首次渲染发生在 await 之前，data/accounts/usage 全是空对象。此时若照常计算，
  // 页面会先断言「0 个供应商 / 暂无可用凭证 / 还没有配置供应商 / 等待服务端响应」，
  // 然后在数据到达后被改写——把「还没读到」当成「没有」，正是这些卡片最容易
  // 误导人的地方。未加载完成时数字位统一留空，不写结论。
  const loaded = !!(localState.data || localState.accounts || localState.usage);
  const noAccounts = loaded && totals.accounts === 0;
  // providersKnown：供应商清单是否真的读到了。登录用户的数据来自总览接口，
  // 匿名访客只从 /panel/api/omni/accounts 拿到（后端按归属过滤后仍保留 provider
  // 元信息）。两个来源都没有时只能写「—」，不能报 0 个供应商。
  const providersKnown = !!(Array.isArray(data.providers) || localState.accounts);
  const supplierBits = [];
  // 这些数字数的是「供应商」，不是「密钥」：一个密钥型供应商可能配了多把
  // 上游密钥（线上就有 2/2/1/1/1）。写成「N 个密钥直连」会被读成密钥总数。
  if (totals.keyReady) supplierBits.push(`${fmtInt(totals.keyReady)} 个密钥型（直连）`);
  if (totals.keyEmpty) supplierBits.push(`${fmtInt(totals.keyEmpty)} 个密钥型（待配置）`);
  if (totals.accountProviders) supplierBits.push(`${fmtInt(totals.accountProviders)} 个账号型`);
  const supplierMeta = !loaded ? '' : (supplierBits.join(' · ') || '暂无可用凭证');
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

  // 普通用户的账号与用量都经过归属过滤：KPI 里的「账号健康」不是平台全量。
  // 不写清楚，用户会把自己名下的 3/5 读成平台只有 5 个账号。密钥型供应商的
  // 流量是平台共享的，和个人账号口径不同，也要在同一句里说明。
  const personalScope = state.authenticated && state.role === 'user';
  page.appendChild(pageBar({
    context: [
      ...(personalScope
        ? [
          badge('个人范围', 'info'),
          el('span', { text: '账号与用量仅统计归属你的账号；平台密钥型供应商的共享流量不在个人范围内。' }),
        ]
        : []),
      el('span', { class: 'live-dot', 'aria-hidden': 'true' }),
      el('span', { text: '每 15 秒自动刷新' }),
    ],
    actions: [refreshBtn],
  }));

  // 身份信息（用户名 + 角色）已由外壳常驻展示：右上角 #session-chip 与侧边栏底部
  // #sidebar-session（见 main.js renderShellSession）。总览不再重复一条同样的
  // 说明，只保留 Mock 数据提示。
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

  page.appendChild(el('div', { class: 'kpi-grid kpi-grid-4' },
    kpiCard('服务状态', data.version ? '运行中' : (loading ? '连接中' : '未知'), data.version ? `版本 ${data.version}` : '', data.version ? 'ok' : 'muted', { iconName: 'power', tone: data.version ? 'ok' : 'muted' }),
    kpiCard(!loaded ? '供应商 / 账号' : (noAccounts ? '供应商' : '账号健康'),
      !loaded || !providersKnown ? '—' : (noAccounts ? fmtInt(totals.providers) : `${fmtInt(totals.healthy)} / ${fmtInt(totals.accounts)}`),
      !loaded
        ? ''
        : (!providersKnown ? '供应商明细未读取到' : (noAccounts ? supplierMeta : healthMeta(totals))),
      !loaded || !providersKnown ? 'muted' : (noAccounts ? 'muted' : (totals.failed ? 'warn' : 'ok')),
      { iconName: noAccounts ? 'key' : 'shield' }),
    // 匿名的用量接口按归属过滤，返回的 0 意思是「没有你看得到的记录」，
    // 不是「平台这段时间没有请求」。直接写 0 等于替平台宣布空转。
    kpiCard('近 24h 请求', !loaded ? '—' : (state.authenticated ? fmtCompact(totals.requests) : '—'),
      !loaded ? '' : (state.authenticated ? (totals.requests ? `成功率 ${fmtPercent(totals.successRate)}` : '暂无流量') : '登录后统计'),
      'accent', { iconName: 'activity' }),
    kpiCard('近 24h 消耗', !loaded ? '—' : (state.authenticated ? fmtCompact(totals.tokens) : '—'),
      !loaded ? '' : (state.authenticated ? `${fmtCredits(totals.credits)} 积分 · 均延迟 ${fmtDuration(totals.avgLatency)}` : '登录后统计'),
      'muted', { iconName: 'models' }),
  ));

  // 顺序按「先看结论，再看构成」：健康与流量表在最前，其次引擎版本与趋势，
  // 最后是模型锁与近期请求。原来健康/供应商是两张卡并排，说的却是同一批供应商。
  page.appendChild(healthCard(totals, providerRows, ctx, { loaded, providersKnown }));

  page.appendChild(el('div', { class: 'overview-main-grid' },
    engineCard(data, totals, contextOf(data), { loading }),
    trendCard(series, ctx, { loaded }),
  ));

  page.appendChild(locksCard(data.model_locks || [], totals, {
    locksKnown: Array.isArray(data.model_locks),
    loaded,
  }));
  page.appendChild(recentCard(recent, ctx, state, { loaded }));

  const footer = el('div', { class: 'page-footer-note' },
    loaded ? el('span', { text: `运行 ${fmtUptime(data.uptime_sec)}` }) : null,
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
  // 匿名访客：后端把 providers 明细与账号列表一起按归属过滤掉了，accounts 恒为空。
  // 这个空代表「看不到」，行里不能据此写「未配置 / 无账号」。
  const anonymous = !authState().authenticated;
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
        // 账号接口下发的 keys 计数是匿名也可见的元信息；缺字段才是「不可见」。
        keysKnown: item.keys != null,
        accountsKnown: !anonymous,
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
      if (provider.keys != null) row.keysKnown = true;
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
      keysKnown: provider.keys != null,
      accountsKnown: !anonymous,
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

function engineCard(data, totals, context, { loading = false } = {}) {
  // 引擎卡原来的四格事实（供应商 / 健康账号 / 成功率 / 平均延迟）与顶部 KPI
  // 完全重复。现在只回答 KPI 回答不了的问题：跑的是什么版本、跑了多久、
  // 当前生效的路由默认值。数字统一留给 KPI 条。
  const body = el('div', { class: 'engine-card-body' },
    el('div', { class: 'engine-version' },
      el('span', { class: 'engine-mark', 'aria-hidden': 'true' }, 'OG'),
      el('div', {},
        el('div', { class: 'engine-name', text: 'OmniGate Engine' }),
        // 「等待服务端响应」只在首屏还没回来时出现；若真的失败了，下面的错误卡
        // 会说清原因，这里不能把「不知道」写成「运行中」，也不能长期挂着等待。
        el('div', {
          class: 'engine-sub',
          text: data.version
            ? `${data.version} · 运行 ${fmtUptime(data.uptime_sec)}`
            : (loading ? '读取中…' : '版本未知'),
        }),
      ),
      el('span', { class: 'engine-state' }, statusDot(
        data.version ? 'ok' : (loading ? 'info' : 'muted'),
        data.version ? '运行中' : (loading ? '读取中' : '未知'),
      )),
    ),
    context.length ? el('div', { class: 'fact-tags' }, context.map((text) => badge(text, 'muted'))) : null,
    el('div', { class: 'quick-actions' },
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, icon('server', 14), '供应商'),
      // 路由配置只对 admin/viewer 可读；普通用户与匿名访客的入口已在导航层收窄，
      // 总览的快捷入口也必须同口径，否则会留下一条点进去只有提示的链接。
      (isAdmin() || isViewer()) ? el('a', { class: 'btn btn-ghost btn-sm', href: '#/routing' }, icon('routing', 14), '路由') : null,
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/usage' }, icon('usage', 14), '用量'),
    ),
  );
  return card({ title: '引擎状态', subtitle: '版本、运行时长与当前生效的路由默认值', body, class: 'engine-card' });
}

function healthCard(totals, providerRows, ctx, { loaded = true, providersKnown = true } = {}) {
  const total = Math.max(1, totals.accounts || 0);
  const segments = [
    { key: 'healthy', value: totals.healthy, tone: 'ok', label: '正常' },
    { key: 'cooling', value: totals.cooling, tone: 'warn', label: '冷却' },
    { key: 'failed', value: totals.failed, tone: 'danger', label: '异常' },
  ];
  const bar = el('div', {
    class: 'health-bar',
    attrs: { role: 'img', 'aria-label': `账号健康分布：正常 ${totals.healthy}，冷却 ${totals.cooling}，异常 ${totals.failed}` },
  });
  if (totals.accounts) {
    for (const seg of segments) {
      if (!seg.value) continue;
      bar.appendChild(el('span', { class: `health-seg health-${seg.tone}`, style: { width: `${(seg.value / total) * 100}%` } }));
    }
  } else {
    bar.appendChild(el('span', { class: 'health-seg health-empty', style: { width: '100%' } }));
  }

  // 之前「账号健康」和「供应商健康」是两张卡，列的却是同一批供应商：一张说账号
  // 可用率、一张说 24h 请求，读的人要在两处来回对照。合并成一张表，一行说清
  // 一个供应商的接入形态、健康与流量；账号可用率保留为表头摘要。
  const tableRows = providerRows.map((row) => row);
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
        label: '接入',
        align: 'right',
        render: (row) => {
          if (row.keyBased) {
            if (!row.keysKnown) return el('span', { class: 'muted', text: '凭证不可见' });
            return el('span', { class: row.keys ? 'mono' : 'mono muted', text: row.keys ? `${row.keys} 把密钥` : '缺凭证' });
          }
          // 匿名的账号数组被后端按归属过滤成空：那是「看不到」，不是「没配置」。
          // 与「健康」列说同一件事时只保留一处，避免同一行出现两遍「账号不可见」。
          if (!row.accountsKnown) return el('span', { class: 'muted', text: '—' });
          // 账号型供应商 0 个账号时，「未配置」与健康列的「无账号」是同一句话，
          // 同一行写两遍只是噪音；让健康列说，这里留空。
          if (!row.accounts) return el('span', { class: 'muted', text: '—' });
          return el('span', { class: 'mono', text: `${fmtInt(row.healthy)} / ${fmtInt(row.accounts)}` });
        },
      },
      {
        key: 'health',
        label: '健康',
        render: (row) => {
          if (row.keyBased) {
            if (!row.keysKnown) return statusDot('muted', '状态不可见');
            return statusDot(row.keys ? 'ok' : 'warn', row.keys ? '密钥直连' : '未配置密钥');
          }
          if (!row.accountsKnown) return statusDot('muted', '账号不可见');
          const parts = [];
          if (row.healthy) parts.push(`${row.healthy} 正常`);
          if (row.cooling) parts.push(`${row.cooling} 冷却`);
          if (row.failed) parts.push(`${row.failed} 异常`);
          return statusDot(
            row.failed ? 'danger' : (row.cooling ? 'warn' : (row.accounts ? 'ok' : 'muted')),
            parts.join(' · ') || '无账号',
          );
        },
      },
      {
        key: 'requests',
        label: '24h 请求',
        align: 'right',
        render: (row) => (row.requests ? fmtCompact(row.requests) : '—'),
      },
    ],
    rows: tableRows,
    // 匿名视角的供应商明细来自 /panel/api/omni/accounts；这次没读到就写「还没有
    // 配置供应商」，等于替平台宣布它是空库。
    empty: !loaded
      ? '正在读取供应商…'
      : (!providersKnown ? '供应商明细未读取到' : '还没有配置供应商'),
    onRowClick: (row) => ctx.navigate(`/providers?name=${encodeURIComponent(row.name)}`),
  });

  const state = authState();
  const anonymous = !state.authenticated;
  const personal = state.authenticated && state.role === 'user';
  return card({
    title: (totals.accounts && !anonymous) ? '账号与供应商健康' : '供应商状态',
    subtitle: !loaded
      ? '正在读取供应商状态'
      : (anonymous
        ? '匿名视角只显示供应商级凭证信息，账号池明细需登录'
        : (totals.accounts
          ? `账号可用率 ${fmtPercent((totals.healthy / total) * 100)} · 异常优先`
          : (personal
            // 普通用户没有归属账号，不等于平台「不维护账号池」；后者是管理员
            // 视角才能下的结论。
            ? '仅显示归属你的账号；平台账号池不可见'
            : '当前使用供应商级密钥直连，不维护账号池'))),
    body: el('div', { class: 'stack-sm' },
      // 匿名时 accounts 恒为 0，那不是「账号全都不可用」，所以不画健康条。
      (totals.accounts && !anonymous)
        ? el('div', { class: 'health-legend health-legend-inline' }, segments.map((s) => el('div', { class: 'legend-row' },
          el('span', { class: `legend-dot health-${s.tone}`, 'aria-hidden': 'true' }),
          el('span', { class: 'legend-label', text: s.label }),
          el('span', { class: 'legend-value', text: String(s.value) }),
        )))
        : null,
      (totals.accounts && !anonymous) ? bar : null,
      table.el,
    ),
    // 只有管理员能在供应商页改配置；其他角色点进去是只读视图，
    // 写「管理」会误导成当前角色有写权限。
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/providers' }, isAdmin() ? '管理供应商' : '查看供应商'),
  });
}

function trendCard(series, ctx, { loaded = true } = {}) {
  // 匿名的用量接口按归属过滤，series 恒为空。这里画「暂无趋势」没问题，但
  // 副标题写「0 次请求 · 峰值 1 次/桶」等于替平台宣布空转，所以分三种讲法。
  const anonymous = !isAuthenticated();
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
    subtitle: !loaded
      ? ''
      : (anonymous
        ? '需要登录'
        : `${fmtInt(totalReq)} 次请求 · 峰值 ${fmtInt(max)} 次/桶`),
    body: el('div', { class: 'trend-card-body' },
      !loaded
        ? loadingBlock('加载用量')
        : (anonymous
          ? emptyState('趋势不可见', '按时间桶的请求量属于账号级数据，登录后按角色显示。')
          : (points.length ? svg : emptyState('暂无趋势', '有请求后这里会显示每小时请求量。'))),
    ),
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/usage' }, '用量详情'),
  });
}

function locksCard(locks, totals, { locksKnown = false, loaded = true } = {}) {
  const hasAccounts = totals?.accounts > 0;
  // 三个状态必须分开：读得到且有锁、读得到且没锁、读不到。
  // 之前只判断数组长度，缺字段（匿名 / 普通用户）会渲染成「没有模型锁」——
  // 而线上 atria 两把密钥正被额度耗尽禁用中。
  // 加载中既不是「没有」也不是「不可见」：那是还没读到，给加载块。
  const body = !loaded
    ? loadingBlock('加载密钥状态')
    : (!locksKnown
      ? emptyState('锁定状态不可见', '密钥锁定与恢复时间属于平台级状态；登录管理员或只读账号后可见。')
      : (locks.length
        ? el('div', { class: 'lock-list' }, locks.map((lock) => {
          const target = lock.provider || lock.model || '未知对象';
          const kindLabel = lock.kind === 'quota' ? '额度' : (lock.kind === 'rate_limit' ? '限流' : (lock.kind === 'auth' ? '凭证失效' : (lock.kind || '锁定')));
          return el('div', { class: 'lock-row' },
            el('div', { class: 'lock-main' },
              el('span', { class: 'lock-model mono', text: target }),
              badge(kindLabel, lock.kind === 'quota' ? 'warn' : 'danger'),
            ),
            el('div', { class: 'lock-meta' },
              el('span', { text: lock.keys != null ? `${lock.keys} 把密钥` : (lock.accounts != null ? `${lock.accounts} 个账号` : '') }),
              lock.until ? el('span', { text: `恢复 ${fmtRelative(lock.until)}` }) : el('span', { text: '暂无恢复时间' }),
            ),
            lock.reason ? el('p', { class: 'lock-reason', text: lock.reason }) : null,
          );
        }))
        : emptyState('没有模型锁', hasAccounts ? '当前没有账号或密钥处于限流、额度耗尽状态。' : '当前没有密钥处于限流、额度耗尽状态。')));
  return card({
    title: '模型锁与冷却',
    subtitle: !loaded
      ? '正在读取密钥状态'
      : (locksKnown
        ? (hasAccounts ? '账号与密钥的限流、额度耗尽与恢复时间' : '上游密钥的限流、额度耗尽与恢复时间')
        : '需要平台读取权限'),
    body,
  });
}

function requestSource(account) {
  const value = String(account || '').trim();
  return (!value || value === 'api-key') ? '密钥直连' : value;
}

function recentCard(recent, ctx, state, { loaded = true } = {}) {
  if (!Array.isArray(recent) || !recent.length) {
    return card({
      title: '近期请求',
      subtitle: '来自请求归档的最近记录',
      body: !loaded
        ? loadingBlock('加载请求记录')
        : emptyState(state.authenticated ? '暂无请求' : '登录后查看请求明细',
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
