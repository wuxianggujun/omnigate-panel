// secret-field.js — 密钥输入框：默认隐藏、可显隐、可复制；掩码值原样展示。

import { el, icon, copyText } from '../core/dom.js';
import { isMasked } from '../core/format.js';
import { toast } from './toast.js';

/**
 * @returns {{el: HTMLElement, input: HTMLInputElement, getValue: () => string, setValue: (v:string)=>void}}
 */
export function createSecretField({
  value = '',
  placeholder = '',
  disabled = false,
  name = '',
  id = '',
  autocomplete = 'new-password',
  copyable = true,
  onInput = null,
} = {}) {
  const masked = isMasked(value);
  const input = el('input', {
    class: 'input secret-input',
    type: masked ? 'text' : 'password',
    value: String(value ?? ''),
    placeholder,
    disabled,
    name: name || undefined,
    id: id || undefined,
    autocomplete,
    spellcheck: 'false',
    attrs: { autocapitalize: 'off', autocorrect: 'off' },
  });

  let visible = masked;
  let copiedTimer = 0;
  const toggle = el('button', {
    class: 'secret-action',
    type: 'button',
    title: masked ? '已配置（掩码）' : '显示或隐藏',
    attrs: { 'aria-label': '显示或隐藏密钥' },
    on: {
      click: () => {
        visible = !visible;
        input.type = visible ? 'text' : 'password';
        const use = visible ? icon('eyeOff', 15) : icon('eye', 15);
        toggle.replaceChildren(use);
      },
    },
  }, visible ? icon('eyeOff', 15) : icon('eye', 15));

  const actions = [toggle];
  if (copyable) {
    actions.push(el('button', {
      class: 'secret-action',
      type: 'button',
      title: '复制当前值',
      attrs: { 'aria-label': '复制密钥' },
      on: {
        click: async () => {
          const ok = await copyText(input.value);
          if (ok) {
            toast('已复制到剪贴板', { type: 'success', duration: 1800 });
          } else {
            toast('复制失败，请手动选择输入框内容', { type: 'warn' });
          }
        },
      },
    }, icon('copy', 15)));
  }

  const wrap = el('div', { class: `secret-field ${disabled ? 'is-disabled' : ''}`.trim() }, input, el('div', { class: 'secret-actions' }, actions));
  if (onInput) input.addEventListener('input', onInput);
  return {
    el: wrap,
    input,
    getValue: () => input.value,
    setValue: (v) => {
      input.value = String(v ?? '');
      if (isMasked(input.value)) { visible = true; input.type = 'text'; } else if (visible) input.type = 'text';
    },
  };
}
