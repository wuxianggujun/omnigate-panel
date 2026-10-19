// drawer.js — 右侧详情抽屉，用于请求详情/运行日志，不打断列表上下文。

import { el, icon } from '../core/dom.js';

const stack = [];

export function openDrawer({
  title = '',
  subtitle = '',
  body = null,
  footer = null,
  size = 'lg',
  onClose = null,
} = {}) {
  const previouslyFocused = document.activeElement;
  const titleNode = el('h2', { class: 'drawer-title', text: title });
  const subtitleNode = el('p', { class: 'drawer-subtitle', text: subtitle });
  const bodyWrap = el('div', { class: 'drawer-body' }, body);
  const panel = el('div', {
    class: `drawer-panel drawer-${size}`,
    attrs: { role: 'dialog', 'aria-modal': 'true', 'aria-label': title || '详情' },
  },
    el('div', { class: 'drawer-head' },
      el('div', { class: 'drawer-heading' }, titleNode, subtitleNode),
      el('button', {
        class: 'btn-icon drawer-close',
        type: 'button',
        attrs: { 'aria-label': '关闭详情' },
        on: { click: () => close() },
      }, icon('close', 18)),
    ),
    bodyWrap,
    footer ? el('div', { class: 'drawer-footer' }, footer) : null,
  );

  const overlay = el('div', { class: 'drawer-overlay', on: { click: (e) => {
    if (e.target === overlay) close();
  } } }, panel);

  let closed = false;
  const onKeydown = (event) => {
    if (event.key === 'Escape' && stack[stack.length - 1]?.overlay === overlay) {
      event.stopPropagation();
      close();
    }
  };
  function close() {
    if (closed) return;
    closed = true;
    document.removeEventListener('keydown', onKeydown, true);
    overlay.remove();
    const idx = stack.findIndex((x) => x.overlay === overlay);
    if (idx >= 0) stack.splice(idx, 1);
    document.body.classList.toggle('drawer-open', stack.length > 0);
    if (previouslyFocused instanceof HTMLElement) previouslyFocused.focus();
    if (typeof onClose === 'function') onClose();
  }

  const entry = {
    overlay,
    panel,
    body: bodyWrap,
    close,
    setTitle: (text) => { titleNode.textContent = text; },
    setSubtitle: (text) => { subtitleNode.textContent = text; },
  };
  document.addEventListener('keydown', onKeydown, true);
  stack.push(entry);
  document.body.classList.add('drawer-open');
  (document.getElementById('drawer-root') || document.body).appendChild(overlay);
  requestAnimationFrame(() => {
    const focusable = panel.querySelector('button, input, select, textarea, [tabindex]');
    if (focusable) focusable.focus();
  });
  return entry;
}
