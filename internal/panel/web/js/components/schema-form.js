// schema-form.js — 由字段描述驱动的动态表单。
//
// 这是「新增供应商不改前端」的关键：字段类型（text/secret/textarea/list/number/
// duration/bool/enum/multiselect/url/json）、分组（group）、高级折叠（advanced）、
// 条件显隐（visible_if）、帮助（help）、默认值（default）、必填（required）全部
// 来自元数据。表单不拼 innerHTML，所有值都经 DOM API 写入。

import { el, icon, clear } from '../core/dom.js';
import { getPath, deepClone } from '../core/format.js';
import { createSecretField } from './secret-field.js';

const DURATION_RE = /^(?:[0-9]+(?:\.[0-9]+)?(?:ns|us|µs|ms|s|m|h))+$/;

function safeId(prefix, name) {
  return `${prefix}-${String(name).replace(/[^\w-]+/g, '-')}`;
}

function optionList(options) {
  return (options || []).map((opt) => {
    if (typeof opt === 'string') return { value: opt, label: opt };
    return { value: String(opt?.value ?? opt?.id ?? ''), label: String(opt?.label ?? opt?.name ?? opt?.value ?? '') };
  });
}

function normalizeArray(value) {
  if (Array.isArray(value)) return value.map((v) => (v == null ? '' : String(v)));
  if (value == null || value === '') return [];
  if (typeof value === 'string') {
    return value.split(/\r?\n|,/).map((s) => s.trim()).filter(Boolean);
  }
  return [];
}

function readValue(state, name) {
  if (!name) return undefined;
  if (Object.prototype.hasOwnProperty.call(state, name)) return state[name];
  return getPath(state, name);
}

/** 条件表达式求值。支持字符串、对象、函数。 */
export function evaluateCondition(condition, state) {
  if (condition == null) return true;
  if (typeof condition === 'function') {
    try { return !!condition(state); } catch { return true; }
  }
  if (typeof condition === 'string') {
    const expr = condition.trim();
    if (!expr) return true;
    if (expr.startsWith('!')) return !readValue(state, expr.slice(1).trim());
    const neq = expr.match(/^(.+?)\s*!=\s*(.+)$/);
    if (neq) {
      const left = readValue(state, neq[1].trim());
      return String(left ?? '') !== stripQuotes(neq[2].trim());
    }
    const eq = expr.match(/^(.+?)\s*==\s*(.+)$/);
    if (eq) {
      const left = readValue(state, eq[1].trim());
      return String(left ?? '') === stripQuotes(eq[2].trim());
    }
    return !!readValue(state, expr);
  }
  if (Array.isArray(condition)) return condition.every((c) => evaluateCondition(c, state));
  if (typeof condition !== 'object') return !!condition;

  if (Array.isArray(condition.all) && !condition.all.every((c) => evaluateCondition(c, state))) return false;
  if (Array.isArray(condition.any) && !condition.any.some((c) => evaluateCondition(c, state))) return false;
  if (condition.not && evaluateCondition(condition.not, state)) return false;

  if (condition.field) {
    const value = readValue(state, condition.field);
    if ('exists' in condition) return condition.exists ? value !== undefined && value !== null && value !== '' : value === undefined || value === null || value === '';
    if ('truthy' in condition && !!value !== !!condition.truthy) return false;
    if ('falsy' in condition && !value !== !condition.falsy) return false;
    if ('equals' in condition && String(value ?? '') !== String(condition.equals ?? '')) return false;
    if ('not_equals' in condition && String(value ?? '') === String(condition.not_equals ?? '')) return false;
    if (Array.isArray(condition.in) && !condition.in.map(String).includes(String(value ?? ''))) return false;
    if (Array.isArray(condition.not_in) && condition.not_in.map(String).includes(String(value ?? ''))) return false;
    if ('value' in condition && String(value ?? '') !== String(condition.value ?? '')) return false;
  }
  return true;
}

