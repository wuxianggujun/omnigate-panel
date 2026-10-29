// fixtures.js — 本地预览数据：8 种供应商 + 空壳、多账号、多租户、失败请求。
// 其中 anyrouter 专门覆盖 Cookie 型中转站（AnyRouter / AgentRouter）：Claude Code
// 身份 + anthropic 方言 + sign_in 签到，两个账号分别是「UID 完整」与「待补全 UID」。
//
// 这里的数据刻意贴近真实后端形状：config 为完整 omnigate 配置、accounts 为
// {providers:[{name,type,accounts:[]}]}、models 为 OpenAI ModelList 形态。mock/api.js
// 负责鉴权、脱敏、过滤与状态变更；本文件只提供初始世界与生成器。

import { BUILTIN_PROVIDER_TYPES } from '../core/format.js';

export { BUILTIN_PROVIDER_TYPES as MOCK_PROVIDER_TYPES };

export const MOCK_USERS = [
  { username: 'admin', password: 'admin123', role: 'admin', display: '平台管理员' },
  { username: 'viewer', password: 'viewer123', role: 'viewer', display: '只读审计' },
  { username: 'alice', password: 'alice123', role: 'user', display: 'Alice' },
  { username: 'bob', password: 'bob123', role: 'user', display: 'Bob' },
];

export const MOCK_USER_BY_NAME = Object.fromEntries(MOCK_USERS.map((u) => [u.username, u]));

export const OWNERSHIP = {
  'raccoon/main': 'bob',
  'raccoon/backup': 'alice',
  'runable/team-alpha': 'alice',
  'runable/team-beta': 'bob',
  'trae/solo-1': 'bob',
  'justwoker/alice-lab': 'alice',
  'justwoker/ops-shared': 'bob',
  'anyrouter/anyuser': 'bob',
  'anyrouter/uidok': 'alice',
  'gemini/personal': 'alice',
};

const HOUR = 3600 * 1000;
const MIN = 60 * 1000;

function iso(ms) {
  return new Date(ms).toISOString();
}

function secretValue(prefix, n = 32) {
  return `${prefix}-${'0123456789abcdef'.repeat(8).slice(0, n)}`;
}

function baseConfig() {
  return {
    server: { host: '127.0.0.1', port: 8317 },
    data_dir: 'data/omnigate',
    default_provider: 'raccoon',
    default_model: 'raccoon/gpt-4o',
    mode: 'text',
    reasoning: false,
    model_whitelist: [],
    api_keys: [
      { key: 'sk-omni-prod-8f3c2a1d9b7e', name: '生产客户端', providers: [] },
      { key: 'sk-omni-raccoon-45a1f0c2', name: '浣熊专用', providers: ['raccoon', 'runable'] },
      { key: 'sk-omni-audit-7d9e2b0a', name: '只读探针', providers: ['opencode'] },
    ],
    credit_routing: {
      enabled: true,
      expiring_soon: '168h',
      min_expiring: 5,
      refresh_interval: '10m',
    },
    routing: {
      prefer: ['raccoon', 'runable', 'opencode', 'justwoker', 'anyrouter', 'trae', 'gemini'],
    },
    fingerprint: {
      jitter_enabled: false,
      version_sync_enabled: true,
      version_sync_interval_hours: 24,
      tls_enabled: true,
    },
    maintenance: {
      keepalive_enabled: false,
      keepalive_interval_minutes: 30,
      chat_cleanup_enabled: false,
      chat_cleanup_keep_hours: 24,
      chat_cleanup_interval_hours: 6,
      chat_cleanup_skip_pinned: true,
    },
    rate_limit: { enabled: false, window: 60, max: 10, trust_proxy: false },
    fallback: { enabled: true, models: ['gemini/gemini-2.5-pro', 'runable/zai/glm-5.3'] },
    checkin: {
      enabled: true,
      interval_minutes: 360,
      jitter_minutes: 30,
      tasks: [
        { name: 'raccoon-checkin', type: 'raccoon', provider: 'raccoon', interval_minutes: 720 },
        { name: 'runable-keepalive', type: 'runable', provider: 'runable', interval_minutes: 360 },
        { name: 'trae-checkin', type: 'trae', provider: 'trae', interval_minutes: 720 },
        { name: 'newapi-refresh', type: 'newapi-refresh', provider: 'justwoker', interval_minutes: 180 },
        { name: 'anyrouter-checkin', type: 'newapi', provider: 'anyrouter', interval_minutes: 1440 },
      ],
    },
    providers: [
      {
        name: 'raccoon',
        type: 'raccoon',
        main_origin: 'https://xiaohuanxiong.com',
        llm_base: 'https://xiaohuanxiong.com/api/llm',
        auth_base: 'https://xiaohuanxiong.com/api/auth',
        system_prompt: '',
        max_prompt_chars: 0,
        models: [],
        accounts: [
          {
            label: 'main',
            name: '主号',
            email: 'ops@example.com',
            cookie: secretValue('raccoon-access', 40),
            refresh_token: secretValue('raccoon-refresh', 40),
          },
          {
            label: 'backup',
            name: '备用号',
            email: 'backup@example.com',
            cookie: '',
            refresh_token: '',
          },
        ],
      },
      {
        name: 'runable',
        type: 'runable',
        base_url: 'https://api.runable.com',
        device_header: 'dweb/a69d4f72ddd711c660237adaf5c76287ac5473f5',
        system_prompt: '',
        max_prompt_chars: 120000,
        accounts: [
          { label: 'team-alpha', name: 'Team Alpha', cookie: secretValue('runable-session', 36), email: 'alice@example.com' },
          { label: 'team-beta', name: 'Team Beta', cookie: secretValue('runable-session', 36), email: 'bob@example.com' },
        ],
      },
      {
        name: 'deepseek',
        type: 'openai',
        base_url: 'https://api.deepseek.com/v1',
        api_key: secretValue('sk-deepseek', 40),
        api_keys: [secretValue('sk-deepseek-alt', 40)],
        models: ['deepseek-chat', 'deepseek-reasoner'],
        system_prompt: '你是严谨的工程助手，优先给出可执行的结论。',
        max_prompt_chars: 0,
        accounts: [],
      },
      {
        name: 'opencode',
        type: 'opencode',
        base_url: 'https://opencode.ai/zen/v1',
        tier_prefer: 'go',
        go_base_url: 'https://opencode.ai/zen/go',
        zen_keys: [secretValue('sk-zen', 36)],
        go_keys: [secretValue('sk-go', 36), secretValue('sk-go', 36)],
        anonymous: true,
        models: ['qwen3-coder', 'claude-sonnet-4'],
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [],
      },
      {
        name: 'trae',
        type: 'trae',
        agent_host: 'https://trae-api-cn.mchost.guru',
        ug_host: 'https://api.trae.cn',
        oauth_host: 'https://api.trae.com.cn',
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [
          {
            label: 'solo-1',
            name: 'SOLO 一号',
            cookie: secretValue('trae-access', 36),
            refresh_token: secretValue('trae-refresh', 36),
            uid: 'trae-user-1024',
            machine_id: 'machine-9a8b7c',
            device_id: 'device-6f5e4d',
            api_host: 'https://api.trae.cn',
          },
        ],
      },
      {
        name: 'justwoker',
        type: 'newapi',
        base_url: 'https://api.justwoker.example',
        api_key: '',
        api_keys: [secretValue('sk-newapi-pool', 36)],
        models: ['gpt-4.1', 'o3', 'glm-4.6'],
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [
          {
            label: 'alice-lab',
            name: 'Alice Lab',
            cookie: secretValue('newapi-session', 32),
            refresh_token: secretValue('newapi-refresh', 32),
            api_key: secretValue('sk-newapi-alice', 36),
            session_cookie: 'session=abc123',
          },
          {
            label: 'ops-shared',
            name: '运维共享号',
            cookie: secretValue('newapi-session', 32),
            refresh_token: '',
            api_key: secretValue('sk-newapi-ops', 36),
            session_cookie: 'session=def456',
          },
        ],
      },
      {
        // AnyRouter / AgentRouter：cookie 型站点。账号没有 access_token，用户接口
        // 认会话 Cookie + New-Api-User；其中一个账号缺数字 UID（可修），另一个完整。
        name: 'anyrouter',
        type: 'newapi',
        base_url: 'https://anyrouter.example',
        protocol: 'anthropic',
        client_preset: 'claude-code',
        auth_mode: 'cookie',
        user_header: 'New-Api-User',
        checkin_path: '/api/user/sign_in',
        signin_path: '/login',
        api_key: '',
        api_keys: [],
        models: ['claude-sonnet-4'],
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [
          {
            label: 'anyuser',
            name: 'Any User',
            cookie: '',
            refresh_token: '',
            api_key: secretValue('sk-any-chat', 36),
            session_cookie: 'session=any123',
            uid: '',
          },
          {
            label: 'uidok',
            name: 'UID 完整',
            cookie: '',
            refresh_token: '',
            api_key: secretValue('sk-any-chat2', 36),
            session_cookie: 'session=any456',
            uid: '42',
          },
        ],
      },
      {
        name: 'gemini',
        type: 'geminiweb',
        base_url: 'https://gemini.google.com',
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [
          { label: 'personal', name: '个人号', cookie: secretValue('gemini-cookie', 48) },
        ],
      },
      {
        name: 'deepseek-free',
        type: 'openai',
        base_url: 'https://api.deepseek.example/v1',
        api_key: '',
        api_keys: [],
        models: [],
        system_prompt: '',
        max_prompt_chars: 0,
        accounts: [],
      },
    ],
  };
}

