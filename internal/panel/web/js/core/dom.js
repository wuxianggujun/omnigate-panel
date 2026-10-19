// dom.js — 安全的 DOM 构造与通用 UI 原语。
//
// 约定：所有动态文本一律走 el() 的 text / 子节点（字符串自动成为文本节点），
// 或 esc() 转义后再进入其它上下文。本模块刻意不提供 html 属性，避免把用户
// 数据拼进 innerHTML。组件与页面只能通过 DOM API 组装界面。

const SVG_NS = 'http://www.w3.org/2000/svg';

export function esc(value) {
  return String(value ?? '').replace(/[&<>"']/g, (ch) => ({
    '&': '&amp;',
    '<': '&lt;',
    '>': '&gt;',
    '"': '&quot;',
    "'": '&#39;',
  }[ch]));
}

function isNode(value) {
  return value instanceof Node;
}

function appendChild(node, child) {
  if (child == null || child === false || child === true) return;
  if (Array.isArray(child)) {
    for (const item of child) appendChild(node, item);
    return;
  }
  if (isNode(child)) {
    node.appendChild(child);
    return;
  }
  node.appendChild(document.createTextNode(String(child)));
}

const PROPERTY_KEYS = new Set([
  'value', 'checked', 'selected', 'disabled', 'readOnly', 'required', 'multiple',
  'hidden', 'autofocus', 'indeterminate', 'open', 'muted',
]);

/**
 * 创建元素。
 * el('div', { class: 'card', text: '标题' }, childNode, '文本')
 * el('button', { class: 'btn', attrs: { type: 'button' }, on: { click: fn } }, '保存')
 */
export function el(tag, ...args) {
  const node = document.createElement(tag);
  let props = null;
  if (args.length && args[0] && typeof args[0] === 'object' && !Array.isArray(args[0]) && !isNode(args[0])) {
    props = args.shift();
  }
  if (props) applyProps(node, props);
  for (const child of args) appendChild(node, child);
  return node;
}

function applyProps(node, props) {
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) {
      if (key === 'disabled' || key === 'checked' || key === 'selected' || key === 'readOnly' || key === 'required' || key === 'multiple' || key === 'hidden') {
        // 布尔属性显式置假。
        try { node[key] = false; } catch { /* ignore */ }
      }
      continue;
    }
    switch (key) {
      case 'class':
      case 'className':
        node.className = Array.isArray(value) ? value.filter(Boolean).join(' ') : String(value);
        break;
      case 'text':
        node.textContent = String(value);
        break;
      case 'attrs':
        for (const [k, v] of Object.entries(value)) {
          if (v != null) node.setAttribute(k, String(v));
        }
        break;
      case 'dataset':
        for (const [k, v] of Object.entries(value)) {
          if (v != null) node.dataset[k] = String(v);
        }
        break;
      case 'style':
        if (typeof value === 'object') Object.assign(node.style, value);
        else if (typeof value === 'string') node.style.cssText = value;
        break;
      case 'on':
        for (const [event, fn] of Object.entries(value)) {
          if (typeof fn === 'function') node.addEventListener(event, fn);
        }
        break;
      case 'children':
        appendChild(node, value);
        break;
      default:
        if (key.startsWith('aria-') || key.startsWith('data-') || key === 'role') {
          node.setAttribute(key, String(value));
        } else if (PROPERTY_KEYS.has(key)) {
          node[key] = value;
        } else if (key in node) {
          try { node[key] = value; } catch { node.setAttribute(key, String(value)); }
        } else {
          node.setAttribute(key, String(value));
        }
    }
  }
}

/** 创建 SVG 元素（图标的底层构造）。 */
export function svgEl(tag, attrs = {}, children = []) {
  const node = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v != null) node.setAttribute(k, String(v));
  }
  for (const child of children) {
    if (child) node.appendChild(child);
  }
  return node;
}

