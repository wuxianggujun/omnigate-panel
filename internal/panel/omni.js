'use strict';
/* OmniGate 面板页：与主面板共用同一把 API Key（localStorage wb2api.key），
   调用同源的 /omni/* 接口。严格 CSP（script-src 'self'）下不使用内联事件处理器，
   全部通过 addEventListener / 事件委托绑定。 */
const LS_KEY = 'wb2api.key';
const $ = id => document.getElementById(id);

function getKey() { return localStorage.getItem(LS_KEY) || ''; }
function setKey(v) { localStorage.setItem(LS_KEY, v.trim()); }

function msg(id, text, cls) {
  const el = $(id);
  if (!el) return;
  el.textContent = text || '';
  el.className = 'msg' + (cls ? ' ' + cls : '');
}
function esc(s) { return String(s == null ? '' : s).replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c])); }

async function api(path, opts) {
  opts = opts || {};
  const headers = opts.headers || {};
  if (opts.body && !headers['Content-Type']) headers['Content-Type'] = 'application/json';
  const key = getKey();
  if (key) headers['Authorization'] = 'Bearer ' + key;
  const r = await fetch(path, { method: opts.method || 'GET', headers, body: opts.body });
  const text = await r.text();
  let data = null;
  try { data = text ? JSON.parse(text) : null; } catch (e) { data = { raw: text }; }
  if (!r.ok) {
    const m = (data && data.error && data.error.message) || (data && data.raw) || ('HTTP ' + r.status);
    throw new Error(m);
  }
  return data;
}

/* ── 上游/账号概览 ── */
async function loadAll() {
  try {
    const info = await api('/omni/healthz');
    renderProviders(info);
    msg('statusMsg', '服务正常 · 模式 ' + (info.mode || '-') + ' · 运行 ' + (info.uptime_seconds || 0) + 's', 'ok');
    $('statusCnt').textContent = Object.keys(info.providers || {}).length + ' 个上游';
  } catch (e) {
    msg('statusMsg', '加载失败：' + e.message, 'err');
  }
  fillRaccoonProviders();
  loadAccounts();
  loadModels();
  await loadConfig();
  loadOutbound();
}

function renderProviders(info) {
  const box = $('providers');
  const provs = info.providers || {};
  const names = Object.keys(provs);
  if (!names.length) { box.innerHTML = '<div class="empty">未配置上游</div>'; return; }
  box.innerHTML = names.map(name => {
    const p = provs[name] || {};
    const accts = p.accounts || [];
    const ready = accts.filter(a => a.indexOf('(ready)') >= 0).length;
    const pills = accts.length
      ? accts.map(a => '<span class="pill ' + (a.indexOf('(ready)') >= 0 ? 'ok' : 'warn') + '">' + esc(a) + '</span>').join(' ')
      : '<span class="pill warn">无账号</span>';
    return `<div class="prov">
      <div class="t">${esc(name)} <span class="pill">${esc(p.type || '')}</span></div>
      <div class="s">账号 ${accts.length} 个 · 就绪 ${ready} · active #${p.active == null ? 0 : p.active}</div>
      <div class="s" style="margin-top:6px">${pills}</div>
    </div>`;
  }).join('');
}

async function fillRaccoonProviders() {
  const sel = $('raccoonProvider');
  if (sel.dataset.filled === '1') return;
  try {
    const info = await api('/omni/healthz');
    const provs = info.providers || {};
    const raccoon = Object.keys(provs).filter(n => (provs[n].type || '') === 'raccoon');
    const names = raccoon.length ? raccoon : ['raccoon'];
    sel.innerHTML = names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    sel.dataset.filled = '1';
  } catch (e) {
    sel.innerHTML = '<option value="raccoon">raccoon</option>';
  }
}

/* ── 账号管理（多账号：网页授权 / Token 粘贴） ── */
function provName() { return $('raccoonProvider').value || 'raccoon'; }

function acctCount() { return $('accts').querySelectorAll('tr[data-label]').length; }
function nextLabel() { return '浣熊账号' + (acctCount() + 1); }

