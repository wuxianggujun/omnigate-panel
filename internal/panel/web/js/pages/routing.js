// routing.js — 全局路由：默认供应商/模型、优先级、别名映射、密钥 scope 与解析预览。

import { api, describeError, isForbidden } from '../core/api.js';
import {
  authState, isAdmin, isAuthenticated, openLoginModal, requireAdmin,
} from '../core/auth.js';
import {
  el, card, badge, statusDot, emptyState, loadingBlock, icon, debounce, pageBar, sectionBand,
} from '../core/dom.js';
import {
  BUILTIN_PROVIDER_TYPES, healthOfAccount, isKeyBasedProvider, normalizeKeyList,
  normalizeProviderTypes, parseDate, prettyJSON, providerLabel,
} from '../core/format.js';
import { openModal, confirmDialog } from '../components/modal.js';
import { toastError, toastSuccess, toastWarn } from '../components/toast.js';

let pageSignal = null;
let rootEl = null;
let ui = initialState();

function initialState() {
  return {
    loading: true,
    loadError: '',
    config: null,
    configError: '',
    configPath: '',
    mappings: {},
    disabled: {},
    mappingError: '',
    accountsPayload: null,
    models: [],
    types: BUILTIN_PROVIDER_TYPES,
    defaultProvider: '',
    defaultModel: '',
    prefer: [],
    keyIndex: -1,
    resolutionInput: '',
  };
}

export async function mount(root) {
  pageSignal = new AbortController();
  rootEl = root;
  ui = initialState();
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
  ui.loadError = '';
  render();
  const results = await Promise.allSettled([
    api.config.get({ signal: pageSignal.signal }),
    api.mapping.get({ signal: pageSignal.signal }),
    api.accounts.list({ signal: pageSignal.signal }),
    api.models.get({ signal: pageSignal.signal }),
    api.providerTypes.get({ signal: pageSignal.signal }),
  ]);
  if (!pageSignal || pageSignal.signal.aborted) return;
  const [configRes, mappingRes, accountsRes, modelsRes, typesRes] = results;
  ui.loading = false;

  if (configRes.status === 'fulfilled') {
    const payload = configRes.value || {};
    ui.config = payload.config || (Array.isArray(payload.providers) ? payload : null);
    ui.configPath = payload.path || '';
    ui.configError = '';
    if (ui.config) {
      ui.defaultProvider = String(ui.config.default_provider || '');
      ui.defaultModel = String(ui.config.default_model || '');
      const providerNames = new Set(providerList().map((p) => p.name));
      ui.prefer = (Array.isArray(ui.config.routing?.prefer) ? ui.config.routing.prefer : [])
        .filter((name) => providerNames.has(name));
      if (!ui.resolutionInput) {
        ui.resolutionInput = ui.defaultModel || ui.models[0]?.id || '';
      }
    }
  } else {
    ui.config = null;
    ui.configError = isForbidden(configRes.reason)
      ? '当前角色无法读取路由配置（需要管理员或只读角色）'
      : describeError(configRes.reason);
    ui.loadError = ui.configError;
  }

  if (mappingRes.status === 'fulfilled') {
    applyMappingPayload(mappingRes.value);
    ui.mappingError = '';
  } else {
    ui.mappings = {};
    ui.disabled = {};
    ui.mappingError = isForbidden(mappingRes.reason) ? '当前角色无法查看模型别名表' : describeError(mappingRes.reason);
  }

  if (accountsRes.status === 'fulfilled') {
    ui.accountsPayload = accountsRes.value?.data || accountsRes.value || null;
  } else {
    ui.accountsPayload = null;
  }

  if (modelsRes.status === 'fulfilled') {
    const payload = modelsRes.value || {};
    ui.models = Array.isArray(payload.data) ? payload.data : (Array.isArray(payload) ? payload : []);
  } else {
    ui.models = [];
  }

  ui.types = typesRes.status === 'fulfilled'
    ? (normalizeProviderTypes(typesRes.value) || BUILTIN_PROVIDER_TYPES)
    : BUILTIN_PROVIDER_TYPES;

  if (ui.config && !ui.resolutionInput) {
    ui.resolutionInput = ui.defaultModel || ui.models[0]?.id || '';
  }
  render();
}

function applyMappingPayload(payload) {
  const mappings = payload?.mappings ?? payload?.data?.mappings ?? payload ?? {};
  const disabled = payload?.disabled ?? payload?.data?.disabled ?? {};
  const out = {};
  for (const [alias, targets] of Object.entries(mappings || {})) {
    out[String(alias)] = Array.isArray(targets) ? targets.map(String) : [String(targets)];
  }
  ui.mappings = out;
  ui.disabled = {};
  if (Array.isArray(disabled)) {
    for (const alias of disabled) ui.disabled[String(alias)] = true;
  } else if (disabled && typeof disabled === 'object') {
    for (const [alias, value] of Object.entries(disabled)) {
      if (value) ui.disabled[String(alias)] = true;
    }
  }
}

function providerList() {
  const rows = [];
  const seen = new Set();
  for (const provider of ui.config?.providers || []) {
    rows.push({ name: provider.name, type: provider.type || '', config: provider });
    seen.add(provider.name);
  }
  for (const provider of ui.accountsPayload?.providers || []) {
    if (seen.has(provider.name)) continue;
    rows.push({ name: provider.name, type: provider.type || '', config: null });
  }
  return rows;
}

function accountsFor(name) {
  const provider = (ui.accountsPayload?.providers || []).find((p) => p.name === name);
  return provider?.accounts || [];
}

function typeMeta(type) {
  return (ui.types || []).find((t) => t.type === type)
    || BUILTIN_PROVIDER_TYPES.find((t) => t.type === type)
    || { type, label: providerLabel(type), capabilities: [], config_schema: [], account_schema: [], actions: [] };
}

function keyList() {
  return normalizeKeyList(ui.config?.api_keys);
}

function hasProviderKeys(provider) {
  if (!provider) return false;
  return !!(provider.api_key || (provider.api_keys || []).length || (provider.zen_keys || []).length || (provider.go_keys || []).length);
}

