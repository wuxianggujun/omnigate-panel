// main.js — 应用入口：主题、壳层导航、会话、hash 路由与页面懒加载。
//
// 无构建、无 npm、无内联脚本；所有静态资源从 /panel/ 根加载。?mock=1 时在
// 任何 API 调用之前安装 fetch 拦截层，页面与真实后端保持完全相同的代码路径。

import {
  authState, authStore, logout, openLoginModal, refreshSession,
} from './core/auth.js';
import { createRouter } from './core/router.js';
import {
  el, icon, clear, badge, statusDot, qs,
} from './core/dom.js';
import { roleLabel } from './core/format.js';
import { toast, toastWarn, toastError } from './components/toast.js';
import { closeAllModals } from './components/modal.js';

const isMock = new URLSearchParams(location.search).get('mock') === '1';

const ROUTES = [
  {
    path: '/overview',
    label: '总览',
    title: '总览',
    subtitle: '引擎健康与近期活动',
    icon: 'overview',
    load: () => import('./pages/overview.js'),
  },
  {
    group: '配置',
    path: '/providers',
    label: '供应商',
    title: '供应商',
    subtitle: '账号、上游密钥、模型目录与运行参数',
    icon: 'providers',
    load: () => import('./pages/providers.js'),
  },
  {
    group: '配置',
    path: '/models',
    label: '模型',
    title: '模型',
    subtitle: '带供应商前缀的对外模型名；别名由「路由」维护',
    icon: 'models',
    load: () => import('./pages/models.js'),
  },
  {
    group: '配置',
    path: '/routing',
    label: '路由',
    title: '路由',
    subtitle: '别名优先于优先级，优先级优先于默认供应商',
    icon: 'routing',
    load: () => import('./pages/routing.js'),
  },
  {
    group: '配置',
    path: '/users',
    label: '用户',
    title: '用户',
    subtitle: '按入站密钥的人与访问范围：谁在用、能用哪些供应商',
    // 按用户统计需要全量归属视图；普通用户只会得到一句「不可见」，
    // 所以不下发这个入口。直接访问 #/users 仍由页面本身说明原因。
    roles: ['admin', 'viewer'],
    icon: 'user',
    load: () => import('./pages/users.js'),
  },
  {
    group: '观测',
    path: '/usage',
    label: '用量',
    title: '用量',
    subtitle: 'token、请求数与积分趋势，可按供应商/账号/模型拆分',
    icon: 'usage',
    load: () => import('./pages/usage.js'),
  },
  {
    group: '观测',
    path: '/requests',
    label: '请求',
    title: '请求',
    subtitle: '请求归档与运行日志',
    icon: 'requests',
    load: () => import('./pages/requests.js'),
  },
  {
    group: '观测',
    path: '/system',
    label: '系统',
    title: '系统',
    subtitle: '密钥、账号、代理与存储',
    icon: 'system',
    load: () => import('./pages/system.js'),
  },
];

const THEME_KEY = 'og-theme';
const THEME_MODES = ['auto', 'light', 'dark'];

let router = null;
let themeMode = 'auto';
let mediaQuery = null;
let navLinks = [];
let bottomLinks = [];

function bootTheme() {
  try { themeMode = localStorage.getItem(THEME_KEY) || 'auto'; } catch { themeMode = 'auto'; }
  if (!THEME_MODES.includes(themeMode)) themeMode = 'auto';
  mediaQuery = window.matchMedia('(prefers-color-scheme: dark)');
  applyTheme();
  mediaQuery.addEventListener?.('change', () => { if (themeMode === 'auto') applyTheme(); });
}

function resolvedTheme(mode = themeMode) {
  if (mode === 'light' || mode === 'dark') return mode;
  return mediaQuery?.matches ? 'dark' : 'light';
}

function applyTheme() {
  const resolved = resolvedTheme();
  document.documentElement.dataset.theme = resolved;
  document.documentElement.dataset.themeMode = themeMode;
  document.documentElement.style.colorScheme = resolved;
  const btn = qs('#theme-btn');
  if (btn) {
    const iconName = themeMode === 'auto' ? 'auto' : (themeMode === 'light' ? 'sun' : 'moon');
    btn.replaceChildren(icon(iconName, 17));
    const label = themeMode === 'auto' ? '跟随系统' : (themeMode === 'light' ? '浅色' : '深色');
    btn.title = `主题：${label}（点击切换）`;
    btn.setAttribute('aria-label', `主题：${label}，点击切换`);
    btn.dataset.mode = themeMode;
  }
}

