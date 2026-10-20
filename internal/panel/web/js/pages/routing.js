// routing.js — 全局路由：默认供应商/模型、优先级、别名映射、密钥 scope 与解析预览。

import { api, describeError, isForbidden } from '../core/api.js';
import {
  authState, isAdmin, isAuthenticated, openLoginModal, requireAdmin,
} from '../core/auth.js';
import {
  el, card, badge, statusDot, emptyState, loadingBlock, icon, debounce,
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

function providerHealth(name) {
  const accounts = accountsFor(name);
  if (!accounts.length) {
    const provider = providerList().find((p) => p.name === name);
    const meta = typeMeta(provider?.type);
    if (isKeyBasedProvider(meta)) {
      return { tone: hasProviderKeys(provider?.config) ? 'ok' : 'warn', label: hasProviderKeys(provider?.config) ? '供应商密钥' : '无可用密钥' };
    }
    return { tone: 'warn', label: '无账号' };
  }
  for (const account of accounts) {
    const health = healthOfAccount(account);
    if (health.tone === 'ok') return { tone: 'ok', label: '有可用账号' };
  }
  return { tone: 'warn', label: '暂无健康账号' };
}

function render() {
  if (!rootEl) return;
  const page = el('div', { class: 'page page-routing' });
  const auth = authState();
  const canEdit = isAdmin();

  page.appendChild(el('div', { class: 'page-head' },
    el('div', {},
      el('div', { class: 'eyebrow', text: 'ROUTING CONTROL' }),
      el('h1', { class: 'page-title', text: '路由' }),
      el('p', { class: 'page-subtitle', text: '裸模型名的供应商优先级、全局别名映射、客户端密钥 scope 与一次完整的解析预览。' }),
    ),
    el('div', { class: 'page-actions' },
      el('span', { class: 'auto-refresh-note', text: canEdit ? '修改后需保存' : '只读视图' }),
      el('button', {
        class: 'btn btn-ghost btn-sm', type: 'button',
        on: { click: () => loadAll() },
      }, icon('refresh', 15), '刷新'),
      canEdit && ui.config
        ? el('button', {
          class: 'btn btn-primary btn-sm', type: 'button',
          on: { click: () => saveRouting() },
        }, icon('check', 15), '保存默认与优先级')
        : null,
    ),
  ));

  if (!ui.config) {
    if (ui.loading) {
      page.appendChild(loadingBlock('加载路由配置'));
    } else {
      page.appendChild(el('div', { class: 'notice notice-info' },
        icon('info', 16),
        el('div', { class: 'notice-content' },
          el('strong', { text: isAuthenticated() ? '路由配置不可读' : '登录后查看路由' }),
          el('span', { text: ui.configError || '路由配置需要管理员或只读角色。' }),
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

  page.appendChild(el('div', { class: 'context-strip' },
    badge(canEdit ? '管理员' : '只读', canEdit ? 'accent' : 'info'),
    el('span', { class: 'context-note', text: '解析预览基于当前模型目录与账号快照，不会触发真实请求。' }),
    ui.configPath ? el('span', { class: 'context-path mono', text: ui.configPath }) : null,
  ));

  page.appendChild(el('div', { class: 'routing-top-grid' },
    defaultsCard(canEdit),
    priorityCard(canEdit),
  ));
  page.appendChild(mappingCard(canEdit));
  page.appendChild(el('div', { class: 'routing-bottom-grid' },
    scopeCard(canEdit),
    resolutionCard(canEdit),
  ));

  page.appendChild(el('div', { class: 'page-footer-note' },
    el('span', { text: '别名优先于 routing.prefer；带 provider/ 前缀的请求不经过优先级选择。' }),
    el('span', { text: 'scope 由客户端 api_key 的 providers 列表决定；空列表 = 全量。' }),
  ));
  rootEl.replaceChildren(page);
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
    on: { input: () => { ui.defaultModel = modelInput.value.trim(); } },
  });
  return card({
    title: '默认路由',
    subtitle: '裸模型名的兜底供应商与默认模型',
    actions: canEdit ? null : badge('只读', 'muted'),
    body: el('div', { class: 'form-stack' },
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '默认供应商'),
        providerSelect,
        el('p', { class: 'field-help', text: '所有供应商都没有该裸模型时的最后兜底。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '默认模型'),
        modelInput,
        modelDatalist,
        el('p', { class: 'field-help', text: '可为裸名或带 provider/ 前缀；支持从目录中补全。' }),
      ),
      el('div', { class: 'default-hint' },
        statusDot(providerHealth(ui.defaultProvider).tone, providerHealth(ui.defaultProvider).label),
        el('span', { class: 'muted small', text: ui.defaultProvider ? `当前模板供应商：${ui.defaultProvider}` : '尚未选择默认供应商' }),
      ),
    ),
  });
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
    body.appendChild(el('p', { class: 'section-note', text: 'scope 由服务端按客户端密钥校验；此卡片用于配置核对与解析预览。' }));
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
  if (!chain.length) chain = [raw];

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
  const targetsInput = el('textarea', {
    class: 'input textarea mono', rows: 4,
    value: editing ? (ui.mappings[alias] || []).join('\n') : '',
    placeholder: 'provider/model，每行一个；自上而下构成回退链\n例如 atria/Atria-Dawn-Preview\ncline-pass/deepseek-v4.1-flash',
  });
  const enabledInput = el('input', { type: 'checkbox', checked: editing ? !ui.disabled[alias] : true });
  const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });

  // 列表选择：先选供应商，再从它的目录模型里挑目标，点「加入目标链」追加到下方文本框。
  // 文本框保留手动输入，两种方式写的是同一份目标链。
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
    const target = `${name}/${model}`;
    const lines = targetsInput.value.split(/\r?\n|,/).map((s) => s.trim()).filter(Boolean);
    if (lines.includes(target)) {
      errorBox.textContent = '目标链中已存在该模型';
      return;
    }
    lines.push(target);
    targetsInput.value = lines.join('\n');
    errorBox.textContent = '';
  });

  const saveBtn = el('button', { class: 'btn btn-primary', type: 'button' }, editing ? '保存映射' : '添加映射');
  const cancelBtn = el('button', { class: 'btn btn-ghost', type: 'button' }, '取消');

  const modal = openModal({
    title: editing ? `编辑映射 ${alias}` : '添加模型别名',
    subtitle: '别名为客户端可见模型名；目标链从上到下依次回退。',
    size: 'md',
    body: el('div', { class: 'mapping-form' },
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '别名', el('span', { class: 'required-mark', text: '*' })),
        aliasInput,
        aliasOptions,
        el('p', { class: 'field-help', text: editing ? '编辑时不允许改名；如需新名字请删除后重新添加。' : '建议使用客户端现有的模型名；输入框支持从模型目录补全。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '从模型列表选择目标'),
        el('div', { class: 'mapping-picker' }, providerSelect, modelSelect, addTargetBtn),
        el('p', { class: 'field-help', text: '选好供应商与模型后点「加入目标链」；也可以在下方手动输入任意 provider/model。' }),
      ),
      el('div', { class: 'field' },
        el('label', { class: 'field-label' }, '目标链', el('span', { class: 'required-mark', text: '*' })),
        targetsInput,
        el('p', { class: 'field-help', text: '每行一个 provider/model；首项为主目标，后续为自动回退。' }),
      ),
      el('label', { class: 'check-inline' }, enabledInput, el('span', { text: '启用该映射' })),
      errorBox,
    ),
    footer: [cancelBtn, saveBtn],
    initialFocus: editing ? targetsInput : aliasInput,
  });
  cancelBtn.addEventListener('click', () => modal.close());
  saveBtn.addEventListener('click', async () => {
    errorBox.textContent = '';
    const name = aliasInput.value.trim();
    const targets = targetsInput.value.split(/\r?\n|,/).map((s) => s.trim()).filter(Boolean);
    if (!name) { errorBox.textContent = '别名不能为空'; return; }
    if (!targets.length) { errorBox.textContent = '至少填写一个目标'; return; }
    if (!(await requireAdmin())) return;
    saveBtn.disabled = true;
    try {
      await api.mapping.set({ alias: name, targets, enabled: enabledInput.checked });
      toastSuccess('映射已保存');
      modal.close();
      await refreshMappings();
    } catch (error) {
      errorBox.textContent = describeError(error);
      saveBtn.disabled = false;
    }
  });
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