// providerHealth 给优先级列表里的每一行打一个状态点。
//
// 这一页只有拿到 ui.config 才渲染（admin / viewer 都读得到配置），所以「有没有配
// 供应商密钥」是已知的。但账号明细未必读得到：/panel/api/omni/accounts 可能失败，
// 匿名/普通用户的账号数组也会被后端按归属过滤。那种情况下不能写「无账号 / 暂无
// 健康账号」——那是把「看不到」说成「没有」。
function providerHealth(name) {
  const accounts = accountsFor(name);
  const accountsKnown = !!ui.accountsPayload;
  const provider = providerList().find((p) => p.name === name);
  if (!accounts.length) {
    const meta = typeMeta(provider?.type);
    if (isKeyBasedProvider(meta)) {
      // 密钥型没有账号记录：状态由「配置里有没有密钥」决定，配置是读得到的。
      const hasKeys = provider?.config ? hasProviderKeys(provider.config) : null;
      if (hasKeys == null) return { tone: 'muted', label: '凭证不可见' };
      return hasKeys
        ? { tone: 'ok', label: '供应商密钥' }
        : { tone: 'warn', label: '缺上游密钥' };
    }
    // 账号型：账号数组为空只有在「读到了」的时候才等于「没有账号」。
    if (!accountsKnown) return { tone: 'muted', label: '账号不可见' };
    return { tone: 'warn', label: '无账号' };
  }
  for (const account of accounts) {
    const health = healthOfAccount(account);
    if (health.tone === 'ok') return { tone: 'ok', label: '有可用账号' };
  }
  // 有账号记录但没有一个是健康的：这是读到的事实，可以直说。
  return { tone: 'warn', label: '暂无健康账号' };
}

function render() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-routing' });
  const canEdit = isAdmin();

  // 工具条只在真的读到了配置时才谈「保存」与「只读视图」：普通用户和匿名访客
  // 连配置都读不到，写着「只读视图」会误导成「能看不能改」；而且空工具条会留下
  // 一条 30px 的空白横条，所以读不到时整个不渲染，由下面的提示条说明原因。
  if (ui.config) {
    page.appendChild(pageBar({
      context: [
        el('span', {
          text: canEdit
            ? '默认值与优先级改动需点保存；别名与 scope 即时生效'
            : '只读视图：可查看配置，但不能保存',
        }),
        ui.configPath ? el('span', { class: 'mono', text: ui.configPath }) : null,
      ],
      actions: [
        el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button',
          on: { click: () => loadAll() },
        }, icon('refresh', 15), '刷新'),
        canEdit
          ? el('button', {
            class: 'btn btn-primary btn-sm', type: 'button',
            on: { click: () => saveRouting() },
          }, icon('check', 15), '保存默认与优先级')
          : null,
      ],
    }));
  }

  if (!ui.config) {
    if (ui.loading) {
      page.appendChild(loadingBlock('加载路由配置'));
    } else {
      page.appendChild(el('div', { class: 'notice notice-info' },
        icon('info', 16),
        el('div', { class: 'notice-content' },
          el('strong', { text: isAuthenticated() ? '路由配置不可读' : '登录后查看路由' }),
          // 匿名的 ui.configError 是 api.js 的统一文案「需要登录后才能执行此操作」，
          // 与标题重复；这里换成说清这页有什么的那句。
          el('span', {
            text: !isAuthenticated()
              ? '默认供应商、裸模型优先级与模型别名都属于平台级路由设置；登录后按角色显示。'
              : (ui.configError || '路由配置需要管理员或只读角色。'),
          }),
        ),
        !isAuthenticated() ? el('button', {
          class: 'btn btn-primary btn-sm', type: 'button',
          on: { click: () => openLoginModal({ message: '登录后查看与修改路由。' }) },
        }, '登录') : null,
      ));
    }
    rootEl.replaceChildren(page);
    return;
  }

  if (ui.loadError) {
    page.appendChild(el('div', { class: 'notice notice-warn' },
      icon('alert', 16),
      el('div', { class: 'notice-content' }, el('strong', { text: '部分数据加载失败' }), el('span', { text: ui.loadError })),
    ));
  }

  page.appendChild(sectionBand({
    title: '优先顺序',
    note: '决定一个裸模型名最终打到哪个供应商：别名 > 优先级 > 默认供应商。',
  }));
  page.appendChild(el('div', { class: 'routing-top-grid' },
    defaultsCard(canEdit),
    priorityCard(canEdit),
  ));
  page.appendChild(mappingCard(canEdit));

  page.appendChild(sectionBand({
    title: '访问控制与解析',
    note: '密钥范围只做收窄；解析预览按同一套规则给出结论，不发真实请求。',
  }));
  page.appendChild(el('div', { class: 'routing-bottom-grid' },
    scopeCard(canEdit),
    resolutionCard(canEdit),
  ));

  rootEl.replaceChildren(page);
}

// stripProviderPrefix 去掉已知供应商前缀，得到客户端实际会发来的裸模型名。
// 默认模型允许存成 "provider/model"，但别名表以裸名（有时是上游原始 ID）为键，
// 两边不对齐就会漏掉「这个默认模型其实是别名」的判断。
function stripProviderPrefix(model, providers) {
  const value = String(model || '').trim();
  if (!value) return '';
  const names = providers.map((p) => p.name).sort((a, b) => b.length - a.length);
  for (const name of names) {
    if (value.startsWith(`${name}/`)) return value.slice(name.length + 1);
  }
  return value;
}

// aliasShadowInfo 解释「明明设了默认供应商，请求却还是走别的供应商」。
//
// 语义上别名是硬规则：命中别名的模型一律走目标链的第一项，default_provider 与
// routing.prefer 只在**没有别名命中**时参与选择（与后端 routeScoped 同口径）。
// 所以改默认供应商对已经有别名的模型完全无效——这是最容易被当成 bug 的地方，
// 也是这套路由最难自己看出来的一点。这里把结论直接摊在卡片上，并点名要去改哪条
// 别名，而不是让运维反复改默认供应商却看不到任何效果。
function aliasShadowInfo() {
  const providers = providerList();
  const aliases = Object.keys(ui.mappings);
  const defaultProvider = ui.defaultProvider || '';
  const info = { notes: [], warnings: [], others: [] };
  if (!aliases.length) return info;

  // 别名的「实际首选供应商」= 目标链第一项解析出来的供应商。走与解析预览完全
  // 相同的解析函数（含 routing.prefer），保证面板结论不会和后端路由各说各话。
  const headProviderOf = (alias) => {
    const chain = ui.mappings[alias] || [];
    if (!chain.length) return '';
    const parsed = parseTarget(chain[0], providers);
    if (!parsed) return '';
    const resolution = resolveProviderForTarget(parsed, {
      providers, prefer: ui.prefer, defaultProvider, modelCatalog: ui.models,
    });
    return resolution?.provider || '';
  };

  const bare = stripProviderPrefix(ui.defaultModel, providers);
  if (!bare) return info;

  if (ui.mappings[bare]) {
    const head = headProviderOf(bare);
    if (ui.disabled[bare]) {
      info.notes.push(
        `默认模型「${bare}」的别名已停用，会按裸模型名解析：先在 routing.prefer 里找目录命中，`
        + `都没有才回落默认供应商 ${defaultProvider || '（未设置）'}。`,
      );
    } else if (head && head === defaultProvider) {
      info.notes.push(`默认模型「${bare}」命中别名，首选供应商 ${head}，与默认供应商一致。`);
    } else if (head) {
      const chain = (ui.mappings[bare] || []).join(' → ');
      info.warnings.push(
        `默认模型「${bare}」命中别名，实际首选是 ${head}，不是默认供应商 ${defaultProvider || '（未设置）'}。`
        + `别名优先于默认供应商与路由优先级，所以改默认供应商对这个模型无效。`
        + `要换首选，请在「模型别名映射」里编辑「${bare}」，把 ${defaultProvider || '目标供应商'}/... 移到目标链首位。`
        + `当前链：${chain}`,
      );
    }
  }

  // 其余别名同样不会走默认供应商。列出来是为了让「默认供应商到底还管不管用」有
  // 一个明确答案：在这个模型集合下，它只兜住没有别名的裸模型。
  for (const alias of aliases) {
    if (alias === bare || ui.disabled[alias]) continue;
    const head = headProviderOf(alias);
    if (!head || head === defaultProvider) continue;
    info.others.push(`${alias} → ${head}`);
  }
  return info;
}