async function loadAccounts() {
  const tbody = $('accts');
  try {
    const data = await api('/omni/admin/raccoon/accounts?provider=' + encodeURIComponent(provName()));
    const list = (data && data.accounts) || [];
    $('acctCnt').textContent = list.length + ' 个账号';
    if (!list.length) { tbody.innerHTML = '<tr><td colspan="4" class="empty">暂无账号，点「＋ 添加账号（网页授权）」开始</td></tr>'; return; }
    tbody.innerHTML = list.map(a => {
      let bal = '-';
      if (a.balance_error) bal = '<span class="pill warn">' + esc(a.balance_error) + '</span>';
      else if (a.available != null) bal = '<span class="num">' + esc(a.available) + '</span>';
      const status = a.has_token ? '<span class="pill ok">已授权</span>' : '<span class="pill warn">未授权</span>';
      const exp = a.expires_at ? '<div style="color:var(--ink-3);font-size:11px">至 ' + esc(a.expires_at) + '</div>' : '';
      return `<tr data-label="${esc(a.label)}">
        <td>${esc(a.label)}${exp}</td>
        <td>${bal}</td>
        <td>${status}</td>
        <td>
          <button data-action="checkin" data-label="${esc(a.label)}">签到</button>
          <button class="danger" data-action="remove" data-label="${esc(a.label)}">移除</button>
        </td>
      </tr>`;
    }).join('');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="4" class="empty">' + esc(e.message) + '</td></tr>';
  }
}

/* 网页授权：优先走「自动捕获」——登录后浏览器直接回调本服务，无需粘贴；
   若上游未走该分支则回退手动粘贴回调。可重复添加多个账号。 */
let autoPollTimer = null;
function stopAutoPoll() { if (autoPollTimer) { clearInterval(autoPollTimer); autoPollTimer = null; } }

async function authorize() {
  stopAutoPoll();
  $('tokenPanel').style.display = 'none';
  $('authPanel').style.display = 'block';
  $('authLabel').value = nextLabel();
  $('callback').value = '';
  $('authUrl').value = '';
  $('authMode').textContent = '';
  const redirectBase = location.origin + '/omni/admin/raccoon/redirect';
  msg('authMsg', '正在生成授权链接…');
  try {
    const data = await api('/omni/admin/raccoon/authorize?provider=' + encodeURIComponent(provName())
      + '&label=' + encodeURIComponent($('authLabel').value)
      + '&redirect_base=' + encodeURIComponent(redirectBase));
    const url = data.authorize_url || data.url || data.auth_url || '';
    $('authUrl').value = url;
    if (data.mode === 'auto') {
      $('authMode').textContent = '自动捕获';
      msg('authMsg', '已打开登录页；登录并完成验证后会自动添加账号，本页将自动刷新。若未自动完成，可把回调地址粘到下面提交。', 'ok');
      startAutoPoll($('authLabel').value);
    } else {
      $('authMode').textContent = '手动粘贴';
      msg('authMsg', '已生成授权链接，正在打开浏览器；登录后把回调地址粘到下面提交。', 'ok');
    }
    if (url) window.open(url, '_blank', 'noopener');
  } catch (e) {
    msg('authMsg', '生成授权链接失败：' + e.message, 'err');
  }
}

/* 自动捕获：轮询账号列表，出现新账号即视为授权成功。 */
function startAutoPoll(label) {
  const before = acctCount();
  let tries = 0;
  autoPollTimer = setInterval(async () => {
    tries++;
    if (tries > 100) { stopAutoPoll(); msg('authMsg', '等待超时，可手动粘贴回调，或重新发起。', 'err'); return; }
    try {
      const data = await api('/omni/admin/raccoon/accounts?provider=' + encodeURIComponent(provName()));
      const list = (data && data.accounts) || [];
      if (list.length > before || list.some(a => a.label === label)) {
        stopAutoPoll();
        msg('authMsg', '授权成功：' + label, 'ok');
        $('authPanel').style.display = 'none';
        loadAll();
      }
    } catch (e) { /* 继续轮询 */ }
  }, 3000);
}

function openAuth() {
  const u = $('authUrl').value;
  if (!u) { msg('authMsg', '还没有授权链接，请重新生成', 'err'); return; }
  window.open(u, '_blank', 'noopener');
}

