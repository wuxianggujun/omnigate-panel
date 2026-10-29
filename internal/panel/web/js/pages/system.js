// system.js — 系统设置：访问密钥、面板账号、出站代理、运行参数、存储。
//
// 写权限：仅 admin。viewer 只读。user 只看自己的面板账号（改自己的密码），
// 平台配置接口对 user 返回 403。供应商账号归属（provider/label）在「供应商」页。

import { api, describeError, isForbidden } from '../core/api.js';
import {
  authState, canWrite, isAdmin, isAuthenticated, openLoginModal, requireAdmin,
} from '../core/auth.js';
import {
  el, card, badge, emptyState, loadingBlock, icon, copyText, pageBar,
} from '../core/dom.js';
import {
  deepClone, flattenObject, fmtBytes, fmtDateTime, fmtInt, fmtRelative, fmtTime, fmtUptime,
  normalizeKeyList, providerLabel, roleLabel, setPath, truncate,
} from '../core/format.js';
import { createTabs } from '../components/tabs.js';
import { createTable } from '../components/table.js';
import { createSchemaForm } from '../components/schema-form.js';
import { createSecretField } from '../components/secret-field.js';
import { openModal, confirmDialog } from '../components/modal.js';
import { toast, toastError, toastSuccess, toastWarn } from '../components/toast.js';

const RUNTIME_FIELDS = [
  { name: 'mode', label: '运行模式', type: 'text', group: '运行模式', placeholder: 'text' },
  { name: 'reasoning', label: '默认启用推理', type: 'bool', group: '运行模式', help: '客户端未指定时是否请求上游推理能力' },
  { name: 'model_whitelist', label: '对外模型白名单', type: 'list', group: '模型', help: '留空 = 放行全部；填写对外暴露的完整模型 ID 或别名' },
  { name: 'fallback.enabled', label: '第三方兜底', type: 'bool', group: '模型', default: false, help: 'Gemini 失败或空回复时切换到第三方模型' },
  { name: 'fallback.models', label: '兜底候选模型', type: 'list', group: '模型', help: '留空 = 自动选择所有可聊天的第三方模型', visible_if: { field: 'fallback.enabled', truthy: true } },
  { name: 'credit_routing.enabled', label: '快过期积分优先', type: 'bool', group: '积分路由', default: true },
  { name: 'credit_routing.expiring_soon', label: '快过期窗口', type: 'duration', group: '积分路由', default: '168h', help: 'Go duration，例如 168h' },
  { name: 'credit_routing.min_expiring', label: '最低触发积分', type: 'number', group: '积分路由', default: 5, help: '低于该值不参与快过期优先' },
  { name: 'credit_routing.refresh_interval', label: '余额快照刷新间隔', type: 'duration', group: '积分路由', default: '10m' },
  { name: 'fingerprint.jitter_enabled', label: '请求抖动', type: 'bool', group: '指纹伪装', default: false, advanced: true },
  { name: 'fingerprint.version_sync_enabled', label: 'Chrome 版本同步', type: 'bool', group: '指纹伪装', default: false, advanced: true },
  { name: 'fingerprint.version_sync_interval_hours', label: '版本同步间隔（小时）', type: 'number', group: '指纹伪装', default: 24, advanced: true },
  { name: 'fingerprint.tls_enabled', label: 'TLS ClientHello 伪装', type: 'bool', group: '指纹伪装', default: true, advanced: true },
  { name: 'maintenance.keepalive_enabled', label: 'Gemini 会话保活', type: 'bool', group: '后台维护', default: false },
  { name: 'maintenance.keepalive_interval_minutes', label: '保活间隔（分钟）', type: 'number', group: '后台维护', default: 30 },
  { name: 'maintenance.chat_cleanup_enabled', label: '网页会话清理', type: 'bool', group: '后台维护', default: false, help: '会真实删除上游会话，默认关闭' },
  { name: 'maintenance.chat_cleanup_keep_hours', label: '会话保留（小时）', type: 'number', group: '后台维护', default: 24, advanced: true, visible_if: { field: 'maintenance.chat_cleanup_enabled', truthy: true } },
  { name: 'maintenance.chat_cleanup_interval_hours', label: '清理间隔（小时）', type: 'number', group: '后台维护', default: 6, advanced: true, visible_if: { field: 'maintenance.chat_cleanup_enabled', truthy: true } },
  { name: 'maintenance.chat_cleanup_skip_pinned', label: '跳过置顶会话', type: 'bool', group: '后台维护', default: true, advanced: true, visible_if: { field: 'maintenance.chat_cleanup_enabled', truthy: true } },
  { name: 'rate_limit.enabled', label: '入站 IP 限流', type: 'bool', group: '限流', default: false },
  { name: 'rate_limit.window', label: '限流窗口（秒）', type: 'number', group: '限流', default: 60, visible_if: { field: 'rate_limit.enabled', truthy: true } },
  { name: 'rate_limit.max', label: '窗口内最大请求', type: 'number', group: '限流', default: 10, visible_if: { field: 'rate_limit.enabled', truthy: true } },
  { name: 'rate_limit.trust_proxy', label: '信任代理头', type: 'bool', group: '限流', default: false, advanced: true, visible_if: { field: 'rate_limit.enabled', truthy: true }, help: '仅在反向代理会覆写 X-Forwarded-For 时开启' },
  { name: 'checkin.enabled', label: '定时签到', type: 'bool', group: '签到', default: true },
  { name: 'checkin.interval_minutes', label: '签到间隔（分钟）', type: 'number', group: '签到', default: 360, visible_if: { field: 'checkin.enabled', truthy: true } },
  { name: 'checkin.jitter_minutes', label: '签到抖动（分钟）', type: 'number', group: '签到', default: 30, visible_if: { field: 'checkin.enabled', truthy: true } },
  { name: 'checkin.tasks', label: '签到任务', type: 'json', group: '签到', rows: 12, help: 'JSON 数组；每项包含 name/type/provider/interval_minutes。', advanced: true },
];