// unusedDefaultInfo 说明「默认供应商对所有受限密钥都不可达」。
//
// scope 是入站密钥维度的硬限制：受限密钥只能打自己 providers 列表里的供应商。
// 默认供应商若不在任何受限密钥的列表里，它对那些密钥等于不存在——后端会按顺序
// 跳过后回落到第一个允许的供应商。面板仍会显示「默认兜底」，于是「设了默认却
// 走了别家」再次被当成 bug。这里把它单独点出来，并给出结论：要么给密钥加授权，
// 要么换一个所有密钥都能访问的默认供应商。
function unusedDefaultInfo() {
  const defaultProvider = ui.defaultProvider || '';
  if (!defaultProvider) return null;
  const keys = keyList().filter((key) => (key.providers || []).length);
  if (!keys.length) return null; // 全是全量密钥：默认供应商对谁都可达
  if (keys.some((key) => (key.providers || []).includes(defaultProvider))) return null;
  return `默认供应商 ${defaultProvider} 不在任何受限密钥的授权范围内（共 ${keys.length} 把受限密钥），`
    + `请求会被路由到各密钥自己范围内的第一个供应商，这里的「默认兜底」对它们不生效。`
    + `要么在「用户 / 访问范围」里给对应密钥加上 ${defaultProvider}，要么换一个所有密钥都能访问的默认供应商。`;
}

function defaultsCard(canEdit) {
  const providers = providerList();
  const providerSelect = el('select', {
    class: 'input select', disabled: !canEdit,
    on: {
      change: () => {
        ui.defaultProvider = providerSelect.value;
        const provider = providerList().find((p) => p.name === ui.defaultProvider);
        const models = provider?.config?.models || [];
        if (models.length) {
          const candidate = String(models[0]).startsWith(`${ui.defaultProvider}/`) ? models[0] : `${ui.defaultProvider}/${models[0]}`;
          if (!ui.defaultModel || ui.defaultModel === ui.config.default_model) ui.defaultModel = candidate;
        }
        render();
      },
    },
  },
    el('option', { value: '', text: '（未设置）' }),
    providers.map((p) => el('option', { value: p.name, text: `${p.name} · ${typeMeta(p.type).label || providerLabel(p.type)}`, selected: ui.defaultProvider === p.name })),
  );
  const modelList = uniqueModels();
  const modelDatalist = el('datalist', { id: 'routing-model-list' },
    modelList.map((id) => el('option', { value: id })),
  );
  const modelInput = el('input', {
    class: 'input mono', type: 'text', value: ui.defaultModel, disabled: !canEdit,
    placeholder: 'provider/model 或裸模型名', attrs: { list: 'routing-model-list' },
    on: {
      input: () => {
        ui.defaultModel = modelInput.value.trim();
        // 只刷新提示区：整卡重绘会让输入框失焦，边打字边重绘根本没法用。
        shadowNotice(shadowHost);
        hintText.textContent = ui.defaultProvider ? `当前模板供应商：${ui.defaultProvider}` : '尚未选择默认供应商';
      },
    },
  });
  const shadowHost = el('div', { class: 'form-stack' });
  shadowNotice(shadowHost);
  const hintText = el('span', { class: 'muted small', text: ui.defaultProvider ? `当前模板供应商：${ui.defaultProvider}` : '尚未选择默认供应商' });
  return card({
    title: '默认路由',
    subtitle: '裸模型名的兜底供应商与默认模型',
    actions: canEdit ? null : badge('只读', 'muted'),
    body: el('div', { class: 'form-stack' },
      shadowHost,
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '默认供应商'),
        providerSelect,
        el('p', { class: 'field-help', text: '仅当优先级里没有供应商命中该模型时才用这里。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '默认模型'),
        modelInput,
        modelDatalist,
        el('p', { class: 'field-help', text: '裸名或 provider/ 前缀；可从目录补全。' }),
      ),
      el('div', { class: 'default-hint' },
        statusDot(providerHealth(ui.defaultProvider).tone, providerHealth(ui.defaultProvider).label),
        hintText,
      ),
    ),
  });
}

// shadowNotice 渲染 aliasShadowInfo 的结论。没有冲突时返回 null，不占版面。
// host 由调用方持有，输入时只替换内容，不重绘整张卡片——否则输入框会失焦。
function shadowNotice(host = null) {
  const target = host || el('div', { class: 'form-stack' });
  const info = aliasShadowInfo();
  const unused = unusedDefaultInfo();
  if (!info.warnings.length && !info.notes.length && !unused) {
    target.replaceChildren();
    return host ? null : target;
  }
  target.replaceChildren(
    ...info.warnings.map((text) => el('div', { class: 'notice notice-warn notice-inline' },
      icon('alert', 15),
      el('div', { class: 'notice-content' }, el('span', { text })),
    )),
    ...info.notes.map((text) => el('div', { class: 'notice notice-info notice-inline' },
      icon('info', 15),
      el('div', { class: 'notice-content' }, el('span', { text })),
    )),
    ...(unused ? [el('div', { class: 'notice notice-warn notice-inline' },
      icon('alert', 15),
      el('div', { class: 'notice-content' }, el('span', { text: unused })),
    )] : []),
  );
  if (info.others.length) {
    target.appendChild(el('details', { class: 'inline-details' },
      el('summary', { text: `另有 ${info.others.length} 个别名也不会走默认供应商` }),
      el('p', { class: 'section-note', text: info.others.join('，') }),
    ));
  }
  return host ? null : target;
}