const ICONS = {
  overview: ['M4 4h7v7H4z', 'M13 4h7v4h-7z', 'M13 10h7v10h-7z', 'M4 13h7v7H4z'],
  providers: ['M4 5h16v5H4z', 'M4 14h16v5H4z', 'M7 7.5h.01', 'M7 16.5h.01'],
  models: ['M12 3l8 4.5v9L12 21l-8-4.5v-9z', 'M12 12l8-4.5', 'M12 12v9', 'M12 12L4 7.5'],
  routing: ['M5 5h4v4H5z', 'M15 15h4v4h-4z', 'M9 7h4a2 2 0 0 1 2 2v8', 'M6 9v6a2 2 0 0 0 2 2h7'],
  usage: ['M4 20V10', 'M10 20V4', 'M16 20v-7', 'M22 20H2'],
  requests: ['M5 4h14v16H5z', 'M8 8h8', 'M8 12h8', 'M8 16h5'],
  system: ['M12 15.2a3.2 3.2 0 1 0 0-6.4 3.2 3.2 0 0 0 0 6.4z', 'M19.4 15a1.7 1.7 0 0 0 .34 1.87l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06A1.7 1.7 0 0 0 15 19.36a1.7 1.7 0 0 0-1 1.55V21a2 2 0 1 1-4 0v-.09A1.7 1.7 0 0 0 9 19.36a1.7 1.7 0 0 0-1.87.34l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06A1.7 1.7 0 0 0 4.64 15a1.7 1.7 0 0 0-1.55-1H3a2 2 0 1 1 0-4h.09A1.7 1.7 0 0 0 4.64 9a1.7 1.7 0 0 0-.34-1.87l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06A1.7 1.7 0 0 0 9 4.64a1.7 1.7 0 0 0 1-1.55V3a2 2 0 1 1 4 0v.09a1.7 1.7 0 0 0 1 1.55 1.7 1.7 0 0 0 1.87-.34l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06A1.7 1.7 0 0 0 19.36 9c.14.4.43.74.8.96.3.17.64.26.98.26H21a2 2 0 1 1 0 4h-.09a1.7 1.7 0 0 0-1.51.78z'],
  menu: ['M4 7h16', 'M4 12h16', 'M4 17h16'],
  close: ['M6 6l12 12', 'M18 6L6 18'],
  refresh: ['M20 11a8 8 0 1 0-2.34 6.34', 'M20 4v7h-7'],
  plus: ['M12 5v14', 'M5 12h14'],
  trash: ['M4 7h16', 'M10 11v6', 'M14 11v6', 'M6 7l1 13h10l1-13', 'M9 7V4h6v3'],
  edit: ['M4 20h4L20 8l-4-4L4 16z', 'M14 6l4 4'],
  chevron: ['M9 6l6 6-6 6'],
  chevronDown: ['M6 9l6 6 6-6'],
  check: ['M5 12l5 5L20 7'],
  alert: ['M12 4l9 16H3z', 'M12 9v5', 'M12 17h.01'],
  info: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M12 11v5', 'M12 8h.01'],
  copy: ['M9 9h10v11H9z', 'M5 15V4h10'],
  eye: ['M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12z', 'M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z'],
  eyeOff: ['M3 3l18 18', 'M10.6 6.2A10.9 10.9 0 0 1 12 6c6.5 0 10 6 10 6a17 17 0 0 1-3.1 3.6', 'M6.6 6.6C3.6 8.4 2 12 2 12s3.5 6 10 6a10.6 10.6 0 0 0 4.2-.9', 'M9.9 9.9a3 3 0 0 0 4.2 4.2'],
  sun: ['M12 17a5 5 0 1 0 0-10 5 5 0 0 0 0 10z', 'M12 2v2', 'M12 20v2', 'M4.9 4.9l1.4 1.4', 'M17.7 17.7l1.4 1.4', 'M2 12h2', 'M20 12h2', 'M4.9 19.1l1.4-1.4', 'M17.7 6.3l1.4-1.4'],
  moon: ['M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z'],
  auto: ['M4 5h16v12H4z', 'M8 21h8', 'M12 17v4'],
  logout: ['M10 5H5v14h5', 'M14 8l4 4-4 4', 'M18 12H9'],
  user: ['M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M4 21a8 8 0 0 1 16 0'],
  search: ['M11 19a8 8 0 1 0 0-16 8 8 0 0 0 0 16z', 'M21 21l-4.35-4.35'],
  arrowUp: ['M12 19V5', 'M6 11l6-6 6 6'],
  arrowDown: ['M12 5v14', 'M6 13l6 6 6-6'],
  arrowRight: ['M5 12h14', 'M13 6l6 6-6 6'],
  login: ['M14 5h5v14h-5', 'M10 8l4 4-4 4', 'M14 12H3'],
  key: ['M14 7a4 4 0 1 0-3.5 4L4 17.5V21h3.5l1-1v-2h2v-2h2l1.5-1.5A4 4 0 0 0 14 7z', 'M16.5 6.5h.01'],
  shield: ['M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z', 'M9 12l2 2 4-4'],
  server: ['M4 4h16v6H4z', 'M4 14h16v6H4z', 'M8 7h.01', 'M8 17h.01'],
  activity: ['M3 12h4l2-7 4 14 2-7h6'],
  clock: ['M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18z', 'M12 7v5l3 2'],
  download: ['M12 4v11', 'M7 10l5 5 5-5', 'M4 20h16'],
  filter: ['M4 5h16', 'M7 12h10', 'M10 19h4'],
  more: ['M6 12h.01', 'M12 12h.01', 'M18 12h.01'],
  link: ['M10 13a5 5 0 0 0 7 0l2-2a5 5 0 0 0-7-7l-1 1', 'M14 11a5 5 0 0 0-7 0l-2 2a5 5 0 0 0 7 7l1-1'],
  power: ['M12 3v9', 'M6.4 6.4a8 8 0 1 0 11.2 0'],
};