function stripQuotes(s) {
  return s.replace(/^['"]|['"]$/g, '');
}

function isFieldVisible(field, state) {
  return evaluateCondition(field.visible_if, state);
}

function isEmpty(value) {
  if (value == null) return true;
  if (typeof value === 'string') return value.trim() === '';
  if (Array.isArray(value)) return value.filter((v) => v != null && String(v).trim() !== '').length === 0;
  if (typeof value === 'object') return Object.keys(value).length === 0;
  return false;
}

function isAdvancedField(field) {
  return field.advanced === true || field.group === '高级';
}

/**
 * 渲染 schema 表单。
 * @param {{
 *   fields: Array<any>,
 *   values?: object,
 *   disabled?: boolean,
 *   idPrefix?: string,
 *   onChange?: (values:object, name:string, value:any) => void,
 *   submitLabel?: string,
 *   onSubmit?: (values:object) => void,
 * }} options
 */
export function createSchemaForm({
  fields = [],
  values = {},
  disabled = false,
  idPrefix = `sf-${Math.random().toString(36).slice(2, 8)}`,
  onChange = null,
  submitLabel = '',
  onSubmit = null,
} = {}) {
  let state = applyDefaults(deepClone(values) || {}, fields);
  // 使用真实 form 元素：submit 按钮才会触发 submit 事件（div 不会）。
  const root = el('form', { class: 'schema-form', novalidate: true });
  const controls = new Map(); // name -> { wrap, field, input?, error, getValue }
  let advancedDetails = [];

  function build() {
    clear(root);
    controls.clear();
    advancedDetails = [];
    if (!Array.isArray(fields) || !fields.length) {
      root.appendChild(el('p', { class: 'muted small', text: '该类型没有可编辑的字段。' }));
      return;
    }
    const groups = new Map();
    for (const field of fields) {
      if (!field || !field.name) continue;
      const groupName = field.group || '基础';
      if (!groups.has(groupName)) groups.set(groupName, []);
      groups.get(groupName).push(field);
    }
    for (const [groupName, groupFields] of groups) {
      const normal = groupFields.filter((f) => !isAdvancedField(f));
      const advanced = groupFields.filter(isAdvancedField);
      const fieldset = el('fieldset', { class: 'form-group' },
        el('legend', { class: 'form-legend', text: groupName }),
      );
      const grid = el('div', { class: 'form-grid' });
      for (const field of normal) grid.appendChild(buildField(field));
      if (advanced.length) {
        const details = el('details', { class: 'form-advanced' },
          el('summary', { class: 'form-advanced-summary' },
            el('span', { text: '高级设置' }),
            el('span', { class: 'form-advanced-count', text: String(advanced.length) }),
          ),
        );
        const innerGrid = el('div', { class: 'form-grid' });
        for (const field of advanced) innerGrid.appendChild(buildField(field));
        details.appendChild(innerGrid);
        fieldset.appendChild(grid);
        fieldset.appendChild(details);
        advancedDetails.push(details);
        applyVisibility();
        root.appendChild(fieldset);
        continue;
      }
      fieldset.appendChild(grid);
      root.appendChild(fieldset);
    }

    if (submitLabel) {
      root.appendChild(el('div', { class: 'form-submit-row' },
        el('button', { class: 'btn btn-primary', type: 'submit' }, submitLabel),
      ));
    }
    applyVisibility();
  }

  function applyDefaults(source, fieldList) {
    const out = source || {};
    for (const field of fieldList || []) {
      if (!field?.name) continue;
      const has = Object.prototype.hasOwnProperty.call(out, field.name) || getPath(out, field.name) !== undefined;
      if (!has && field.default !== undefined) {
        if (Array.isArray(field.default)) out[field.name] = field.default.slice();
        else if (field.default && typeof field.default === 'object') out[field.name] = deepClone(field.default);
        else out[field.name] = field.default;
      }
    }
    return out;
  }

  function setValue(name, value, field) {
    state[name] = value;
    // 同步 dotted 路径的根对象（系统运行参数用扁平名字）。
    if (name.includes('.') && state[name] !== undefined) {
      // 扁平值优先，readValue 会先查自有属性。
    }
    const entry = controls.get(name);
    if (entry) {
      entry.wrap.classList.remove('field-invalid');
      if (entry.error) entry.error.textContent = '';
    }
    applyVisibility();
    if (typeof onChange === 'function') {
      onChange(getValues(), name, value, field);
    }
  }

  function applyVisibility() {
    for (const [name, entry] of controls) {
      const visible = isFieldVisible(entry.field, state);
      entry.wrap.hidden = !visible;
      if (!visible && entry.error) entry.error.textContent = '';
    }
  }

  function buildField(field) {
    const type = normalizeType(field.type);
    const id = safeId(idPrefix, field.name);
    const wrap = el('div', { class: `field schema-field schema-field-${type} ${field.fullWidth ? 'field-full' : ''}`.trim() });
    const label = el('label', { class: 'field-label', for: id },
      String(field.label || field.name),
      field.required ? el('span', { class: 'required-mark', text: '*', 'aria-hidden': 'true' }) : null,
    );
    const help = field.help || field.description
      ? el('p', { class: 'field-help', text: String(field.help || field.description) })
      : null;
    const errorEl = el('p', { class: 'field-error', attrs: { role: 'alert' } });
    const entry = { wrap, field, error: errorEl };
    controls.set(field.name, entry);

    let control;
    switch (type) {
      case 'secret': {
        const secret = createSecretField({
          value: state[field.name] ?? '',
          placeholder: field.placeholder || '',
          disabled: disabled || field.readOnly,
          name: field.name,
          id,
          onInput: () => setValue(field.name, secret.getValue(), field),
        });
        control = secret.el;
        entry.secret = secret;
        break;
      }
      case 'textarea':
      case 'json': {
        const textarea = el('textarea', {
          class: `input textarea ${type === 'json' ? 'mono' : ''}`.trim(),
          id,
          name: field.name,
          value: formatForTextarea(state[field.name], type),
          placeholder: field.placeholder || '',
          rows: field.rows || (type === 'json' ? 8 : 3),
          disabled: disabled || field.readOnly,
          spellcheck: type === 'json' ? 'false' : 'true',
        });
        textarea.addEventListener('input', () => setValue(field.name, textarea.value, field));
        control = textarea;
        break;
      }
      case 'list': {
        control = buildList(field, id);
        break;
      }
      case 'number': {
        const input = el('input', {
          class: 'input',
          type: 'number',
          id,
          name: field.name,
          value: state[field.name] == null ? '' : String(state[field.name]),
          placeholder: field.placeholder || '',
          min: field.min,
          max: field.max,
          step: field.step ?? 'any',
          disabled: disabled || field.readOnly,
        });
        input.addEventListener('input', () => {
          setValue(field.name, input.value === '' ? '' : Number(input.value), field);
        });
        control = input;
        break;
      }
      case 'duration': {
        const input = el('input', {
          class: 'input mono',
          type: 'text',
          id,
          name: field.name,
          value: state[field.name] == null ? '' : String(state[field.name]),
          placeholder: field.placeholder || '例如 30m、168h、10m',
          disabled: disabled || field.readOnly,
          spellcheck: 'false',
        });
        input.addEventListener('input', () => setValue(field.name, input.value.trim(), field));
        control = el('div', { class: 'input-with-suffix' }, input, el('span', { class: 'input-suffix', text: 'Go duration' }));
        break;
      }
      case 'bool': {
        const checkbox = el('input', {
          type: 'checkbox',
          id,
          name: field.name,
          checked: !!state[field.name],
          disabled: disabled || field.readOnly,
        });
        checkbox.addEventListener('change', () => setValue(field.name, checkbox.checked, field));
        control = el('label', { class: 'switch-row', for: id },
          el('span', { class: 'switch' }, checkbox, el('span', { class: 'switch-track', 'aria-hidden': 'true' })),
          el('span', { class: 'switch-text', text: field.switchLabel || (checkbox.checked ? '已启用' : '已关闭') }),
        );
        checkbox.addEventListener('change', () => {
          const text = control.querySelector('.switch-text');
          if (text) text.textContent = field.switchLabel || (checkbox.checked ? '已启用' : '已关闭');
        });
        break;
      }
      case 'enum': {
        const select = el('select', {
          class: 'input select',
          id,
          name: field.name,
          disabled: disabled || field.readOnly,
        });
        if (!field.required) select.appendChild(el('option', { value: '', text: field.placeholder || '（继承默认）' }));
        for (const opt of optionList(field.options)) {
          select.appendChild(el('option', { value: opt.value, text: opt.label, selected: String(state[field.name] ?? '') === opt.value }));
        }
        select.addEventListener('change', () => setValue(field.name, select.value, field));
        control = select;
        break;
      }
      case 'multiselect': {
        const selected = new Set(normalizeArray(state[field.name]));
        const options = optionList(field.options);
        const listBox = el('div', { class: 'check-chip-list', id, attrs: { role: 'group' } });
        for (const opt of options) {
          const cb = el('input', { type: 'checkbox', value: opt.value, checked: selected.has(opt.value), disabled: disabled || field.readOnly });
          cb.addEventListener('change', () => {
            const values = Array.from(listBox.querySelectorAll('input:checked')).map((x) => x.value);
            setValue(field.name, values, field);
          });
          listBox.appendChild(el('label', { class: `check-chip ${selected.has(opt.value) ? 'checked' : ''}`.trim() },
            cb,
            el('span', { text: opt.label }),
          ));
        }
        if (!options.length) {
          listBox.appendChild(el('p', { class: 'muted small', text: '没有可选项' }));
        }
        control = listBox;
        break;
      }
      case 'url':
      case 'email':
      case 'text':
      default: {
        const input = el('input', {
          class: `input ${type === 'url' || type === 'email' ? 'mono' : ''}`.trim(),
          type: type === 'url' ? 'url' : (type === 'email' ? 'email' : 'text'),
          id,
          name: field.name,
          value: state[field.name] == null ? '' : String(state[field.name]),
          placeholder: field.placeholder || '',
          disabled: disabled || field.readOnly,
          spellcheck: 'false',
          autocomplete: 'off',
        });
        input.addEventListener('input', () => setValue(field.name, input.value, field));
        control = input;
        break;
      }
    }

    if (field.typeHints === 'mono' && control.classList) control.classList.add('mono');
    wrap.appendChild(label);
    wrap.appendChild(control);
    if (help) wrap.appendChild(help);
    wrap.appendChild(errorEl);
    entry.control = control;
    return wrap;
  }

  function buildList(field, id) {
    const box = el('div', { class: 'list-field', id });
    const rows = el('div', { class: 'list-rows' });
    const addBtn = el('button', {
      class: 'btn btn-ghost btn-sm',
      type: 'button',
      disabled,
      on: {
        click: () => {
          const current = normalizeArray(state[field.name]);
          current.push('');
          state[field.name] = current;
          rebuildRows();
          applyVisibility();
          if (typeof onChange === 'function') onChange(getValues(), field.name, current, field);
        },
      },
    }, icon('plus', 14), '添加一项');

    function rebuildRows() {
      clear(rows);
      const items = normalizeArray(state[field.name]);
      if (!items.length) {
        rows.appendChild(el('div', { class: 'list-empty', text: field.placeholder || '尚未配置，可留空' }));
      }
      items.forEach((item, index) => {
        let inputNode;
        let valueGetter;
        if (field.item_type === 'secret') {
          const secret = createSecretField({
            value: item,
            placeholder: field.item_placeholder || '密钥',
            disabled,
            onInput: () => update(index, secret.getValue()),
          });
          inputNode = secret.el;
          valueGetter = () => secret.getValue();
        } else {
          const input = el('input', {
            class: 'input',
            type: 'text',
            value: item,
            placeholder: field.item_placeholder || '',
            disabled,
            spellcheck: 'false',
          });
          input.addEventListener('input', () => update(index, input.value));
          inputNode = input;
          valueGetter = () => input.value;
        }
        const row = el('div', { class: 'list-row' },
          el('span', { class: 'list-index mono', text: String(index + 1).padStart(2, '0') }),
          el('div', { class: 'list-control' }, inputNode),
          el('button', {
            class: 'btn btn-icon btn-ghost btn-sm',
            type: 'button',
            title: '删除这一项',
            disabled,
            attrs: { 'aria-label': '删除这一项' },
            on: {
              click: () => {
                const values = normalizeArray(state[field.name]);
                values.splice(index, 1);
                state[field.name] = values;
                rebuildRows();
                if (typeof onChange === 'function') onChange(getValues(), field.name, values, field);
              },
            },
          }, icon('trash', 14)),
        );
        if (field.item_type === 'secret') {
          // secret-field 内已有复制/显隐；避免二次包裹。
        }
        rows.appendChild(row);
      });
    }

    function update(index, value) {
      const values = normalizeArray(state[field.name]);
      values[index] = value;
      state[field.name] = values;
      if (typeof onChange === 'function') onChange(getValues(), field.name, values, field);
    }

    box.appendChild(rows);
    box.appendChild(el('div', { class: 'list-actions' }, addBtn));
    rebuildRows();
    return box;
  }

  function getValues() {
    const out = {};
    for (const field of fields || []) {
      if (!field?.name) continue;
      let value = state[field.name];
      const type = normalizeType(field.type);
      if (type === 'number') {
        if (value === '' || value == null) value = null;
        else if (Number.isFinite(Number(value))) value = Number(value);
      } else if (type === 'list' || type === 'multiselect') {
        value = normalizeArray(value);
      } else if (type === 'json') {
        if (typeof value === 'string') {
          try { value = value.trim() ? JSON.parse(value) : null; } catch { /* 保留原文，validate 报错 */ }
        }
      }
      out[field.name] = value;
    }
    return out;
  }

  function validate() {
    const errors = {};
    for (const field of fields || []) {
      if (!field?.name) continue;
      if (!isFieldVisible(field, state)) continue;
      const entry = controls.get(field.name);
      if (!entry) continue;
      const type = normalizeType(field.type);
      let value = state[field.name];
      let message = '';

      if (field.required && isEmpty(value)) {
        message = '此项为必填';
      } else if (!isEmpty(value)) {
        if (type === 'url') {
          try {
            const u = new URL(String(value));
            if (!['http:', 'https:'].includes(u.protocol)) message = '仅支持 http/https 地址';
          } catch { message = '请输入合法 URL'; }
        } else if (type === 'duration') {
          if (!DURATION_RE.test(String(value))) message = '格式如 30m、168h（Go duration）';
        } else if (type === 'number') {
          const n = Number(value);
          if (!Number.isFinite(n)) message = '请输入数字';
          else if (field.min != null && n < Number(field.min)) message = `不得小于 ${field.min}`;
          else if (field.max != null && n > Number(field.max)) message = `不得大于 ${field.max}`;
        } else if (type === 'json') {
          if (typeof value === 'string') {
            try { JSON.parse(value); } catch { message = 'JSON 格式不合法'; }
          } else {
            // 初始值可能已经是对象/数组（直接来自配置），无需再 parse。
            try { JSON.stringify(value); } catch { message = 'JSON 无法序列化'; }
          }
        }
      }
      if (message) {
        errors[field.name] = message;
        entry.wrap.classList.add('field-invalid');
        entry.error.textContent = message;
        let parent = entry.wrap.parentElement;
        while (parent) {
          if (parent.tagName === 'DETAILS') parent.open = true;
          parent = parent.parentElement;
        }
      } else {
        entry.wrap.classList.remove('field-invalid');
        entry.error.textContent = '';
      }
    }
    if (Object.keys(errors).length) {
      controls.get(Object.keys(errors)[0])?.wrap.scrollIntoView?.({ block: 'center', behavior: 'smooth' });
    }
    return { valid: Object.keys(errors).length === 0, errors };
  }

  root.addEventListener('submit', (event) => {
    event.preventDefault();
    if (!validate().valid) return;
    if (typeof onSubmit === 'function') onSubmit(getValues());
  });

  function setValues(next, { reset = false } = {}) {
    state = applyDefaults(reset ? (deepClone(next) || {}) : { ...state, ...(deepClone(next) || {}) }, fields);
    build();
  }

  build();

  return {
    el: root,
    getValues,
    validate,
    setValues,
    getState: () => state,
  };
}

function normalizeType(type) {
  switch (String(type || '').toLowerCase()) {
    case 'password':
    case 'apikey':
    case 'api_key':
    case 'token':
    case 'secret_string':
      return 'secret';
    case 'select':
      return 'enum';
    case 'checkbox':
      return 'bool';
    case 'string':
    case '':
      return 'text';
    case 'int':
    case 'float':
      return 'number';
    default:
      return String(type || 'text').toLowerCase();
  }
}

function formatForTextarea(value, type) {
  if (value == null) return '';
  if (type === 'json') {
    if (typeof value === 'string') return value;
    try { return JSON.stringify(value, null, 2); } catch { return String(value); }
  }
  if (Array.isArray(value)) return value.join('\n');
  if (typeof value === 'object') {
    try { return JSON.stringify(value, null, 2); } catch { return String(value); }
  }
  return String(value);
}