const PROXY_FIELDS = [
  { name: 'name', label: '代理名称', type: 'text', required: true, group: '基础' },
  { name: 'url', label: '代理 URL', type: 'url', group: '基础', placeholder: 'http://127.0.0.1:7890', help: '单代理填这里；使用代理池时留空' },
  { name: 'username', label: '代理用户名', type: 'text', group: '认证', advanced: true },
  { name: 'password', label: '代理密码', type: 'secret', group: '认证', advanced: true },
  { name: 'pool', label: '代理池列表', type: 'list', group: '代理池', help: '每行一个 host:port 或完整 URL', advanced: true },
  { name: 'pool_url', label: '远程池地址', type: 'url', group: '代理池', advanced: true },
  { name: 'pool_scheme', label: '池协议', type: 'enum', group: '代理池', options: ['http', 'https', 'socks5', 'socks5h'], default: 'http', advanced: true },
  { name: 'refresh_sec', label: '池刷新间隔（秒）', type: 'number', group: '代理池', default: 300, advanced: true },
  { name: 'probe_url', label: '探活地址', type: 'url', group: '代理池', default: 'https://www.gstatic.com/generate_204', advanced: true },
  { name: 'strict', label: '无可用代理时禁止直连', type: 'bool', group: '代理池', default: false, advanced: true },
];

let pageSignal = null;
let rootEl = null;
let ui = initialState();

function initialState() {
  return {
    loading: true,
    tab: 'access',
    allowRegistration: false,
    config: null,
    configPath: '',
    configError: '',
    users: null,
    userMeta: null,
    usersError: '',
    outbound: null,
    outboundError: '',
    outboundDirty: false,
    overview: null,
    overviewError: '',
    models: [],
    loadError: '',
  };
}

export async function mount(root) {
  pageSignal = new AbortController();
  rootEl = root;
  ui = initialState();
  ui.tab = isAdmin() ? 'access' : 'accounts';
  render();
  await loadAll();
}

export function unmount() {
  if (pageSignal) pageSignal.abort();
  pageSignal = null;
  rootEl = null;
  ui = initialState();
}

async function loadAll() {
  if (!pageSignal) return;
  ui.loading = true;
  render();
  const auth = authState();
  const requests = [
    api.config.get({ signal: pageSignal.signal }),
    api.overview.get({ signal: pageSignal.signal }),
    auth.role === 'admin' ? api.admin.users.list({ signal: pageSignal.signal }) : Promise.resolve(null),
    auth.role === 'admin' || auth.role === 'viewer' ? api.outbound.get({ signal: pageSignal.signal }) : Promise.resolve(null),
    api.models.get({ signal: pageSignal.signal }),
  ];
  const [configRes, overviewRes, usersRes, outboundRes, modelsRes] = await Promise.allSettled(requests);
  if (!pageSignal || pageSignal.signal.aborted) return;
  ui.loading = false;

  if (configRes.status === 'fulfilled') {
    const payload = configRes.value || {};
    ui.config = payload.config || (Array.isArray(payload.providers) ? payload : null);
    ui.configPath = payload.path || '';
    ui.configError = '';
  } else {
    ui.config = null;
    ui.configError = isForbidden(configRes.reason) ? '当前角色无法读取平台配置' : describeError(configRes.reason);
  }

  if (overviewRes.status === 'fulfilled') {
    ui.overview = overviewRes.value;
    ui.overviewError = '';
  } else {
    ui.overview = null;
    ui.overviewError = describeError(overviewRes.reason);
  }

  if (modelsRes.status === 'fulfilled') {
    const payload = modelsRes.value || {};
    ui.models = Array.isArray(payload.data) ? payload.data : (Array.isArray(payload) ? payload : []);
  } else {
    ui.models = [];
  }

  if (usersRes.status === 'fulfilled') {
    if (usersRes.value) {
      ui.users = usersRes.value.users || [];
      ui.userMeta = usersRes.value;
      ui.allowRegistration = !!usersRes.value.allow_registration;
      ui.usersError = '';
    } else {
      ui.users = null;
      ui.userMeta = null;
      ui.usersError = '';
    }
  } else {
    ui.users = null;
    ui.userMeta = null;
    ui.usersError = describeError(usersRes.reason);
  }

  if (outboundRes.status === 'fulfilled') {
    if (outboundRes.value) {
      const payload = outboundRes.value.outbound || outboundRes.value;
      ui.outbound = normalizeOutbound(payload);
      ui.outboundError = '';
      ui.outboundDirty = false;
    } else {
      ui.outbound = null;
      ui.outboundError = '';
    }
  } else {
    ui.outbound = null;
    ui.outboundError = isForbidden(outboundRes.reason) ? '当前角色无法查看出站代理' : describeError(outboundRes.reason);
  }

  render();
}

function normalizeOutbound(value) {
  const out = value && typeof value === 'object' ? deepClone(value) : {};
  out.proxies = Array.isArray(out.proxies) ? out.proxies : [];
  out.routes = out.routes && typeof out.routes === 'object' ? out.routes : {};
  return out;
}

