// toast.js — 非阻塞通知。错误信息来自 api.js 的统一描述。

import { el, icon, clear } from '../core/dom.js';
import { describeError } from '../core/api.js';

let container = null;
let counter = 0;

function ensureContainer() {
  if (container && document.body.contains(container)) return container;
  container = document.getElementById('toast-root');
  if (!container) {
    container = el('div', { id: 'toast-root', class: 'toast-root', attrs: { 'aria-live': 'polite', 'aria-atomic': 'false' } });
    document.body.appendChild(container);
  }
  return container;
}

const TONE_ICON = { success: 'check', error: 'alert', warn: 'alert', info: 'info' };

export function toast(message, { type = 'info', title = '', duration = 3600 } = {}) {
  const root = ensureContainer();
  const id = `toast-${++counter}`;
  const tone = TONE_ICON[type] ? type : 'info';
  const node = el('div', { class: `toast toast-${tone}`, attrs: { role: tone === 'error' ? 'alert' : 'status', id } },
    el('span', { class: 'toast-icon', 'aria-hidden': 'true' }, icon(TONE_ICON[tone], 16)),
    el('div', { class: 'toast-content' },
      title ? el('div', { class: 'toast-title', text: title }) : null,
      el('div', { class: 'toast-message', text: String(message ?? '') }),
    ),
    el('button', {
      class: 'toast-close',
      attrs: { type: 'button', 'aria-label': '关闭通知' },
      on: { click: () => remove() },
    }, icon('close', 14)),
  );
  let timer = 0;
  const remove = () => {
    clearTimeout(timer);
    if (!node.isConnected) return;
    node.classList.add('toast-out');
    setTimeout(() => node.remove(), 160);
  };
  root.appendChild(node);
  while (root.children.length > 5) root.firstElementChild?.remove();
  if (duration > 0) timer = setTimeout(remove, duration);
  node.addEventListener('mouseenter', () => clearTimeout(timer));
  node.addEventListener('mouseleave', () => { if (duration > 0) timer = setTimeout(remove, 1600); });
  return remove;
}

export function toastSuccess(message, options = {}) {
  return toast(message, { ...options, type: 'success' });
}

export function toastWarn(message, options = {}) {
  return toast(message, { ...options, type: 'warn' });
}

export function toastError(error, options = {}) {
  const message = typeof error === 'string' ? error : describeError(error);
  return toast(message, { ...options, type: 'error', duration: options.duration ?? 5200 });
}

export function clearToasts() {
  if (!container) return;
  clear(container);
}
