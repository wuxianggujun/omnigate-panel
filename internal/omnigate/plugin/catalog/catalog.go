// Package catalog declares metadata for the built-in provider types.
//
// It is intentionally metadata-only: no gateway, config or server imports, so
// every layer (config validation, panel, tests) can read the catalog without a
// dependency cycle. Registration is explicit — call Register at startup or
// RegisterInto with a custom registry; there is no init() side effect.
package catalog

import (
	"sync"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/newapi"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/opencode"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/trae"
)

var registerOnce sync.Once

// All returns a fresh copy of the built-in descriptors. The order here is
// declarative; consumers that need a stable UI order should use Registry.All.
func All() []plugin.Plugin {
	return []plugin.Plugin{
		raccoonSpec(),
		runableSpec(),
		opencodeSpec(),
		newAPISpec(),
		openAISpec(),
		geminiWebSpec(),
		traeSpec(),
	}
}

// Register adds every built-in descriptor to the process-wide registry. It is
// safe to call more than once; the first call wins. A type already registered by
// someone else panics (startup configuration conflict).
func Register() {
	registerOnce.Do(func() {
		for _, p := range All() {
			plugin.MustRegister(p)
		}
	})
}

// RegisterInto adds every built-in descriptor to r. Callers that own an isolated
// registry (tests, embedded deployments) use this instead of Register.
func RegisterInto(r *plugin.Registry) {
	for _, p := range All() {
		r.MustRegister(p)
	}
}

func raccoonSpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "raccoon",
		DisplayName: "办公小浣熊",
		Description: "小浣熊网页端的 OpenAI 兼容接口，支持 OAuth 登录、积分余额与每日签到。",
		AccountMode: plugin.AccountModeDynamic,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return raccoon.NewProvider(s.Name, s.MainOrigin, s.LLMBase, s.AuthBase), nil
		},
		Traits: plugin.Traits{
			WireFormat:          plugin.WireOpenAI,
			ReasoningEffort:     true,
			AccountScopedModels: true,
			SystemPrompt:        true,
		},
		ConfigFields: []plugin.Field{
			field("main_origin", "Main Origin", plugin.GroupConnection, plugin.KindURL,
				defaultValue(raccoon.DefaultMainOrigin),
				placeholder(raccoon.DefaultMainOrigin),
				help("站点根地址；llm_base / auth_base 留空时按它推导。")),
			field("llm_base", "LLM Base", plugin.GroupConnection, plugin.KindURL,
				advanced(),
				placeholder(raccoon.DefaultMainOrigin+raccoon.LLMPath),
				help("留空 = main_origin + /api/web/llm/v2。")),
			field("auth_base", "Auth Base", plugin.GroupConnection, plugin.KindURL,
				advanced(),
				placeholder(raccoon.DefaultMainOrigin+raccoon.AuthPath),
				help("留空 = main_origin + /api/web/auth/v1。")),
			systemPromptField(),
		},
		AccountFields: []plugin.Field{
			accountLabelField(),
			accessTokenField("Access Token", "OAuth 换来的 access_token；留空时用 refresh_token 续期。", false),
			refreshTokenField("用于自动续期 access_token；与 access_token 至少填一个。"),
		},
		Actions: []plugin.Action{
			action(plugin.ActionCheckin, "签到", plugin.ActionScopeAccount, "领取当日登录积分。", false),
			action(plugin.ActionRefresh, "刷新授权", plugin.ActionScopeAccount, "轮换 access_token。", false),
			action(plugin.ActionLogin, "登录 / 添加账号", plugin.ActionScopeAccount, "走浏览器授权流程添加账号。", false),
			action(plugin.ActionRemove, "移除账号", plugin.ActionScopeAccount, "从账号池移除该账号。", true),
		},
		Tasks: []plugin.Task{
			{Type: "raccoon", Label: "小浣熊签到", Description: "定时领取登录积分。", Action: plugin.ActionCheckin, Scope: plugin.ActionScopeAccount},
		},
		Login: &plugin.LoginSpec{
			Kind:      plugin.LoginKindOAuth,
			Transport: plugin.LoginTransportAdmin,
			Label:     "浏览器授权登录",
			Help:      "在浏览器完成小浣熊登录后，把回调地址或授权码粘贴回来即可添加账号。",
			Fields: []plugin.Field{
				loginLabelField(),
				field("redirect_base", "回调地址前缀", plugin.GroupAdvanced, plugin.KindURL,
					advanced(),
					help("可选：留空用桌面自定义协议（需手动粘贴回调）；填写时授权页会回跳到该地址，面板自动抓取授权码。")),
			},
		},
		Capabilities: []plugin.Capability{
			plugin.CapAccounts,
			plugin.CapBalance,
			plugin.CapCheckin,
			plugin.CapLogin,
			plugin.CapModels,
			plugin.CapRefresh,
		},
	}
}

func runableSpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "runable",
		DisplayName: "Runable",
		Description: "api.runable.com 的会话式接口；工具调用由网关在 prompt 中模拟。",
		AccountMode: plugin.AccountModePool,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return runable.NewProvider(s.Name, s.BaseURL, s.DeviceHeader), nil
		},
		Traits: plugin.Traits{
			WireFormat:           plugin.WireRunable,
			ToolEmulation:        true,
			MaxPromptChars:       runable.MaxPromptChars,
			SystemPrompt:         true,
			SessionCookie:        true,
			FallbackDefaultModel: true,
		},
		ConfigFields: []plugin.Field{
			field("base_url", "Base URL", plugin.GroupConnection, plugin.KindURL,
				defaultValue(runable.DefaultBaseURL),
				placeholder(runable.DefaultBaseURL),
				help("留空 = https://api.runable.com。")),
			field("device_header", "Device Header", plugin.GroupConnection, plugin.KindText,
				advanced(),
				defaultValue(runable.DefaultDeviceHeader),
				help("上游要求的设备标识；留空 = 内置默认值。")),
			field("max_prompt_chars", "单条 Prompt 上限", plugin.GroupAdvanced, plugin.KindNumber,
				advanced(),
				defaultValue(runable.MaxPromptChars),
				help("0 = 使用默认上限；负数 = 关闭截断。")),
			systemPromptField(),
		},
		AccountFields: []plugin.Field{
			accountLabelField(),
			field("email", "邮箱", plugin.GroupCredentials, plugin.KindText,
				help("用于邮箱密码登录；已有 Cookie 时可留空。")),
			field("password", "密码", plugin.GroupCredentials, plugin.KindSecret,
				help("用于邮箱密码登录；已有 Cookie 时可留空。")),
			accessTokenField("Session Cookie", "runable.com 的 session_token Cookie；留空时用邮箱/密码登录。", false),
		},
		Actions: []plugin.Action{
			action(plugin.ActionVerify, "校验账号", plugin.ActionScopeAccount, "校验 Cookie 或邮箱密码并读取当前积分。", false),
			action(plugin.ActionCheckin, "会话巡检", plugin.ActionScopeAccount, "重新登录并检查会话是否有效。", false),
			action(plugin.ActionLogin, "登录 / 添加账号", plugin.ActionScopeAccount, "用 Cookie 或邮箱密码添加账号。", false),
			action(plugin.ActionRemove, "移除账号", plugin.ActionScopeAccount, "从账号池移除该账号。", true),
		},
		Tasks: []plugin.Task{
			{Type: "runable", Label: "Runable 会话巡检", Description: "检查账号 Cookie 是否仍有效。", Action: plugin.ActionCheckin, Scope: plugin.ActionScopeAccount},
		},
		Login: &plugin.LoginSpec{
			Kind:      plugin.LoginKindPaste,
			Transport: plugin.LoginTransportAccount,
			Label:     "粘贴会话 Cookie",
			Help:      "runable 只支持 Google / Facebook 登录，会话是 httpOnly Cookie，网页脚本读不到；请从浏览器开发者工具复制 session_token 粘贴进来。",
			Fields: []plugin.Field{
				loginLabelField(),
				field("email", "邮箱", plugin.GroupCredentials, plugin.KindText,
					help("可选：填了邮箱/密码则用服务端登录校验，替代 Cookie。")),
				field("password", "密码", plugin.GroupCredentials, plugin.KindSecret,
					help("可选：与邮箱配对使用。")),
				accessTokenField("Session Cookie", "runable.com 的 session_token Cookie 值。", false),
			},
		},
		Capabilities: []plugin.Capability{
			plugin.CapAccounts,
			plugin.CapBalance,
			plugin.CapCheckin,
			plugin.CapLogin,
			plugin.CapModels,
		},
	}
}

func opencodeSpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "opencode",
		DisplayName: "OpenCode",
		Description: "OpenCode Zen / Go 与匿名免费通道；密钥按 tier 分池轮询。",
		AccountMode: plugin.AccountModeNone,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return opencode.New(opencode.Options{
				Name:      s.Name,
				ZenKeys:   s.ZenKeys,
				GoKeys:    s.GoKeys,
				Prefer:    s.TierPrefer,
				ZenBase:   s.BaseURL,
				GoBase:    s.GoBaseURL,
				Models:    s.Models,
				Anonymous: s.Anonymous,
			}), nil
		},
		Traits: plugin.Traits{
			WireFormat:      plugin.WireOpenAI,
			ReasoningEffort: true,
			SystemPrompt:    true,
		},
		ConfigFields: []plugin.Field{
			field("base_url", "Base URL（Zen）", plugin.GroupConnection, plugin.KindURL,
				defaultValue(opencode.DefaultZenBase),
				placeholder(opencode.DefaultZenBase),
				help("留空 = OpenCode Zen 默认地址。")),
			field("go_base_url", "Go Base URL", plugin.GroupConnection, plugin.KindURL,
				defaultValue(opencode.DefaultGoBase),
				placeholder(opencode.DefaultGoBase),
				help("Go 订阅套餐的地址；留空 = 默认地址。")),
			field("zen_keys", "Zen 密钥池", plugin.GroupCredentials, plugin.KindList,
				itemKind(plugin.KindSecret),
				help("按量付费密钥，一行一个；多把按请求轮询，失败自动冷却。")),
			field("go_keys", "Go 密钥池", plugin.GroupCredentials, plugin.KindList,
				itemKind(plugin.KindSecret),
				help("订阅套餐密钥，一行一个；与 Zen 密钥不通用。")),
			enumField("tier_prefer", "优先 Tier", plugin.GroupBehavior, opencode.TierGo,
				plugin.EnumOption{Value: opencode.TierGo, Label: "Go（订阅优先）"},
				plugin.EnumOption{Value: opencode.TierZen, Label: "Zen（按量付费）"}),
			field("anonymous", "匿名免费通道", plugin.GroupBehavior, plugin.KindBool,
				defaultValue(true),
				help("零密钥即可调用免费模型；关闭后只使用 zen/go 密钥池。")),
			modelsField("留空 = 自动发现；填写时作为目录不可用时的回退清单。"),
			systemPromptField(),
		},
		AccountFields: []plugin.Field{},
		Actions: []plugin.Action{
			action(plugin.ActionQuota, "查询额度", plugin.ActionScopeProvider, "读取 Go 套餐的剩余额度与重置时间。", false),
		},
		Tasks: []plugin.Task{},
		Capabilities: []plugin.Capability{
			plugin.CapUpstreamKeys,
			plugin.CapQuota,
			plugin.CapModels,
		},
	}
}

func newAPISpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "newapi",
		DisplayName: "New API",
		Description: "New API 中转站（含 AnyRouter / AgentRouter）；用户接口管签到与额度，sk- 密钥管聊天。",
		AccountMode: plugin.AccountModeDynamic,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return newapi.NewProviderWithOptions(newapi.Options{
				Name:      s.Name,
				BaseURL:   s.BaseURL,
				Models:    s.Models,
				Endpoints: s.Endpoints,
				Client:    s.Client,
				Protocol:  s.Protocol,
			}), nil
		},
		Traits: plugin.Traits{
			WireFormat:          plugin.WireOpenAI,
			ReasoningEffort:     true,
			AccountScopedModels: true,
			SystemPrompt:        true,
			// newapi 自己决定出站指纹（默认浏览器形态），但允许 operator 用
			// provider.client 覆盖成 Claude Code / Codex——这正是 AnyRouter 一类
			// 只收官方客户端流量的中转站能被接入的前提。
			ClientIdentity: true,
		},
		ConfigFields: append([]plugin.Field{
			field("base_url", "Base URL", plugin.GroupConnection, plugin.KindURL,
				required(),
				placeholder("https://api.example.com"),
				help("中转站根地址，不含 /v1；每个站点不同，必填。")),
			modelsField("留空 = 用账号的 sk- 密钥自动发现；填写时作为回退清单。"),
			systemPromptField(),
			protocolField(),
		}, append(clientIdentityFields(), userAPIEndpointFields()...)...),
		AccountFields: []plugin.Field{
			accountLabelField(),
			accessTokenField("Access Token", "Bearer 凭证，用于签到与 /api/user/*；可由 session 自动刷新。", false),
			refreshTokenField("X-Auth-Session 会话 id，用于轮换 access_token。"),
			field("api_key", "Chat API Key", plugin.GroupCredentials, plugin.KindSecret,
				help("sk-... 密钥，用于 /v1/chat/completions 与模型目录。可稍后补填（未填时该账号不能聊天，但签到/额度照常）。")),
			expiresAtField(),
			field("session_cookie", "Session Cookie", plugin.GroupCredentials, plugin.KindSecret,
				advanced(),
				help("站点会话 Cookie（整条，含 name=value）。AnyRouter / AgentRouter 这类 Cookie 型站点用它做签到与额度查询；bearer 型站点只在轮换 token 时一并发送。")),
			field("uid", "用户 ID", plugin.GroupCredentials, plugin.KindText,
				advanced(),
				placeholder("如 12345"),
				help("Cookie 型站点会用 New-Api-User 头传这个值；浏览器登录抓取时自动填。手动粘贴 Cookie 时必须填，否则上游认不出账号。")),
		},
		Actions: []plugin.Action{
			action(plugin.ActionCheckin, "签到", plugin.ActionScopeAccount, "领取站点每日额度。", false),
			action(plugin.ActionRefresh, "刷新令牌", plugin.ActionScopeAccount, "轮换 access_token。", false),
			action(plugin.ActionLogin, "登录 / 添加账号", plugin.ActionScopeAccount, "捕获站点会话并添加账号。", false),
			action(plugin.ActionRemove, "移除账号", plugin.ActionScopeAccount, "从账号池移除该账号。", true),
		},
		Tasks: []plugin.Task{
			{Type: "newapi", Label: "New API 签到", Description: "定时领取站点每日额度。", Action: plugin.ActionCheckin, Scope: plugin.ActionScopeAccount},
			{Type: "newapi-refresh", Label: "New API 刷新令牌", Description: "定时轮换即将过期的 access_token。", Action: plugin.ActionRefresh, Scope: plugin.ActionScopeAccount},
		},
		Login: &plugin.LoginSpec{
			Kind:      plugin.LoginKindSnippet,
			Transport: plugin.LoginTransportNewAPI,
			Label:     "浏览器会话捕获",
			Help:      "在站点登录后，把面板生成的脚本粘贴进浏览器控制台运行；脚本会把 access_token 与会话回传给面板并自动建号。",
			Fields: []plugin.Field{
				loginLabelField(),
				field("api_key", "Chat API Key", plugin.GroupCredentials, plugin.KindSecret,
					help("可选：站点里的 sk- 密钥，用于聊天与模型目录；也可稍后在账号里补填。")),
			},
		},
		Capabilities: []plugin.Capability{
			plugin.CapAccounts,
			plugin.CapBalance,
			plugin.CapCheckin,
			plugin.CapLogin,
			plugin.CapModels,
			plugin.CapRefresh,
		},
	}
}

func openAISpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "openai",
		DisplayName: "OpenAI 兼容",
		Description: "任意 OpenAI 兼容上游（OpenAI / DeepSeek / GLM / Kimi / OpenRouter / Ollama）。",
		AccountMode: plugin.AccountModeNone,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return openaibackend.NewWithOptions(openaibackend.Options{
				Name:     s.Name,
				BaseURL:  s.BaseURL,
				APIKey:   s.APIKey,
				APIKeys:  s.APIKeys,
				Models:   s.Models,
				Client:   s.Client,
				Protocol: s.Protocol,
			}), nil
		},
		Traits: plugin.Traits{
			WireFormat:      plugin.WireOpenAI,
			ReasoningEffort: true,
			SystemPrompt:    true,
			// 通用 OpenAI 兼容后端的默认身份是「如实申报 omnigate/…」；允许 operator
			// 改为浏览器 / Claude Code / Codex，用于只收官方客户端流量的中转站。
			ClientIdentity: true,
		},
		ConfigFields: append([]plugin.Field{
			field("base_url", "Base URL", plugin.GroupConnection, plugin.KindURL,
				defaultValue("https://api.openai.com/v1"),
				placeholder("https://api.openai.com/v1"),
				help("应包含 /v1 等版本前缀。")),
			field("api_key", "API Key", plugin.GroupCredentials, plugin.KindSecret,
				help("单个上游密钥；与 api_keys 二选一。")),
			field("api_keys", "API Key 池", plugin.GroupCredentials, plugin.KindList,
				itemKind(plugin.KindSecret),
				help("一行一个；多把按请求轮询，失败自动冷却。留空 = 用 api_key。")),
			modelsField("留空 = 自动向上游发现；填写 = 只使用这些模型，不再自动拉取。填上游原始模型 ID，目录展示时自动加 provider/ 前缀。"),
			systemPromptField(),
			protocolField(),
		}, clientIdentityFields()...),
		AccountFields: []plugin.Field{},
		Actions:       []plugin.Action{},
		Tasks:         []plugin.Task{},
		Capabilities: []plugin.Capability{
			plugin.CapUpstreamKeys,
			plugin.CapModels,
		},
	}
}

func geminiWebSpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "geminiweb",
		DisplayName: "Gemini Web",
		Description: "gemini.google.com 网页端；Google Cookie 凭证，支持图片附件与生成。",
		AccountMode: plugin.AccountModeDynamic,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return geminiweb.NewProvider(s.Name, s.Models), nil
		},
		Traits: plugin.Traits{
			WireFormat:       plugin.WireGeminiWeb,
			ToolEmulation:    true,
			Attachments:      true,
			RotatesCookie:    true,
			SystemPrompt:     true,
			FallbackExcluded: true,
		},
		ConfigFields: []plugin.Field{
			modelsField("留空 = 内置 gemini-pro / gemini-flash / gemini-flash-thinking / gemini-flash-lite。"),
			systemPromptField(),
		},
		AccountFields: []plugin.Field{
			accountLabelField(),
			accessTokenField("Google Cookie", "gemini.google.com 的完整 Cookie（含 __Secure-1PSID / __Secure-1PSIDTS）。", true),
		},
		Actions: []plugin.Action{
			action(plugin.ActionCheckin, "验活", plugin.ActionScopeAccount, "重建会话并验证 Cookie 是否仍有效。", false),
			action(plugin.ActionRefresh, "重建会话", plugin.ActionScopeAccount, "轮换 Cookie 并重建网页会话。", false),
			action(plugin.ActionLogin, "登录 / 添加账号", plugin.ActionScopeAccount, "粘贴 Google Cookie 并添加账号。", false),
			action(plugin.ActionRemove, "移除账号", plugin.ActionScopeAccount, "从账号池移除该账号。", true),
		},
		Tasks: []plugin.Task{
			{Type: "geminiweb", Label: "Gemini Web 验活", Description: "定时探测会话并持久化轮换后的 Cookie。", Action: plugin.ActionCheckin, Scope: plugin.ActionScopeAccount},
		},
		Login: &plugin.LoginSpec{
			Kind:      plugin.LoginKindPaste,
			Transport: plugin.LoginTransportAdmin,
			Label:     "粘贴 Google Cookie",
			Help:      "从浏览器复制 gemini.google.com 的完整 Cookie（需含 __Secure-1PSID 与 __Secure-1PSIDTS），粘贴后服务端会立即验活。",
			Fields: []plugin.Field{
				loginLabelField(),
				accessTokenField("Google Cookie", "gemini.google.com 的完整 Cookie。", true),
			},
		},
		Capabilities: []plugin.Capability{
			plugin.CapAccounts,
			plugin.CapCheckin,
			plugin.CapLogin,
			plugin.CapModels,
			plugin.CapRefresh,
			plugin.CapAttachments,
			plugin.CapCookieRotation,
			plugin.CapFingerprint,
		},
	}
}