async function copyAuth() {
  const u = $('authUrl').value;
  if (!u) return;
  try { await navigator.clipboard.writeText(u); msg('authMsg', '授权链接已复制', 'ok'); }
  catch (e) { msg('authMsg', '复制失败，请手动选中链接复制', 'err'); }
}

function cancelAuth() { stopAutoPoll(); $('authPanel').style.display = 'none'; msg('authMsg', ''); }

async function submitCallback() {
  const cb = $('callback').value.trim();
  if (!cb) { msg('authMsg', '请先粘贴回调 URL 或 code', 'err'); return; }
  const label = $('authLabel').value.trim();
  try {
    const data = await api('/omni/admin/raccoon/callback', {
      method: 'POST',
      body: JSON.stringify({ provider: provName(), label, callback: cb }),
    });
    const acc = (data && data.account) || {};
    stopAutoPoll();
    msg('authMsg', '授权成功：' + (acc.name || acc.label || '账号已添加'), 'ok');
    $('authPanel').style.display = 'none';
    $('callback').value = '';
    loadAll();
  } catch (e) {
    msg('authMsg', '回调失败：' + e.message, 'err');
  }
}

/* 直接粘贴 Token（内联表单，替代 prompt）。 */
function setToken() {
  $('authPanel').style.display = 'none';
  $('tokenPanel').style.display = 'block';
  $('tokLabel').value = nextLabel();
  $('tokAccess').value = '';
  $('tokRefresh').value = '';
  msg('tokenMsg', '');
}

function cancelToken() { $('tokenPanel').style.display = 'none'; msg('tokenMsg', ''); }

async function saveToken() {
  const label = $('tokLabel').value.trim();
  const access = $('tokAccess').value.trim();
  if (!label) { msg('tokenMsg', '请填写账号名', 'err'); return; }
  if (!access) { msg('tokenMsg', '请粘贴 access_token', 'err'); return; }
  try {
    await api('/omni/admin/raccoon/token', {
      method: 'POST',
      body: JSON.stringify({ provider: provName(), label, access_token: access, refresh_token: $('tokRefresh').value.trim() }),
    });
    msg('tokenMsg', 'Token 已保存', 'ok');
    $('tokenPanel').style.display = 'none';
    loadAll();
  } catch (e) {
    msg('tokenMsg', '保存失败：' + e.message, 'err');
  }
}

async function doCheckin(label) {
  try {
    const data = await api('/omni/admin/raccoon/checkin', {
      method: 'POST',
      body: JSON.stringify({ provider: provName(), label }),
    });
    const results = (data && data.results) || [];
    msg('acctMsg', results.map(r => `${r.label}: ${r.error ? ('失败 ' + r.error) : (r.success ? '成功 ' : '未成功 ') + (r.msg || '')}`).join('\n'), 'ok');
    loadAccounts();
  } catch (e) {
    msg('acctMsg', '签到失败：' + e.message, 'err');
  }
}

async function removeAcct(label) {
  if (!confirm('确定移除账号「' + label + '」？')) return;
  try {
    await api('/omni/admin/raccoon/remove', { method: 'POST', body: JSON.stringify({ provider: provName(), label }) });
    msg('acctMsg', '已移除 ' + label, 'ok');
    loadAccounts();
  } catch (e) {
    msg('acctMsg', '移除失败：' + e.message, 'err');
  }
}

/* ── 供应商配置（增删改 + 进程内热生效） ── */
let omniCfg = null;

async function loadConfig() {
  try {
    const data = await api('/panel/api/omni/config');
    omniCfg = data.config || {};
    if (!Array.isArray(omniCfg.providers)) omniCfg.providers = [];
    $('cfgPath').textContent = data.path || '';
    $('cfgDefaultModel').value = omniCfg.default_model || '';
    $('cfgMode').value = omniCfg.mode || 'text';
    $('cfgReasoning').checked = !!omniCfg.reasoning;
    renderProvEditor();
    msg('cfgMsg', '已加载 ' + omniCfg.providers.length + ' 个供应商', 'ok');
  } catch (e) {
    $('provEditor').innerHTML = '<div class="empty">供应商配置不可用：' + esc(e.message) + '</div>';
    msg('cfgMsg', '加载失败：' + e.message, 'err');
  }
}

