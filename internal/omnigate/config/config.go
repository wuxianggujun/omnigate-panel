// Package config loads omnigate's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
)

// DefaultDeviceHeader mirrors the value hard-coded in the original APK.
const DefaultDeviceHeader = "dweb/a69d4f72ddd711c660237adaf5c76287ac5473f5"

// Config is the root configuration object.
type Config struct {
	Server          ServerConfig     `json:"server"`
	DataDir         string           `json:"data_dir"`
	DefaultProvider string           `json:"default_provider"`
	DefaultModel    string           `json:"default_model"`
	Mode            string           `json:"mode"`
	Reasoning       bool             `json:"reasoning"`
	APIKeys         APIKeyList       `json:"api_keys"`
	Providers       []ProviderConfig `json:"providers"`
	Checkin         CheckinConfig    `json:"checkin"`
	// ModelWhitelist 是「对外暴露模型」的全局白名单，移植自参考实现的
	// MODEL_WHITELIST：非空时 /v1/models、/v1beta/models 只列出其中出现的模型 ID
	// （与对外暴露的名字一致，如 runable/zai/glm-5.3-flash，或配置的映射别名）。
	// 为空 = 不过滤（放行全部）。仅影响目录列举，不改变实际路由。
	ModelWhitelist []string `json:"model_whitelist,omitempty"`
	// CreditRouting 控制「快过期积分优先」选号（见下方类型注释）。缺省启用。
	CreditRouting *CreditRouting `json:"credit_routing,omitempty"`

	// Routing 控制裸模型名（不带 "provider/" 前缀）的供应商选择顺序（见下方类型
	// 注释）。缺省 nil = 旧行为（一律 default_provider）。
	Routing *RoutingConfig `json:"routing,omitempty"`

	// Fingerprint 控制 Gemini Web 供应商的反检测 / 协议伪装层（动态请求头、
	// TLS ClientHello 伪装、请求抖动、Cookie 落盘、Chrome 版本同步）。缺省
	// （nil）由 applyDefaults 填成一份合理默认值。
	Fingerprint *FingerprintConfig `json:"fingerprint,omitempty"`

	// Maintenance 控制 OmniGate 上游账号的后台维护循环（目前只有 geminiweb 的
	// 会话保活）。缺省（nil）由 applyDefaults 填成关闭状态：网关本就在请求时按需
	// 刷新会话，后台循环只在账号池长期空闲时才有意义。
	Maintenance *MaintenanceConfig `json:"maintenance,omitempty"`

	// RateLimit 控制入站 API 的「按客户端 IP 限流」。缺省（nil / enabled=false）
	// 保持不限流，与未挂限流完全一致（零回归）。
	RateLimit *RateLimitConfig `json:"rate_limit,omitempty"`

	// Fallback 控制「Gemini 失败/空回复 → 第三方兜底」。缺省关闭（零回归）。
	Fallback *FallbackConfig `json:"fallback,omitempty"`

	// PanelKey 是面板自己那把网关密钥（= config.json 的 api_key），由 cmd/server 在
	// 构建运行时注入。它**恒为全量**且**绝不落盘**（json:"-"）：
	//   - 面板与客户端共用同一把全量钥匙；
	//   - 它不属于「发给客户端的收窄密钥」，不该出现在面板的密钥卡里，也不该被写进
	//     omnigate.json（否则轮换 api_key 后旧钥匙会残留在文件里继续可用）。
	// 空 = 未注入（例如直接跑 OmniGate 独立二进制时）。
	PanelKey string `json:"-"`
}

// FingerprintConfig 是 Gemini Web 反检测层的开关与路径。移植自 gemini2api 的
// app/core/fingerprint（config / header_builder / cookie_jar / version_sync /
// jitter）+ curl_cffi 的 impersonate 语义。
type FingerprintConfig struct {
	// ConfigPath 是 fingerprint.json 的落盘路径；空 = 默认 <data_dir>/fingerprint.json。
	ConfigPath string `json:"config_path,omitempty"`
	// CookieDir 是持久化 Cookie 的目录；空 = 默认 <data_dir>/cookies。
	CookieDir string `json:"cookie_dir,omitempty"`
	// JitterEnabled 打开请求间隔抖动（抹掉机器节奏）。
	JitterEnabled bool `json:"jitter_enabled,omitempty"`
	// VersionSyncEnabled 打开后台 Chrome 版本同步。
	VersionSyncEnabled bool `json:"version_sync_enabled,omitempty"`
	// VersionSyncIntervalHours 是版本同步间隔（小时）；<=0 视为 24。
	VersionSyncIntervalHours int `json:"version_sync_interval_hours,omitempty"`
	// TLSEnabled 打开 utls 的 TLS ClientHello 伪装。指针以便区分「未设置」
	// （默认 true）与显式 false。
	TLSEnabled *bool `json:"tls_enabled,omitempty"`
}