function modelEntry(provider, name, {
  context = 128000,
  maxTokens = 8192,
  credits = null,
  creditUnit = 'multiplier',
  free = false,
  pricing = null,
  capabilities = ['chat', 'stream', 'tools'],
} = {}) {
  return {
    id: `${provider}/${name}`,
    object: 'model',
    created: 1735689600,
    owned_by: provider,
    context_window: context,
    max_tokens: maxTokens,
    ...(credits != null ? { credits, credit_unit: creditUnit } : {}),
    ...(free ? { is_free: true } : {}),
    ...(pricing ? { pricing } : {}),
    capabilities,
  };
}

export function buildModelCatalog(config = baseConfig()) {
  const list = [
    modelEntry('raccoon', 'gpt-4o', {
      context: 128000, maxTokens: 16384, credits: 1.15, pricing: { input: 0.0000025, output: 0.00001 },
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    }),
    modelEntry('raccoon', 'claude-sonnet-4.5', {
      context: 200000, maxTokens: 32000, credits: 1.4,
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning', 'code'],
    }),
    modelEntry('raccoon', 'gemini-2.5-pro', {
      context: 1048576, maxTokens: 65536, credits: 0.9,
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    }),
    modelEntry('raccoon', 'deepseek-v3.2', {
      context: 131072, maxTokens: 16384, credits: 0.55,
      capabilities: ['chat', 'stream', 'tools', 'reasoning', 'code'],
    }),
    modelEntry('runable', 'zai/glm-5.3', {
      context: 128000, maxTokens: 16384, credits: 7.2, creditUnit: 'credits_per_million_tokens',
      pricing: { input: 0.0000008, output: 0.0000032 },
      capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    }),
    modelEntry('runable', 'qwen3-max', {
      context: 262144, maxTokens: 32768, credits: 5.4, creditUnit: 'credits_per_million_tokens',
      capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    }),
    modelEntry('runable', 'kimi-k2', {
      context: 262144, maxTokens: 16384, credits: 4.1, creditUnit: 'credits_per_million_tokens',
      capabilities: ['chat', 'stream', 'tools'],
    }),
    modelEntry('runable', 'deepseek-v3', {
      context: 131072, maxTokens: 16384, credits: 2.6, creditUnit: 'credits_per_million_tokens',
      free: true,
      capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    }),
    modelEntry('deepseek', 'deepseek-chat', {
      context: 131072, maxTokens: 8192, pricing: { input: 0.00000027, output: 0.0000011 },
      capabilities: ['chat', 'stream', 'tools', 'code'],
    }),
    modelEntry('deepseek', 'deepseek-reasoner', {
      context: 131072, maxTokens: 32768, pricing: { input: 0.00000055, output: 0.0000022 },
      capabilities: ['chat', 'stream', 'reasoning', 'code'],
    }),
    modelEntry('opencode', 'qwen3-coder', {
      context: 262144, maxTokens: 32768, credits: 0.8, free: true,
      capabilities: ['chat', 'stream', 'tools', 'code'],
    }),
    modelEntry('opencode', 'claude-sonnet-4', {
      context: 200000, maxTokens: 32000, credits: 1.1,
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning', 'code'],
    }),
    modelEntry('opencode', 'gpt-5-codex', {
      context: 256000, maxTokens: 32768, credits: 1.25,
      capabilities: ['chat', 'stream', 'tools', 'reasoning', 'code'],
    }),
    modelEntry('trae', 'solo-coder', {
      context: 128000, maxTokens: 16384, credits: 3.5, creditUnit: 'credits_per_million_tokens',
      capabilities: ['chat', 'stream', 'tools', 'code'],
    }),
    modelEntry('trae', 'claude-3.7-sonnet', {
      context: 200000, maxTokens: 32000, credits: 6.8, creditUnit: 'credits_per_million_tokens',
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    }),
    modelEntry('justwoker', 'gpt-4.1', {
      context: 1048576, maxTokens: 32768, pricing: { input: 0.000002, output: 0.000008 },
      capabilities: ['chat', 'stream', 'tools', 'vision'],
    }),
    modelEntry('justwoker', 'o3', {
      context: 200000, maxTokens: 100000, pricing: { input: 0.000002, output: 0.000008 },
      capabilities: ['chat', 'stream', 'tools', 'vision', 'reasoning'],
    }),
    modelEntry('justwoker', 'glm-4.6', {
      context: 200000, maxTokens: 16384, pricing: { input: 0.0000006, output: 0.0000022 },
      capabilities: ['chat', 'stream', 'tools', 'reasoning'],
    }),
    modelEntry('anyrouter', 'claude-sonnet-4', {
      context: 200000, maxTokens: 64000,
      capabilities: ['chat', 'stream', 'tools', 'vision'],
    }),
    modelEntry('gemini', 'gemini-2.5-pro', {
      context: 1048576, maxTokens: 65536, free: true,
      capabilities: ['chat', 'stream', 'vision', 'reasoning'],
    }),
    modelEntry('gemini', 'gemini-2.5-flash', {
      context: 1048576, maxTokens: 65536, free: true,
      capabilities: ['chat', 'stream', 'vision'],
    }),
  ];
  // 配置里显式 models 覆盖（仅作存在性兜底，不改变价格目录）。
  const known = new Set(list.map((m) => m.id));
  for (const provider of config.providers || []) {
    for (const name of provider.models || []) {
      const id = `${provider.name}/${name}`;
      if (!known.has(id)) {
        list.push(modelEntry(provider.name, name));
        known.add(id);
      }
    }
  }
  return list;
}

