// modal.js — 居中对话框、确认框。焦点约束、Esc 关闭、恢复焦点。

import { el, icon } from '../core/dom.js';

const stack = [];

function rootEl() {
  return document.getElementById('modal-root') || document.body;
}

function updateBodyState() {
  document.body.classList.toggle('modal-open', stack.length > 0);
}

export function openModal({
  title = '',
  subtitle = '',
  body = null,
  footer = null,
  size = 'md',
  dismissible = true,
  onClose = null,
  initialFocus = null,
  class: className = '',
} = {}) {
  const previouslyFocused = document.activeElement;
  const panel = el('div', {
    class: `modal-panel modal-${size} ${className}`.trim(),
    attrs: { role: 'dialog', 'aria-modal': 'true', 'aria-label': title || '对话框' },
  });
  const closeBtn = el('button', {
    class: 'modal-close btn-icon',
    type: 'button',
    attrs: { 'aria-label': '关闭' },
    on: { click: () => close() },
  }, icon('close', 18));

  const head = el('div', { class: 'modal-head' },
    el('div', { class: 'modal-heading' },
      title ? el('h2', { class: 'modal-title', text: title }) : null,
      subtitle ? el('p', { class: 'modal-subtitle', text: subtitle }) : null,
    ),
    dismissible ? closeBtn : null,
  );
  const bodyWrap = el('div', { class: 'modal-body' }, body);
  panel.appendChild(head);
  panel.appendChild(bodyWrap);
  if (footer) panel.appendChild(el('div', { class: 'modal-footer' }, footer));

  // 只有「按下」和「松开」都发生在遮罩本身时才关闭。不能用 click：
  // click 的 target 是按下点与松开点的最近公共祖先——在表单里选中文本后把
  // 鼠标拖到面板外的遮罩上松开，target 会变成遮罩，导致填了一半的表单被关掉。
  let pressedOnOverlay = false;
  const overlay = el('div', { class: 'modal-overlay', on: {
    mousedown: (e) => { pressedOnOverlay = e.target === overlay; },
    mouseup: (e) => {
      const shouldClose = dismissible && pressedOnOverlay && e.target === overlay;
      pressedOnOverlay = false;
      if (shouldClose) close();
    },
  } }, panel);

  let closed = false;
  const onKeydown = (event) => {
    if (event.key === 'Escape' && dismissible) {
      event.stopPropagation();
      close();
      return;
    }
    if (event.key === 'Tab') trapFocus(event, panel);
  };

  function close() {
    if (closed) return;
    closed = true;
    document.removeEventListener('keydown', onKeydown, true);
    overlay.remove();
    const idx = stack.indexOf(entry);
    if (idx >= 0) stack.splice(idx, 1);
    updateBodyState();
    if (previouslyFocused instanceof HTMLElement) previouslyFocused.focus();
    if (typeof onClose === 'function') onClose();
  }

  const entry = { overlay, panel, close, body: bodyWrap, footer: panel.querySelector('.modal-footer') };
  document.addEventListener('keydown', onKeydown, true);
  stack.push(entry);
  updateBodyState();
  rootEl().appendChild(overlay);

  const focusTarget = initialFocus instanceof HTMLElement
    ? initialFocus
    : panel.querySelector('input:not([type=hidden]):not([disabled]), textarea:not([disabled]), select:not([disabled]), button:not([disabled])');
  requestAnimationFrame(() => {
    if (focusTarget && focusTarget.isConnected) focusTarget.focus();
    else panel.setAttribute('tabindex', '-1'), panel.focus();
  });
  return entry;
}

function trapFocus(event, panel) {
  const focusables = Array.from(panel.querySelectorAll(
    'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )).filter((node) => node.offsetParent !== null || node === document.activeElement);
  if (!focusables.length) return;
  const first = focusables[0];
  const last = focusables[focusables.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

export function confirmDialog({
  title = '确认操作',
  message = '',
  confirmText = '确定',
  cancelText = '取消',
  danger = false,
} = {}) {
  return new Promise((resolve) => {
    let done = false;
    const finish = (value) => {
      if (done) return;
      done = true;
      resolve(value);
    };
    const modal = openModal({
      title,
      size: 'sm',
      body: el('p', { class: 'confirm-message', text: message }),
      footer: [
        el('button', {
          class: 'btn btn-ghost',
          type: 'button',
          on: { click: () => { finish(false); modal.close(); } },
        }, cancelText),
        el('button', {
          class: `btn ${danger ? 'btn-danger' : 'btn-primary'}`,
          type: 'button',
          on: { click: () => { finish(true); modal.close(); } },
        }, confirmText),
      ],
      onClose: () => finish(false),
    });
  });
}

export function closeAllModals() {
  for (const entry of [...stack]) entry.close();
}
