// store.js — 极轻量可订阅状态。页面不需要引入框架：createStore 足够。

export function createStore(initialState = {}) {
  let state = { ...initialState };
  const listeners = new Set();

  function get() {
    return state;
  }

  function set(patch) {
    const next = typeof patch === 'function' ? patch(state) : patch;
    if (next == null || next === state) return state;
    if (typeof next !== 'object') return state;
    state = { ...state, ...next };
    for (const listener of listeners) {
      try { listener(state); } catch (error) { console.error('store listener failed', error); }
    }
    return state;
  }

  function subscribe(fn, { immediate = false } = {}) {
    listeners.add(fn);
    if (immediate) fn(state);
    return () => listeners.delete(fn);
  }

  function reset(patch = {}) {
    state = { ...patch };
    for (const listener of listeners) listener(state);
    return state;
  }

  return { get, set, subscribe, reset, update: set };
}
