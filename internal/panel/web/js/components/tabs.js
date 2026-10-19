// tabs.js — 二级页签。支持计数徽标、键盘左右切换、程序化设值。

import { el, icon } from '../core/dom.js';

/**
 * @param {{items:Array<{id:string,label:string,count?:number,icon?:string,disabled?:boolean}>, value?:string, onChange?:Function, ariaLabel?:string, className?:string}} options
 */
export function createTabs({ items = [], value = '', onChange = null, ariaLabel = '页签', className = '' } = {}) {
  let current = value || items.find((it) => !it.disabled)?.id || '';
  const list = el('div', {
    class: `tabs ${className}`.trim(),
    attrs: { role: 'tablist', 'aria-label': ariaLabel },
  });

  const buttons = new Map();
  for (const item of items) {
    const btn = el('button', {
      class: 'tab',
      type: 'button',
      dataset: { tab: item.id },
      attrs: {
        role: 'tab',
        'aria-selected': 'false',
        tabindex: '-1',
        ...(item.disabled ? { disabled: 'disabled' } : {}),
      },
    },
      item.icon ? icon(item.icon, 15) : null,
      el('span', { class: 'tab-label', text: item.label }),
      item.count != null ? el('span', { class: 'tab-count', text: String(item.count) }) : null,
    );
    buttons.set(item.id, btn);
    list.appendChild(btn);
  }

  const setValue = (next, { emit = false } = {}) => {
    if (!buttons.has(next)) return;
    current = next;
    for (const [id, btn] of buttons) {
      const active = id === next;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-selected', active ? 'true' : 'false');
      btn.setAttribute('tabindex', active ? '0' : '-1');
    }
    if (emit && typeof onChange === 'function') onChange(next);
  };

  list.addEventListener('click', (event) => {
    const btn = event.target instanceof Element ? event.target.closest('[data-tab]') : null;
    if (!btn || !list.contains(btn) || btn.disabled) return;
    const id = btn.dataset.tab;
    if (id === current) return;
    setValue(id, { emit: true });
  });

  list.addEventListener('keydown', (event) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    const enabled = items.filter((it) => !it.disabled);
    if (!enabled.length) return;
    const idx = enabled.findIndex((it) => it.id === current);
    let nextIdx = idx;
    if (event.key === 'ArrowLeft') nextIdx = (idx - 1 + enabled.length) % enabled.length;
    if (event.key === 'ArrowRight') nextIdx = (idx + 1) % enabled.length;
    if (event.key === 'Home') nextIdx = 0;
    if (event.key === 'End') nextIdx = enabled.length - 1;
    event.preventDefault();
    const next = enabled[nextIdx];
    setValue(next.id, { emit: true });
    buttons.get(next.id)?.focus();
  });

  setValue(current);
  return {
    el: list,
    getValue: () => current,
    setValue,
    setCount(id, count) {
      const btn = buttons.get(id);
      if (!btn) return;
      let badge = btn.querySelector('.tab-count');
      if (count == null) { badge?.remove(); return; }
      if (!badge) {
        badge = el('span', { class: 'tab-count' });
        btn.appendChild(badge);
      }
      badge.textContent = String(count);
    },
  };
}