export const MOCK_MAPPINGS = {
  'gpt-4o': ['raccoon/gpt-4o', 'justwoker/gpt-4.1'],
  claude: ['raccoon/claude-sonnet-4.5', 'runable/zai/glm-5.3'],
  fast: ['opencode/qwen3-coder', 'runable/zai/glm-5.3'],
  coder: ['opencode/gpt-5-codex', 'trae/solo-coder'],
  deepseek: ['deepseek/deepseek-chat'],
  // 与 deepseek 供应商自有模型同名的别名：用来演示「别名接管同名模型」的告警。
  'deepseek-chat': ['runable/deepseek-v3', 'deepseek/deepseek-chat'],
  'legacy-claude': ['runable/zai/glm-5.3'],
};

export const MOCK_DISABLED_MAPPINGS = { 'legacy-claude': true };

export const MOCK_OUTBOUND = {
  proxies: [
    {
      name: 'hk',
      url: 'http://127.0.0.1:7890',
      username: 'proxy-user',
      password: 'proxy-pass',
    },
    {
      name: 'sg-pool',
      pool: ['1.2.3.4:8000', '5.6.7.8:8000'],
      pool_scheme: 'http',
      refresh_sec: 300,
      probe_url: 'https://www.gstatic.com/generate_204',
      strict: false,
    },
  ],
  routes: {
    'omnigate:raccoon': 'hk',
    'omnigate:geminiweb': 'sg-pool',
    panel: 'hk',
  },
};

function runtimeMap(now) {
  return {
    'raccoon/main': {
      available: 486.5,
      credits_limit: 600,
      credits_used: 113.5,
      checkin_done: true,
      last_checkin: iso(now - 7 * HOUR),
      expires_at: iso(now + 21 * 24 * HOUR),
      user_id: 'raccoon-88231',
      org_name: 'Omni Ops',
    },
    'raccoon/backup': {
      auth_invalid: true,
      auth_error: 'access_token 已失效且自动刷新失败，请重新登录该账号',
      balance_error: '授权失效，未查询余额',
      last_error: '401 invalid_token',
    },
    'runable/team-alpha': {
      available: 322,
      wallets: [
        { type: 'monthly_credits', displayName: '每月额度', balance: 260 },
        { type: 'daily_credits', displayName: '每日额度', balance: 62 },
      ],
      checkin_done: true,
      last_checkin: iso(now - 3 * HOUR),
      expires_at: iso(now + 2 * HOUR),
      session_verified_at: iso(now - 3 * HOUR),
    },
    'runable/team-beta': {
      available: 148,
      wallets: [
        { type: 'monthly_credits', displayName: '每月额度', balance: 120 },
        { type: 'daily_credits', displayName: '每日额度', balance: 28 },
      ],
      credits_expiring: 28,
      credits_earliest_expiry: iso(now + 26 * HOUR),
      session_verified_at: iso(now - 5 * HOUR),
    },
    'trae/solo-1': {
      available: null,
      balance_error: '上游额度查询超时（20s）',
      credits_limit: 500,
      credits_used: 136,
      expires_at: iso(now + 9 * 24 * HOUR),
      user_id: 'trae-user-1024',
      org_name: 'SOLO Team',
    },
    'justwoker/alice-lab': {
      available: 72.4,
      used: 27.6,
      group: 'default',
      name: 'Alice Lab',
      expires_at: iso(now + 11 * 24 * HOUR),
    },
    'justwoker/ops-shared': {
      available: 15.2,
      used: 84.8,
      group: 'shared',
      cooling_until: iso(now + 8 * MIN),
      last_error: '429 rate limited（上游要求 30s 后重试）',
    },
    // Cookie 型中转站（AnyRouter / AgentRouter）账号缺数字用户 ID：还能聊天，
    // 只是签到与额度用不了。用来验证面板把它渲染成「待补全 UID」而不是「失效」。
    'anyrouter/anyuser': {
      available: null,
      user_id: '',
      uid_missing: true,
      uid_error: '缺少数字用户 ID（New-Api-User）：请在面板重新抓取一次该账号的会话 Cookie',
      balance_error: '缺少数字用户 ID（New-Api-User）：请在面板重新抓取一次该账号的会话 Cookie',
    },
    'anyrouter/uidok': {
      available: 128.5,
      used: 21.5,
      group: 'default',
      name: 'UID 完整',
      user_id: '42',
    },
    'gemini/personal': {
      available: null,
      auth_invalid: true,
      auth_error: 'Gemini 会话已过期，请更新 Cookie',
      session_expired_at: iso(now - 20 * HOUR),
      session_error: 'CheckAccount: 401 unauthorized',
      last_error: '401 unauthorized',
    },
  };
}

function keyHealth(now) {
  return {
    providers: [
      {
        name: 'deepseek',
        keys: [
          { key: 'sk-deep…4f2a…(len=40)', disabled: false, strikes: 0 },
          { key: 'sk-deep…9c1d…(len=40)', disabled: true, reason: 'quota', strikes: 5, until: Math.floor((now + 2 * HOUR) / 1000), first_seen: Math.floor((now - 2 * 24 * HOUR) / 1000), cleanup_at: Math.floor((now + 5 * 24 * HOUR) / 1000), last_error: 'Token quota exhausted.' },
        ],
      },
      {
        name: 'opencode',
        keys: [
          { key: 'sk-go-…a1b2…(len=36)', disabled: false, strikes: 1, last_error: 'HTTP 429 (已恢复)' },
          { key: 'sk-go-…c3d4…(len=36)', disabled: false, strikes: 0 },
          { key: 'sk-zen…e5f6…(len=36)', disabled: false, strikes: 0 },
        ],
      },
    ],
    cleanup_strikes: 5,
    cleanup_days: 3,
    // 与 /healthz route_cooldowns 同形：provider 级额度窗口用 model:*，
    // 普通失败只影响具体模型。时间用秒，fmtRelative 与后端 RFC3339 都能解析。
    route_cooldowns: [
      { provider: 'deepseek', model: '*', scope: 'provider', until: Math.floor((now + 2 * HOUR) / 1000), strikes: 3, reason: 'weekly limit; resets in 2h' },
      { provider: 'opencode', model: 'gpt-5-codex', scope: 'model', until: Math.floor((now + 30 * MIN) / 1000), strikes: 2, reason: 'HTTP 500 upstream error' },
    ],
  };
}

