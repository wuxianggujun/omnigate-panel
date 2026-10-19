// auth.js — 面板会话、角色能力判断、登录/注册对话框。
//
// 权限口径：
//   - admin：全部读写
//   - viewer：只读（配置、密钥、账号明细可见，但不能写）
//   - user：只读平台配置不可见；仅能操作归属自己的账号（后端按 provider/label 过滤）
//   - 匿名：公开只读页（总览 / 模型 / 部分用量），写操作引导登录

import { createStore } from './store.js';
import { api, describeError } from './api.js';
import { el, icon } from './dom.js';
import { openModal } from '../components/modal.js';
import { createTabs } from '../components/tabs.js';
import { toastError, toastSuccess } from '../components/toast.js';

function emptyState() {
  return {
    ready: false,
    authenticated: false,
    configured: false,
    username: '',
    role: '',
    allowRegistration: false,
    build: '',
    error: '',
    session: null,
  };
}

export const authStore = createStore(emptyState());

export function authState() {
  return authStore.get();
}

export function isAuthenticated() {
  return !!authStore.get().authenticated;
}

export function isAdmin() {
  return authStore.get().role === 'admin';
}

export function isViewer() {
  return authStore.get().role === 'viewer';
}

export function isUser() {
  return authStore.get().role === 'user';
}

/** 账号运维（签到/移除/添加）：admin 或 user；viewer 只读。 */
export function canOperateAccounts() {
  return isAdmin() || isUser();
}

/** 平台级写操作：仅 admin。 */
export function canWrite() {
  return isAdmin();
}

let authSignature = '';

function signature(state) {
  return `${state.authenticated ? 1 : 0}|${state.username}|${state.role}`;
}

export async function refreshSession({ silent = false } = {}) {
  const before = authStore.get();
  try {
    const data = await api.session.get({ dedupe: false });
    const session = data && typeof data === 'object' ? data : {};
    const next = {
      ready: true,
      authenticated: !!session.authenticated,
      configured: !!session.configured,
      username: String(session.username || ''),
      role: String(session.role || ''),
      allowRegistration: !!session.allow_registration,
      build: String(session.build || ''),
      error: '',
      session,
    };
    authStore.set(next);
    const sig = signature(next);
    if (sig !== authSignature) {
      authSignature = sig;
      window.dispatchEvent(new CustomEvent('og:auth-changed', { detail: next }));
    }
    return next;
  } catch (error) {
    const next = { ...before, ready: true, error: describeError(error) };
    authStore.set(next);
    if (!silent) toastError(error);
    return next;
  }
}

export async function logout() {
  try {
    await api.session.logout();
  } catch (error) {
    toastError(error);
  }
  authStore.set({
    ...authStore.get(),
    authenticated: false,
    username: '',
    role: '',
    session: null,
  });
  authSignature = signature(authStore.get());
  window.dispatchEvent(new CustomEvent('og:auth-changed', { detail: authStore.get() }));
  toastSuccess('已退出登录');
}

let loginPromise = null;

/**
 * 打开登录/注册对话框。已登录时直接 resolve(true)。
 * @returns {Promise<boolean>}
 */