function render() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-system' });
  const auth = authState();
  const keys = normalizeKeyList(ui.config?.api_keys);
  // 访问密钥的条数只在「管理员且配置真的读到」时才算数：viewer 能读供应商
  // 配置，但 api_keys 被后端隐藏；普通用户与匿名访客则连 config 都拿不到。
  // 把这些空数组标成 0，会被读成「一个都没配」。
  const keysKnown = isAdmin() && !!ui.config;
  const platformReader = auth.role === 'admin' || auth.role === 'viewer';
  // 分区本身也要按角色出现：普通用户点开「出站代理 / 运行参数」只会看到一句
  // 无权查看，viewer 点开「访问密钥」也一样。直接不渲染这些入口，比留一个
  // 必然失败的标签页更诚实。
  const tabItems = [
    ...(isAdmin() ? [{ id: 'access', label: '访问密钥', count: keysKnown ? keys.length : null }] : []),
    { id: 'accounts', label: '面板账号' },
    ...(platformReader ? [
      { id: 'outbound', label: '出站代理', count: ui.outbound?.proxies?.length },
      { id: 'runtime', label: '运行参数' },
    ] : []),
    { id: 'storage', label: '存储' },
  ];
  if (!tabItems.some((item) => item.id === ui.tab)) ui.tab = tabItems[0].id;
  const tabs = createTabs({
    items: tabItems,
    value: ui.tab,
    ariaLabel: '系统设置分区',
    onChange: (value) => { ui.tab = value; render(); },
  });

  // 身份（用户名 + 角色）由外壳常驻展示（#session-chip / #sidebar-session）。
  // 这里只补一句本页权限含义，并和操作按钮放在同一行，避免两条横条上下叠。
  // 匿名的系统设置全是平台级配置：五个分区都只会回一句「看不到」。摆一排
  // 点不进去的标签页与刷新按钮，等于让人反复触发 403。只留一句说明和登录入口。
  if (!auth.authenticated) {
    page.appendChild(pageBar({
      actions: [el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openLoginModal({ message: '登录系统设置需要管理员或只读角色。' }) },
      }, '登录')],
    }));
    page.appendChild(el('div', { class: 'notice notice-info' },
      icon('info', 16),
      el('div', { class: 'notice-content' },
        el('strong', { text: '系统设置需要登录' }),
        el('span', { text: '访问密钥、面板账号、出站代理、运行参数与存储都属于平台级配置；登录后按角色显示可读部分。' }),
      ),
    ));
    rootEl.replaceChildren(page);
    return;
  }

  page.appendChild(pageBar({
    context: auth.authenticated
      ? el('span', {
        text: canWrite()
          ? '本页可完整读写：改动会立即落盘并热生效。'
          : (auth.role === 'viewer' ? '只读角色：平台配置只读；可修改自己的面板密码。' : '普通用户：仅能管理自己的面板账号；存储信息只读。'),
      })
      : null,
    actions: [el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      on: { click: () => loadAll() },
    }, icon('refresh', 15), '刷新')],
  }));

  page.appendChild(tabs.el);
  const body = el('div', { class: 'system-tab-body' });
  try {
    if (ui.tab === 'accounts') body.appendChild(accountsTab(auth));
    else if (ui.tab === 'outbound') body.appendChild(outboundTab(auth));
    else if (ui.tab === 'runtime') body.appendChild(runtimeTab(auth));
    else if (ui.tab === 'storage') body.appendChild(storageTab(auth));
    else body.appendChild(accessTab(auth));
  } catch (error) {
    console.error('system tab render failed', error);
    body.appendChild(card({ title: '分区渲染失败', body: el('p', { class: 'text-danger', text: describeError(error) }) }));
  }
  page.appendChild(body);

  page.addEventListener('click', onClick);
  rootEl.replaceChildren(page);
}

function accessTab(auth) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!isAdmin()) {
    wrap.appendChild(permissionNotice(
      auth,
      '访问密钥仅管理员可管理',
      auth.authenticated ? '客户端 api_key 属于网关级凭证；viewer 与 user 看不到完整列表。' : '登录管理员账号后可创建、收窄或轮换客户端密钥。',
    ));
    return wrap;
  }
  if (!ui.config) {
    // 首次加载只放一个 loadingBlock；不要在页面顶部再写一行「加载中…」。
    wrap.appendChild(ui.loading
      ? loadingBlock('加载系统配置')
      : notice('warn', '配置不可读', ui.configError || '无法读取引擎配置。'));
    return wrap;
  }
  const keys = normalizeKeyList(ui.config.api_keys);
  const table = createTable({
    columns: [
      {
        key: 'name', label: '名称',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { text: row.name || '未命名密钥' }),
          el('span', { class: 'cell-sub mono', text: truncate(row.key || '', 36) }),
        ),
      },
      {
        key: 'scope', label: '供应商 scope',
        render: (row) => {
          const scope = row.providers || [];
          return scope.length
            ? el('span', { class: 'tag-row' }, scope.map((name) => badge(name, 'info')))
            : badge('全量供应商', 'ok');
        },
      },
      {
        key: 'models', label: '可见模型',
        align: 'right',
        render: (row) => visibleModelsForScope(row.providers || []),
      },
      {
        key: 'actions', label: '', align: 'right', width: '150px',
        render: (row, index) => el('div', { class: 'row-actions' },
          el('button', {
            class: 'btn btn-ghost btn-sm', type: 'button',
            dataset: { action: 'edit-key', index: String(index) },
          }, icon('edit', 13), '编辑'),
          el('button', {
            class: 'btn btn-danger-ghost btn-sm', type: 'button',
            dataset: { action: 'delete-key', index: String(index), name: row.name || `密钥 ${index + 1}` },
          }, icon('trash', 13), '删除'),
        ),
      },
    ],
    rows: keys,
    empty: '还没有客户端访问密钥',
  });

  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '客户端访问密钥' }),
      el('span', { class: 'muted small', text: `${keys.length} 把；用于调用 /omni/v1/* 等网关接口` }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openKeyModal(-1) },
      }, icon('plus', 14), '添加密钥'),
    ),
  ));
  wrap.appendChild(card({ body: table.el, class: 'table-card' }));
  wrap.appendChild(el('div', { class: 'notice notice-info' },
    icon('user', 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: '按用户分配渠道' }),
      el('span', { text: '同一用户名下可能有多把密钥（newapi 多 token）。到「用户」页可以按人勾选供应商范围，一次更新同名全部密钥，并按范围查看分组。' }),
    ),
    el('a', { class: 'btn btn-ghost btn-sm', href: '#/users' }, '前往用户页'),
  ));
  wrap.appendChild(el('div', { class: 'notice notice-info' },
    icon('info', 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: '密钥以掩码回显' }),
      el('span', { text: '出于安全考虑，已保存的密钥不会在页面上显示明文；如需轮换，请直接输入新密钥覆盖。' }),
    ),
  ));
  const authRequired = ui.overview?.auth_required;
  wrap.appendChild(card({
    title: '网关鉴权状态',
    body: el('div', { class: 'facts-grid compact' },
      fact('鉴权要求', authRequired == null ? '未知' : (authRequired ? '已启用' : '未启用')),
      fact('客户端密钥', `${keys.length} 把`),
      fact('全量密钥', `${keys.filter((k) => !(k.providers || []).length).length} 把`),
      fact('收窄密钥', `${keys.filter((k) => (k.providers || []).length).length} 把`),
    ),
  }));
  return wrap;
}

