// router.js — hash 路由 + 页面生命周期（mount/unmount）。
//
// 每个页面模块导出 mount(root, ctx) 与 unmount()。切换页面时先 unmount 旧页面，
// 再清空 outlet 并挂载新页面。页面自己持有 AbortController / 定时器并在 unmount
// 里清理，路由不做资源猜测。

import { el, icon, loadingBlock } from './dom.js';

export function parseHash(hash = window.location.hash) {
  let raw = String(hash || '').replace(/^#/, '');
  if (!raw.startsWith('/')) raw = `/${raw}`;
  const qIndex = raw.indexOf('?');
  const path = (qIndex >= 0 ? raw.slice(0, qIndex) : raw).replace(/\/+$/, '') || '/overview';
  const query = {};
  if (qIndex >= 0) {
    const params = new URLSearchParams(raw.slice(qIndex + 1));
    for (const [key, value] of params.entries()) query[key] = value;
  }
  return { path, query, full: raw };
}

export function createRouter({
  routes = [],
  outlet,
  onNavigate = null,
  context = {},
} = {}) {
  if (!outlet) throw new Error('createRouter: outlet is required');
  let current = null;
  let token = 0;
  let started = false;

  function findRoute(path) {
    return routes.find((r) => r.path === path) || null;
  }

  async function unmountCurrent() {
    if (!current) return;
    const page = current;
    current = null;
    try {
      if (page.module && typeof page.module.unmount === 'function') {
        await page.module.unmount();
      }
    } catch (error) {
      console.error('page unmount failed', error);
    }
  }

  async function mountRoute() {
    const routeInfo = parseHash();
    const route = findRoute(routeInfo.path);
    const myToken = ++token;
    await unmountCurrent();
    if (myToken !== token) return;

    outlet.replaceChildren(loadingBlock('正在加载页面'));
    outlet.setAttribute('aria-busy', 'true');
    if (typeof onNavigate === 'function') onNavigate(routeInfo, route);

    if (!route) {
      renderNotFound(routeInfo);
      outlet.removeAttribute('aria-busy');
      return;
    }

    try {
      const module = route.load ? await route.load() : route.module;
      if (myToken !== token) return;
      if (!module || typeof module.mount !== 'function') {
        throw new Error(`页面 ${route.path} 未导出 mount()`);
      }
      const ctx = {
        ...context,
        route: routeInfo,
        navigate,
        reload: () => mountRoute(),
      };
      outlet.replaceChildren();
      const handle = await module.mount(outlet, ctx);
      if (myToken !== token) {
        if (module.unmount) await module.unmount();
        return;
      }
      current = { module, handle, route };
      outlet.removeAttribute('aria-busy');
      window.scrollTo({ top: 0, behavior: 'auto' });
      outlet.focus({ preventScroll: true });
    } catch (error) {
      console.error('page mount failed', error);
      if (myToken !== token) return;
      outlet.removeAttribute('aria-busy');
      renderError(error);
    }
  }

  function renderNotFound(routeInfo) {
    outlet.replaceChildren(el('div', { class: 'page-error' },
      el('div', { class: 'page-error-mark', 'aria-hidden': 'true' }, icon('alert', 26)),
      el('h1', { text: '页面不存在' }),
      el('p', { text: `找不到路由 ${routeInfo.path}` }),
      el('a', { class: 'btn btn-primary', href: '#/overview', text: '返回总览' }),
    ));
  }

  function renderError(error) {
    outlet.replaceChildren(el('div', { class: 'page-error' },
      el('div', { class: 'page-error-mark', 'aria-hidden': 'true' }, icon('alert', 26)),
      el('h1', { text: '页面加载失败' }),
      el('p', { text: String(error?.message || error || '未知错误') }),
      el('button', {
        class: 'btn btn-primary',
        type: 'button',
        on: { click: () => mountRoute() },
      }, '重试'),
    ));
  }

  function navigate(path, { replace = false } = {}) {
    const target = `#${path.startsWith('/') ? path : `/${path}`}`;
    if (replace) {
      history.replaceState(null, '', target);
      mountRoute();
      return;
    }
    if (window.location.hash === target) {
      mountRoute();
      return;
    }
    window.location.hash = target;
  }

  function onHashChange() {
    mountRoute();
  }

  function start() {
    if (started) return;
    started = true;
    if (!window.location.hash) {
      history.replaceState(null, '', '#/overview');
    }
    window.addEventListener('hashchange', onHashChange);
    mountRoute();
  }

  function stop() {
    started = false;
    window.removeEventListener('hashchange', onHashChange);
    token += 1;
    unmountCurrent();
  }

  return {
    start,
    stop,
    navigate,
    reload: mountRoute,
    getRoute: () => parseHash(),
    getCurrent: () => current,
  };
}