function quota(now) {
  return {
    provider: 'opencode',
    checked_at: iso(now),
    keys: [
      {
        fingerprint: 'a1b2c3d4',
        masked: 'sk-go-…a1b2…(len=36)',
        status: 'ok',
        fetched_at: iso(now - 2 * MIN),
        checked_at: iso(now),
        windows: {
          '5h': { status: 'ok', used_percent: 42, remaining_percent: 58, resets_at: iso(now + 2.4 * HOUR) },
          weekly: { status: 'ok', used_percent: 61, remaining_percent: 39, resets_at: iso(now + 3 * 24 * HOUR) },
        },
      },
      {
        fingerprint: 'c3d4e5f6',
        masked: 'sk-go-…c3d4…(len=36)',
        status: 'stale',
        error: '额度接口超时，展示上次成功结果',
        fetched_at: iso(now - 40 * MIN),
        checked_at: iso(now),
        windows: {
          '5h': { status: 'ok', used_percent: 18, remaining_percent: 82, resets_at: iso(now + 4 * HOUR) },
        },
      },
      {
        fingerprint: 'e5f6a7b8',
        masked: 'sk-zen…e5f6…(len=36)',
        status: 'unavailable',
        error: '该密钥未开通 Go 套餐',
        checked_at: iso(now),
      },
    ],
  };
}

function logsFor(now) {
  const rows = [
    { t: now - 26 * HOUR, channel: 'sys', text: '[scheduler] 加载签到任务 4 个；抖动窗口 ±30 分钟' },
    { t: now - 25.6 * HOUR, channel: 'sys', text: '[omnigate] 已启用：默认 provider=raccoon，provider 数=8' },
    { t: now - 24.2 * HOUR, channel: 'task', text: '[checkin] raccoon/main 签到成功，获得 5 积分（今日已领）' },
    { t: now - 23.8 * HOUR, channel: 'task', text: '[checkin] runable/team-alpha 会话有效，积分 322（月 260 / 日 62）' },
    { t: now - 22.1 * HOUR, channel: 'chat', text: '[chat] raccoon/main model=raccoon/gpt-4o status=200 duration=1.8s tokens=1840' },
    { t: now - 21.7 * HOUR, channel: 'chat', text: '[chat] runable/team-alpha model=runable/zai/glm-5.3 status=200 duration=2.4s tokens=920' },
    { t: now - 20.4 * HOUR, channel: 'sys', text: '[keyhealth] deepseek 密钥池第 2 把连续 5 次失败，已自动禁用并进入清理倒计时' },
    { t: now - 19.9 * HOUR, channel: 'chat', text: '[chat] justwoker/ops-shared model=justwoker/o3 status=429 outcome=rate_limited 上游要求 30s 后重试' },
    { t: now - 18.2 * HOUR, channel: 'task', text: '[checkin] newapi/justwoker 自动刷新：1 个账号成功，1 个跳过（冷却中）' },
    { t: now - 17.5 * HOUR, channel: 'chat', text: '[chat] gemini/personal model=gemini/gemini-2.5-pro status=401 outcome=http_error 会话已过期' },
    { t: now - 16.3 * HOUR, channel: 'sys', text: '[outbound] 出站代理 hk 探活成功（latency 42ms），sg-pool 可用 2/2' },
    { t: now - 15.2 * HOUR, channel: 'chat', text: '[chat] opencode/— model=opencode/qwen3-coder status=200 duration=1.1s tokens=2310' },
    { t: now - 14.6 * HOUR, channel: 'task', text: '[checkin] trae/solo-1 签到成功，连续签到 12 天' },
    { t: now - 13.9 * HOUR, channel: 'chat', text: '[chat] trae/solo-1 model=trae/solo-coder status=502 outcome=http_error 上游返回 502 Bad Gateway，已尝试下一账号' },
    { t: now - 12.8 * HOUR, channel: 'sys', text: '[usage] 用量分片已落盘（30s 防抖），当前窗口 24h' },
    { t: now - 11.2 * HOUR, channel: 'chat', text: '[chat] deepseek/— model=deepseek/deepseek-chat status=200 duration=760ms tokens=640' },
    { t: now - 10.5 * HOUR, channel: 'chat', text: '[chat] raccoon/main model=raccoon/claude-sonnet-4.5 status=200 duration=3.2s tokens=4820' },
    { t: now - 9.4 * HOUR, channel: 'sys', text: '[modelmap] 全局映射已加载 6 条，其中 1 条停用' },
    { t: now - 8.1 * HOUR, channel: 'task', text: '[checkin] runable/team-beta 每日额度刷新：148 积分' },
    { t: now - 6.7 * HOUR, channel: 'chat', text: '[chat] runable/team-beta model=runable/kimi-k2 status=200 duration=1.7s tokens=1280' },
    { t: now - 5.2 * HOUR, channel: 'chat', text: '[chat] opencode/— model=opencode/gpt-5-codex status=200 duration=4.1s tokens=6540' },
    { t: now - 3.9 * HOUR, channel: 'sys', text: '[panel] admin 更新了路由优先级：raccoon > runable > opencode' },
    { t: now - 2.6 * HOUR, channel: 'chat', text: '[chat] raccoon/main model=raccoon/gemini-2.5-pro status=200 duration=2.9s tokens=3120' },
    { t: now - 1.4 * HOUR, channel: 'chat', text: '[chat] justwoker/alice-lab model=justwoker/gpt-4.1 status=200 duration=2.0s tokens=2240' },
    { t: now - 22 * MIN, channel: 'sys', text: '[keyhealth] deepseek 密钥 1 恢复：连败计数清零' },
    { t: now - 9 * MIN, channel: 'chat', text: '[chat] runable/team-alpha model=runable/qwen3-max status=499 outcome=interrupted 客户端提前断开' },
    { t: now - 3 * MIN, channel: 'sys', text: '[omnigate] 余额快照刷新完成：7 个账号，2 个失败' },
  ];
  return rows.map((row) => ({
    time: iso(row.t),
    channel: row.channel,
    text: row.text,
  }));
}

function pick(rng, list) {
  return list[Math.floor(rng() * list.length)];
}