function visibleModelsForScope(scope) {
  if (!ui.models.length) return '—';
  if (!scope.length) return `${fmtInt(ui.models.length)} 个`;
  return `${fmtInt(ui.models.filter((m) => scope.includes(m.owned_by)).length)} 个`;
}

function accountsTab(auth) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!auth.authenticated) {
    wrap.appendChild(permissionNotice(auth, '面板账号', '登录后查看自己的账号并修改密码。'));
    return wrap;
  }
  wrap.appendChild(myAccountCard(auth));

  if (!isAdmin()) {
    wrap.appendChild(notice('info', '仅显示自己的账号', '其他面板账号与归属信息不可见。'));
    return wrap;
  }

  if (ui.usersError) {
    wrap.appendChild(notice('warn', '面板账号列表加载失败', ui.usersError));
  }
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '面板账号' }),
      el('span', { class: 'muted small', text: `${ui.users?.length || 0} 个账号 · 管理员 / 只读 / 用户` }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('button', {
        class: 'btn btn-primary btn-sm', type: 'button',
        on: { click: () => openUserModal('') },
      }, icon('plus', 14), '添加账号'),
    ),
  ));

  const table = createTable({
    columns: [
      {
        key: 'username', label: '用户名',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.username }),
          row.username === auth.username ? el('span', { class: 'cell-sub', text: '当前登录' }) : null,
        ),
      },
      { key: 'role', label: '角色', render: (row) => badge(roleLabel(row.role), row.role === 'admin' ? 'accent' : (row.role === 'viewer' ? 'info' : 'muted')) },
      {
        key: 'actions', label: '', align: 'right', width: '260px',
        render: (row) => el('div', { class: 'row-actions' },
          el('button', {
            class: 'btn btn-ghost btn-sm', type: 'button',
            dataset: { action: 'edit-user', username: row.username },
          }, icon('edit', 13), '编辑'),
          el('button', {
            class: 'btn btn-ghost btn-sm', type: 'button',
            dataset: { action: 'reset-user', username: row.username },
          }, '重置密码'),
          el('button', {
            class: 'btn btn-danger-ghost btn-sm', type: 'button',
            disabled: row.username === auth.username,
            dataset: { action: 'delete-user', username: row.username },
          }, icon('trash', 13), '删除'),
        ),
      },
    ],
    rows: ui.users || [],
    empty: '还没有面板账号',
  });
  wrap.appendChild(card({ body: table.el, class: 'table-card' }));

  const allowToggle = el('input', {
    type: 'checkbox', checked: !!ui.allowRegistration,
    on: {
      change: async (event) => {
        const checked = event.target.checked;
        event.target.disabled = true;
        try {
          const res = await api.admin.users.setRegistration(checked, { signal: pageSignal?.signal });
          ui.allowRegistration = !!res?.allow_registration;
          toastSuccess(checked ? '已开启自助注册' : '已关闭自助注册');
        } catch (error) {
          event.target.checked = !checked;
          toastError(error);
        } finally {
          event.target.disabled = false;
        }
      },
    },
  });
  wrap.appendChild(card({
    title: '注册与登录安全',
    body: el('div', {},
      el('label', { class: 'switch-row' },
        el('span', { class: 'switch' }, allowToggle, el('span', { class: 'switch-track', 'aria-hidden': 'true' })),
        el('span', { class: 'switch-text', text: '允许自助注册' }),
      ),
      el('div', { class: 'facts-grid compact' },
        fact('会话有效期', ui.userMeta?.session_hours ? `${ui.userMeta.session_hours} 小时` : '—'),
        fact('登录失败上限', ui.userMeta?.max_failures ? `${ui.userMeta.max_failures} 次` : '—'),
        fact('锁定时长', ui.userMeta?.lock_minutes ? `${ui.userMeta.lock_minutes} 分钟` : '—'),
        fact('账号归属键', 'provider/label（供应商详情内维护）'),
      ),
      el('p', { class: 'section-note', text: '删除面板账号会清理其账号归属；账号本身留在供应商配置中，由管理员继续管理。' }),
    ),
  }));
  return wrap;
}

function myAccountCard(auth) {
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const current = el('input', { class: 'input', type: 'password', autocomplete: 'current-password', placeholder: '当前密码' });
  const next = el('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '新密码（至少 6 位）' });
  const confirm = el('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '确认新密码' });
  const submit = el('button', { class: 'btn btn-primary btn-sm', type: 'submit' }, '修改密码');
  const form = el('form', { class: 'password-form' },
    el('div', { class: 'form-grid three-col' },
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '当前密码'), current),
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '新密码'), next),
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '确认新密码'), confirm),
    ),
    errorBox,
    el('div', { class: 'form-actions' }, submit),
  );
  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    errorBox.textContent = '';
    if (!current.value) { errorBox.textContent = '请输入当前密码'; return; }
    if (next.value.length < 6) { errorBox.textContent = '新密码至少 6 位'; return; }
    if (next.value !== confirm.value) { errorBox.textContent = '两次输入的新密码不一致'; return; }
    submit.disabled = true;
    try {
      await api.session.changePassword(current.value, next.value);
      toastSuccess('密码已修改；其他会话可能需要重新登录');
      current.value = ''; next.value = ''; confirm.value = '';
    } catch (error) {
      errorBox.textContent = describeError(error);
    } finally {
      submit.disabled = false;
    }
  });

  return card({
    title: '我的账号',
    subtitle: '面板会话与个人密码',
    actions: badge(roleLabel(auth.role), auth.role === 'admin' ? 'accent' : 'info'),
    body: el('div', {},
      el('div', { class: 'facts-grid compact' },
        fact('用户名', auth.username),
        fact('角色', roleLabel(auth.role)),
        fact('鉴权方式', '会话 Cookie（SameSite=Strict）'),
        fact('账号归属', 'provider/label'),
      ),
      form,
    ),
  });
}

