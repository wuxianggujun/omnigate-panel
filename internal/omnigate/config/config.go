// Package config loads omnigate's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
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

	// PanelKey 是面板自己那把网关密钥（= config.json 的 api_key），由 cmd/server 在
	// 构建运行时注入。它**恒为全量**且**绝不落盘**（json:"-"）：
	//   - 面板前端要用它调 /omni/*，所以它必须永远有效；
	//   - 它不属于「发给客户端的密钥」，不该出现在面板的密钥卡里，也不该被写进
	//     omnigate.json（否则轮换 api_key 后旧钥匙会残留在文件里继续可用）。
	// 空 = 未注入（例如直接跑 OmniGate 独立二进制时）。
	PanelKey string `json:"-"`
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
	Type         string   `json:"type"` // "runable" | "openai" | "raccoon"
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
}

// AccountConfig is one credential set.
type AccountConfig struct {
	Label    string `json:"label"`
	Cookie   string `json:"cookie"`
	Email    string `json:"email"`
	Password string `json:"password"`
	// RefreshToken is used by token-based providers (raccoon). When set, the
	// gateway can mint fresh access tokens without re-authorizing.
	RefreshToken string `json:"refresh_token"`
	// UID / MachineID / DeviceID / ApiHost are TRAE-specific per-account
	// fingerprint fields (see provider.Account). Omitted for other types.
	UID       string `json:"uid,omitempty"`
	MachineID string `json:"machine_id,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	ApiHost   string `json:"api_host,omitempty"`
}

// CheckinConfig controls the scheduled check-in / keep-alive tasks.
type CheckinConfig struct {
	Enabled         bool          `json:"enabled"`
	IntervalMinutes int           `json:"interval_minutes"`
	Tasks           []CheckinTask `json:"tasks"`
}

// CheckinTask is either a generic HTTP request or a built-in provider task.
type CheckinTask struct {
	Name            string            `json:"name"`
	Provider        string            `json:"provider"`
	Type            string            `json:"type"` // "http" | "runable"
	Method          string            `json:"method"`
	URL             string            `json:"url"`
	Headers         map[string]string `json:"headers"`
	Body            string            `json:"body"`
	IntervalMinutes int               `json:"interval_minutes"`
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
		case "runable", "openai", "raccoon", "trae":
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