// TLSImpersonationEnabled 报告 TLS 伪装是否启用（未设置时默认 true）。
func (f *FingerprintConfig) TLSImpersonationEnabled() bool {
	return f == nil || f.TLSEnabled == nil || *f.TLSEnabled
}

// MaintenanceConfig 是 OmniGate 上游账号的后台维护开关。移植自 gemini2api
// gclient.py 的 _auto_refresh_loop（定期轮换 Cookie → 重建会话 → 心跳）与
// main.py 的 web_chat_cleanup_loop（定时清理 Gemini 网页端堆积会话）。
type MaintenanceConfig struct {
	// KeepaliveEnabled 打开 geminiweb 账号的后台保活。
	KeepaliveEnabled bool `json:"keepalive_enabled,omitempty"`
	// KeepaliveIntervalMinutes 是保活间隔（分钟）；<=0 视为 30。
	KeepaliveIntervalMinutes int `json:"keepalive_interval_minutes,omitempty"`

	// ChatCleanupEnabled 打开 geminiweb 网页端会话的定时清理。默认关闭：清理
	// 会真的删除上游数据，只应由运维显式开启。
	ChatCleanupEnabled bool `json:"chat_cleanup_enabled,omitempty"`
	// ChatCleanupKeepHours 是保留窗口（小时）；<=0 视为 24（上游另有一小时下界）。
	ChatCleanupKeepHours float64 `json:"chat_cleanup_keep_hours,omitempty"`
	// ChatCleanupIntervalHours 是清理间隔（小时）；<=0 视为 6。
	ChatCleanupIntervalHours float64 `json:"chat_cleanup_interval_hours,omitempty"`
	// ChatCleanupSkipPinned 跳过置顶会话；缺省（nil）视为 true。
	ChatCleanupSkipPinned *bool `json:"chat_cleanup_skip_pinned,omitempty"`
}

// ChatCleanupSkipPinnedOrDefault 报告清理是否跳过置顶会话（未设置时默认 true）。
func (m *MaintenanceConfig) ChatCleanupSkipPinnedOrDefault() bool {
	return m == nil || m.ChatCleanupSkipPinned == nil || *m.ChatCleanupSkipPinned
}

// RateLimitConfig 是入站 API 的「按客户端 IP 限流」开关。移植自 gemini2api 的
// app/core/limiter.py（slowapi + rate_limit_enabled / rate_limit_window /
// rate_limit_max）。默认关闭：关闭时与未挂限流完全一致（零回归）。限流只作用于
// 聊天类端点（OpenAI chat / Claude messages / Gemini generateContent）。
type RateLimitConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Window 是限流窗口（秒）；<=0 视为 60。
	Window int `json:"window,omitempty"`
	// Max 是每个窗口内允许的请求数；<=0 视为 10。
	Max int `json:"max,omitempty"`
	// TrustProxy 信任 X-Forwarded-For / X-Real-IP 里的真实客户端 IP（对应参考实现
	// 的 TRUST_PROXY_HEADERS）。默认 false：直接用 TCP 对端地址，因为这两个头可被
	// 客户端伪造，开启后必须确保部署在会覆写它们的反向代理之后。
	TrustProxy bool `json:"trust_proxy,omitempty"`
}

// FallbackConfig 是「Gemini 失败 / 空回复 → 第三方兜底」开关。移植自 gemini2api
// 的 app/core/fallback.py：主请求失败或产出为空时，改用其它已配置的第三方模型重试
// 同一请求，客户端始终只看到一个模型名。默认关闭（零回归）。
type FallbackConfig struct {
	Enabled bool `json:"enabled,omitempty"`
	// Models 是可选的有序候选模型名；为空 = 自动使用所有「看起来能聊天」的第三方
	// 模型（随机顺序）。对应参考实现的 FALLBACK_MODELS。
	Models []string `json:"models,omitempty"`
}

// WindowSeconds 是限流窗口秒数（<=0 时默认 60）。
func (r *RateLimitConfig) WindowSeconds() int {
	if r == nil || r.Window <= 0 {
		return 60
	}
	return r.Window
}

// MaxPerWindow 是每个窗口的请求上限（<=0 时默认 10）。
func (r *RateLimitConfig) MaxPerWindow() int {
	if r == nil || r.Max <= 0 {
		return 10
	}
	return r.Max
}

// On 报告是否启用限流。
func (r *RateLimitConfig) On() bool { return r != nil && r.Enabled }