function cycleTheme() {
  const index = THEME_MODES.indexOf(themeMode);
  themeMode = THEME_MODES[(index + 1) % THEME_MODES.length];
  try { localStorage.setItem(THEME_KEY, themeMode); } catch { /* ignore */ }
  applyTheme();
  toast(`主题：${themeMode === 'auto' ? '跟随系统' : (themeMode === 'light' ? '浅色' : '深色')}`, { type: 'info', duration: 1600 });
}

function buildNav() {
  const nav = qs('#nav');
  const bottom = qs('#bottomnav');
  if (!nav || !bottom) return;
  clear(nav);
  clear(bottom);
  navLinks = [];
  bottomLinks = [];
  const auth = authState();
  // 8 个平铺项看不出彼此关系。按「配置 → 观测」分组：前四页决定请求怎么走，
  // 后四页看它到底走了哪。分组标题只在侧边栏显示，移动端底栏保持平铺。
  let lastGroup = null;
  for (const route of ROUTES) {
    if (route.roles && !route.roles.includes(auth.role)) continue;
    if (route.group && route.group !== lastGroup) {
      lastGroup = route.group;
      nav.appendChild(el('div', { class: 'nav-group-label', text: route.group }));
    }
    const link = el('a', {
      class: 'nav-item',
      href: `#${route.path}`,
      dataset: { route: route.path },
      attrs: { 'aria-label': route.label },
    },
      el('span', { class: 'nav-icon', 'aria-hidden': 'true' }, icon(route.icon, 18)),
      el('span', { class: 'nav-label', text: route.label }),
    );
    nav.appendChild(link);
    navLinks.push(link);

    const mobile = el('a', {
      class: 'bottom-item',
      href: `#${route.path}`,
      dataset: { route: route.path },
      attrs: { 'aria-label': route.label },
    },
      el('span', { class: 'bottom-icon', 'aria-hidden': 'true' }, icon(route.icon, 19)),
      el('span', { class: 'bottom-label', text: route.label }),
    );
    bottom.appendChild(mobile);
    bottomLinks.push(mobile);
  }
}