function priorityCard(canEdit) {
  const providers = providerList();
  const remaining = providers.filter((p) => !ui.prefer.includes(p.name));
  const addSelect = el('select', { class: 'input select select-sm' },
    el('option', { value: '', text: '选择供应商…' }),
    remaining.map((p) => el('option', { value: p.name, text: p.name })),
  );
  const addBtn = el('button', {
    class: 'btn btn-ghost btn-sm', type: 'button', disabled: !canEdit || !remaining.length,
    on: {
      click: () => {
        const name = addSelect.value;
        if (!name) return;
        ui.prefer.push(name);
        render();
      },
    },
  }, icon('plus', 14), '加入');
  const rows = el('div', { class: 'priority-list' });
  if (!ui.prefer.length) {
    rows.appendChild(emptyState('未设置优先级', '列表为空时，裸模型名一律走默认供应商。'));
  }
  ui.prefer.forEach((name, index) => {
    const provider = providers.find((p) => p.name === name);
    const health = providerHealth(name);
    rows.appendChild(el('div', { class: 'priority-row' },
      el('span', { class: 'priority-rank mono', text: String(index + 1).padStart(2, '0') }),
      el('div', { class: 'priority-main' },
        el('div', { class: 'priority-name' },
          el('span', { class: 'mono', text: name }),
          badge(typeMeta(provider?.type).label || providerLabel(provider?.type), 'muted'),
          statusDot(health.tone, health.label),
        ),
        el('span', { class: 'priority-meta', text: `${accountsFor(name).length} 账号 · ${provider?.config?.models?.length ? `${provider.config.models.length} 个显式模型` : '自动发现模型'}` }),
      ),
      el('div', { class: 'priority-actions' },
        el('button', {
          class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '上移', disabled: !canEdit || index === 0,
          attrs: { 'aria-label': '上移' },
          on: { click: () => { swapPrefer(index, -1); } },
        }, icon('arrowUp', 14)),
        el('button', {
          class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '下移', disabled: !canEdit || index === ui.prefer.length - 1,
          attrs: { 'aria-label': '下移' },
          on: { click: () => { swapPrefer(index, 1); } },
        }, icon('arrowDown', 14)),
        el('button', {
          class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '移出路由', disabled: !canEdit,
          attrs: { 'aria-label': '移出路由' },
          on: { click: () => { ui.prefer.splice(index, 1); render(); } },
        }, icon('close', 14)),
      ),
    ));
  });
  return card({
    title: '裸模型优先级',
    subtitle: '按顺序取第一个「目录里真的有该模型」的供应商',
    actions: canEdit ? el('div', { class: 'inline-controls' }, addSelect, addBtn) : badge('只读', 'muted'),
    body: rows,
    class: 'priority-card',
  });
}

function swapPrefer(index, delta) {
  const next = index + delta;
  if (next < 0 || next >= ui.prefer.length) return;
  const tmp = ui.prefer[index];
  ui.prefer[index] = ui.prefer[next];
  ui.prefer[next] = tmp;
  render();
}

function mappingCard(canEdit) {
  const aliases = Object.keys(ui.mappings).sort((a, b) => a.localeCompare(b));
  const rows = aliases.map((alias) => {
    const targets = ui.mappings[alias] || [];
    const disabled = !!ui.disabled[alias];
    return el('div', { class: `mapping-row ${disabled ? 'is-disabled' : ''}`.trim() },
      el('div', { class: 'mapping-main' },
        el('div', { class: 'mapping-alias' },
          el('span', { class: 'mono', text: alias }),
          disabled ? badge('已停用', 'warn') : badge('启用', 'ok'),
        ),
        el('div', { class: 'mapping-targets' }, targets.length
          ? targets.map((target, index) => el('span', { class: 'mapping-target' },
            index > 0 ? el('span', { class: 'mapping-arrow', text: '↓' }) : null,
            index === 0 ? badge('首选', 'accent') : badge(`回退 ${index}`, 'muted'),
            el('span', { class: 'mono', text: target }),
          ))
          : el('span', { class: 'muted small', text: '没有目标（配置异常）' })),
      ),
      el('div', { class: 'mapping-actions' },
        el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button', disabled: !canEdit,
          on: { click: () => toggleMapping(alias) },
        }, disabled ? '启用' : '停用'),
        el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button', disabled: !canEdit,
          on: { click: () => openMappingModal(alias) },
        }, icon('edit', 14), '编辑'),
        el('button', {
          class: 'btn btn-danger-ghost btn-sm', type: 'button', disabled: !canEdit,
          on: { click: () => deleteMapping(alias) },
        }, icon('trash', 14), '删除'),
      ),
    );
  });
  if (!rows.length) rows.push(emptyState('还没有别名映射', '把客户端常用的 gpt-4o 等名字映射到实际模型，并支持回退链。'));
  return card({
    title: '模型别名映射',
    subtitle: '别名优先于优先级排序；目标链在失败/额度耗尽时依次回退',
    actions: canEdit ? el('button', {
      class: 'btn btn-primary btn-sm', type: 'button',
      on: { click: () => openMappingModal('') },
    }, icon('plus', 14), '添加映射') : badge('只读', 'muted'),
    body: el('div', {},
      el('div', { class: 'notice notice-info notice-inline' },
        icon('info', 15),
        el('span', { text: '实际首选 = 这条别名目标链的第一项；default_provider 与「裸模型优先级」只在没有别名命中时生效。要换首选，编辑别名并调整目标顺序。' }),
      ),
      ui.mappingError ? el('div', { class: 'notice notice-warn notice-inline' }, icon('alert', 15), el('span', { text: ui.mappingError })) : null,
      el('div', { class: 'mapping-list' }, rows),
    ),
    class: 'mapping-card',
  });
}

function scopeCard(canEdit) {
  const keys = keyList();
  const role = authState().role;
  const body = el('div', { class: 'scope-body' });
  if (!keys.length) {
    body.appendChild(emptyState(
      role === 'admin' ? '还没有客户端访问密钥' : '客户端密钥不可见',
      role === 'admin' ? '在「系统 · 访问密钥」中添加 api_key，并在路由预览里验证 scope。' : '只有管理员可以查看客户端密钥列表；scope 预览以管理员视图为准。',
      role === 'admin' ? el('a', { class: 'btn btn-ghost btn-sm', href: '#/system' }, '前往系统') : null,
    ));
  } else {
    keys.forEach((key, index) => {
      const scope = key.providers || [];
      const allowedModels = countModelsForScope(scope);
      const active = ui.keyIndex === index;
      body.appendChild(el('button', {
        class: `scope-key-card ${active ? 'active' : ''}`.trim(),
        type: 'button',
        on: { click: () => { ui.keyIndex = active ? -1 : index; render(); } },
      },
        el('div', { class: 'scope-key-head' },
          el('span', { class: 'scope-key-name', text: key.name || `密钥 ${index + 1}` }),
          active ? badge('用于预览', 'accent') : null,
        ),
        el('code', { class: 'scope-key-value', text: key.key || '（空）' }),
        el('div', { class: 'scope-tags' },
          scope.length
            ? scope.map((name) => badge(name, 'info'))
            : badge('全量供应商', 'ok'),
        ),
        el('div', { class: 'scope-key-foot' },
          el('span', { text: scope.length ? `允许 ${scope.length} 个供应商` : '可访问全部供应商' }),
          el('span', { text: `目录内 ${allowedModels} 个模型` }),
        ),
      ));
    });
    body.appendChild(el('p', { class: 'section-note', text: '此处只做核对；实际校验在服务端，按密钥的 providers 列表收窄。' }));
  }
  return card({
    title: '客户端密钥 scope',
    subtitle: '按 api_keys[].providers 收窄模型可见范围',
    actions: el('a', { class: 'btn btn-ghost btn-sm', href: '#/system' }, '管理密钥'),
    body,
    class: 'scope-card',
  });
}