function outboundTab(auth) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!isAuthenticated() || !(isAdmin() || auth.role === 'viewer')) {
    wrap.appendChild(permissionNotice(auth, '出站代理', '出站代理属于平台配置，需要管理员或只读角色。'));
    return wrap;
  }
  if (!ui.outbound) {
    wrap.appendChild(notice('warn', '出站代理不可读', ui.outboundError || '未加载到 outbound 配置。'));
    return wrap;
  }
  const dirty = ui.outboundDirty;
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '出站代理' }),
      el('span', { class: 'muted small', text: '面板上游与内置 OmniGate 共用；保存后热应用' }),
      dirty ? badge('有未保存修改', 'warn') : null,
    ),
    el('div', { class: 'toolbar-actions' },
      canWrite() ? el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => openProxyModal(-1) },
      }, icon('plus', 14), '添加代理') : null,
      canWrite() ? el('button', {
        class: 'btn btn-primary btn-sm', type: 'button', disabled: !dirty,
        on: { click: () => persistOutbound() },
      }, icon('check', 14), '保存出站配置') : badge('只读', 'muted'),
    ),
  ));

  const proxyTable = createTable({
    columns: [
      {
        key: 'name', label: '代理',
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'mono', text: row.name || '—' }),
          el('span', { class: 'cell-sub', text: row.pool?.length ? `代理池 ${row.pool.length} 个` : (row.pool_url ? '远程代理池' : '单代理') }),
        ),
      },
      {
        key: 'endpoint', label: '地址',
        render: (row) => el('span', { class: 'mono small', text: row.url || row.pool_url || (row.pool || []).join(', ') || '—' }),
      },
      {
        key: 'refresh', label: '刷新 / 探活',
        hideOnMobile: true,
        render: (row) => el('div', { class: 'cell-title' },
          el('span', { class: 'small', text: row.refresh_sec ? `${row.refresh_sec}s` : '—' }),
          el('span', { class: 'cell-sub mono', text: row.probe_url || '默认探活' }),
        ),
      },
      {
        key: 'routes', label: '引用路由', align: 'right',
        render: (row) => String(Object.values(ui.outbound.routes || {}).filter((name) => name === row.name).length),
      },
      {
        key: 'actions', label: '', align: 'right', width: '150px',
        render: (row, index) => canWrite() ? el('div', { class: 'row-actions' },
          el('button', { class: 'btn btn-ghost btn-sm', type: 'button', dataset: { action: 'edit-proxy', index: String(index) } }, icon('edit', 13), '编辑'),
          el('button', { class: 'btn btn-danger-ghost btn-sm', type: 'button', dataset: { action: 'delete-proxy', index: String(index) } }, icon('trash', 13), '删除'),
        ) : el('span', { class: 'muted small', text: '只读' }),
      },
    ],
    rows: ui.outbound.proxies,
    empty: '还没有配置出站代理；默认直连。',
  });
  wrap.appendChild(card({ body: proxyTable.el, class: 'table-card' }));

  const routes = Object.entries(ui.outbound.routes || {});
  const routeTable = createTable({
    columns: [
      { key: 'target', label: '目标', render: (row) => el('span', { class: 'mono', text: row[0] }) },
      { key: 'proxy', label: '代理', render: (row) => badge(row[1], 'info') },
      {
        key: 'actions', label: '', align: 'right', width: '80px',
        render: (row) => canWrite() ? el('button', {
          class: 'btn btn-danger-ghost btn-sm', type: 'button',
          dataset: { action: 'remove-route', target: row[0] },
        }, '移除') : '—',
      },
    ],
    rows: routes,
    empty: '没有目标路由；全部目标直连。',
  });
  const proxyOptions = ui.outbound.proxies.map((proxy) => el('option', { value: proxy.name, text: proxy.name }));
  const targetInput = el('input', {
    class: 'input mono', type: 'text', placeholder: '目标，例如 omnigate:raccoon', list: 'outbound-targets', autocomplete: 'off',
  });
  const providerNames = ui.config?.providers?.map((p) => p.name) || [];
  const datalist = el('datalist', { id: 'outbound-targets' },
    [el('option', { value: 'panel' }), ...providerNames.map((name) => el('option', { value: `omnigate:${name}` }))],
  );
  const proxySelect = el('select', { class: 'input select select-sm' }, proxyOptions);
  const addRouteBtn = el('button', {
    class: 'btn btn-ghost btn-sm', type: 'button', disabled: !canWrite() || !ui.outbound.proxies.length,
    on: {
      click: () => {
        const target = targetInput.value.trim();
        if (!target) { toastWarn('请输入目标'); return; }
        if (!proxySelect.value) { toastWarn('请选择代理'); return; }
        ui.outbound.routes[target] = proxySelect.value;
        ui.outboundDirty = true;
        render();
      },
    },
  }, icon('plus', 14), '添加路由');
  const routeControls = canWrite()
    ? el('div', { class: 'route-controls' }, targetInput, datalist, proxySelect, addRouteBtn)
    : null;
  wrap.appendChild(card({
    title: '目标路由',
    subtitle: 'target → 代理名；空对象表示全部直连',
    body: el('div', {}, routeControls, routeTable.el),
  }));
  wrap.appendChild(el('div', { class: 'notice notice-info' },
    icon('info', 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: '代理口令以掩码显示' }),
      el('span', { text: '编辑时会保留未改动的用户名/密码；如果要换口令，直接填写新值。' }),
    ),
  ));
  return wrap;
}