function modelsText(p) { return Array.isArray(p.models) ? p.models.join('\n') : (p.models || ''); }

function provCard(p, i) {
  const type = p.type || 'runable';
  const typeOpts = ['runable', 'raccoon', 'openai']
    .map(t => `<option value="${t}"${t === type ? ' selected' : ''}>${t}</option>`).join('');
  const raccoonFields = type === 'raccoon' ? `
      <label>main_origin<input data-i="${i}" data-f="main_origin" value="${esc(p.main_origin || '')}" placeholder="https://..."></label>
      <label>llm_base<input data-i="${i}" data-f="llm_base" value="${esc(p.llm_base || '')}"></label>
      <label>auth_base<input data-i="${i}" data-f="auth_base" value="${esc(p.auth_base || '')}"></label>` : '';
  const deviceField = type === 'runable' ? `
      <label>device_header<input data-i="${i}" data-f="device_header" value="${esc(p.device_header || '')}" placeholder="留空 = 默认"></label>` : '';
  const accts = (p.accounts || []).map((a, j) => acctRow(a, i, j)).join('');
  return `<div class="prov">
    <div class="row" style="justify-content:space-between">
      <div class="t">供应商 #${i + 1} <span class="pill">${esc(type)}</span></div>
      <button class="danger" data-act="delProv" data-i="${i}">删除</button>
    </div>
    <div class="fields">
      <label>名称（唯一）<input data-i="${i}" data-f="name" value="${esc(p.name || '')}" placeholder="如 my-openai"></label>
      <label>类型<select data-i="${i}" data-f="type">${typeOpts}</select></label>
      <label class="full">base_url<input data-i="${i}" data-f="base_url" value="${esc(p.base_url || '')}" placeholder="https://api.example.com/v1"></label>
      <label class="full">api_key<input data-i="${i}" data-f="api_key" value="${esc(p.api_key || '')}" placeholder="上游密钥（openai 类用）"></label>
      ${deviceField}${raccoonFields}
      <label class="full">models（逗号或换行分隔，留空 = 自动发现）<textarea data-i="${i}" data-f="models" rows="2">${esc(modelsText(p))}</textarea></label>
    </div>
    <div class="sep"></div>
    <div class="row" style="justify-content:space-between">
      <span class="s">账号 ${(p.accounts || []).length} 个</span>
      <button data-act="addAcct" data-i="${i}">+ 添加账号</button>
    </div>
    <div class="accts">${accts || '<div class="empty">暂无账号（raccoon 可用下方「打开浏览器授权」添加）</div>'}</div>
  </div>`;
}

function acctRow(a, i, j) {
  return `<div class="row acct" style="margin-top:6px">
    <input data-i="${i}" data-j="${j}" data-f="label" value="${esc(a.label || '')}" placeholder="label" style="width:120px">
    <input data-i="${i}" data-j="${j}" data-f="email" value="${esc(a.email || '')}" placeholder="email" style="flex:1;min-width:130px">
    <input data-i="${i}" data-j="${j}" data-f="password" value="${esc(a.password || '')}" placeholder="password" style="flex:1;min-width:130px">
    <input data-i="${i}" data-j="${j}" data-f="cookie" value="${esc(a.cookie || '')}" placeholder="cookie / access_token" style="flex:1;min-width:150px">
    <input data-i="${i}" data-j="${j}" data-f="refresh_token" value="${esc(a.refresh_token || '')}" placeholder="refresh_token" style="width:150px">
    <button class="danger" data-act="delAcct" data-i="${i}" data-j="${j}">×</button>
  </div>`;
}

function renderProvEditor() {
  const box = $('provEditor');
  const provs = (omniCfg && omniCfg.providers) || [];
  const sel = $('cfgDefaultProvider');
  const cur = (omniCfg && omniCfg.default_provider) || '';
  sel.innerHTML = provs.map(p => `<option value="${esc(p.name || '')}">${esc(p.name || '(未命名)')}</option>`).join('');
  if (cur) sel.value = cur;
  if (!provs.length) { box.innerHTML = '<div class="empty">还没有供应商，点「+ 添加供应商」开始</div>'; return; }
  box.innerHTML = provs.map((p, i) => provCard(p, i)).join('');
}