function countModelsForScope(scope) {
  if (!ui.models.length) return 0;
  if (!scope.length) return ui.models.length;
  return ui.models.filter((m) => scope.includes(m.owned_by)).length;
}

function resolutionCard() {
  const filterList = uniqueModels();
  const datalist = el('datalist', { id: 'resolution-model-list' }, filterList.map((id) => el('option', { value: id })));
  const input = el('input', {
    class: 'input mono resolution-input', type: 'text', value: ui.resolutionInput,
    placeholder: '输入模型名，例如 gpt-4o / gemini-2.5-pro / runable/zai/glm-5.3',
    attrs: { list: 'resolution-model-list' },
  });
  const keys = keyList();
  const keySelect = el('select', { class: 'input select select-sm' },
    el('option', { value: '-1', text: '全量视角（不带 scope）', selected: ui.keyIndex < 0 }),
    keys.map((key, index) => el('option', {
      value: String(index),
      text: `${key.name || `密钥 ${index + 1}`} · ${scopeText(key)}`,
      selected: ui.keyIndex === index,
    })),
  );
  const resultHost = el('div', { class: 'resolution-result' });
  const update = () => {
    ui.resolutionInput = input.value;
    resultHost.replaceChildren(renderResolution());
  };
  input.addEventListener('input', debounce(update, 120));
  keySelect.addEventListener('change', () => {
    ui.keyIndex = Number(keySelect.value);
    update();
  });
  update();

  return card({
    title: '解析预览',
    subtitle: '输入模型名，追踪别名 → 供应商 → 账号与 scope 的完整解析',
    actions: el('span', { class: 'small muted', text: '不触发真实请求' }),
    body: el('div', { class: 'resolution-body' },
      el('div', { class: 'resolution-controls' }, input, keySelect, datalist),
      resultHost,
    ),
    class: 'resolution-card',
  });
}

function scopeText(key) {
  const scope = key.providers || [];
  return scope.length ? scope.join(', ') : '全量';
}

function uniqueModels() {
  const set = new Set();
  for (const model of ui.models) {
    if (model?.id) set.add(String(model.id));
    if (Array.isArray(model?.alias_of)) {
      for (const target of model.alias_of) set.add(String(target));
    }
  }
  for (const alias of Object.keys(ui.mappings)) set.add(alias);
  if (ui.defaultModel) set.add(ui.defaultModel);
  return Array.from(set).sort();
}

// aliasSuggestions：别名输入框的补全候选（目录模型 + 已剥离 provider 前缀的裸名 +
// 现有别名 + 供应商显式模型）。只是建议，不限制手输。
function aliasSuggestions() {
  const set = new Set();
  for (const model of ui.models) {
    const id = String(model?.id || '');
    if (!id) continue;
    set.add(id);
    const slash = id.indexOf('/');
    if (slash > 0) set.add(id.slice(slash + 1));
  }
  for (const alias of Object.keys(ui.mappings)) set.add(alias);
  for (const provider of providerList()) {
    for (const model of provider?.config?.models || []) set.add(String(model));
  }
  return Array.from(set).sort();
}

// modelIdsForProvider：某供应商在目录里可选的模型，统一去掉 "provider/" 前缀，
// 便于拼成映射目标的 "provider/model" 形式。
function modelIdsForProvider(name) {
  if (!name) return [];
  const set = new Set();
  for (const model of ui.models) {
    const id = String(model?.id || '');
    // 别名是路由层的名字，不能再作为另一个映射的上游目标，否则容易配出环。
    if (!id || model.alias_of) continue;
    if (model.owned_by === name) {
      set.add(id.startsWith(`${name}/`) ? id.slice(name.length + 1) : id);
    } else if (id.startsWith(`${name}/`)) {
      set.add(id.slice(name.length + 1));
    }
  }
  const provider = providerList().find((p) => p.name === name);
  for (const model of provider?.config?.models || []) {
    // 配置里存的是上游原始 ID；即使它恰好以 provider 名开头（例如
    // provider=cline-pass、上游模型=cline-pass/deepseek-v4.1-flash），
    // 也不能再剥一层，否则会把上游 ID 改坏。
    set.add(String(model));
  }
  return Array.from(set).sort();
}

function renderResolution() {
  const inputValue = String(ui.resolutionInput || '').trim();
  const box = el('div', { class: 'resolve-box' });
  if (!inputValue) {
    box.appendChild(emptyState('输入模型名开始解析', '支持裸模型名、provider/model、全局别名与默认模型。'));
    return box;
  }
  const result = resolveModel(inputValue);
  const status = result.denied
    ? { tone: 'danger', text: 'scope 拒绝' }
    : (result.account?.healthy === false ? { tone: 'warn', text: '无可用账号' } : { tone: 'ok', text: '可路由' });
  box.appendChild(el('div', { class: 'resolve-headline' },
    el('div', { class: 'resolve-target' },
      el('span', { class: 'resolve-original mono', text: inputValue }),
      el('span', { class: 'resolve-arrow', 'aria-hidden': 'true' }, icon('arrowRight', 16)),
      el('span', { class: 'resolve-final mono', text: `${result.provider || '—'}/${result.model || '—'}` }),
    ),
    statusDot(status.tone, status.text),
  ));

  const steps = el('div', { class: 'resolve-steps' });
  steps.appendChild(resolveStep(1, '输入解析', result.aliasHit
    ? `命中全局别名「${result.alias}」${ui.disabled[result.alias] ? '（规则已停用，按普通模型处理）' : ''}`
    : (result.prefixed ? '带供应商前缀，直接指定上游' : '裸模型名，按优先级与目录选择供应商')));
  steps.appendChild(resolveStep(2, '供应商选择', result.provider
    ? `${result.provider}（${result.providerReason}）`
    : '没有可用供应商'));
  steps.appendChild(resolveStep(3, '账号选择', result.account
    ? `${result.account.label}${result.account.healthy ? '' : '（当前不可用）'}`
    : '未找到可用账号'));
  steps.appendChild(resolveStep(4, '客户端 scope', result.denied
    ? `拒绝：密钥仅允许 ${result.keyProviders.join(', ') || '无供应商'}`
    : (result.key ? `允许：${result.keyProviders.length ? result.keyProviders.join(', ') : '全量供应商'}` : '未指定客户端密钥，按全量视角')));
  box.appendChild(steps);

  if (result.notes.length) {
    box.appendChild(el('div', { class: 'resolve-notes' },
      result.notes.map((text) => el('div', { class: 'resolve-note' }, icon('info', 14), el('span', { text })))));
  }
  if (result.warnings.length) {
    box.appendChild(el('div', { class: 'resolve-warnings' },
      result.warnings.map((text) => el('div', { class: 'resolve-warning' }, icon('alert', 14), el('span', { text })))));
  }
  if (result.candidates.length) {
    box.appendChild(el('div', { class: 'candidate-list' },
      el('div', { class: 'candidate-head' },
        el('span', { text: '候选供应商' }),
        el('span', { class: 'muted small', text: result.candidates.length ? '按优先级排列' : '' }),
      ),
      result.candidates.map((candidate) => el('div', { class: 'candidate-row' },
        el('span', { class: `candidate-rank mono ${candidate.selected ? 'selected' : ''}`.trim(), text: candidate.selected ? '●' : String(candidate.rank).padStart(2, '0') }),
        el('span', { class: 'candidate-name mono', text: candidate.name }),
        candidate.models.includes(result.model) ? badge('目录命中', 'ok') : badge('未见模型', 'muted'),
        statusDot(candidate.health.tone, candidate.health.label),
      )),
    ));
  }
  return box;
}

