// login-wizard.js — 由 LoginSpec 驱动的账号登录向导。
//
// 这是「新增供应商不改前端」在登录侧的对应物：向导不判断 provider 类型，只读
// 元数据里的 login.kind（用户要做什么）与 login.transport（面板往哪发请求）：
//
//   kind=oauth   + transport=admin    → 授权链接 + 粘贴回调
//   kind=paste   + transport=admin    → 粘贴凭证走管理端点（token）
//   kind=paste   + transport=account  → 粘贴凭证走统一校验端点（account/login）
//   kind=snippet + transport=newapi   → 生成控制台脚本 + 轮询抓取
//
// 所有字段来自 login.fields，用与供应商表单相同的 schema 控件渲染。

import { api, describeError } from '../core/api.js';
import { requireAccountOperator } from '../core/auth.js';
import {
  el, icon, badge, copyText, clear,
} from '../core/dom.js';
import { loginKindLabel } from '../core/format.js';
import { createSchemaForm } from './schema-form.js';
import { openModal } from './modal.js';
import { toastError, toastSuccess, toastWarn } from './toast.js';

const SNIPPET_POLL_MS = 3000;
const SNIPPET_POLL_MAX = 100; // 100 × 3s ≈ 5 分钟

/**
 * 打开登录向导。
 * @param {{
 *   provider: {name:string, type:string},
 *   meta: any,
 *   values?: object,
 *   onDone?: () => void|Promise<void>,
 * }} options
 */