func traeSpec() plugin.Plugin {
	return plugin.Plugin{
		Type:        "trae",
		DisplayName: "TRAE SOLO",
		Description: "TRAE SOLO 的自定义 SSE 协议；账号带独立的设备指纹。",
		AccountMode: plugin.AccountModeDynamic,
		Factory: func(s plugin.ProviderSpec, _ plugin.Deps) (plugin.Provider, error) {
			return trae.NewProvider(s.Name, s.AgentHost, s.UgHost, s.OAuthHost), nil
		},
		Traits: plugin.Traits{
			WireFormat:          plugin.WireTrae,
			ReasoningEffort:     true,
			AccountScopedModels: true,
			SystemPrompt:        true,
		},
		ConfigFields: []plugin.Field{
			field("agent_host", "Agent Host", plugin.GroupConnection, plugin.KindURL,
				defaultValue(trae.DefaultAgentHost),
				placeholder(trae.DefaultAgentHost),
				help("SOLO 推理端点；留空 = 默认地址。")),
			field("ug_host", "UG Host", plugin.GroupConnection, plugin.KindURL,
				defaultValue(trae.DefaultUgHost),
				placeholder(trae.DefaultUgHost),
				help("签到与用量端点；留空 = 默认地址。")),
			field("oauth_host", "OAuth Host", plugin.GroupConnection, plugin.KindURL,
				defaultValue(trae.DefaultOAuthHost),
				placeholder(trae.DefaultOAuthHost),
				help("登录与 token 交换端点；留空 = 默认地址。")),
			systemPromptField(),
		},
		AccountFields: []plugin.Field{
			accountLabelField(),
			accessTokenField("Access Token", "JWT access_token；可由 refresh_token 自动续期。", false),
			refreshTokenField("用于自动续期 access_token。"),
			expiresAtField(),
			field("uid", "UID", plugin.GroupBasic, plugin.KindText,
				advanced(),
				help("上游用户 id；留空 = 从 token 解析。")),
			field("machine_id", "Machine ID", plugin.GroupFingerprint, plugin.KindText,
				advanced(),
				help("每账号独立的机器指纹，签到要求账号间不重复。")),
			field("device_id", "Device ID", plugin.GroupFingerprint, plugin.KindText,
				advanced(),
				help("每账号独立的设备 id，签到要求账号间不重复。")),
			field("api_host", "API Host", plugin.GroupAdvanced, plugin.KindURL,
				advanced(),
				help("该账号专属的 API 主机；留空 = 默认。")),
		},
		Actions: []plugin.Action{
			action(plugin.ActionCheckin, "签到", plugin.ActionScopeAccount, "领取每日签到积分。", false),
			action(plugin.ActionRefresh, "刷新令牌", plugin.ActionScopeAccount, "轮换 access_token。", false),
			action(plugin.ActionLogin, "登录 / 添加账号", plugin.ActionScopeAccount, "走 TRAE 授权回调添加账号。", false),
			action(plugin.ActionRemove, "移除账号", plugin.ActionScopeAccount, "从账号池移除该账号。", true),
		},
		Tasks: []plugin.Task{
			{Type: "trae", Label: "TRAE 签到", Description: "定时领取每日签到积分。", Action: plugin.ActionCheckin, Scope: plugin.ActionScopeAccount},
		},
		Login: &plugin.LoginSpec{
			Kind:      plugin.LoginKindOAuth,
			Transport: plugin.LoginTransportAdmin,
			Label:     "浏览器授权登录",
			Help:      "在浏览器完成 TRAE 登录后，把回调地址粘贴回来即可添加账号；设备指纹在登录时生成并随账号保存。",
			Fields: []plugin.Field{
				loginLabelField(),
			},
		},
		Capabilities: []plugin.Capability{
			plugin.CapAccounts,
			plugin.CapBalance,
			plugin.CapCheckin,
			plugin.CapLogin,
			plugin.CapModels,
			plugin.CapRefresh,
			plugin.CapFingerprint,
		},
	}
}