function resolveStep(index, title, text) {
  return el('div', { class: 'resolve-step' },
    el('span', { class: 'resolve-step-index', text: String(index) }),
    el('div', { class: 'resolve-step-content' },
      el('span', { class: 'resolve-step-title', text: title }),
      el('span', { class: 'resolve-step-text', text }),
    ),
  );
}

function resolveModel(raw) {
  const providers = providerList();
  const aliases = ui.mappings;
  const disabled = ui.disabled;
  const keys = keyList();
  const key = ui.keyIndex >= 0 ? keys[ui.keyIndex] : null;
  const keyProviders = key?.providers || [];
  const prefer = ui.prefer.slice();
  const defaultProvider = ui.defaultProvider || ui.config?.default_provider || providers[0]?.name || '';
  const result = {
    aliasHit: false,
    alias: '',
    prefixed: false,
    provider: '',
    model: '',
    providerReason: '',
    account: null,
    denied: false,
    key,
    keyProviders,
    warnings: [],
    notes: [],
    candidates: [],
  };

  let chain = [];
  if (aliases[raw]) {
    result.aliasHit = true;
    result.alias = raw;
    if (disabled[raw]) {
      result.warnings.push('该别名规则已停用，将继续按普通模型名解析。');
    } else {
      chain = (aliases[raw] || []).slice();
      result.notes.push(`别名映射目标链：${chain.join(' → ')}`);
    }
  }
  // 带 provider/ 前缀的请求不经过别名表（与后端 routeResolved 同口径）。若裸名
  // 恰好有一条别名，这里必须说清楚：客户端发带前缀的名字会绕过别名，而发裸名
  // 才会走别名链——否则「同一个模型两种写法落到不同供应商」会被当成随机行为。
  if (!result.aliasHit) {
    const bare = stripProviderPrefix(raw, providers);
    if (bare && bare !== raw && aliases[bare] && !disabled[bare]) {
      result.warnings.push(
        `「${raw}」带供应商前缀，会直接命中该上游，不经过别名表。`
        + `客户端若发裸名「${bare}」，则会走别名链 ${(aliases[bare] || []).join(' → ')}。`,
      );
    }
  }
  if (!chain.length) chain = [raw];

  // chainScopeSkips 记录「别名链上哪些回退目标会被入站密钥的 scope 直接跳过」。
  // 别名链是硬规则，但网关只在入口校验主目标的 scope；链上的后续目标由
  // tryFallback 按 KeyProviders 过滤，越界的那条被静默丢弃。运维配了回退却
  // 永远不触发，日志里只有一行「未授权该供应商」——这里提前说明。
  //
  // 必须独立扫完整条链：下面的选择循环一旦命中健康目标就 break，若把统计写进
  // 那个循环，回退目标根本轮不到被检查（实测：只报了主目标，回退告警从未出现）。
  const chainScopeSkips = [];
  if (keyProviders.length) {
    chain.forEach((target, index) => {
      if (index === 0) return; // 主目标由下面的 denied 逻辑覆盖
      const parsed = parseTarget(target, providers);
      if (!parsed) return;
      const resolution = resolveProviderForTarget(parsed, {
        providers, prefer, defaultProvider, modelCatalog: ui.models,
      });
      if (!resolution) return;
      if (!keyProviders.includes(resolution.provider)) {
        chainScopeSkips.push({ target, provider: resolution.provider });
      }
    });
  }
  const selectedCandidates = [];
  let chosen = null;
  for (const target of chain) {
    const parsed = parseTarget(target, providers);
    if (!parsed) continue;
    const resolution = resolveProviderForTarget(parsed, {
      providers, prefer, defaultProvider, modelCatalog: ui.models,
    });
    if (!resolution) continue;
    const health = providerHealth(resolution.provider);
    const account = pickAccount(resolution.provider);
    const candidateList = resolution.candidates.map((candidate, index) => ({
      ...candidate,
      rank: index + 1,
      selected: candidate.name === resolution.provider,
      health: providerHealth(candidate.name),
      models: modelsOf(candidate.name),
    }));
    selectedCandidates.push(...candidateList.map((c) => ({ ...c, selected: c.name === resolution.provider })));
    const targetResult = {
      ...result,
      prefixed: parsed.prefixed,
      provider: resolution.provider,
      model: parsed.model,
      providerReason: resolution.reason,
      account: account
        ? { label: account.label || '供应商密钥', healthy: health.tone === 'ok', tone: health.tone }
        : { label: '无可用账号', healthy: false, tone: 'warn' },
      candidates: candidateList,
    };
    if (!chosen) chosen = targetResult;
    const healthy = health.tone === 'ok';
    if (healthy && account) {
      chosen = targetResult;
      break;
    }
    if (!chosen) chosen = targetResult;
  }

  const final = chosen || {
    ...result,
    provider: '',
    model: raw,
    providerReason: '',
    account: null,
    candidates: [],
    notes: result.notes,
    warnings: [...result.warnings, '模型目录与供应商列表都为空，无法解析。'],
  };
  final.candidates = dedupeCandidates(selectedCandidates)
    .map((candidate) => ({ ...candidate, selected: candidate.name === final.provider }));
  final.denied = !!(final.provider && keyProviders.length && !keyProviders.includes(final.provider));
  if (final.denied) {
    final.warnings.push(`客户端密钥「${key.name || '未命名'}」的 scope 不包含供应商 ${final.provider}。`);
  }
  if (final.account && !final.account.healthy) {
    final.warnings.push('该供应商当前没有健康账号；实际调用会进入冷却/切换或失败。');
  }
  if (!final.account) final.warnings.push('没有匹配的账号或供应商密钥；实际调用会返回无可用凭证。');
  // 回退目标被 scope 拒掉：配了回退却永远不会生效。chainScopeSkips 只含回退项
  // （主目标已由上面的 denied 逻辑覆盖）。
  for (const skip of chainScopeSkips) {
    final.warnings.push(
      `别名链的回退目标「${skip.target}」落在供应商 ${skip.provider}，`
      + `但客户端密钥「${key?.name || '未命名'}」只授权 ${keyProviders.join(', ')}——该回退会被静默跳过。`,
    );
  }
  if (final.model && !modelsOf(final.provider).includes(final.model) && !final.prefixed) {
    final.warnings.push('当前模型目录没有该裸模型，已回退默认供应商；上游可能返回模型不存在。');
  }
  return final;
}