function runtimeTab(auth) {
  const wrap = el('div', { class: 'tab-stack' });
  if (!isAuthenticated() || (!isAdmin() && auth.role !== 'viewer')) {
    wrap.appendChild(permissionNotice(auth, '运行参数', '运行参数属于平台配置，需要管理员或只读角色。'));
    return wrap;
  }
  if (!ui.config) {
    wrap.appendChild(notice('warn', '运行参数不可读', ui.configError || '无法读取引擎配置。'));
    return wrap;
  }
  const flat = flattenObject(ui.config);
  const form = createSchemaForm({
    fields: RUNTIME_FIELDS,
    values: flat,
    disabled: !canWrite(),
    idPrefix: 'runtime',
    submitLabel: canWrite() ? '保存运行参数' : '',
    onSubmit: (values) => saveRuntime(values),
  });
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '运行参数' }),
      el('span', { class: 'muted small', text: '保存后热重载；字段来自引擎配置结构' }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('a', { class: 'btn btn-ghost btn-sm', href: '#/routing' }, '路由默认值'),
      ui.configPath ? badge(truncate(ui.configPath, 34), 'muted') : null,
    ),
  ));
  wrap.appendChild(card({
    title: '引擎参数',
    subtitle: canWrite() ? '签到任务使用 JSON 编辑；小心保结构' : '只读角色不可保存',
    body: form.el,
    class: 'form-card runtime-form-card',
  }));
  wrap.appendChild(el('div', { class: 'notice notice-info' },
    icon('info', 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: '数据目录与供应商配置' }),
      el('span', { text: '供应商级参数在「供应商 · 高级」内维护；这里只包含全局运行参数。' }),
    ),
  ));
  return wrap;
}

function storageTab() {
  const wrap = el('div', { class: 'tab-stack' });
  const overview = ui.overview || {};
  const storage = overview.storage || {};
  const totals = overview.totals || {};
  wrap.appendChild(el('div', { class: 'section-toolbar' },
    el('div', { class: 'toolbar-title' },
      el('h3', { text: '存储与运行信息' }),
      el('span', { class: 'muted small', text: '只读展示；写入由引擎自身管理' }),
    ),
    el('div', { class: 'toolbar-actions' },
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => loadAll() },
      }, icon('refresh', 14), '重新读取'),
    ),
  ));
  if (ui.overviewError) {
    wrap.appendChild(notice('warn', '运行信息不可用', ui.overviewError));
  }
  wrap.appendChild(card({
    title: '引擎',
    body: el('div', { class: 'facts-grid' },
      fact('版本', overview.version || '—'),
      fact('构建', overview.build || '—'),
      fact('运行时长', overview.uptime_sec != null ? fmtUptime(overview.uptime_sec) : '—'),
      fact('鉴权要求', overview.auth_required == null ? '—' : (overview.auth_required ? '已启用' : '未启用')),
      fact('会话存储', overview.redis_mode || '—'),
      fact('供应商 / 账号', `${fmtInt(totals.providers ?? (ui.config?.providers?.length || 0))} / ${fmtInt(totals.accounts ?? overview.total ?? 0)}`),
    ),
  }));
  wrap.appendChild(card({
    title: '文件与归档',
    body: el('div', { class: 'facts-grid' },
      fact('配置存储', storage.config_path || ui.configPath || '—'),
      fact('数据目录', storage.data_dir || ui.config?.data_dir || '—'),
      fact('用量文件', storage.usage_file_bytes != null ? fmtBytes(storage.usage_file_bytes) : '—'),
      fact('请求归档', storage.request_archive_files != null ? `${fmtInt(storage.request_archive_files)} 个文件` : '—'),
      fact('归档体积', storage.request_archive_bytes != null ? fmtBytes(storage.request_archive_bytes) : '—'),
      fact('丢弃写入', storage.dropped_writes != null ? fmtInt(storage.dropped_writes) : '—'),
    ),
  }));
  wrap.appendChild(card({
    title: '运维提示',
    body: el('div', { class: 'storage-tips' },
      el('p', { text: '供应商、面板账号与网关配置保存在 PostgreSQL；保存后热重载运行时，模型目录、密钥健康与全局映射同样落库。' }),
      el('p', { text: '用量与请求归档按引擎的保留策略滚动清理；面板不提供直接删除按钮。' }),
      el('div', { class: 'quick-actions' },
        el('a', { class: 'btn btn-ghost btn-sm', href: '#/usage' }, '查看用量'),
        el('a', { class: 'btn btn-ghost btn-sm', href: '#/requests' }, '查看请求'),
      ),
    ),
  }));
  return wrap;
}

// ---------------------------------------------------------------------------
// 访问密钥编辑
// ---------------------------------------------------------------------------