function rngFrom(seed) {
  let a = seed >>> 0;
  return () => {
    a += 0x6D2B79F5;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function chatAccountsFor(config) {
  const out = [];
  for (const provider of config.providers || []) {
    for (const account of provider.accounts || []) {
      out.push({ provider: provider.name, type: provider.type, label: account.label, name: account.name || account.label });
    }
  }
  return out;
}

export function buildRequestLogs(state, limit = 220) {
  const rng = rngFrom(0x9e3779b9);
  const now = state.now;
  const models = buildModelCatalog(state.config)
    .filter((m) => m.id.includes('/') && !['legacy-claude'].includes(m.id));
  const accounts = chatAccountsFor(state.config);
  const byProvider = new Map();
  for (const m of models) {
    const prov = m.id.split('/')[0];
    if (!byProvider.has(prov)) byProvider.set(prov, []);
    byProvider.get(prov).push(m.id.split('/').slice(1).join('/'));
  }
  const providers = Array.from(byProvider.keys());
  const clients = ['10.8.0.14', '192.168.1.22', '172.16.3.90', '10.8.0.31', '203.0.113.8'];
  // 入站密钥标签：mock 里的「用户」页需要它与真实后端同名字段（api_key），
  // 否则 ?mock=1 下按用户统计永远是空表，等于没验证。
  const keyNames = (state.config.api_keys || [])
    .map((k) => (typeof k === 'string' ? '' : String(k.name || '').trim()))
    .filter(Boolean);
  const agents = [
    'OpenAI-Python/1.66.0',
    'openai-node/4.68.0',
    'Claude-Code/2.1.0',
    'curl/8.9.1',
    'OmniGate-Web/1.0',
  ];
  const statuses = [
    { ok: true, status: 200, outcome: 'success', weight: 0.84 },
    { ok: false, status: 429, outcome: 'rate_limited', weight: 0.045 },
    { ok: false, status: 502, outcome: 'http_error', weight: 0.05 },
    { ok: false, status: 401, outcome: 'http_error', weight: 0.025 },
    // interrupted 带具体成因：面板要能显示「谁先断的」。线上排查时只有 outcome
    // 没有原因，根本分不清是客户端主动取消还是中间层按读超时掐断。
    {
      ok: false, status: 499, outcome: 'interrupted', weight: 0.04,
      failure: 'client canceled the request (context canceled)',
    },
  ];
  const rows = [];
  for (let i = 0; i < limit; i += 1) {
    const provider = pick(rng, providers);
    const providerAccounts = accounts.filter((a) => a.provider === provider);
    const account = providerAccounts.length ? pick(rng, providerAccounts) : null;
    const model = `${provider}/${pick(rng, byProvider.get(provider))}`;
    const roll = rng();
    let acc = 0;
    let outcome = statuses[0];
    for (const candidate of statuses) {
      acc += candidate.weight;
      if (roll <= acc) { outcome = candidate; break; }
    }
    const base = now - Math.floor(rng() * 24 * HOUR);
    const pt = Math.floor(120 + rng() * 2600);
    const ct = Math.floor(40 + rng() * 1800);
    const duration = Math.floor(400 + rng() * 7200);
    const row = {
      time: iso(base),
      request_id: `req_${(0xa0000 + i).toString(16)}${Math.floor(rng() * 0xffff).toString(16).padStart(4, '0')}`,
      path: rng() > 0.18 ? '/v1/chat/completions' : '/v1/responses',
      provider,
      account: account?.label || '',
      account_name: account?.name || '',
      // 入站密钥标签：与后端 request_logs.api_key 同名列。
      api_key: keyNames.length ? pick(rng, keyNames) : '',
      model,
      realm: 'cn',
      status: outcome.status,
      ok: outcome.ok,
      outcome: outcome.outcome,
      // 失败原因（interrupted 的行才有）：面板详情直接显示「谁先断的」。
      ...(outcome.failure ? { failure: outcome.failure } : {}),
      duration_ms: duration,
      ttfb_ms: Math.max(80, Math.floor(duration * (0.15 + rng() * 0.4))),
      attempts: outcome.ok ? 1 : 1 + Math.floor(rng() * 2),
      prompt_tokens: outcome.status === 499 ? 0 : pt,
      completion_tokens: outcome.status === 499 ? 0 : ct,
      total_tokens: outcome.status === 499 ? 0 : pt + ct,
      credit: outcome.ok || outcome.status === 502 ? Number((pt * 0.00045 + ct * 0.0012).toFixed(3)) : 0,
      credit_known: outcome.ok,
      reasoning_effort: rng() > 0.7 ? pick(rng, ['low', 'medium', 'high']) : '',
      reasoning_tokens: rng() > 0.75 ? Math.floor(rng() * 800) : 0,
      reasoning: rng() > 0.75,
      client_ip: pick(rng, clients),
      user_agent: pick(rng, agents),
      cache_hit_tokens: Math.floor(rng() * pt * 0.4),
      cache_miss_tokens: pt,
    };
    // upstream_model：让 mock 也能演示「模型 / 重定向」列与详情里的重定向区块。
    // 只对一小部分行模拟「模型本体被改写」（别名 / 目录补全），其余保持同一个
    // 模型——真实环境里大多数请求都不发生改写，全部标成重定向反而看不出重点。
    const bareModel = model.split('/').slice(1).join('/');
    row.upstream_model = rng() > 0.88 ? `${bareModel}-1017` : bareModel;
    if (!outcome.ok) {
      row.error = outcome.status === 429
        ? 'upstream rate limited: retry after 30s'
        : outcome.status === 401
          ? 'unauthorized: access token expired'
          : outcome.status === 499
            ? 'client closed request before upstream finished'
            : 'upstream returned 502 Bad Gateway';
    }
    rows.push(row);
  }
  rows.sort((a, b) => new Date(b.time) - new Date(a.time));
  return rows;
}

export function buildKeyUsage(state, rows) {
  const acc = new Map();
  for (const row of rows) {
    const key = String(row.api_key || '').trim();
    // 与后端口径一致：没有入站密钥的历史行不参与聚合。
    if (!key) continue;
    if (!acc.has(key)) {
      acc.set(key, {
        key, requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0,
        total_tokens: 0, duration_ms: 0, last_seen: '', models: new Map(),
      });
    }
    const it = acc.get(key);
    it.requests += 1;
    it.errors += row.ok ? 0 : 1;
    it.prompt_tokens += Number(row.prompt_tokens || 0);
    it.completion_tokens += Number(row.completion_tokens || 0);
    it.total_tokens += Number(row.total_tokens || 0);
    it.duration_ms += Number(row.duration_ms || 0);
    if (!it.last_seen || new Date(row.time) > new Date(it.last_seen)) it.last_seen = row.time;
    const model = String(row.model || '');
    if (!it.models.has(model)) it.models.set(model, { model, requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 });
    const m = it.models.get(model);
    m.requests += 1;
    m.errors += row.ok ? 0 : 1;
    m.prompt_tokens += Number(row.prompt_tokens || 0);
    m.completion_tokens += Number(row.completion_tokens || 0);
    m.total_tokens += Number(row.total_tokens || 0);
  }

  // 与后端 /usage/by-key 的 configured_keys 同口径：同名多把密钥合并成一行，
  // 范围取并集；只要有一把是全量，这个用户就是全量。
  const accessByName = new Map();
  for (const raw of state.config.api_keys || []) {
    if (!raw || typeof raw !== 'object') continue;
    const name = String(raw.name || '').trim();
    if (!name) continue;
    const provs = Array.isArray(raw.providers)
      ? raw.providers.map((p) => String(p || '').trim()).filter(Boolean)
      : [];
    const entry = accessByName.get(name) || {
      name, providers: [], mixed: false, full: false,
    };
    if (!provs.length) {
      if (entry.providers.length) entry.mixed = true;
      entry.full = true;
    } else if (entry.full) {
      entry.mixed = true;
    } else {
      if (entry.providers.length && !sameStringSet(entry.providers, provs)) {
        entry.mixed = true;
      }
      for (const p of provs) {
        if (!entry.providers.includes(p)) entry.providers.push(p);
      }
    }
    accessByName.set(name, entry);
  }
  const configuredKeys = Array.from(accessByName.values())
    .map((entry) => ({
      name: entry.name,
      ...(entry.full ? {} : { providers: entry.providers }),
      ...(entry.mixed ? { mixed: true } : {}),
    }))
    .sort((a, b) => a.name.localeCompare(b.name, 'zh-CN'));
  const configured = configuredKeys.map((k) => k.name);

  const list = Array.from(acc.values())
    .map((it) => ({
      key: it.key,
      requests: it.requests,
      errors: it.errors,
      prompt_tokens: it.prompt_tokens,
      completion_tokens: it.completion_tokens,
      total_tokens: it.total_tokens,
      avg_duration_ms: it.requests ? Math.round(it.duration_ms / it.requests) : 0,
      last_seen: it.last_seen,
      models: Array.from(it.models.values()).sort((a, b) => b.requests - a.requests),
    }))
    .sort((a, b) => b.requests - a.requests);

  // 已配置但窗口内没有流量的密钥也返回（与真实后端一致），前端排在最后。
  for (const name of configured) {
    if (!list.some((it) => it.key === name)) {
      list.push({
        key: name, requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0,
        // 与真实后端一致：time.Time 零值序列化为 0001-01-01T00:00:00Z，
        // 用户页必须把它当「无记录」而不是公元 1 年。
        total_tokens: 0, avg_duration_ms: 0, last_seen: '0001-01-01T00:00:00Z', models: [],
      });
    }
  }

  const total = list.reduce((sum, it) => ({
    requests: sum.requests + it.requests,
    errors: sum.errors + it.errors,
    prompt_tokens: sum.prompt_tokens + it.prompt_tokens,
    completion_tokens: sum.completion_tokens + it.completion_tokens,
    total_tokens: sum.total_tokens + it.total_tokens,
  }), { requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0 });

  return {
    from: new Date(state.now - 7 * 24 * HOUR).toISOString(),
    to: new Date(state.now).toISOString(),
    rows: list,
    total,
    retention_days: 7,
    configured,
    configured_keys: configuredKeys,
  };
}

function sameStringSet(a, b) {
  return a.length === b.length && a.every((item) => b.includes(item));
}

export function buildUsageSnapshot(state, { hours = 24, accounts = null, providers = null } = {}) {
  const now = state.now;
  const visibleAccounts = accounts || [];
  if (Array.isArray(accounts) && accounts.length === 0) {
    return {
      ok: true,
      totals: {
        requests: 0, errors: 0, interrupted_requests: 0, prompt_tokens: 0, completion_tokens: 0,
        total_tokens: 0, credits: 0, credit_samples: 0, credit_tokens: 0, credits_per_1m_tokens: 0,
        cache_hit_tokens: 0, cache_miss_tokens: 0, cache_hit_rate: 0, avg_latency_ms: 0, avg_tokens_per_second: 0,
      },
      by_realm: [], by_provider: [], by_account: [], by_model: [], credit_by_account: [], credit_by_model: [],
      series: [], buckets: 0, file_bytes: 0, window_from: new Date(now - hours * HOUR).toISOString(),
      window_to: new Date(now).toISOString(), generated: new Date(now).toISOString(), hours,
      storage: {
        config_path: 'config.json', data_dir: state.config.data_dir, usage_file_bytes: 0,
        request_archive_files: 0, request_archive_bytes: 0, dropped_writes: 0,
      },
    };
  }
  const rng = rngFrom(0x51ed270b + Math.floor(hours));
  const stepH = hours <= 48 ? 1 : hours <= 192 ? 3 : hours <= 744 ? 12 : 24;
  const points = Math.max(2, Math.min(120, Math.floor(hours / stepH)));
  const end = new Date(Math.floor(now / HOUR) * HOUR);
  const allowedProviders = Array.isArray(providers) ? new Set(providers) : null;
  const models = buildModelCatalog(state.config)
    .filter((m) => !m.id.startsWith('deepseek-free/'))
    .filter((m) => !allowedProviders || allowedProviders.has(m.owned_by));
  const activeAccounts = visibleAccounts.filter((a) => a.has_token !== false && !a.auth_invalid);
  const providerNames = Array.from(new Set((state.config.providers || []).map((p) => p.name)))
    .filter((name) => !allowedProviders || allowedProviders.has(name));
  const accountRows = activeAccounts.length ? activeAccounts : [];
  const modelRows = models.length ? models : buildModelCatalog(state.config).filter((m) => !m.id.startsWith('deepseek-free/'));
  const series = [];
  const byAccount = new Map();
  const byProvider = new Map();
  const byModel = new Map();
  let totalReq = 0;
  let totalErr = 0;
  let totalPT = 0;
  let totalCT = 0;
  let totalCR = 0;
  let latSum = 0;
  let latN = 0;
  let cacheHit = 0;
  let cacheMiss = 0;

  for (let i = points - 1; i >= 0; i -= 1) {
    const t = new Date(end.getTime() - i * stepH * HOUR);
    const hour = t.getHours();
    const curve = 0.55 + 0.45 * Math.sin(((hour + 2) / 24) * Math.PI * 2);
    const req = Math.max(1, Math.floor((30 + rng() * 90) * curve * (hours > 48 ? 2.4 : 1)));
    const err = Math.floor(req * (0.02 + rng() * 0.04));
    const pt = req * Math.floor(380 + rng() * 900);
    const ct = req * Math.floor(120 + rng() * 500);
    const cr = Number((req * (0.008 + rng() * 0.012)).toFixed(3));
    const lat = 900 + rng() * 2100;
    const point = {
      t: t.toISOString(),
      scope: stepH >= 24 ? 'day' : 'hour',
      requests: req,
      errors: err,
      prompt_tokens: pt,
      completion_tokens: ct,
      total_tokens: pt + ct,
      credits: cr,
      credit_samples: req,
      credit_tokens: pt + ct,
      avg_latency_ms: Math.round(lat),
      avg_tokens_per_second: Math.round(24 + rng() * 40),
      cache_hit_tokens: Math.floor(pt * (0.18 + rng() * 0.24)),
      cache_miss_tokens: pt,
    };
    series.push(point);
    totalReq += req;
    totalErr += err;
    totalPT += pt;
    totalCT += ct;
    totalCR += cr;
    latSum += lat * req;
    latN += req;
    cacheHit += point.cache_hit_tokens;
    cacheMiss += point.cache_miss_tokens;

    const providerWeights = providerNames.map((name) => ({ name, w: name === 'raccoon' ? 3.2 : name === 'runable' ? 2.1 : name === 'opencode' ? 1.5 : 1 }));
    const weightSum = providerWeights.reduce((s, x) => s + x.w, 0);
    for (const p of providerWeights) {
      const share = p.w / weightSum;
      const providerModels = modelRows.filter((m) => m.owned_by === p.name);
      if (!providerModels.length) continue;
      const model = pick(rng, providerModels);
      const providerRow = byProvider.get(p.name) || { key: p.name, requests: 0, errors: 0, total_tokens: 0, credits: 0, latencySum: 0, latencyN: 0 };
      const pReq = Math.max(0, Math.round(req * share));
      const pErr = Math.round(err * share);
      const pTok = Math.round((pt + ct) * share);
      const pCr = Number((cr * share).toFixed(3));
      providerRow.requests += pReq;
      providerRow.errors += pErr;
      providerRow.total_tokens += pTok;
      providerRow.credits += pCr;
      providerRow.latencySum += lat * pReq;
      providerRow.latencyN += pReq;
      byProvider.set(p.name, providerRow);

      const modelRow = byModel.get(model.id) || { key: model.id, realm: 'cn', requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, credits: 0 };
      modelRow.requests += pReq;
      modelRow.errors += pErr;
      modelRow.prompt_tokens += Math.round(pt * share);
      modelRow.completion_tokens += Math.round(ct * share);
      modelRow.total_tokens += pTok;
      modelRow.credits += pCr;
      byModel.set(model.id, modelRow);

      const candidates = accountRows.filter((a) => a.provider === p.name);
      if (candidates.length) {
        const a = pick(rng, candidates);
        const key = `${a.provider}/${a.label}`;
        const aRow = byAccount.get(key) || { key, realm: 'cn', extra: a.name || a.label, requests: 0, errors: 0, prompt_tokens: 0, completion_tokens: 0, total_tokens: 0, credits: 0, latencySum: 0, latencyN: 0 };
        const aShare = 1 / Math.max(1, candidates.length) + rng() * 0.15;
        const aReq = Math.max(0, Math.round(pReq * Math.min(0.9, aShare)));
        aRow.requests += aReq;
        aRow.errors += Math.round(pErr * Math.min(1, aShare));
        aRow.prompt_tokens += Math.round(pt * share * aShare);
        aRow.completion_tokens += Math.round(ct * share * aShare);
        aRow.total_tokens += Math.round(pTok * aShare);
        aRow.credits += Number((pCr * aShare).toFixed(3));
        aRow.latencySum += lat * aReq;
        aRow.latencyN += aReq;
        byAccount.set(key, aRow);
      }
    }
  }

  const finalize = (row) => {
    const { latencySum, latencyN, ...rest } = row;
    return {
      ...rest,
      avg_latency_ms: latencyN ? Math.round(latencySum / latencyN) : 0,
      credit_samples: rest.requests || 0,
      credit_tokens: rest.total_tokens || 0,
      credits_per_1m_tokens: rest.total_tokens ? Number(((rest.credits / rest.total_tokens) * 1e6).toFixed(2)) : 0,
      cache_hit_rate: rest.total_tokens ? Number(((rest.cache_hit_tokens || 0) / Math.max(1, rest.total_tokens)) * 100).toFixed(1) : 0,
    };
  };

  const from = new Date(now - hours * HOUR);
  return {
    ok: true,
    totals: {
      requests: totalReq,
      errors: totalErr,
      interrupted_requests: Math.round(totalErr * 0.25),
      prompt_tokens: totalPT,
      completion_tokens: totalCT,
      total_tokens: totalPT + totalCT,
      credits: Number(totalCR.toFixed(3)),
      credit_samples: totalReq,
      credit_tokens: totalPT + totalCT,
      credits_per_1m_tokens: totalPT + totalCT ? Number(((totalCR / (totalPT + totalCT)) * 1e6).toFixed(2)) : 0,
      cache_hit_tokens: cacheHit,
      cache_miss_tokens: cacheMiss,
      cache_hit_rate: cacheHit + cacheMiss ? Number(((cacheHit / (cacheHit + cacheMiss)) * 100).toFixed(1)) : 0,
      avg_latency_ms: latN ? Math.round(latSum / latN) : 0,
      avg_tokens_per_second: 36.8,
    },
    by_realm: [{ key: 'cn', requests: totalReq, errors: totalErr, total_tokens: totalPT + totalCT, credits: Number(totalCR.toFixed(3)) }],
    by_provider: Array.from(byProvider.values()).map(finalize).sort((a, b) => b.requests - a.requests),
    by_account: Array.from(byAccount.values()).map(finalize).sort((a, b) => b.requests - a.requests),
    by_model: Array.from(byModel.values()).map(finalize).sort((a, b) => b.requests - a.requests),
    credit_by_account: Array.from(byAccount.values()).map((row) => ({
      key: row.key, nickname: row.extra, requests: row.requests, credits: row.credits,
      credit_samples: row.requests, credit_tokens: row.total_tokens,
      credits_per_1m_tokens: row.total_tokens ? Number(((row.credits / row.total_tokens) * 1e6).toFixed(2)) : 0,
    })),
    credit_by_model: Array.from(byModel.values()).map((row) => ({
      key: row.key.split('/').slice(1).join('/'), requests: row.requests, credits: row.credits,
      credit_samples: row.requests, credit_tokens: row.total_tokens,
      credits_per_1m_tokens: row.total_tokens ? Number(((row.credits / row.total_tokens) * 1e6).toFixed(2)) : 0,
    })),
    series,
    buckets: series.length,
    file_bytes: 284672,
    since: iso(now - 31 * 24 * HOUR),
    window_from: from.toISOString(),
    window_to: new Date(now).toISOString(),
    generated: new Date(now).toISOString(),
    hours,
    storage: {
      config_path: 'config.json',
      data_dir: state.config.data_dir,
      usage_file_bytes: 284672,
      request_archive_files: 14,
      request_archive_bytes: 8263512,
      dropped_writes: 0,
    },
  };
}

export function createMockState() {
  const now = Date.now();
  return {
    now,
    allowRegistration: true,
    sessionHours: 72,
    maxFailures: 5,
    lockMinutes: 15,
    config: baseConfig(),
    runtime: runtimeMap(now),
    ownership: { ...OWNERSHIP },
    mappings: JSON.parse(JSON.stringify(MOCK_MAPPINGS)),
    disabledMappings: { ...MOCK_DISABLED_MAPPINGS },
    outbound: JSON.parse(JSON.stringify(MOCK_OUTBOUND)),
    keyHealth: keyHealth(now),
    quota: quota(now),
    logs: logsFor(now),
    requestLogs: [],
    users: MOCK_USERS.map((u) => ({ ...u })),
    sessionUser: null,
  };
}

export function buildAccountsPayload(state, { withBalance = false, role = '', username = '' } = {}) {
  const providers = [];
  for (const provider of state.config.providers || []) {
    const accounts = [];
    for (const raw of provider.accounts || []) {
      const key = `${provider.name}/${raw.label}`;
      const runtime = state.runtime[key] || {};
      if (role === 'user' && state.ownership[key] !== username) continue;
      if (role === '' || role === null) continue; // 匿名不返回账号明细
      const row = {
        label: raw.label,
        name: runtime.name || raw.name || '',
        // 与后端一致：Cookie 型站点（AnyRouter / AgentRouter）没有 access_token，
        // 会话 Cookie 就是用户接口凭据，只看 cookie 会误判成「未配置凭证」。
        has_token: !!(raw.cookie || raw.session_cookie),
        ...(runtime.available != null ? { available: runtime.available } : {}),
        ...(runtime.credits != null ? { credits: runtime.credits } : {}),
        ...(runtime.credits_limit != null ? { credits_limit: runtime.credits_limit } : {}),
        ...(runtime.credits_used != null ? { credits_used: runtime.credits_used } : {}),
        ...(runtime.wallets ? { wallets: runtime.wallets } : {}),
        ...(runtime.expires_at ? { expires_at: runtime.expires_at } : {}),
        ...(runtime.user_id ? { user_id: runtime.user_id } : {}),
        ...(runtime.org_name ? { org_name: runtime.org_name } : {}),
        ...(runtime.auth_invalid ? { auth_invalid: true } : {}),
        ...(runtime.auth_error ? { auth_error: runtime.auth_error } : {}),
        ...(runtime.uid_missing ? { uid_missing: true } : {}),
        ...(runtime.uid_error ? { uid_error: runtime.uid_error } : {}),
        ...(runtime.balance_error ? { balance_error: runtime.balance_error } : {}),
        ...(runtime.session_error ? { session_error: runtime.session_error } : {}),
        ...(runtime.session_expired_at ? { session_expired_at: runtime.session_expired_at } : {}),
        ...(runtime.session_verified_at ? { session_verified_at: runtime.session_verified_at } : {}),
        ...(runtime.cooling_until ? { cooling_until: runtime.cooling_until } : {}),
        ...(runtime.last_error ? { last_error: runtime.last_error } : {}),
        ...(runtime.credits_expiring ? { credits_expiring: runtime.credits_expiring } : {}),
        ...(runtime.credits_earliest_expiry ? { credits_earliest_expiry: runtime.credits_earliest_expiry } : {}),
        ...(runtime.group ? { group: runtime.group } : {}),
        ...(runtime.used != null ? { used: runtime.used } : {}),
        ...(runtime.checkin_done ? { checkin_done: true } : {}),
        ...(runtime.last_checkin ? { last_checkin: runtime.last_checkin } : {}),
        ...(withBalance ? { balance_checked: true } : {}),
        ...(provider.type === 'newapi' ? { has_api_key: !!raw.api_key } : {}),
      };
      accounts.push(row);
    }
    // 与后端一致：账号接口同时给出该供应商的上游密钥组数，这是匿名也可见的
    // 元信息（密钥本身仍然不可见）。前端用它区分「没有凭证」与「凭证不可见」。
    const secrets = provider.type === 'opencode'
      ? (provider.zen_keys || []).length + (provider.go_keys || []).length
      : [provider.api_key, ...(provider.api_keys || [])].filter(Boolean).length;
    providers.push({ name: provider.name, type: provider.type, keys: secrets, accounts });
  }
  return { ok: true, providers };
}

export function findAccount(state, providerName, label) {
  const provider = state.config.providers.find((p) => p.name === providerName);
  if (!provider) return null;
  return (provider.accounts || []).find((a) => a.label === label) || null;
}

export function buildOverview(state, role, username) {
  const accountsPayload = buildAccountsPayload(state, { role, username });
  const flatAccounts = [];
  const providerRows = [];
  for (const prov of accountsPayload.providers) {
    let healthy = 0;
    let cooling = 0;
    let failed = 0;
    for (const a of prov.accounts) {
      flatAccounts.push({ ...a, provider: prov.name, type: prov.type });
      if (a.auth_invalid) failed += 1;
      else if (a.cooling_until && new Date(a.cooling_until) > new Date(state.now)) cooling += 1;
      else if (a.has_token) healthy += 1;
    }
    providerRows.push({
      name: prov.name,
      type: prov.type,
      accounts: prov.accounts.length,
      healthy,
      cooling,
      failed,
      route_rank: null,
    });
  }
  const usageProviders = providerRows
    .filter((p) => (role === 'admin' || role === 'viewer') || p.accounts > 0)
    .map((p) => p.name);
  const usage = buildUsageSnapshot(state, { hours: 24, accounts: flatAccounts, providers: role ? usageProviders : [] });
  const visibleRequests = visibleRequestsFor(state, role, username);
  const totalTokens = usage.totals.total_tokens;
  const totalRequests = usage.totals.requests;
  // 与后端同口径：model_locks 只下发给能看到平台配置的角色；匿名与普通用户
  // 拿到的是「字段缺失」，前端据此显示「锁定状态不可见」，而不是「没有模型锁」。
  const privileged = role === 'admin' || role === 'viewer';
  const modelLocks = privileged
    ? [
      {
        provider: 'raccoon',
        keys: 1,
        kind: 'rate_limit',
        until: iso(state.now + 26 * MIN),
        reason: '上游 429，账号级冷却中',
      },
      {
        provider: 'justwoker',
        keys: 2,
        kind: 'quota',
        until: iso(state.now + 8 * MIN),
        reason: '共享号额度耗尽，等待额度刷新',
      },
    ]
    : undefined;
  return {
    ok: true,
    version: 'v1.4.0-mock',
    build: 'mock-2026-10-19',
    uptime_sec: 3 * 86400 + 7 * 3600 + 42 * 60,
    auth_required: true,
    redis_mode: 'memory',
    sticky_sessions: 0,
    mock: true,
    default_provider: state.config.default_provider,
    default_model: state.config.default_model,
    total: flatAccounts.length,
    healthy: providerRows.reduce((s, p) => s + p.healthy, 0),
    cooling: providerRows.reduce((s, p) => s + p.cooling, 0),
    disabled: providerRows.reduce((s, p) => s + p.failed, 0),
    paused: 0,
    in_flight_full: 0,
    totals: {
      providers: providerRows.length,
      accounts: flatAccounts.length,
      healthy: providerRows.reduce((s, p) => s + p.healthy, 0),
      cooling: providerRows.reduce((s, p) => s + p.cooling, 0),
      failed: providerRows.reduce((s, p) => s + p.failed, 0),
      requests_24h: totalRequests,
      tokens_24h: totalTokens,
      credits_24h: usage.totals.credits,
      success_rate: totalRequests ? ((totalRequests - usage.totals.errors) / totalRequests) * 100 : 0,
      avg_latency_ms: usage.totals.avg_latency_ms,
    },
    providers: providerRows,
    accounts: flatAccounts,
    series: usage.series,
    model_locks: modelLocks,
    recent: visibleRequests.slice(0, 8),
    storage: usage.storage,
    generated: new Date(state.now).toISOString(),
  };
}

export function visibleRequestsFor(state, role, username) {
  if (!role) return [];
  if (role === 'admin' || role === 'viewer') return state.requestLogs;
  return state.requestLogs.filter((row) => row.provider && row.account && state.ownership[`${row.provider}/${row.account}`] === username);
}

export function visibleLogsFor(state, role, username) {
  if (!role) return [];
  if (role === 'admin' || role === 'viewer') return state.logs;
  return state.logs.filter((entry) => {
    if (/admin|路由|面板/.test(entry.text)) return false;
    if (!username) return false;
    const labels = Object.entries(state.ownership)
      .filter(([, owner]) => owner === username)
      .map(([key]) => key.split('/')[1]);
    return labels.some((label) => entry.text.includes(label));
  });
}

export function maskSecret(value) {
  const s = String(value ?? '');
  if (!s) return s;
  if (s.includes('…') || /\(len=\d+\)/.test(s)) return s;
  if (s.length <= 8) return `${s.slice(0, 2)}…(len=${s.length})`;
  return `${s.slice(0, 6)}…${s.slice(-4)}…(len=${s.length})`;
}

export function isMask(value) {
  const s = String(value ?? '');
  return s.includes('…') || /\(len=\d+\)/.test(s);
}

export { iso, HOUR, MIN };