// ---------------------------------------------------------------------------
// schema helpers

type fieldOption func(plugin.Field) plugin.Field

func field(key, label, group string, kind plugin.FieldKind, opts ...fieldOption) plugin.Field {
	f := plugin.Field{Key: key, Label: label, Group: group, Kind: kind}
	for _, opt := range opts {
		f = opt(f)
	}
	return f
}

func required() fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Required = true
		return f
	}
}

func advanced() fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Advanced = true
		return f
	}
}

func help(text string) fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Help = text
		return f
	}
}

func placeholder(text string) fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Placeholder = text
		return f
	}
}

func defaultValue(v any) fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Default = v
		return f
	}
}

func itemKind(k plugin.FieldKind) fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.ItemKind = k
		return f
	}
}

func enumOptions(opts ...plugin.EnumOption) fieldOption {
	return func(f plugin.Field) plugin.Field {
		f.Enum = opts
		return f
	}
}

func enumField(key, label, group, def string, opts ...plugin.EnumOption) plugin.Field {
	return field(key, label, group, plugin.KindEnum, defaultValue(def), enumOptions(opts...))
}

func systemPromptField() plugin.Field {
	return field("system_prompt", "系统提示词", plugin.GroupBehavior, plugin.KindTextarea,
		help("非空时注入该供应商的每个请求；消息型上游插入 system 消息，prompt 型上游拼到开头。"))
}

func modelsField(helpText string) plugin.Field {
	return field("models", "模型列表", plugin.GroupModels, plugin.KindList,
		help(helpText))
}

func accountLabelField() plugin.Field {
	return field("label", "账号标签", plugin.GroupBasic, plugin.KindText,
		required(),
		help("同一供应商内唯一，面板与状态文件用它定位账号。"))
}

// loginLabelField is the wizard's optional label input: interactive flows
// auto-assign a free label when it is left blank.
func loginLabelField() plugin.Field {
	return field("label", "账号标签", plugin.GroupBasic, plugin.KindText,
		help("留空 = 自动命名（如「账号 1」）。"))
}

func accessTokenField(label, helpText string, requiredFlag bool) plugin.Field {
	opts := []fieldOption{help(helpText)}
	if requiredFlag {
		opts = append(opts, required())
	}
	return field("cookie", label, plugin.GroupCredentials, plugin.KindSecret, opts...)
}

func refreshTokenField(helpText string) plugin.Field {
	return field("refresh_token", "Refresh Token", plugin.GroupCredentials, plugin.KindSecret,
		help(helpText))
}

func expiresAtField() plugin.Field {
	return field("expires_at", "过期时间", plugin.GroupAdvanced, plugin.KindNumber,
		advanced(),
		help("Unix 秒；0 / 留空 = 未知，按 token 内容推断。"))
}

func action(name plugin.ActionName, label string, scope plugin.ActionScope, helpText string, confirm bool) plugin.Action {
	return plugin.Action{Name: name, Label: label, Scope: scope, Help: helpText, Confirm: confirm}
}