function onProvInput(ev) {
  const el = ev.target;
  if (!el.dataset || el.dataset.f == null || el.dataset.i == null) return;
  const p = omniCfg.providers[+el.dataset.i];
  if (!p) return;
  if (el.dataset.f === 'type') { p.type = el.value; renderProvEditor(); return; }
  if (el.dataset.j != null) {
    p.accounts = p.accounts || [];
    const j = +el.dataset.j;
    p.accounts[j] = p.accounts[j] || {};
    p.accounts[j][el.dataset.f] = el.value;
  } else {
    p[el.dataset.f] = el.value;
  }
}

function onProvClick(ev) {
  const btn = ev.target.closest('button[data-act]');
  if (!btn) return;
  const act = btn.dataset.act, i = +btn.dataset.i, j = +btn.dataset.j;
  if (act === 'delProv') {
    if (!confirm('删除供应商「' + (omniCfg.providers[i].name || i) + '」？')) return;
    omniCfg.providers.splice(i, 1);
  } else if (act === 'addAcct') {
    omniCfg.providers[i].accounts = omniCfg.providers[i].accounts || [];
    omniCfg.providers[i].accounts.push({});
  } else if (act === 'delAcct') {
    omniCfg.providers[i].accounts.splice(j, 1);
  } else {
    return;
  }
  renderProvEditor();
}

function addProvider() {
  omniCfg = omniCfg || {};
  omniCfg.providers = omniCfg.providers || [];
  omniCfg.providers.push({ name: '', type: 'runable', base_url: '', models: [], accounts: [] });
  renderProvEditor();
}

function normalizeForSave(cfg) {
  const out = JSON.parse(JSON.stringify(cfg || {}));
  out.providers = (out.providers || []).map(p => {
    p.models = (typeof p.models === 'string' ? p.models.split(/[\n,]+/) : (p.models || []))
      .map(s => String(s).trim()).filter(Boolean);
    p.accounts = (p.accounts || []).filter(a => a && (a.label || a.email || a.password || a.cookie || a.refresh_token));
    return p;
  });
  return out;
}

async function saveConfig() {
  if (!omniCfg) { msg('cfgMsg', '配置尚未加载', 'err'); return; }
  const body = normalizeForSave(omniCfg);
  body.default_provider = $('cfgDefaultProvider').value;
  body.default_model = $('cfgDefaultModel').value.trim();
  body.mode = $('cfgMode').value;
  body.reasoning = $('cfgReasoning').checked;
  try {
    await api('/panel/api/omni/config', { method: 'POST', body: JSON.stringify(body) });
    msg('cfgMsg', '已保存并热生效', 'ok');
    await loadConfig();
    loadOutbound(); // 供应商增删后刷新出站代理的目标路由
    loadAll();
  } catch (e) {
    msg('cfgMsg', '保存失败：' + e.message, 'err');
  }
}

/* ── 出站代理（命名代理 + 目标路由；保存即热生效） ── */
let obState = { proxies: [], routes: {} };
let obEditIdx = -1;

async function loadOutbound() {
  try {
    const data = await api('/panel/api/omni/outbound');
    const ob = (data && data.outbound) || {};
    obState = {
      proxies: Array.isArray(ob.proxies) ? ob.proxies : [],
      routes: ob.routes || {},
    };
    renderOutbound();
    msg('obMsg', '已载入 ' + obState.proxies.length + ' 个代理', 'ok');
  } catch (e) {
    msg('obMsg', '载入失败：' + e.message, 'err');
  }
}

/* 目标列表：面板上游 WorkBuddy + 每个 OmniGate 供应商。 */
function obTargets() {
  const list = [{ target: 'workbuddy', label: '面板上游 · WorkBuddy' }];
  const provs = (omniCfg && omniCfg.providers) || [];
  provs.forEach(p => {
    const n = p && p.name;
    if (n) list.push({ target: 'omnigate:' + n, label: '供应商 · ' + n + (p.type ? ' (' + p.type + ')' : '') });
  });
  return list;
}