function openKeyModal(index) {
  if (!isAdmin() || !ui.config) return;
  const keys = normalizeKeyList(ui.config.api_keys);
  const editing = index >= 0;
  const existing = editing ? keys[index] : { key: '', name: '', providers: [] };
  const nameInput = el('input', { class: 'input', type: 'text', value: existing.name || '', placeholder: '例如 生产客户端 / 浣熊专用', autocomplete: 'off' });
  const secret = createSecretField({
    value: existing.key || '',
    placeholder: 'sk-omni-…',
    name: 'client_api_key',
  });
  const generateBtn = el('button', {
    class: 'btn btn-ghost btn-sm', type: 'button',
    on: {
      click: () => {
        secret.setValue(`sk-omni-${randomHex(24)}`);
        secret.input.type = 'text';
      },
    },
  }, icon('key', 14), '生成新密钥');
  const providerNames = (ui.config.providers || []).map((p) => p.name);
  const scopeSet = new Set(existing.providers || []);
  const scopeBox = el('div', { class: 'check-chip-list' },
    providerNames.map((name) => {
      const cb = el('input', { type: 'checkbox', value: name, checked: scopeSet.has(name) });
      return el('label', { class: 'check-chip' }, cb, el('span', { text: name }));
    }),
  );
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, editing ? '保存密钥' : '添加密钥');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');
  const modal = openModal({
    title: editing ? '编辑客户端密钥' : '添加客户端密钥',
    subtitle: 'providers 为空表示全量；收窄后该密钥只能看到并调用指定供应商的模型。',
    size: 'md',
    body: el('div', { class: 'key-form' },
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '名称'),
        nameInput,
        el('p', { class: 'field-help', text: '仅用于面板识别，不会发送给上游。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '密钥', el('span', { class: 'required-mark', text: '*' })),
        secret.el,
        el('div', { class: 'form-actions' }, generateBtn),
        el('p', { class: 'field-help', text: editing ? '已保存的密钥是掩码；如需轮换，直接输入或生成新密钥。' : '密钥只会提交到本地服务端。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '供应商 scope'),
        scopeBox,
        el('p', { class: 'field-help', text: '不勾选任何供应商 = 全量。勾选后模型目录会收窄到这些供应商。' }),
        el('p', { class: 'field-help' },
          '同一个名字下可能有多把密钥（newapi 多 token）；这里只改当前这一把。要按用户统一改，请到 ',
          el('a', { href: '#/users' }, '用户页'),
          ' 编辑访问权限。',
        ),
      ),
      errorBox,
    ),
    footer: [cancelBtn, saveBtn],
    initialFocus: nameInput,
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    const keyValue = secret.getValue().trim();
    if (!keyValue) { errorBox.textContent = '密钥不能为空'; return; }
    const providers = Array.from(scopeBox.querySelectorAll('input:checked')).map((x) => x.value);
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    try {
      const next = deepClone(ui.config);
      const list = normalizeKeyList(next.api_keys);
      const item = { key: keyValue, name: nameInput.value.trim(), providers };
      if (editing) list[index] = item;
      else list.push(item);
      next.api_keys = list;
      const ok = await persistConfig(next, editing ? '客户端密钥已保存' : '客户端密钥已添加');
      if (ok) modal.close();
      else saveBtn.disabled = false;
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
    }
  });
}

function randomHex(bytes) {
  const arr = new Uint8Array(bytes);
  if (window.crypto?.getRandomValues) window.crypto.getRandomValues(arr);
  else for (let i = 0; i < arr.length; i += 1) arr[i] = Math.floor(Math.random() * 256);
  return Array.from(arr).map((b) => b.toString(16).padStart(2, '0')).join('');
}

async function deleteKey(index, name) {
  if (!(await requireAdmin())) return;
  const ok = await confirmDialog({
    title: '删除客户端密钥',
    message: `确定删除「${name}」吗？使用该密钥的客户端会立即失去访问权限。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok || !ui.config) return;
  const next = deepClone(ui.config);
  const list = normalizeKeyList(next.api_keys);
  list.splice(index, 1);
  next.api_keys = list;
  await persistConfig(next, '客户端密钥已删除');
}

// ---------------------------------------------------------------------------
// 面板账号
// ---------------------------------------------------------------------------

function openUserModal(username) {
  if (!isAdmin()) return;
  const editing = !!username;
  const existing = (ui.users || []).find((u) => u.username === username);
  const self = username === authState().username;
  const usernameInput = el('input', {
    class: 'input mono', type: 'text', value: username || '', disabled: editing,
    placeholder: '小写用户名', autocomplete: 'off', spellcheck: 'false',
  });
  const roleSelect = el('select', { class: 'input select', disabled: self },
    [
      { value: 'admin', label: '管理员（读写）' },
      { value: 'viewer', label: '只读（查看全部）' },
      { value: 'user', label: '用户（仅自己的账号）' },
    ].map((role) => el('option', { value: role.value, text: role.label, selected: (existing?.role || 'user') === role.value })),
  );
  const password = el('input', {
    class: 'input', type: 'password', autocomplete: 'new-password',
    placeholder: editing ? '留空 = 不修改密码' : '至少 6 位',
  });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, editing ? '保存账号' : '添加账号');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');
  const modal = openModal({
    title: editing ? `编辑账号 ${username}` : '添加面板账号',
    subtitle: '管理员可写；viewer 只读；user 仅能查看/操作归属自己的供应商账号。',
    size: 'sm',
    body: el('div', { class: 'user-form' },
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '用户名'), usernameInput),
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '角色'), roleSelect,
        self ? el('p', { class: 'field-help', text: '不能修改当前登录账号自己的角色。' }) : null),
      el('div', { class: 'field' }, el('label', { class: 'field-label' }, '密码'),
        password, el('p', { class: 'field-help', text: editing ? '留空保留原密码。' : '新账号必须设置密码。' })),
      errorBox,
    ),
    footer: [cancelBtn, saveBtn],
    initialFocus: editing ? password : usernameInput,
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    const user = usernameInput.value.trim();
    const role = roleSelect.value;
    const pass = password.value;
    if (!user) { errorBox.textContent = '用户名不能为空'; return; }
    if (!editing && pass.length < 6) { errorBox.textContent = '新账号密码至少 6 位'; return; }
    if (pass && pass.length < 6) { errorBox.textContent = '密码至少 6 位'; return; }
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    try {
      await api.admin.users.save({ username: user, role, password: pass }, { signal: pageSignal?.signal });
      toastSuccess(editing ? '面板账号已更新' : '面板账号已添加');
      modal.close();
      await loadUsers();
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
    }
  });
}

function openResetPasswordModal(username) {
  const password = el('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '新密码（至少 6 位）' });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, '重置密码');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');
  const modal = openModal({
    title: `重置密码：${username}`,
    subtitle: '重置后该账号的现有会话不会被强制踢出，但下次登录需使用新密码。',
    size: 'sm',
    body: el('div', { class: 'field' }, el('label', { class: 'field-label' }, '新密码'), password, errorBox),
    footer: [cancelBtn, saveBtn],
    initialFocus: password,
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    if (password.value.length < 6) { errorBox.textContent = '密码至少 6 位'; return; }
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    try {
      await api.admin.users.resetPassword(username, password.value);
      toastSuccess('密码已重置');
      modal.close();
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
    }
  });
}

async function deleteUser(username) {
  if (!(await requireAdmin())) return;
  const ok = await confirmDialog({
    title: '删除面板账号',
    message: `确定删除面板账号「${username}」吗？其名下供应商账号的归属会被清空，账号仍留在配置中。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  try {
    await api.admin.users.remove(username);
    toastSuccess('面板账号已删除');
    await loadUsers();
  } catch (error) {
    toastError(error);
  }
}

async function loadUsers() {
  if (!pageSignal) return;
  try {
    const res = await api.admin.users.list({ signal: pageSignal.signal, dedupe: false });
    ui.users = res?.users || [];
    ui.userMeta = res;
    ui.allowRegistration = !!res?.allow_registration;
    ui.usersError = '';
  } catch (error) {
    ui.usersError = describeError(error);
    toastError(error);
  }
  render();
}

// ---------------------------------------------------------------------------
// 出站代理
// ---------------------------------------------------------------------------

function openProxyModal(index) {
  if (!isAdmin() || !ui.outbound) return;
  const editing = index >= 0;
  const existing = editing ? ui.outbound.proxies[index] : { pool_scheme: 'http', refresh_sec: 300, strict: false };
  const form = createSchemaForm({
    fields: PROXY_FIELDS,
    values: existing,
    idPrefix: `proxy-${index}`,
  });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, editing ? '保存代理' : '添加代理');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');
  const modal = openModal({
    title: editing ? `编辑代理 ${existing.name}` : '添加出站代理',
    subtitle: '单代理与代理池二选一；保存出站配置后热应用到面板与引擎。',
    size: 'lg',
    body: el('div', { class: 'proxy-form' }, form.el, errorBox),
    footer: [cancelBtn, saveBtn],
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    if (!form.validate().valid) return;
    const values = form.getValues();
    if (!values.name) { errorBox.textContent = '代理名不能为空'; return; }
    if (ui.outbound.proxies.some((p, i) => i !== index && p.name === values.name)) {
      errorBox.textContent = '代理名已存在';
      return;
    }
    if (!values.url && !(values.pool || []).length && !values.pool_url) {
      errorBox.textContent = '请填写代理 URL 或代理池';
      return;
    }
    if (!(await requireAdmin())) return;
    const next = normalizeOutbound(ui.outbound);
    if (editing) next.proxies[index] = { ...next.proxies[index], ...values };
    else next.proxies.push(values);
    ui.outbound = next;
    ui.outboundDirty = true;
    modal.close();
    render();
    toastSuccess('代理已加入待保存修改');
  });
}

async function persistOutbound() {
  if (!(await requireAdmin())) return;
  if (!ui.outbound) return;
  try {
    const res = await api.outbound.save(ui.outbound, { signal: pageSignal?.signal });
    toastSuccess('出站代理已保存并热应用');
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`);
    }
    const payload = await api.outbound.get({ signal: pageSignal.signal, dedupe: false });
    ui.outbound = normalizeOutbound(payload?.outbound || payload || {});
    ui.outboundDirty = false;
    render();
  } catch (error) {
    toastError(error);
  }
}

// ---------------------------------------------------------------------------
// 配置保存
// ---------------------------------------------------------------------------

async function persistConfig(next, successMessage) {
  if (!(await requireAdmin({ message: '保存配置需要管理员权限' }))) return false;
  try {
    const res = await api.config.save(next, { signal: pageSignal?.signal });
    toastSuccess(successMessage || '配置已保存');
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`);
    }
    await loadConfig();
    return true;
  } catch (error) {
    toastError(error);
    return false;
  }
}