// ---------------------------------------------------------------------------
// outbound identity / protocol fields
//
// Shared by every type whose Traits.ClientIdentity is true. The identity is what
// the upstream sees: which UA it thinks we are, which headers we carry, and the
// TLS ClientHello we present. Presets keep the common cases one click away, the
// override fields exist so an operator never has to wait for a rebuild when a
// relay starts rejecting an old CLI version.

// clientIdentityFields is the identity block (UA / headers / TLS ClientHello).
func clientIdentityFields() []plugin.Field {
	return []plugin.Field{
		enumField("client_preset", "出站客户端身份", plugin.GroupFingerprint, "",
			plugin.EnumOption{Value: "omnigate", Label: "omnigate（如实申报）"},
			plugin.EnumOption{Value: "browser", Label: "桌面 Chrome 浏览器"},
			plugin.EnumOption{Value: "claude-code", Label: "Claude Code CLI"},
			plugin.EnumOption{Value: "codex", Label: "OpenAI Codex CLI"},
		),
		field("client_user_agent", "User-Agent 覆盖", plugin.GroupFingerprint, plugin.KindText,
			advanced(),
			placeholder("留空 = 用预设自带的 UA"),
			help("预设声明的 UA 过期时改这里；整串替换，不要只填版本号。")),
		field("client_headers", "附加请求头", plugin.GroupFingerprint, plugin.KindJSON,
			advanced(),
			help(`JSON 对象，覆盖预设同名头。例：{"anthropic-beta":"claude-code-20250219"}`)),
		field("client_tls_profile", "TLS ClientHello", plugin.GroupFingerprint, plugin.KindText,
			advanced(),
			placeholder("留空 = 预设自带；none = 关闭"),
			help("如 chrome120；只对 HTTPS 生效，需 utls 支持该版本。")),
	}
}

// protocolField is the outbound dialect selector for a generic OpenAI-compatible
// backend. Anthropic is what AnyRouter/AgentRouter accept; responses is the Codex
// wire format.
func protocolField() plugin.Field {
	return enumField("protocol", "出站协议", plugin.GroupConnection, "openai",
		plugin.EnumOption{Value: "openai", Label: "chat/completions（标准 OpenAI）"},
		plugin.EnumOption{Value: "anthropic", Label: "messages（Anthropic / Claude Code）"},
		plugin.EnumOption{Value: "responses", Label: "responses（OpenAI Codex）"},
	)
}

// userAPIEndpointFields is the New API user-surface block: the facts that differ
// between New API deployments (see plugin.UserAPIEndpoints).
func userAPIEndpointFields() []plugin.Field {
	return []plugin.Field{
		enumField("auth_mode", "用户接口鉴权", plugin.GroupCredentials, "bearer",
			plugin.EnumOption{Value: "bearer", Label: "Authorization: Bearer <access_token>"},
			plugin.EnumOption{Value: "cookie", Label: "Cookie 会话（AnyRouter / AgentRouter）"},
		),
		field("user_header", "用户身份头", plugin.GroupCredentials, plugin.KindText,
			placeholder("留空 = 不发；AnyRouter 用 New-Api-User"),
			help("AnyRouter / AgentRouter 要求 New-Api-User: <数字用户 ID>。")),
		field("checkin_path", "签到路径", plugin.GroupAdvanced, plugin.KindText,
			advanced(),
			placeholder("/api/user/checkin"),
			help("AnyRouter / AgentRouter 用 /api/user/sign_in；留空 = 默认。")),
		field("self_path", "资料路径", plugin.GroupAdvanced, plugin.KindText,
			advanced(),
			placeholder("/api/user/self"),
			help("读取用户名与额度；留空 = 默认。")),
		field("refresh_path", "刷新令牌路径", plugin.GroupAdvanced, plugin.KindText,
			advanced(),
			placeholder("/api/user/auth/refresh"),
			help("轮换 access_token；Cookie 型站点用不到。留空 = 默认。")),
		field("signin_path", "登录页路径", plugin.GroupAdvanced, plugin.KindText,
			advanced(),
			placeholder("/sign-in"),
			help("浏览器登录向导打开的页面；AnyRouter / AgentRouter 用 /login。留空 = 默认。")),
	}
}