function renderOutbound() {
  $('obCnt').textContent = obState.proxies.length + ' 个代理';
  const tb = $('obProxies');
  if (!obState.proxies.length) {
    tb.innerHTML = '<tr><td colspan="4" class="empty">暂无代理，点「＋ 添加代理」开始</td></tr>';
  } else {
    tb.innerHTML = obState.proxies.map((p, i) => {
      const auth = p.username ? esc(p.username) + (p.password ? ' / ******' : '') : '—';
      return `<tr>
        <td>${esc(p.name)}</td>
        <td><span class="num">${esc(p.url)}</span></td>
        <td>${auth}</td>
        <td>
          <button data-obact="edit" data-i="${i}">编辑</button>
          <button class="danger" data-obact="del" data-i="${i}">删除</button>
        </td>
      </tr>`;
    }).join('');
  }
  $('obRoutes').innerHTML = obTargets().map(t => {
    const cur = obState.routes[t.target] || '';
    let opts = `<option value=""${cur === '' ? ' selected' : ''}>直连</option>`;
    opts += obState.proxies.map(p =>
      `<option value="${esc(p.name)}"${p.name === cur ? ' selected' : ''}>${esc(p.name)}</option>`).join('');
    return `<tr>
      <td>${esc(t.label)}</td>
      <td><select data-obtarget="${esc(t.target)}" style="min-width:200px">${opts}</select></td>
    </tr>`;
  }).join('');
}

function obShowForm(i) {
  obEditIdx = (typeof i === 'number' && i >= 0) ? i : -1;
  const p = obEditIdx >= 0 ? (obState.proxies[obEditIdx] || {}) : {};
  $('obFormTitle').textContent = obEditIdx >= 0 ? '编辑代理' : '添加代理';
  $('obName').value = p.name || '';
  $('obUrl').value = p.url || '';
  $('obUser').value = p.username || '';
  $('obPass').value = p.password || '';
  msg('obFormMsg', '');
  $('obForm').style.display = 'block';
}

function obCancelForm() { $('obForm').style.display = 'none'; msg('obFormMsg', ''); }

function obFormSave() {
  const name = $('obName').value.trim();
  const url = $('obUrl').value.trim();
  if (!name) { msg('obFormMsg', '请填写名称', 'err'); return; }
  if (!url) { msg('obFormMsg', '请填写地址', 'err'); return; }
  if (obState.proxies.some((p, idx) => p.name === name && idx !== obEditIdx)) {
    msg('obFormMsg', '代理名「' + name + '」已存在', 'err');
    return;
  }
  const entry = { name, url, username: $('obUser').value.trim(), password: $('obPass').value };
  if (obEditIdx >= 0) {
    const oldName = (obState.proxies[obEditIdx] || {}).name;
    obState.proxies[obEditIdx] = entry;
    if (oldName && oldName !== name) {
      Object.keys(obState.routes).forEach(k => { if (obState.routes[k] === oldName) obState.routes[k] = name; });
    }
  } else {
    obState.proxies.push(entry);
  }
  $('obForm').style.display = 'none';
  renderOutbound();
  msg('obMsg', '已更新，记得点「保存并热生效」', 'ok');
}

function onObClick(ev) {
  const btn = ev.target.closest('button[data-obact]');
  if (!btn) return;
  const i = +btn.dataset.i;
  if (btn.dataset.obact === 'edit') {
    obShowForm(i);
  } else if (btn.dataset.obact === 'del') {
    const p = obState.proxies[i] || {};
    if (!confirm('删除代理「' + (p.name || i) + '」？引用它的目标将回退直连。')) return;
    const name = p.name;
    obState.proxies.splice(i, 1);
    Object.keys(obState.routes).forEach(k => { if (obState.routes[k] === name) delete obState.routes[k]; });
    renderOutbound();
    msg('obMsg', '已删除，记得点「保存并热生效」', 'ok');
  }
}

function onObRouteChange(ev) {
  const sel = ev.target.closest('select[data-obtarget]');
  if (!sel) return;
  const t = sel.dataset.obtarget;
  if (sel.value) obState.routes[t] = sel.value;
  else delete obState.routes[t];
}