// On 报告是否启用第三方兜底。
func (f *FallbackConfig) On() bool { return f != nil && f.Enabled }

// APIKey 是一把入站网关密钥。
//
// Providers 为空 = 全量（可访问所有供应商）；否则只放行列出的供应商名。之所以按
// 供应商收窄就够用：模型本来就按供应商分组，所以「密钥按供应商授权」等价于「按模型
// 授权」——客户端拿到的 /v1/models 会自然收窄成它有权用的那批。
type APIKey struct {
	Key       string   `json:"key"`
	Name      string   `json:"name,omitempty"`
	Providers []string `json:"providers,omitempty"`
}

// Allows 报告这把密钥能否访问指定供应商。Providers 为空 = 全量放行。
func (k APIKey) Allows(provider string) bool {
	if len(k.Providers) == 0 {
		return true
	}
	for _, p := range k.Providers {
		if p == provider {
			return true
		}
	}
	return false
}

// Scoped 报告这把密钥是否收窄了供应商。
func (k APIKey) Scoped() bool { return len(k.Providers) > 0 }

// APIKeyList 承载 api_keys 字段，同时接受两种写法：
//
//	"sk-xxx"                                      // 裸串 = 全量（历史写法，原样兼容）
//	{"key":"sk-xxx","name":"raccoon 专用","providers":["raccoon"]}
//
// 序列化时未命名且未收窄的密钥仍写回裸串，让既有配置文件原样往返、手改友好。
type APIKeyList []APIKey

// UnmarshalJSON 逐元素解析：字符串按裸串，对象按完整结构。
func (l *APIKeyList) UnmarshalJSON(b []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	out := make(APIKeyList, 0, len(raw))
	for _, r := range raw {
		var s string
		if err := json.Unmarshal(r, &s); err == nil {
			out = append(out, APIKey{Key: s})
			continue
		}
		var k APIKey
		if err := json.Unmarshal(r, &k); err != nil {
			return fmt.Errorf("api_keys 条目既不是字符串也不是对象: %s", strings.TrimSpace(string(r)))
		}
		out = append(out, k)
	}
	*l = out
	return nil
}