/** 线性图标（无 emoji、无外部资源）。 */
export function icon(name, size = 18, extraClass = '') {
  const paths = ICONS[name] || ICONS.info;
  const node = svgEl('svg', {
    viewBox: '0 0 24 24',
    width: size,
    height: size,
    fill: 'none',
    stroke: 'currentColor',
    'stroke-width': 1.8,
    'stroke-linecap': 'round',
    'stroke-linejoin': 'round',
    'aria-hidden': 'true',
    class: `icon ${extraClass}`.trim(),
    focusable: 'false',
  });
  for (const d of paths) node.appendChild(svgEl('path', { d }));
  return node;
}

export function clear(node) {
  while (node && node.firstChild) node.removeChild(node.firstChild);
  return node;
}

export function mountNodes(root, ...children) {
  clear(root);
  for (const child of children) appendChild(root, child);
  return root;
}

/** 事件委托：handler(event, matchedElement)。 */
export function delegate(root, type, selector, handler, options) {
  const listener = (event) => {
    const target = event.target instanceof Element ? event.target.closest(selector) : null;
    if (target && root.contains(target)) handler(event, target);
  };
  root.addEventListener(type, listener, options);
  return () => root.removeEventListener(type, listener, options);
}

export function qs(selector, root = document) {
  return root.querySelector(selector);
}

export function qsa(selector, root = document) {
  return Array.from(root.querySelectorAll(selector));
}

/** 内容卡片。body 可为 Node / Node[] / 字符串。 */
export function card({ title, subtitle, actions, body, class: className = '', compact = false, tag = 'section' } = {}) {
  const node = el(tag, { class: `card ${compact ? 'card-compact' : ''} ${className}`.trim() });
  if (title || subtitle || actions) {
    node.appendChild(el('div', { class: 'card-head' },
      el('div', { class: 'card-heading' },
        title ? el('h2', { class: 'card-title', text: title }) : null,
        subtitle ? el('p', { class: 'card-subtitle', text: subtitle }) : null,
      ),
      actions ? el('div', { class: 'card-actions' }, actions) : null,
    ));
  }
  if (body != null) {
    node.appendChild(el('div', { class: 'card-body' }, body));
  }
  return node;
}

export function badge(text, tone = 'muted', { dot = false, title } = {}) {
  return el('span', { class: `badge badge-${tone}`, title: title || undefined },
    dot ? el('span', { class: 'dot', 'aria-hidden': 'true' }) : null,
    String(text),
  );
}

export function statusDot(tone = 'muted', label) {
  return el('span', { class: `status status-${tone}`, title: label || undefined },
    el('span', { class: 'status-dot', 'aria-hidden': 'true' }),
    label ? el('span', { class: 'status-label', text: label }) : null,
  );
}

export function emptyState(title, description, actions = null) {
  return el('div', { class: 'empty-state' },
    el('div', { class: 'empty-mark', 'aria-hidden': 'true' }, icon('info', 22)),
    el('h3', { class: 'empty-title', text: title }),
    description ? el('p', { class: 'empty-desc', text: description }) : null,
    actions ? el('div', { class: 'empty-actions' }, actions) : null,
  );
}

export function loadingBlock(text = '加载中') {
  return el('div', { class: 'loading-block', role: 'status' },
    el('span', { class: 'spinner', 'aria-hidden': 'true' }),
    el('span', { text }),
  );
}

export function field(label, control, { help, error, required = false, inline = false, id } = {}) {
  return el('div', { class: `field ${inline ? 'field-inline' : ''}`.trim() },
    el('label', { class: 'field-label', for: id || undefined },
      label,
      required ? el('span', { class: 'required-mark', text: '*', 'aria-hidden': 'true' }) : null,
    ),
    control,
    help ? el('p', { class: 'field-help', text: help }) : null,
    error ? el('p', { class: 'field-error', text: error }) : null,
  );
}

export function copyText(text) {
  const value = String(text ?? '');
  if (navigator.clipboard && window.isSecureContext) {
    return navigator.clipboard.writeText(value).then(() => true).catch(() => fallbackCopy(value));
  }
  return Promise.resolve(fallbackCopy(value));
}

function fallbackCopy(value) {
  const area = el('textarea', { value, class: 'visually-hidden', attrs: { readonly: 'readonly' } });
  document.body.appendChild(area);
  area.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch { ok = false; }
  area.remove();
  return ok;
}

/** 简易防抖。 */
export function debounce(fn, wait = 200) {
  let timer = 0;
  const wrapped = (...args) => {
    clearTimeout(timer);
    timer = setTimeout(() => fn(...args), wait);
  };
  wrapped.cancel = () => clearTimeout(timer);
  return wrapped;
}

export function isAbortError(error) {
  return !!error && (error.name === 'AbortError' || error.code === 20);
}

export const ICON_NAMES = Object.freeze(Object.keys(ICONS));