function setActiveNav(path) {
  for (const link of navLinks) {
    const active = link.dataset.route === path;
    link.classList.toggle('active', active);
    if (active) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
  for (const link of bottomLinks) {
    const active = link.dataset.route === path;
    link.classList.toggle('active', active);
    if (active) link.setAttribute('aria-current', 'page');
    else link.removeAttribute('aria-current');
  }
}

function renderShellSession() {
  const auth = authState();
  const chip = qs('#session-chip');
  const sidebarFoot = qs('#sidebar-session');
  if (!chip || !sidebarFoot) return;
  clear(chip);
  clear(sidebarFoot);

  if (auth.authenticated) {
    chip.appendChild(el('span', { class: 'session-user', title: auth.username },
      el('span', { class: 'session-avatar', 'aria-hidden': 'true' }, icon('user', 15)),
      el('span', { class: 'session-name', text: auth.username }),
    ));
    chip.appendChild(badge(roleLabel(auth.role), auth.role === 'admin' ? 'accent' : (auth.role === 'viewer' ? 'info' : 'muted')));
    const outBtn = el('button', {
      class: 'btn btn-icon btn-ghost btn-sm',
      type: 'button',
      title: '退出登录',
      attrs: { 'aria-label': '退出登录' },
      on: { click: async () => { await logout(); } },
    }, icon('logout', 15));
    chip.appendChild(outBtn);

    sidebarFoot.appendChild(el('div', { class: 'sidebar-user' },
      el('span', { class: 'sidebar-avatar', 'aria-hidden': 'true' }, icon('user', 16)),
      el('div', { class: 'sidebar-user-info' },
        el('span', { class: 'sidebar-username', text: auth.username }),
        el('span', { class: 'sidebar-role', text: roleLabel(auth.role) }),
      ),
      el('button', {
        class: 'btn btn-icon btn-ghost btn-sm',
        type: 'button',
        title: '退出登录',
        attrs: { 'aria-label': '退出登录' },
        on: { click: async () => { await logout(); } },
      }, icon('logout', 15)),
    ));
  } else {
    chip.appendChild(el('button', {
      class: 'btn btn-primary btn-sm login-btn',
      type: 'button',
      on: { click: () => openLoginModal() },
    }, icon('login', 14), '登录'));
    sidebarFoot.appendChild(el('div', { class: 'sidebar-guest' },
      el('div', { class: 'guest-info' },
        statusDot('muted', '只读浏览'),
        el('span', { class: 'sidebar-hint', text: '登录后可写配置与账号' }),
      ),
      el('button', {
        class: 'btn btn-ghost btn-sm btn-block',
        type: 'button',
        on: { click: () => openLoginModal() },
      }, icon('login', 14), '登录面板'),
    ));
  }

  if (isMock) {
    const mockBadge = qs('#mock-badge');
    if (mockBadge) {
      mockBadge.hidden = false;
      mockBadge.textContent = 'MOCK';
      mockBadge.title = '本地 fixtures：/panel/api/* 已被拦截';
    }
  }
  const buildNote = qs('#build-note');
  if (buildNote) {
    buildNote.textContent = auth.build ? `build ${auth.build}` : '';
    buildNote.title = auth.build || '';
  }
  // 登录/登出会改变可见页面；导航必须跟着会话重绘，否则会留下点进去只会
  // 显示「不可见」的入口。
  buildNav();
}

function setPageMeta(routeInfo, route) {
  const title = route?.title || '未找到';
  const subtitle = route?.subtitle || '';
  const titleNode = qs('#page-title');
  const subNode = qs('#page-subtitle');
  if (titleNode) titleNode.textContent = title;
  if (subNode) subNode.textContent = subtitle;
  document.title = `${route ? title : '页面不存在'} · OmniGate`;
  setActiveNav(routeInfo.path);
  closeMobileNav();
}

function closeMobileNav() {
  document.body.classList.remove('nav-open');
  const scrim = qs('#mobile-scrim');
  if (scrim) scrim.hidden = true;
  const btn = qs('#menu-btn');
  if (btn) btn.setAttribute('aria-expanded', 'false');
}

function openMobileNav() {
  document.body.classList.add('nav-open');
  const scrim = qs('#mobile-scrim');
  if (scrim) scrim.hidden = false;
  const btn = qs('#menu-btn');
  if (btn) btn.setAttribute('aria-expanded', 'true');
}

function toggleMobileNav() {
  if (document.body.classList.contains('nav-open')) closeMobileNav();
  else openMobileNav();
}

function wireShell() {
  qs('#theme-btn')?.addEventListener('click', cycleTheme);
  qs('#menu-btn')?.addEventListener('click', toggleMobileNav);
  qs('#mobile-scrim')?.addEventListener('click', closeMobileNav);
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      if (document.body.classList.contains('nav-open')) closeMobileNav();
    }
  });
  window.addEventListener('resize', () => {
    if (window.innerWidth > 900) closeMobileNav();
  });
  // 移动端点击导航后立刻收起抽屉。
  qs('#nav')?.addEventListener('click', (event) => {
    if (event.target instanceof Element && event.target.closest('a[data-route]')) closeMobileNav();
  });
  qs('#bottomnav')?.addEventListener('click', () => {
    window.scrollTo({ top: 0, behavior: 'smooth' });
  });
  authStore.subscribe(renderShellSession);
}

async function boot() {
  bootTheme();
  if (isMock) {
    const { installMock } = await import('./mock/api.js');
    installMock();
  }

  renderShellSession();
  wireShell();
  await refreshSession({ silent: true });

  let authSignature = `${authState().authenticated}|${authState().username}|${authState().role}`;
  window.addEventListener('og:auth-changed', () => {
    const auth = authState();
    const next = `${auth.authenticated}|${auth.username}|${auth.role}`;
    if (next === authSignature) return;
    authSignature = next;
    closeAllModals();
    router?.reload();
  });

  let unauthorizedHandling = false;
  window.addEventListener('og:unauthorized', async () => {
    if (unauthorizedHandling) return;
    unauthorizedHandling = true;
    const wasAuthed = authState().authenticated;
    await refreshSession({ silent: true });
    if (wasAuthed) toastWarn('会话已失效，请重新登录');
    setTimeout(() => { unauthorizedHandling = false; }, 1200);
  });

  router = createRouter({
    routes: ROUTES,
    outlet: qs('#view'),
    onNavigate: setPageMeta,
    context: {
      isMock,
      navigate: (path, opts) => router.navigate(path, opts),
    },
  });
  router.start();

  window.addEventListener('unhandledrejection', (event) => {
    if (event.reason?.name === 'AbortError') event.preventDefault();
  });
}

boot().catch((error) => {
  console.error('panel boot failed', error);
  const view = qs('#view');
  if (view) {
    view.replaceChildren(el('div', { class: 'page-error' },
      el('h1', { text: '面板启动失败' }),
      el('p', { text: String(error?.message || error) }),
      el('button', { class: 'btn btn-primary', type: 'button', on: { click: () => location.reload() } }, '重新加载'),
    ));
  }
  try { toastError(error); } catch { /* ignore */ }
});