async function saveOutbound() {
  const proxies = obState.proxies.map(p => {
    const e = { name: p.name, url: p.url };
    if (p.username) e.username = p.username;
    if (p.password) e.password = p.password;
    return e;
  });
  const routes = {};
  Object.keys(obState.routes).forEach(k => { if (obState.routes[k]) routes[k] = obState.routes[k]; });
  try {
    await api('/panel/api/omni/outbound', { method: 'POST', body: JSON.stringify({ proxies, routes }) });
    msg('obMsg', '已保存并热生效', 'ok');
    await loadOutbound();
    loadAll();
  } catch (e) {
    msg('obMsg', '保存失败：' + e.message, 'err');
  }
}

/* ── 模型目录 ── */
async function loadModels() {
  const box = $('models');
  try {
    const data = await api('/omni/v1/models');
    const list = (data && data.data) || [];
    $('modelCnt').textContent = list.length + ' 个模型';
    if (!list.length) { box.innerHTML = '<div class="empty">暂无模型</div>'; return; }
    box.innerHTML = list.map(m => `<div class="prov"><div class="t">${esc(m.id)}</div><div class="s">${esc(m.owned_by || '')}</div></div>`).join('');
  } catch (e) {
    box.innerHTML = '<div class="empty">' + esc(e.message) + '</div>';
  }
}

/* ── 绑定事件 ── */
function bind() {
  $('key').value = getKey();
  $('btnSaveKey').addEventListener('click', () => { setKey($('key').value); msg('statusMsg', 'Key 已保存', 'ok'); loadAll(); });
  $('btnRefresh').addEventListener('click', loadAll);
  $('btnAuth').addEventListener('click', authorize);
  $('btnOpenAuth').addEventListener('click', openAuth);
  $('btnCopyAuth').addEventListener('click', copyAuth);
  $('btnAuthCancel').addEventListener('click', cancelAuth);
  $('btnSubmitCb').addEventListener('click', submitCallback);
  $('btnSetToken').addEventListener('click', setToken);
  $('btnTokenSave').addEventListener('click', saveToken);
  $('btnTokenCancel').addEventListener('click', cancelToken);
  $('btnLoadAccts').addEventListener('click', loadAccounts);
  $('btnCheckinAll').addEventListener('click', () => doCheckin(''));
  $('btnLoadModels').addEventListener('click', loadModels);
  $('raccoonProvider').addEventListener('change', loadAccounts);
  // 供应商配置：增删改 + 保存热生效。
  $('btnAddProv').addEventListener('click', addProvider);
  $('btnSaveCfg').addEventListener('click', saveConfig);
  $('btnReloadCfg').addEventListener('click', loadConfig);
  $('provEditor').addEventListener('input', onProvInput);
  $('provEditor').addEventListener('change', onProvInput);
  $('provEditor').addEventListener('click', onProvClick);
  // 出站代理：增删改 + 目标路由 + 保存热生效。
  $('btnObAdd').addEventListener('click', () => obShowForm());
  $('btnObCancel').addEventListener('click', obCancelForm);
  $('btnObFormSave').addEventListener('click', obFormSave);
  $('btnObSave').addEventListener('click', saveOutbound);
  $('btnObReload').addEventListener('click', loadOutbound);
  $('obProxies').addEventListener('click', onObClick);
  $('obRoutes').addEventListener('change', onObRouteChange);
  // 账号表内按钮：事件委托（严格 CSP 下无内联 handler）。
  $('accts').addEventListener('click', ev => {
    const btn = ev.target.closest('button[data-action]');
    if (!btn) return;
    const label = btn.getAttribute('data-label');
    if (btn.getAttribute('data-action') === 'checkin') doCheckin(label);
    else if (btn.getAttribute('data-action') === 'remove') removeAcct(label);
  });
  const base = location.origin + '/omni/v1';
  $('baseUrl').textContent = base;
  $('callExample').textContent =
    'POST ' + base + '/chat/completions\n' +
    'Authorization: Bearer <面板 Key>\n' +
    'Content-Type: application/json\n\n' +
    '{"model":"raccoon/raccoon-8c4485","messages":[{"role":"user","content":"你好"}],"stream":true}';
  loadAll();
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', bind);
else bind();
