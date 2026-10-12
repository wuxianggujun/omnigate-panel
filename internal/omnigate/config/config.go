// Package config loads omnigate's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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
	// CreditRouting 控制「快过期积分优先」选号（见下方类型注释）。缺省启用。
	CreditRouting *CreditRouting `json:"credit_routing,omitempty"`

	// Fingerprint 控制 Gemini Web 供应商的反检测 / 协议伪装层（动态请求头、
	// TLS ClientHello 伪装、请求抖动、Cookie 落盘、Chrome 版本同步）。缺省
	// （nil）由 applyDefaults 填成一份合理默认值。
	Fingerprint *FingerprintConfig `json:"fingerprint,omitempty"`

	// Maintenance 控制 OmniGate 上游账号的后台维护循环（目前只有 geminiweb 的
	// 会话保活）。缺省（nil）由 applyDefaults 填成关闭状态：网关本就在请求时按需
	// 刷新会话，后台循环只在账号池长期空闲时才有意义。
	Maintenance *MaintenanceConfig `json:"maintenance,omitempty"`

	// PanelKey 是面板自己那把网关密钥（= config.json 的 api_key），由 cmd/server 在
	// 构建运行时注入。它**恒为全量**且**绝不落盘**（json:"-"）：
	//   - 面板前端要用它调 /omni/*，所以它必须永远有效；
	//   - 它不属于「发给客户端的密钥」，不该出现在面板的密钥卡里，也不该被写进
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
// gclient.py 的 _auto_refresh_loop（定期轮换 Cookie → 重建会话 → 心跳）。
type MaintenanceConfig struct {
	// KeepaliveEnabled 打开 geminiweb 账号的后台保活。
	KeepaliveEnabled bool `json:"keepalive_enabled,omitempty"`
	// KeepaliveIntervalMinutes 是保活间隔（分钟）；<=0 视为 30。
	KeepaliveIntervalMinutes int `json:"keepalive_interval_minutes,omitempty"`
}

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
	// Raccoon-specific bases; each defaults to base_url + the upstream path.
	MainOrigin string          `json:"main_origin"`
	LLMBase    string          `json:"llm_base"`
	AuthBase   string          `json:"auth_base"`
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
	Type            string            `json:"type"` // "http" | "runable" | "raccoon" | "trae" | "newapi" | "newapi-refresh"
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
		}
	}
}

func (c *Config) validate() error {
	if len(c.Providers) == 0 {
		return fmt.Errorf("no providers configured")
	}
	seen := map[string]bool{}
	for _, p := range c.Providers {
		if p.Name == "" {
			return fmt.Errorf("provider missing name")
		}
		if seen[p.Name] {
			return fmt.Errorf("duplicate provider %q", p.Name)
		}
		seen[p.Name] = true
		switch p.Type {
		case "runable", "openai", "raccoon", "trae", "newapi", "geminiweb":
		default:
			return fmt.Errorf("provider %q: unknown type %q", p.Name, p.Type)
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

// keyLabel 给密钥条目一个可读标签用于报错（绝不回显密钥本身）。
func keyLabel(k APIKey) string {
	if k.Name != "" {
		return k.Name
	}
	return "未命名"
}

// Normalize 套用默认值并校验（导出供面板热重载复用；Load 内部即走这条路径）。
func (c *Config) Normalize() error {
	c.applyDefaults()
	return c.validate()
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