function dedupeCandidates(list) {
  const seen = new Set();
  const out = [];
  for (const candidate of list) {
    if (seen.has(candidate.name)) continue;
    seen.add(candidate.name);
    out.push(candidate);
  }
  return out;
}

function parseTarget(target, providers) {
  const value = String(target || '').trim();
  if (!value) return null;
  const providerNames = providers.map((p) => p.name).sort((a, b) => b.length - a.length);
  for (const name of providerNames) {
    if (value === name) return { prefixed: true, providerName: name, model: '' };
    if (value.startsWith(`${name}/`)) {
      return { prefixed: true, providerName: name, model: value.slice(name.length + 1) };
    }
  }
  return { prefixed: false, providerName: '', model: value };
}

function resolveProviderForTarget(parsed, { providers, prefer, defaultProvider, modelCatalog }) {
  if (parsed.prefixed) {
    const exists = providers.some((p) => p.name === parsed.providerName);
    if (!exists) return null;
    return {
      provider: parsed.providerName,
      reason: '显式供应商前缀',
      candidates: [{ name: parsed.providerName, models: modelsOf(parsed.providerName) }],
    };
  }
  const model = parsed.model;
  const serving = providers
    .filter((p) => modelsOf(p.name, modelCatalog).includes(model))
    .map((p) => p.name);
  const order = [...prefer.filter((name) => providers.some((p) => p.name === name))];
  const orderedServing = order.filter((name) => serving.includes(name));
  const candidates = orderedServing.length ? orderedServing : (serving.length ? serving : (defaultProvider ? [defaultProvider] : []));
  if (!candidates.length) return null;
  const chosen = candidates[0];
  const reason = orderedServing.length
    ? `routing.prefer 中第 ${prefer.indexOf(chosen) + 1} 顺位，目录命中`
    : (serving.length ? '目录命中（未在优先级中）' : '目录未命中，回退默认供应商');
  return {
    provider: chosen,
    reason,
    candidates: candidates.map((name) => ({ name, models: modelsOf(name, modelCatalog) })),
  };
}

function modelsOf(providerName, catalog = ui.models) {
  return Array.from(new Set((catalog || [])
    .filter((m) => m.owned_by === providerName || String(m.id || '').startsWith(`${providerName}/`))
    .map((m) => {
      const id = String(m.id || '');
      return id.startsWith(`${providerName}/`) ? id.slice(providerName.length + 1) : id;
    })));
}

function pickAccount(providerName) {
  const provider = providerList().find((p) => p.name === providerName);
  const accounts = accountsFor(providerName);
  const meta = typeMeta(provider?.type);
  if (!accounts.length) {
    if (isKeyBasedProvider(meta)) {
      if (hasProviderKeys(provider?.config)) return { label: '供应商密钥', healthy: true };
      return null;
    }
    return null;
  }
  const now = Date.now();
  let fallback = null;
  for (const account of accounts) {
    const health = healthOfAccount(account);
    if (!fallback) fallback = account;
    const cooling = account.cooling_until && (parseDate(account.cooling_until)?.getTime() || 0) > now;
    if (!account.auth_invalid && !cooling && account.has_token !== false) return account;
  }
  return fallback;
}

// ---------------------------------------------------------------------------
// 保存 / 映射操作
// ---------------------------------------------------------------------------

async function saveRouting() {
  if (!(await requireAdmin({ message: '修改路由需要管理员权限' }))) return;
  if (!ui.config) return;
  if (!ui.defaultProvider) {
    toastWarn('请先选择默认供应商');
    return;
  }
  const next = JSON.parse(JSON.stringify(ui.config));
  next.default_provider = ui.defaultProvider;
  next.default_model = ui.defaultModel;
  next.routing = { ...(next.routing || {}), prefer: ui.prefer.slice() };
  try {
    const res = await api.config.save(next, { signal: pageSignal?.signal });
    toastSuccess('路由默认值与优先级已保存');
    if (Array.isArray(res?.restart_required) && res.restart_required.length) {
      toastWarn(`以下设置需重启生效：${res.restart_required.join('、')}`);
    }
    await refreshConfig();
  } catch (error) {
    toastError(error);
  }
}

async function refreshConfig() {
  if (!pageSignal) return;
  try {
    const payload = await api.config.get({ signal: pageSignal.signal, dedupe: false });
    ui.config = payload?.config || (Array.isArray(payload?.providers) ? payload : ui.config);
    ui.configPath = payload?.path || ui.configPath;
    ui.defaultProvider = String(ui.config?.default_provider || '');
    ui.defaultModel = String(ui.config?.default_model || '');
    ui.prefer = (Array.isArray(ui.config?.routing?.prefer) ? ui.config.routing.prefer : []).filter((name) => providerList().some((p) => p.name === name));
    render();
  } catch (error) {
    toastError(error);
  }
}