async function loadConfig() {
  if (!pageSignal) return;
  try {
    const payload = await api.config.get({ signal: pageSignal.signal, dedupe: false });
    ui.config = payload?.config || (Array.isArray(payload?.providers) ? payload : ui.config);
    ui.configPath = payload?.path || ui.configPath;
    ui.configError = '';
  } catch (error) {
    ui.configError = describeError(error);
    toastError(error);
  }
  render();
}

async function saveRuntime(values) {
  if (!(await requireAdmin({ message: '保存运行参数需要管理员权限' }))) return;
  if (!ui.config) return;
  const next = deepClone(ui.config);
  for (const [path, value] of Object.entries(values)) {
    setPath(next, path, value);
  }
  await persistConfig(next, '运行参数已保存');
}

// ---------------------------------------------------------------------------
// 事件与通用组件
// ---------------------------------------------------------------------------

function onClick(event) {
  const target = event.target instanceof Element ? event.target.closest('[data-action]') : null;
  if (!target) return;
  const action = target.dataset.action;
  switch (action) {
    case 'edit-key':
      openKeyModal(Number(target.dataset.index));
      break;
    case 'delete-key':
      deleteKey(Number(target.dataset.index), target.dataset.name || '未命名密钥');
      break;
    case 'edit-user':
      openUserModal(target.dataset.username);
      break;
    case 'reset-user':
      openResetPasswordModal(target.dataset.username);
      break;
    case 'delete-user':
      deleteUser(target.dataset.username);
      break;
    case 'edit-proxy':
      openProxyModal(Number(target.dataset.index));
      break;
    case 'delete-proxy':
      deleteProxy(Number(target.dataset.index));
      break;
    case 'remove-route':
      ui.outbound.routes && delete ui.outbound.routes[target.dataset.target];
      ui.outboundDirty = true;
      render();
      break;
    default:
      break;
  }
}

async function deleteProxy(index) {
  if (!(await requireAdmin())) return;
  const proxy = ui.outbound?.proxies?.[index];
  if (!proxy) return;
  const refs = Object.entries(ui.outbound.routes || {}).filter(([, name]) => name === proxy.name);
  const ok = await confirmDialog({
    title: '删除出站代理',
    message: refs.length
      ? `代理「${proxy.name}」被 ${refs.length} 条目标路由引用；删除后这些路由会一并移除。`
      : `确定删除代理「${proxy.name}」吗？`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  ui.outbound.proxies.splice(index, 1);
  for (const [target] of refs) delete ui.outbound.routes[target];
  ui.outboundDirty = true;
  render();
}

function permissionNotice(auth, title, text) {
  return el('div', { class: 'notice notice-info' },
    icon('info', 16),
    el('div', { class: 'notice-content' },
      el('strong', { text: title }),
      el('span', { text }),
    ),
    !auth.authenticated ? el('button', {
      class: 'btn btn-primary btn-sm', type: 'button',
      on: { click: () => openLoginModal({ message: '登录后访问系统设置。' }) },
    }, '登录') : null,
  );
}

function notice(tone, title, text) {
  return el('div', { class: `notice notice-${tone}` },
    icon(tone === 'warn' ? 'alert' : 'info', 16),
    el('div', { class: 'notice-content' }, el('strong', { text: title }), el('span', { text })),
  );
}

function fact(label, value) {
  return el('div', { class: 'fact' },
    el('span', { class: 'fact-label', text: label }),
    el('span', { class: 'fact-value', text: String(value ?? '—') }),
  );
}