// MarshalJSON 把未命名且未收窄的密钥写成裸串（保持历史文件形态），其余写成对象。
func (l APIKeyList) MarshalJSON() ([]byte, error) {
	out := make([]json.RawMessage, 0, len(l))
	for _, k := range l {
		var (
			b   []byte
			err error
		)
		if !k.Scoped() && k.Name == "" {
			b, err = json.Marshal(k.Key)
		} else {
			b, err = json.Marshal(k)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return json.Marshal(out)
}

// Find 返回与 key 精确匹配的条目。空 key 永不匹配（避免「配置里留个空串 = 全量
// 放行」这种静默开洞）。
func (l APIKeyList) Find(key string) (APIKey, bool) {
	if key == "" {
		return APIKey{}, false
	}
	for _, k := range l {
		if k.Key != "" && k.Key == key {
			return k, true
		}
	}
	return APIKey{}, false
}

// Has 报告列表里是否已存在该 key（用于注入面板密钥前判重）。
func (l APIKeyList) Has(key string) bool {
	_, ok := l.Find(key)
	return ok
}

// ServerConfig controls the listening socket.
type ServerConfig struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// ProviderConfig describes one upstream and its accounts.
type ProviderConfig struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"` // "runable" | "openai" | "raccoon" | "trae" | "newapi"
	BaseURL      string   `json:"base_url"`
	DeviceHeader string   `json:"device_header"`
	APIKey       string   `json:"api_key"`
	Models       []string `json:"models"`
	// APIKeys 是「上游密钥池」：填多个上游 api_key 时按请求轮询，某个 key 拿到
	// 401/403/429 就冷却并自动切下一个（只对 api_key 型上游 openai 生效）。
	// 留空 = 沿用单个 api_key，旧写法完全不变（加法式字段，omitempty）。
	APIKeys []string `json:"api_keys,omitempty"`
	// ZenKeys / GoKeys 是 opencode 供应商专属的两个上游密钥池：OpenCode Zen 按量
	// 付费、OpenCode Go 订阅套餐。两个 tier 的模型集不同（zen 86 个、go 37 个）、
	// 密钥也不通用，所以分开存；各自池内同样按请求轮询、失败冷却。
	// 两个都为空 = 这个供应商没有可用凭证，面板按空壳隐藏、签到守卫会挂起它。
	ZenKeys []string `json:"zen_keys,omitempty"`
	GoKeys  []string `json:"go_keys,omitempty"`
	// TierPrefer 决定 opencode 先走哪个 tier（"zen" 或 "go"）。留空 = "go"：
	// 订阅套餐先跑，用不完再落到按量付费，与上游文档的推荐一致。
	TierPrefer string `json:"tier_prefer,omitempty"`
	// GoBaseURL 覆盖 opencode 的 Go 套餐上游地址（base_url 同理覆盖 zen）。
	// 留空 = https://opencode.ai/zen/go。留这两个口子是因为上游文档给 Go 的地址
	// 写作 /inference/go/v1/...，而实测 /zen/go 同样可达，将来若只有一个可用，
	// 改配置即可，不用改代码。
	GoBaseURL string `json:"go_base_url,omitempty"`
	// Anonymous 是 opencode 的「匿名免费通道」开关：零密钥时按官方客户端形态调用
	// opencode.ai/inference 的公共免费层，免费模型匿名优先（见 provider/opencode）。
	// 三态：字段缺省（nil）= 开启——零密钥的 opencode 供应商默认就能服务免费模型，
	// 面板不按空壳处理；显式 false = 关闭，只用 zen/go 密钥池。
	Anonymous *bool `json:"anonymous,omitempty"`
	// SystemPrompt 是「供应商级系统提示词」：非空时注入发给该上游的每个请求。
	// 消息型上游（openai/newapi/raccoon/trae）在 messages 头部插一条 system；
	// 单条 prompt 型上游（runable/geminiweb）拼到 prompt 开头。留空 = 不注入。
	SystemPrompt string `json:"system_prompt,omitempty"`
	// Raccoon-specific bases; each defaults to base_url + the upstream path.
	MainOrigin string `json:"main_origin"`
	LLMBase    string `json:"llm_base"`
	AuthBase   string `json:"auth_base"`
	// TRAE-specific hosts (SOLO agent / check-in / OAuth). Empty = the defaults.
	AgentHost string          `json:"agent_host"`
	UgHost    string          `json:"ug_host"`
	OAuthHost string          `json:"oauth_host"`
	Accounts  []AccountConfig `json:"accounts"`
	// MaxPromptChars 是「单条 prompt 字符数」上限（上游硬限制，见 runable
	// MaxPromptChars）。只对把整段对话拼成单条 prompt 的供应商（runable）生效：
	// 超限时网关保留开头与结尾、从中间省略后照跑（静默丢一点上下文），不再整条
	// 拒绝。0 = 用该类型的默认值，负数 = 关闭这项限制（超长原样转发给上游）。
	MaxPromptChars int `json:"max_prompt_chars,omitempty"`
}

// AnonymousEnabled 解析 opencode 匿名通道的三态：未设置 = 开启（零密钥即用的
// 默认形态），显式关闭才停用。面板（provAnonEnabled）与网关工厂必须用同一口径，
// 否则会出现「面板说空壳、服务端其实能用」的静默不一致。
func (p *ProviderConfig) AnonymousEnabled() bool {
	return p.Anonymous == nil || *p.Anonymous
}

// AccountConfig is one credential set.
type AccountConfig struct {
	Label    string `json:"label"`
	Cookie   string `json:"cookie"`
	Email    string `json:"email"`
	Password string `json:"password"`
	// RefreshToken is used by token-based providers (raccoon / newapi). When set,
	// the gateway can mint fresh access tokens without re-authorizing. For newapi
	// it holds the session id sent as X-Auth-Session.
	RefreshToken string `json:"refresh_token"`
	// UID / MachineID / DeviceID / ApiHost are TRAE-specific per-account
	// fingerprint fields (see provider.Account). Omitted for other types.
	UID       string `json:"uid,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	ApiHost   string `json:"api_host,omitempty"`
	// APIKey is the newapi chat key (sk-...). It is a second credential next to
	// the access token: the token drives /api/user/* (check-in, self) while the
	// API key drives /v1/chat/completions.
	APIKey string `json:"api_key,omitempty"`
	// ExpiresAt is the access token's expiry (unix seconds) when the provider
	// learns it from the token endpoint (newapi). 0 = unknown.
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// SessionCookie is the optional New API session cookie captured with the
	// session id (see provider.Account.SessionCookie).
	SessionCookie string `json:"session_cookie,omitempty"`
}

// CheckinConfig controls the scheduled check-in / keep-alive tasks.
type CheckinConfig struct {
	Enabled         bool          `json:"enabled"`
	IntervalMinutes int           `json:"interval_minutes"`
	Tasks           []CheckinTask `json:"tasks"`
	// JitterMinutes 给签到时刻加一个 ±N 分钟的随机抖动，避免每天「整点固态」签到
	// 被上游风控识别。缺省（字段缺失）= 30；显式 0 = 关闭；>0 = 该值。任务级
	// jitter_minutes 可覆盖（见 CheckinTask.Jitter）。
	JitterMinutes *int `json:"jitter_minutes,omitempty"`
}

// CheckinTask is either a generic HTTP request or a built-in provider task.
type CheckinTask struct {
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	Type            string            `json:"type"` // "http" | "runable" | "raccoon" | "trae" | "newapi" | "newapi-refresh" | "geminiweb"
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	IntervalMinutes int               `json:"interval_minutes"`
	// JitterMinutes 覆盖全局 checkin.jitter_minutes：缺省 = 继承全局；0 = 该任务
	// 不加抖动；>0 = 该值。
	JitterMinutes *int `json:"jitter_minutes,omitempty"`
}

// DefaultJitter 是签到抖动的默认幅度（±30 分钟），当任务与全局都没显式配置时生效。
const DefaultJitter = 30 * time.Minute

// Jitter 解析任务的有效抖动幅度：任务级 > 全局 > 默认；显式 0 = 关闭。
func (t CheckinTask) Jitter(global *int) time.Duration {
	if t.JitterMinutes != nil {
		if *t.JitterMinutes <= 0 {
			return 0
		}
		return time.Duration(*t.JitterMinutes) * time.Minute
	}
	if global != nil {
		if *global <= 0 {
			return 0
		}
		return time.Duration(*global) * time.Minute
	}
	return DefaultJitter
}

// DefaultInterval is the fallback check-in period.
func (c *Config) DefaultInterval() time.Duration {
	m := c.Checkin.IntervalMinutes
	if m <= 0 {
		m = 360
	}
	return time.Duration(m) * time.Minute
}

// Interval returns the task's own interval or def when unset.
func (t CheckinTask) Interval(def time.Duration) time.Duration {
	if t.IntervalMinutes <= 0 {
		return def
	}
	return time.Duration(t.IntervalMinutes) * time.Minute
}

// CreditRouting 控制 OmniGate 侧的「快过期积分优先」选号（与 WorkBuddy 池的
// pool.prefer_expiring / expiring_soon / expiringMinCredits 同口径，但作用于
// OmniGate 各供应商账号）。
//
// 为什么需要它：raccoon / runable 的积分是「每日积分（当天清零）+ 每月会员积分
// （当月清零）」这类会作废的批次——不用就归零。网关原先只做 ActiveIndex 轮询 +
// 失败才换号，会把请求钉死在某个账号上，快过期积分白白过期。开启后，窗口内仍有
// 快过期批次的账号会被优先选中（stale-while-revalidate 的余额快照驱动）。
type CreditRouting struct {
	// Enabled 显式关闭（false）时不启用；缺省（字段缺失或 null）= 启用。
	Enabled *bool `json:"enabled,omitempty"`
	// ExpiringSoon 快过期窗口（Go duration，如 "168h"）。缺省 168h；留空/非法/<=0
	// 回落默认值（与 pool.expiring_soon 同风格）。
	ExpiringSoon string `json:"expiring_soon,omitempty"`
	// MinExpiring 进入优先层所需的最小快过期积分：低于该值不参与偏好（避免为了
	// 几分钱换号）。缺省 5；<=0 回落默认值。
	MinExpiring int64 `json:"min_expiring,omitempty"`
	// RefreshInterval 余额快照的后台刷新间隔（Go duration，如 "10m"）。缺省 10m；
	// 留空/非法/<=0 回落默认值。
	RefreshInterval string `json:"refresh_interval,omitempty"`
}

// RoutingConfig 控制裸模型名（不带 "provider/" 前缀）的供应商选择。这是对
// 「模型存在性」的路由：Prefer 是供应商名的有序偏好，网关按顺序取第一个「模型
// 目录里真的有这个模型」的供应商；都没命中时回落 default_provider。带
// "provider/" 前缀的请求不经过这里（前缀本身就是明确指定）。
//
// 为什么需要它：没有偏好时，任何裸模型名都会打到 default_provider——上游没有该
// 模型就报错，客户端必须写全限定名、或在别名表里逐个映射。有了偏好，客户端只要
// 发裸名，网关就按「谁真的有这个模型」自动选供应商（缓存目录驱动，不阻塞请求）。
type RoutingConfig struct {
	// Prefer 是有序的供应商偏好（按名）。空 = 旧行为（一律 default_provider）。
	Prefer []string `json:"prefer,omitempty"`
}

// PreferProviders 返回去掉空白项后的路由优先级列表（缺省 nil）。
func (c *Config) PreferProviders() []string {
	if c == nil || c.Routing == nil {
		return nil
	}
	out := make([]string, 0, len(c.Routing.Prefer))
	for _, n := range c.Routing.Prefer {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// 快过期选号的默认参数（缺省值与 WorkBuddy 池一致）。
const (
	defaultCreditExpiringSoon    = 168 * time.Hour
	defaultCreditMinExpiring     = int64(5)
	defaultCreditRefreshInterval = 10 * time.Minute
)

// CreditRoutingEnabled 报告是否启用「快过期积分优先」（缺省启用）。
func (c *Config) CreditRoutingEnabled() bool {
	return c.CreditRouting == nil || c.CreditRouting.Enabled == nil || *c.CreditRouting.Enabled
}

// ModelWhitelisted 报告某个「对外暴露的模型 ID」是否通过全局白名单过滤。白名单为空
// 表示不过滤（放行全部）。移植自参考实现的 MODEL_WHITELIST：按对外暴露的名字精确匹配。
func (c *Config) ModelWhitelisted(id string) bool {
	if len(c.ModelWhitelist) == 0 {
		return true
	}
	for _, m := range c.ModelWhitelist {
		if m == id {
			return true
		}
	}
	return false
}

// CreditExpiringSoon 返回快过期窗口（缺省 168h）。
func (c *Config) CreditExpiringSoon() time.Duration {
	if c.CreditRouting != nil {
		if d := parseDuration(c.CreditRouting.ExpiringSoon); d > 0 {
			return d
		}
	}
	return defaultCreditExpiringSoon
}

// CreditMinExpiring 返回进入优先层的最小快过期积分（缺省 5）。
func (c *Config) CreditMinExpiring() int64 {
	if c.CreditRouting != nil && c.CreditRouting.MinExpiring > 0 {
		return c.CreditRouting.MinExpiring
	}
	return defaultCreditMinExpiring
}

// CreditRefreshInterval 返回余额快照刷新间隔（缺省 10m）。
func (c *Config) CreditRefreshInterval() time.Duration {
	if c.CreditRouting != nil {
		if d := parseDuration(c.CreditRouting.RefreshInterval); d > 0 {
			return d
		}
	}
	return defaultCreditRefreshInterval
}

// parseDuration 解析 Go duration；空串/非法返回 0（调用方按 0 回落默认值）。
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Host == "" {
		c.Server.Host = "127.0.0.1"
	}
	if c.Server.Port == 0 {
		c.Server.Port = 8317
	}
	if c.DataDir == "" {
		c.DataDir = "data"
	}
	if c.Fingerprint == nil {
		c.Fingerprint = &FingerprintConfig{}
	}
	if c.Fingerprint.ConfigPath == "" {
		c.Fingerprint.ConfigPath = filepath.Join(c.DataDir, "fingerprint.json")
	}
	if c.Fingerprint.CookieDir == "" {
		c.Fingerprint.CookieDir = filepath.Join(c.DataDir, "cookies")
	}
	if c.Fingerprint.VersionSyncIntervalHours <= 0 {
		c.Fingerprint.VersionSyncIntervalHours = 24
	}
	if c.Maintenance == nil {
		c.Maintenance = &MaintenanceConfig{}
	}
	if c.Maintenance.KeepaliveIntervalMinutes <= 0 {
		c.Maintenance.KeepaliveIntervalMinutes = 30
	}
	if c.Maintenance.ChatCleanupKeepHours <= 0 {
		c.Maintenance.ChatCleanupKeepHours = 24
	}
	if c.Maintenance.ChatCleanupIntervalHours <= 0 {
		c.Maintenance.ChatCleanupIntervalHours = 6
	}
	if c.RateLimit == nil {
		c.RateLimit = &RateLimitConfig{}
	}
	if c.RateLimit.Window <= 0 {
		c.RateLimit.Window = 60
	}
	if c.RateLimit.Max <= 0 {
		c.RateLimit.Max = 10
	}
	if c.Fallback == nil {
		c.Fallback = &FallbackConfig{}
	}
	if c.Mode == "" {
		c.Mode = "text"
	}
	if c.DefaultProvider == "" && len(c.Providers) > 0 {
		c.DefaultProvider = c.Providers[0].Name
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.Type == "" {
			p.Type = "runable"
		}
		if p.Name == "" {
			p.Name = p.Type
		}
		switch p.Type {
		case "runable":
			if p.DeviceHeader == "" {
				p.DeviceHeader = DefaultDeviceHeader
			}
			if p.BaseURL == "" {
				p.BaseURL = "https://api.runable.com"
			}
		case "openai":
			if p.BaseURL == "" {
				p.BaseURL = "https://api.openai.com/v1"
			}
		case "raccoon":
			if p.MainOrigin == "" {
				p.MainOrigin = "https://xiaohuanxiong.com"
			}
			if p.BaseURL == "" {
				p.BaseURL = p.MainOrigin
			}
			if p.LLMBase == "" {
				p.LLMBase = p.MainOrigin + "/api/web/llm/v2"
			}
			if p.AuthBase == "" {
				p.AuthBase = p.MainOrigin + "/api/web/auth/v1"
			}
		case "trae":
			// Hosts default inside the provider; nothing to fill here.
		case "newapi":
			// BaseURL 必填（每个 New API 站点不同，例如 https://api.justwoker.icu），
			// 没有通用缺省，留空由 provider 自身报错。
		case "geminiweb":
			// 无 BaseURL：客户端固定打 gemini.google.com；账号凭证是 Google Cookie。
		case "opencode":
			// 两个 tier 的地址由 provider 包提供缺省值（DefaultZenBase /
			// DefaultGoBase），这里只在用户显式写了 base_url 时把它当作 zen 的
			// 覆盖（Go 另用 go_base_url）。不填就走缺省，与上游实测一致。
		}
		// 上游密钥池：去掉空白项与重复项（前端整份回传容易带出空行），全空 = 没配。
		// opencode 的两个池（zen/go）走同一套清理规则。
		p.APIKeys = normalizeKeys(p.APIKeys)
		p.ZenKeys = normalizeKeys(p.ZenKeys)
		p.GoKeys = normalizeKeys(p.GoKeys)
		p.TierPrefer = strings.ToLower(strings.TrimSpace(p.TierPrefer))
		p.GoBaseURL = strings.TrimRight(strings.TrimSpace(p.GoBaseURL), "/")
		p.SystemPrompt = strings.TrimSpace(p.SystemPrompt)
	}
}

// normalizeKeys 去掉空白项与重复项；全空返回 nil（而不是空切片），好让
// omitempty 与「有没有配密钥」的判断保持一致。
func normalizeKeys(keys []string) []string {
	if len(keys) == 0 {
		return nil
	}
	out := make([]string, 0, len(keys))
	seen := map[string]bool{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (c *Config) validate() error {
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seen[p.Name] = true
		if err := validateProviderType(p.Type); err != nil {
			return fmt.Errorf("provider %q: %w", p.Name, err)
		}
		// 上游地址只允许 http/https：file://、gopher:// 等协议既不是上游的实际
		// 形态，也会把「配置写权限」放大成任意本地协议访问。
		for _, f := range []struct{ key, val string }{
			{"base_url", p.BaseURL},
			{"main_origin", p.MainOrigin},
			{"llm_base", p.LLMBase},
			{"auth_base", p.AuthBase},
			{"agent_host", p.AgentHost},
			{"ug_host", p.UgHost},
			{"oauth_host", p.OAuthHost},
			{"go_base_url", p.GoBaseURL},
		} {
			if err := validateHTTPURL("provider."+f.key, f.val); err != nil {
				return fmt.Errorf("provider %q: %w", p.Name, err)
			}
		}
		// tier_prefer 写错只会静默退回默认顺序（go 优先），在面板上很难察觉，
		// 所以与其它枚举字段同口径：加载/保存时就拒绝。
		if p.TierPrefer != "" && p.TierPrefer != "go" && p.TierPrefer != "zen" {
			return fmt.Errorf("provider %q: tier_prefer 只能是 \"go\" 或 \"zen\"，得到 %q", p.Name, p.TierPrefer)
		}
		// opencode 的密钥池必须两边分开填：go 订阅 key 打 zen 接口只会拿到
		// Invalid credential，而两边的问题串一模一样，事后很难查。顺手把
		// 「同一把 key 同时出现在两个池里」也拦下来——一定有一个池填错了。
		if p.Type == "opencode" {
			for _, k := range p.ZenKeys {
				for _, g := range p.GoKeys {
					if k == g {
						return fmt.Errorf("provider %q: 同一把密钥同时出现在 zen_keys 与 go_keys（key %s）；"+
							"两个 tier 的密钥不通用，请分开填", p.Name, maskKey(k))
					}
				}
			}
		}
	}
	// 密钥的供应商白名单必须指向真实存在的 provider：写错名字等于这把钥匙什么都用不了，
	// 与其让客户端拿到 403 摸不着头脑，不如在加载/保存时就拒绝。
	for _, k := range c.APIKeys {
		if k.Key == "" {
			return fmt.Errorf("api_keys: 存在空 key 条目（空串会被当成「全量放行」）")
		}
		for _, name := range k.Providers {
			if !seen[name] {
				return fmt.Errorf("api_keys[%s]: 未知 provider %q", keyLabel(k), name)
			}
		}
	}
	// 路由优先级同样必须指向真实存在的 provider：写错名字只会让这一项静默失效，
	// 与「密钥白名单」同理，加载/保存时就拒绝。
	for _, name := range c.PreferProviders() {
		if !seen[name] {
			return fmt.Errorf("routing.prefer: 未知 provider %q", name)
		}
	}
	// 签到任务同理：内置签到任务（runable/raccoon/trae/newapi/geminiweb）的语义是
	// 「维护某个供应商已有账号的会话或额度」，指向不存在的供应商 = 一条永远空转的
	// 任务。与 api_keys / routing.prefer 同一口径，加载/保存时就拒绝，而不是等它
	// 每次到点静默失败。
	//
	// 注意：供应商存在但账号数为 0 属于**运行时状态**（账号随时可能补上），不在这里
	// 拒绝——那种情况由调度器的执行条件守卫挂起，补齐后自动恢复。
	for _, t := range c.Checkin.Tasks {
		if t.Provider == "" {
			continue // 省略 provider = 落到 default_provider
		}
		if !seen[t.Provider] {
			return fmt.Errorf("checkin.tasks[%s]: 未知 provider %q", taskLabel(t), t.Provider)
		}
		if err := validateHTTPURL("checkin.tasks["+taskLabel(t)+"].url", t.URL); err != nil {
			return err
		}
	}
	// 快过期选号的时长字段：写了但解析不了，静默回落默认值会让人以为配置生效了，
	// 不如在加载/保存时直接拒绝（与 pool.expiring_soon 的宽容风格不同——那里是
	// 主配置、这里新引入，fail fast 更不容易踩坑）。
	if c.CreditRouting != nil {
		for _, f := range []struct{ name, val string }{
			{"expiring_soon", c.CreditRouting.ExpiringSoon},
			{"refresh_interval", c.CreditRouting.RefreshInterval},
		} {
			if strings.TrimSpace(f.val) != "" && parseDuration(f.val) <= 0 {
				return fmt.Errorf("credit_routing.%s: 无法解析为时长（如 \"168h\" / \"10m\"）: %q", f.name, f.val)
			}
		}
	}
	return nil
}

// validateProviderType 接受「已注册插件」或内置类型：新增插件只要在 Load 之前
// Register，就能直接出现在配置里；内置类型即使注册表尚未初始化也保持可用，
// 让配置校验不再依赖调用方的启动顺序。
func validateProviderType(typ string) error {
	if _, err := plugin.Get(typ); err == nil {
		return nil
	}
	switch typ {
	case "runable", "openai", "raccoon", "trae", "newapi", "geminiweb", "opencode":
		return nil
	}
	return fmt.Errorf("unknown type %q", typ)
}

// keyLabel 给密钥条目一个可读标签用于报错（绝不回显密钥本身）。
func keyLabel(k APIKey) string {
	if k.Name != "" {
		return k.Name
	}
	return "未命名"
}

// validateHTTPURL 校验可选的上游 URL：空值放行，非空必须是带主机的 http/https。
func validateHTTPURL(field, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s: 无法解析 URL: %w", field, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%s: 仅支持 http/https，得到 %q", field, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%s: URL 缺少主机名", field)
	}
	return nil
}

// taskLabel 给签到任务一个可读标签用于报错。
func taskLabel(t CheckinTask) string {
	if t.Name != "" {
		return t.Name
	}
	if t.Type != "" {
		return t.Type + ":" + t.Provider
	}
	return "未命名"
}

// maskKey 给密钥一个「能认出是哪一把、但抄不走」的提示，只用于报错文案。
// 与面板的 maskSecret 同口径：露前 6 位，其余隐去。绝不回显完整密钥。
func maskKey(k string) string {
	if len(k) <= 6 {
		return "***"
	}
	return k[:6] + "…"
}

// Normalize 套用默认值并校验（导出供面板热重载复用；Load 内部即走这条路径）。
func (c *Config) Normalize() error {
	c.applyDefaults()
	return c.validate()
}

// ProviderKeys 返回该供应商实际生效的上游密钥列表：密钥池非空时用它，否则回落到
// 单个 api_key。两者都空则返回 nil（无鉴权上游，如 geminiweb 靠 cookie）。
func (p *ProviderConfig) ProviderKeys() []string {
	if len(p.APIKeys) > 0 {
		return p.APIKeys
	}
	if p.APIKey != "" {
		return []string{p.APIKey}
	}
	return nil
}

// Provider returns the provider config with the given name, or nil.
func (c *Config) Provider(name string) *ProviderConfig {
	for i := range c.Providers {
		if c.Providers[i].Name == name {
			return &c.Providers[i]
		}
	}
	return nil
}
