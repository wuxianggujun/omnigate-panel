// Package config loads omnigate's JSON configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
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
	APIKeys         []string         `json:"api_keys"`
	Providers       []ProviderConfig `json:"providers"`
	Checkin         CheckinConfig    `json:"checkin"`
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
	return nil
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