export function openLoginModal({ message = '', reason = '' } = {}) {
  if (isAuthenticated()) return Promise.resolve(true);
  if (loginPromise) return loginPromise;

  loginPromise = (async () => {
    if (!authStore.get().ready) await refreshSession({ silent: true });
    return new Promise((resolve) => {
      let settled = false;
      const finish = (value) => {
        if (settled) return;
        settled = true;
        resolve(value);
      };

      let mode = 'login';
      const errorBox = el('p', { class: 'form-error', attrs: { role: 'alert' } });
      const username = el('input', {
        class: 'input', type: 'text', name: 'username', id: 'login-username', autocomplete: 'username',
        placeholder: '用户名', required: true, spellcheck: 'false',
      });
      const password = el('input', {
        class: 'input', type: 'password', name: 'password', id: 'login-password', autocomplete: 'current-password',
        placeholder: '密码', required: true,
      });
      const confirm = el('input', {
        class: 'input', type: 'password', name: 'confirm', id: 'login-confirm', autocomplete: 'new-password',
        placeholder: '再次输入密码',
      });
      const confirmField = el('div', { class: 'field', hidden: true },
        el('label', { class: 'field-label', for: 'login-confirm' }, '确认密码'),
        confirm,
      );
      const submit = el('button', { class: 'btn btn-primary btn-block', type: 'submit' });
      const hint = el('p', { class: 'login-hint' });

      const form = el('form', { class: 'login-form', novalidate: true },
        message ? el('div', { class: 'login-message', text: message }) : null,
        reason ? el('div', { class: 'login-reason', text: reason }) : null,
        el('div', { class: 'field' },
          el('label', { class: 'field-label', for: 'login-username' }, '用户名'),
          username,
        ),
        el('div', { class: 'field' },
          el('label', { class: 'field-label', for: 'login-password' }, '密码'),
          password,
        ),
        confirmField,
        errorBox,
        submit,
        hint,
      );

      const state = authStore.get();
      const canRegister = state.allowRegistration && state.configured;
      let tabs = null;
      if (canRegister) {
        tabs = createTabs({
          items: [
            { id: 'login', label: '登录' },
            { id: 'register', label: '注册' },
          ],
          value: 'login',
          ariaLabel: '登录或注册',
          onChange: (value) => {
            mode = value;
            confirmField.hidden = value !== 'register';
            confirm.required = value === 'register';
            password.autocomplete = value === 'register' ? 'new-password' : 'current-password';
            submit.textContent = value === 'register' ? '注册并登录' : '登录';
            errorBox.textContent = '';
          },
        });
      }

      const updateMode = () => {
        if (mode === 'register') {
          confirmField.hidden = false;
          submit.textContent = '注册并登录';
        } else {
          confirmField.hidden = true;
          submit.textContent = '登录';
        }
        hint.textContent = state.configured
          ? (mode === 'register' ? '注册为普通用户，仅能管理自己的账号。' : '')
          : '面板尚未配置任何账号；请先在服务端创建管理员（见部署文档）。';
      };
      updateMode();

      form.addEventListener('submit', async (event) => {
        event.preventDefault();
        errorBox.textContent = '';
        const user = username.value.trim();
        const pass = password.value;
        if (!user) { errorBox.textContent = '请输入用户名'; username.focus(); return; }
        if (!pass) { errorBox.textContent = '请输入密码'; password.focus(); return; }
        if (mode === 'register' && pass !== confirm.value) {
          errorBox.textContent = '两次输入的密码不一致';
          confirm.focus();
          return;
        }
        submit.disabled = true;
        submit.textContent = mode === 'register' ? '注册中…' : '登录中…';
        try {
          if (mode === 'register') await api.session.register(user, pass);
          else await api.session.login(user, pass);
          await refreshSession({ silent: true });
          toastSuccess(mode === 'register' ? '注册成功，已登录' : '登录成功');
          finish(true);
          modal.close();
        } catch (error) {
          errorBox.textContent = describeError(error);
          submit.disabled = false;
          submit.textContent = mode === 'register' ? '注册并登录' : '登录';
        }
      });

      const body = el('div', { class: 'login-dialog' },
        tabs ? tabs.el : null,
        form,
      );
      const modal = openModal({
        title: state.authenticated ? '登录面板' : '登录 OmniGate 控制台',
        subtitle: '只读浏览无需登录；配置、密钥与账号操作需要相应权限。',
        body,
        size: 'sm',
        initialFocus: username,
        onClose: () => finish(isAuthenticated()),
      });
      if (state.error) {
        hint.textContent = `会话状态读取失败：${state.error}`;
      }
    });
  })();

  loginPromise.finally(() => { loginPromise = null; }).catch(() => { /* 已由调用方处理 */ });
  return loginPromise;
}

/** 要求管理员；返回是否获得权限。 */
export async function requireAdmin({ message = '此操作需要管理员权限' } = {}) {
  if (isAdmin()) return true;
  if (!isAuthenticated()) {
    const ok = await openLoginModal({ reason: message });
    if (ok && isAdmin()) return true;
    if (ok && !isAdmin()) toastError('当前账号不是管理员');
    return false;
  }
  toastError(message);
  return false;
}

/** 要求可运维账号（admin/user）。 */
export async function requireAccountOperator({ message = '只读角色不能操作账号' } = {}) {
  if (canOperateAccounts()) return true;
  if (!isAuthenticated()) {
    const ok = await openLoginModal({ reason: '账号操作需要登录' });
    if (ok && canOperateAccounts()) return true;
    if (ok) toastError(message);
    return false;
  }
  toastError(message);
  return false;
}

/** 要求任意登录（用于读取个人账号 / 改密）。 */
export async function requireAuthenticated({ message = '请先登录' } = {}) {
  if (isAuthenticated()) return true;
  const ok = await openLoginModal({ reason: message });
  if (ok) return true;
  return false;
}

export function authIcon() {
  return icon(isAuthenticated() ? 'user' : 'login', 16);
}