export function openLoginWizard({
  provider, meta, values = {}, onDone = null,
} = {}) {
  const spec = meta?.login;
  if (!spec || !provider?.name) return null;
  const type = provider.type || meta?.type || '';

  const state = {
    step: 'form',
    busy: false,
    error: '',
    note: '',
    values,
    form: null,
    callbackForm: null,
    session: null,
    snippet: '',
    pollTimer: 0,
    polls: 0,
  };

  const body = el('div', { class: 'login-wizard' });
  const modal = openModal({
    title: spec.label || `${loginKindLabel(spec.kind)} · 添加账号`,
    subtitle: `供应商 ${provider.name}（${meta?.label || type}）`,
    body,
    size: 'md',
    onClose: () => stopPoll(),
  });

  function stopPoll() {
    if (state.pollTimer) {
      clearInterval(state.pollTimer);
      state.pollTimer = 0;
    }
  }

  function setError(message) {
    state.error = message || '';
    render();
  }

  function valuesFromForm() {
    const out = state.form ? state.form.getValues() : {};
    return out;
  }

  async function guard() {
    if (!(await requireAccountOperator())) return false;
    return true;
  }

  // ---- 各流程 ----

  async function startOAuth() {
    if (!(await guard())) return;
    const v = valuesFromForm();
    const validation = state.form?.validate();
    if (validation && !validation.valid) return;
    state.busy = true;
    state.error = '';
    render();
    try {
      const res = await api.loginFlow.authorize(type, {
        provider: provider.name,
        label: v.label || '',
        redirect_base: v.redirect_base || '',
      });
      state.session = res || {};
      state.step = 'oauth';
    } catch (error) {
      state.error = describeError(error);
    } finally {
      state.busy = false;
      render();
    }
  }

  async function finishOAuth() {
    const v = state.callbackForm ? state.callbackForm.getValues() : {};
    const callbackURL = String(v.callback_url || '').trim();
    if (!callbackURL) {
      setError('请粘贴浏览器地址栏里的回调地址');
      return;
    }
    state.busy = true;
    state.error = '';
    render();
    try {
      await api.loginFlow.callback(type, {
        provider: provider.name,
        state: state.session?.state || '',
        callback_url: callbackURL,
      });
      toastSuccess('账号已添加');
      await done();
    } catch (error) {
      state.error = describeError(error);
      state.busy = false;
      render();
    }
  }

  async function startToken() {
    if (!(await guard())) return;
    const validation = state.form?.validate();
    if (validation && !validation.valid) return;
    const v = valuesFromForm();
    state.busy = true;
    state.error = '';
    render();
    try {
      await api.loginFlow.token(type, {
        provider: provider.name,
        label: v.label || '',
        cookie: v.cookie || v.token || '',
      });
      toastSuccess('账号已添加');
      await done();
    } catch (error) {
      state.error = describeError(error);
      state.busy = false;
      render();
    }
  }

  async function startAccountLogin() {
    if (!(await guard())) return;
    const validation = state.form?.validate();
    if (validation && !validation.valid) return;
    const v = valuesFromForm();
    if (!String(v.label || '').trim()) {
      setError('账号标签不能为空');
      return;
    }
    state.busy = true;
    state.error = '';
    render();
    try {
      await api.account.login({ provider: provider.name, ...v });
      toastSuccess('登录校验通过，账号已保存');
      await done();
    } catch (error) {
      state.error = describeError(error);
      state.busy = false;
      render();
    }
  }

  async function startSnippet() {
    if (!(await guard())) return;
    const validation = state.form?.validate();
    if (validation && !validation.valid) return;
    const v = valuesFromForm();
    state.busy = true;
    state.error = '';
    render();
    try {
      const res = await api.newapi.authorize({
        provider: provider.name,
        label: v.label || '',
        api_key: v.api_key || '',
      });
      state.session = res || {};
      state.snippet = String(res?.snippet || '');
      state.step = 'snippet';
      startPolling();
    } catch (error) {
      state.error = describeError(error);
    } finally {
      state.busy = false;
      render();
    }
  }

  function startPolling() {
    stopPoll();
    state.polls = 0;
    state.pollTimer = window.setInterval(async () => {
      state.polls += 1;
      try {
        const res = await api.newapi.status(state.session?.state || '');
        if (res && res.pending === false) {
          stopPoll();
          toastSuccess('会话已捕获，账号已创建');
          await done();
          return;
        }
      } catch { /* 轮询失败忽略，下一次再试 */ }
      if (state.polls >= SNIPPET_POLL_MAX) {
        stopPoll();
        state.note = '等待超时：请在浏览器里运行脚本后重新发起。';
        render();
      }
    }, SNIPPET_POLL_MS);
  }

  async function done() {
    stopPoll();
    if (typeof onDone === 'function') await onDone();
    modal.close();
  }

  // ---- 渲染 ----

  function render() {
    clear(body);
    body.appendChild(headline());
    if (state.step === 'form') body.appendChild(formStep());
    else if (state.step === 'oauth') body.appendChild(oauthStep());
    else if (state.step === 'snippet') body.appendChild(snippetStep());
    body.appendChild(footer());
  }

  function headline() {
    return el('div', { class: 'wizard-head' },
      el('div', { class: 'wizard-kind' },
        badge(loginKindLabel(spec.kind), 'info'),
        el('span', { class: 'muted small', text: spec.label || '' }),
      ),
      spec.help ? el('p', { class: 'wizard-help', text: spec.help }) : null,
      state.note ? el('p', { class: 'text-warn small', text: state.note }) : null,
      state.error ? el('p', { class: 'form-error', attrs: { role: 'alert' }, text: state.error }) : null,
    );
  }

  function formStep() {
    const wrap = el('div', { class: 'wizard-step' });
    state.form = createSchemaForm({
      fields: Array.isArray(spec.fields) ? spec.fields : [],
      values: state.values,
      idPrefix: `login-${type}-${Date.now()}`,
    });
    wrap.appendChild(state.form.el);
    return wrap;
  }

  function oauthStep() {
    const wrap = el('div', { class: 'wizard-step' });
    const authorizeURL = state.session?.authorize_url || '';
    const redirectURL = state.session?.redirect_url || '';
    const auto = state.session?.mode === 'auto';

    wrap.appendChild(el('ol', { class: 'wizard-steps' },
      el('li', { text: '点击「打开授权页」，在浏览器里完成上游登录。' }),
      el('li', { text: auto
        ? '登录成功后授权页会自动回跳，面板直接抓取授权码；若未自动完成，请把地址栏 URL 粘贴到下方。'
        : '登录后浏览器会跳到一个打不开的页面，把地址栏里的完整 URL 复制回来。' }),
      el('li', { text: '粘贴回调地址并点击「完成」。' }),
    ));

    if (authorizeURL) {
      wrap.appendChild(el('div', { class: 'wizard-actions-row' },
        el('a', {
          class: 'btn btn-primary btn-sm', href: authorizeURL, target: '_blank', rel: 'noopener noreferrer',
        }, icon('link', 14), '打开授权页'),
        auto && redirectURL
          ? el('span', { class: 'muted small', text: '已启用自动回跳抓取' })
          : el('span', { class: 'muted small', text: state.session?.callback_hint || '' }),
      ));
    }

    const form = createSchemaForm({
      fields: [{
        name: 'callback_url',
        label: '回调地址',
        type: 'textarea',
        rows: 3,
        required: true,
        placeholder: state.session?.callback_hint || '把浏览器地址栏里的完整 URL 粘贴到这里',
      }],
      values: {},
      idPrefix: `login-cb-${type}-${Date.now()}`,
    });
    state.callbackForm = form;
    wrap.appendChild(form.el);
    return wrap;
  }

  function snippetStep() {
    const wrap = el('div', { class: 'wizard-step' });
    const signin = state.session?.signin_url || '';
    // cookie 型站点（AnyRouter / AgentRouter）没有 access_token：脚本读的是浏览器
    // 会话 Cookie，所以「粘贴脚本」之外还得提醒「聊天密钥稍后补」，否则用户会以为
    // 账号建好就能直接聊天。
    const cookieAuth = state.session?.auth_mode === 'cookie';
    wrap.appendChild(el('ol', { class: 'wizard-steps' },
      el('li', { text: '点击「打开登录页」，在站点完成登录。' }),
      el('li', { text: '打开浏览器开发者工具的控制台，粘贴并运行下面的脚本。' }),
      cookieAuth
        ? el('li', { text: '脚本会把会话 Cookie 与用户 ID 回传，面板自动建号；聊天还需在账号里补填站点的 sk- 密钥。' })
        : el('li', { text: '脚本会把会话回传，面板自动建号（本页会自动刷新）。' }),
    ));
    if (signin) {
      wrap.appendChild(el('div', { class: 'wizard-actions-row' },
        el('a', {
          class: 'btn btn-primary btn-sm', href: signin, target: '_blank', rel: 'noopener noreferrer',
        }, icon('link', 14), '打开登录页'),
      ));
    }
    wrap.appendChild(el('div', { class: 'snippet-block' },
      el('div', { class: 'snippet-head' },
        el('span', { text: '控制台脚本' }),
        el('button', {
          class: 'btn btn-ghost btn-sm', type: 'button',
          on: { click: async () => {
            const ok = await copyText(state.snippet);
            if (ok) toastSuccess('脚本已复制');
            else toastWarn('复制失败，请手动选择');
          } },
        }, icon('copy', 14), '复制脚本'),
      ),
      el('pre', { class: 'code-block snippet-code', text: state.snippet }),
    ));
    if (cookieAuth) {
      // 会话 Cookie 若是 httpOnly，控制台读不到——这条兜底必须写在页面上，
      // 否则用户只会看到脚本报「读不到 Cookie」而不知道下一步做什么。
      wrap.appendChild(el('p', { class: 'muted small', text: '若脚本提示读不到 Cookie（httpOnly），请在开发者工具 Application → Cookies 里复制整条 Cookie，关闭本窗口后用「手动添加账号」的 Session Cookie 字段填入。' }));
    }
    wrap.appendChild(el('p', { class: 'muted small' },
      el('span', { class: 'spinner', 'aria-hidden': 'true' }),
      ' 正在等待会话回传…',
    ));
    return wrap;
  }

  function footer() {
    const buttons = [];
    if (state.step === 'form') {
      const start = () => {
        if (spec.transport === 'newapi') return startSnippet();
        if (spec.transport === 'account') return startAccountLogin();
        if (spec.kind === 'oauth') return startOAuth();
        return startToken();
      };
      buttons.push(el('button', {
        class: 'btn btn-primary btn-sm', type: 'button', disabled: state.busy,
        on: { click: start },
      }, state.busy ? '处理中…' : '开始'));
    } else if (state.step === 'oauth') {
      buttons.push(el('button', {
        class: 'btn btn-primary btn-sm', type: 'button', disabled: state.busy,
        on: { click: finishOAuth },
      }, state.busy ? '兑换中…' : '完成'));
    }
    buttons.push(el('button', {
      class: 'btn btn-ghost btn-sm', type: 'button',
      on: { click: () => { stopPoll(); modal.close(); } },
    }, '关闭'));
    return el('div', { class: 'wizard-foot' }, buttons);
  }

  render();
  return modal;
}