function openMappingModal(alias) {
  if (!isAdmin()) {
    toastWarn('需要管理员权限');
    return;
  }
  const editing = !!alias;
  const aliasInput = el('input', {
    class: 'input mono', type: 'text', value: alias || '', placeholder: '例如 gpt-4o',
    disabled: editing, autocomplete: 'off', spellcheck: 'false',
    attrs: { list: 'mapping-alias-options' },
  });
  const aliasOptions = el('datalist', { id: 'mapping-alias-options' },
    aliasSuggestions().map((value) => el('option', { value })),
  );
  const enabledInput = el('input', { type: 'checkbox', checked: editing ? !ui.disabled[alias] : true });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });

  // 目标链用**列表**编辑（顺序即回退优先级：首项为主目标，往下依次回退）。
  // 列表项可直接上移 / 下移 / 删除——「同一个模型想优先走某个供应商」就是把它
  // 拖到第一位。选择器与手动输入都只往这份列表里追加，不再维护自由文本。
  let targets = editing ? (ui.mappings[alias] || []).slice() : [];
  const chainList = el('div', { class: 'chain-list' });

  const addTarget = (target) => {
    const value = String(target || '').trim();
    if (!value) {
      errorBox.textContent = '目标不能为空';
      return false;
    }
    if (!/^[^/\s]+\/.+/.test(value)) {
      errorBox.textContent = '目标需为 provider/model 形式';
      return false;
    }
    if (targets.includes(value)) {
      errorBox.textContent = '目标链中已存在该模型';
      return false;
    }
    targets.push(value);
    errorBox.textContent = '';
    renderChain();
    return true;
  };
  const moveTarget = (index, delta) => {
    const next = index + delta;
    if (next < 0 || next >= targets.length) return;
    const tmp = targets[index];
    targets[index] = targets[next];
    targets[next] = tmp;
    renderChain();
  };

  function renderChain() {
    chainList.replaceChildren();
    if (!targets.length) {
      chainList.appendChild(el('p', { class: 'chain-empty', text: '还没有目标。用下面的选择器添加，或手动输入任意 provider/model。' }));
      return;
    }
    targets.forEach((target, index) => {
      const slash = target.indexOf('/');
      const provider = slash > 0 ? target.slice(0, slash) : '';
      chainList.appendChild(el('div', { class: 'chain-row' },
        el('span', { class: 'chain-rank mono', text: String(index + 1).padStart(2, '0') }),
        el('div', { class: 'chain-main' },
          el('span', { class: 'chain-target mono', text: target }),
          index === 0 ? badge('主目标', 'accent') : badge('回退', 'muted'),
          provider ? el('span', { class: 'chain-provider', text: providerLabel(providerTypeOf(provider)) }) : null,
        ),
        el('div', { class: 'chain-actions' },
          el('button', {
            class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '上移（提高优先级）',
            disabled: index === 0, attrs: { 'aria-label': '上移' },
            on: { click: () => moveTarget(index, -1) },
          }, icon('arrowUp', 14)),
          el('button', {
            class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '下移（降低优先级）',
            disabled: index === targets.length - 1, attrs: { 'aria-label': '下移' },
            on: { click: () => moveTarget(index, 1) },
          }, icon('arrowDown', 14)),
          el('button', {
            class: 'btn btn-icon btn-ghost btn-sm', type: 'button', title: '移出目标链',
            attrs: { 'aria-label': '移出目标链' },
            on: { click: () => { targets.splice(index, 1); errorBox.textContent = ''; renderChain(); } },
          }, icon('close', 14)),
        ),
      ));
    });
  }

  // 列表选择：先选供应商，再从它的目录模型里挑目标，点「加入」追加到目标链列表。
  const providers = providerList();
  const providerSelect = el('select', { class: 'input select' },
    el('option', { value: '', text: '选择供应商…' }),
    providers.map((p) => el('option', {
      value: p.name,
      text: `${p.name} · ${typeMeta(p.type).label || providerLabel(p.type)}`,
      selected: providers.length === 1,
    })),
  );
  const modelSelect = el('select', { class: 'input select' });
  const addTargetBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, icon('plus', 14), '加入目标链');
  const fillModels = () => {
    const name = providerSelect.value;
    const models = modelIdsForProvider(name);
    modelSelect.replaceChildren(
      el('option', { value: '', text: models.length ? '选择模型…' : (name ? '该供应商暂无目录模型' : '先选择供应商') }),
      ...models.map((id) => el('option', { value: id, text: id })),
    );
    modelSelect.disabled = !name || !models.length;
    addTargetBtn.disabled = !name || !models.length;
  };
  providerSelect.addEventListener('change', fillModels);
  fillModels();
  addTargetBtn.addEventListener('click', () => {
    const name = providerSelect.value;
    const model = modelSelect.value;
    if (!name || !model) {
      errorBox.textContent = '请先选择供应商与模型';
      return;
    }
    addTarget(`${name}/${model}`);
  });

  // 手动输入：目录里没有的模型（或需要任意 provider/model）走这里。
  const manualInput = el('input', {
    class: 'input mono', type: 'text', placeholder: '手动输入 provider/model 后回车',
    autocomplete: 'off', spellcheck: 'false',
  });
  const manualAddBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, icon('plus', 14), '添加');
  const manualAdd = () => {
    if (addTarget(manualInput.value)) manualInput.value = '';
  };
  manualAddBtn.addEventListener('click', manualAdd);
  manualInput.addEventListener('keydown', (event) => {
    if (event.key === 'Enter') {
      event.preventDefault();
      manualAdd();
    }
  });

  renderChain();

  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, editing ? '保存映射' : '添加映射');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');

  const modal = openModal({
    title: editing ? `编辑映射 ${alias}` : '添加模型别名',
    subtitle: '别名为客户端可见模型名；目标链从上到下依次回退，第一位优先。',
    size: 'md',
    body: el('div', { class: 'mapping-form' },
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '别名', el('span', { class: 'required-mark', text: '*' })),
        aliasInput,
        aliasOptions,
        el('p', { class: 'field-help', text: editing ? '编辑时不允许改名；如需新名字请删除后重新添加。' : '建议使用客户端现有的模型名；输入框支持从模型目录补全。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '目标链', el('span', { class: 'required-mark', text: '*' })),
        chainList,
        el('div', { class: 'chain-picker' },
          providerSelect,
          modelSelect,
          addTargetBtn,
        ),
        el('div', { class: 'chain-picker' },
          manualInput,
          manualAddBtn,
        ),
        el('p', { class: 'field-help', text: '顺序即优先级：第一位是主目标，后面的依次在失败 / 额度耗尽时回退。用 ↑↓ 调整顺序。' }),
      ),
      el('label', { class: 'check-inline' }, enabledInput, el('span', { text: '启用该映射' })),
      errorBox,
    ),
    footer: [cancelBtn, saveBtn],
    initialFocus: editing ? manualInput : aliasInput,
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    const name = aliasInput.value.trim();
    if (!name) { errorBox.textContent = '别名不能为空'; return; }
    if (!targets.length) { errorBox.textContent = '至少添加一个目标'; return; }
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    try {
      await api.mapping.set({ alias: name, targets: targets.slice(), enabled: enabledInput.checked });
      toastSuccess('映射已保存');
      modal.close();
      await refreshMappings();
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
    }
  });
}

// providerTypeOf 反查某供应商名对应的 type（用于目标链里显示类型标签）。
function providerTypeOf(name) {
  return providerList().find((p) => p.name === name)?.type || '';
}

async function toggleMapping(alias) {
  if (!(await requireAdmin())) return;
  try {
    await api.mapping.set({ alias, enabled: !!ui.disabled[alias] });
    toastSuccess(ui.disabled[alias] ? '映射已启用' : '映射已停用');
    await refreshMappings();
  } catch (error) {
    toastError(error);
  }
}

async function deleteMapping(alias) {
  if (!(await requireAdmin())) return;
  const ok = await confirmDialog({
    title: '删除映射',
    message: `确定删除别名「${alias}」及其目标链吗？客户端再使用该名字时将按路由优先级解析。`,
    confirmText: '删除',
    danger: true,
  });
  if (!ok) return;
  try {
    await api.mapping.remove(alias);
    toastSuccess('映射已删除');
    await refreshMappings();
  } catch (error) {
    toastError(error);
  }
}

async function refreshMappings() {
  if (!pageSignal) return;
  try {
    const payload = await api.mapping.get({ signal: pageSignal.signal, dedupe: false });
    applyMappingPayload(payload);
    ui.mappingError = '';
  } catch (error) {
    ui.mappingError = describeError(error);
    toastError(error);
  }
  render();
}
