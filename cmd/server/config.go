// config.go 加载 bootstrap 配置（PostgreSQL DSN）+ 环境变量覆盖，并定义运行时
// 主配置结构。除 DSN 外的实际配置值来自 PostgreSQL panel_config。
package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// PanelAuthConfig 面板鉴权参数；运行时账号哈希在 PostgreSQL panel_users，
// 这里只持久化会话/限流/注册开关。与网关 api_key 完全独立。
type PanelAuthConfig struct {
	SessionHours      int              `json:"session_hours"`
	MaxFailures       int              `json:"max_failures"`
	LockMinutes       int              `json:"lock_minutes"`
	Users             []panelauth.User `json:"users"`
	AllowRegistration *bool            `json:"allow_registration,omitempty"`
}

// RegistrationAllowed 自助注册是否开启（未配置时默认允许）。
func (p PanelAuthConfig) RegistrationAllowed() bool {
	return p.AllowRegistration == nil || *p.AllowRegistration
}

// SessionTTL 会话有效期（SessionHours<=0 时回落 72h）。
func (p PanelAuthConfig) SessionTTL() time.Duration {
	h := p.SessionHours
	if h <= 0 {
		h = 72
	}
	return time.Duration(h) * time.Hour
}

// MaxFail 连续失败锁定阈值（<=0 回落 5）。
func (p PanelAuthConfig) MaxFail() int {
	if p.MaxFailures <= 0 {
		return 5
	}
	return p.MaxFailures
}

// LockDuration 锁定时长（<=0 回落 15m）。
func (p PanelAuthConfig) LockDuration() time.Duration {
	m := p.LockMinutes
	if m <= 0 {
		m = 15
	}
	return time.Duration(m) * time.Minute
}

// StorageConfig 状态存储后端。配置存储强制要求 PostgreSQL；Driver 为空时
// 只要 DSN 非空也视为 PostgreSQL（便于仅用 OMNIGATE_STORAGE_DSN 启动）。
type StorageConfig struct {
	Driver string `json:"driver"` // "postgres"
	DSN    string `json:"dsn"`
}

// UsePostgres 是否启用 PostgreSQL 后端。
func (s StorageConfig) UsePostgres() bool {
	return strings.EqualFold(strings.TrimSpace(s.Driver), "postgres") ||
		(strings.TrimSpace(s.Driver) == "" && strings.TrimSpace(s.DSN) != "")
}

// LoggingConfig 请求归档与来源记录。
type LoggingConfig struct {
	RequestArchiveEnabled bool `json:"request_archive_enabled"`
	RequestRetentionDays  int  `json:"request_retention_days"`
	RequestArchiveMaxMB   int  `json:"request_archive_max_mb"`
	RequestClientInfo     bool `json:"request_client_info"`
}

// Config 顶层配置（单引擎 OmniGate）。
type Config struct {
	Listen string `json:"listen"`
	APIKey string `json:"api_key"`

	// DataDir 持久化根目录（usage / request-logs / ownership / panel_sessions 等）。
	DataDir string `json:"data_dir"`

	// OmnigateConfig 旧版引擎配置文件路径，只在首次迁移时用于定位 omnigate.json；
	// 运行时始终使用 PostgreSQL panel_config#omnigate。
	OmnigateConfig string `json:"omnigate_config"`

	Storage   StorageConfig   `json:"storage"`
	Outbound  outbound.Config `json:"outbound"`
	Logging   LoggingConfig   `json:"logging"`
	PanelAuth PanelAuthConfig `json:"panel_auth"`

	Server struct {
		ReadTimeout string `json:"read_timeout"`
	} `json:"server"`

	// 解析后
	ServerReadTimeoutDur time.Duration `json:"-"`
}

// Default 默认配置。
func Default() *Config {
	c := &Config{
		Listen:         ":7863",
		DataDir:        "./data",
		OmnigateConfig: "omnigate.json",
	}
	c.Server.ReadTimeout = "300s"
	c.Logging.RequestArchiveEnabled = true
	c.Logging.RequestRetentionDays = 7
	c.Logging.RequestArchiveMaxMB = 100
	c.Logging.RequestClientInfo = true
	c.PanelAuth.SessionHours = 72
	c.PanelAuth.MaxFailures = 5
	c.PanelAuth.LockMinutes = 15
	return c
}

// Load 从 bootstrap 文件读，再用 OMNIGATE_* env 覆盖。返回值只保证 Storage 是
// 权威 bootstrap；其它字段来自文件默认值/旧版迁移输入。
func Load(path string) (*Config, error) {
	c := Default()
	if path != "" {
		if st, statErr := os.Stat(path); statErr == nil && st.IsDir() {
			return nil, fmt.Errorf("config %s 是目录而非文件——"+
				"Docker 部署时若宿主机缺少 config.json，bind mount 会创建同名目录。"+
				"请先 `cp config.example.json config.json` 并填写 PostgreSQL DSN", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if _, err := ParseConfigInto(raw, c); err != nil {
			return nil, err
		}
	}
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	if c.Storage.UsePostgres() && strings.TrimSpace(c.Storage.Driver) == "" {
		c.Storage.Driver = "postgres"
	}
	return c, nil
}

// ParseConfigInto 把 JSON 覆盖到 c 上并 normalize。
func ParseConfigInto(raw []byte, c *Config) (*Config, error) {
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// ParseConfig 基于默认值解析一段配置 JSON。
func ParseConfig(raw []byte) (*Config, error) {
	return ParseConfigInto(raw, Default())
}

// GenerateAPIKey 生成一把网关 api_key：`sk-` + 18 字节 crypto/rand 的 base64url。
func GenerateAPIKey() (string, error) {
	raw := make([]byte, 18)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("gen api_key: %w", err)
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func applyEnv(c *Config) {
	if v := os.Getenv("OMNIGATE_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("OMNIGATE_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("OMNIGATE_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("OMNIGATE_CONFIG"); v != "" {
		c.OmnigateConfig = v
	}
	if v := os.Getenv("OMNIGATE_STORAGE_DRIVER"); v != "" {
		c.Storage.Driver = v
	}
	if v := os.Getenv("OMNIGATE_STORAGE_DSN"); v != "" {
		c.Storage.DSN = v
	}
	if v := os.Getenv("OMNIGATE_LISTEN"); v != "" {
		c.Listen = v
	}
}

func (c *Config) normalize() error {
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	if c.OmnigateConfig == "" {
		c.OmnigateConfig = "omnigate.json"
	}
	if err := c.Outbound.Normalize(); err != nil {
		return err
	}
	if err := c.normalizePanelAuth(); err != nil {
		return err
	}
	if c.Logging.RequestRetentionDays <= 0 {
		c.Logging.RequestRetentionDays = 7
	}
	if c.Logging.RequestArchiveMaxMB <= 0 {
		c.Logging.RequestArchiveMaxMB = 100
	}
	if c.Server.ReadTimeout == "" {
		c.Server.ReadTimeout = "300s"
	}
	d, err := time.ParseDuration(c.Server.ReadTimeout)
	if err != nil {
		return fmt.Errorf("server.read_timeout: %w", err)
	}
	if d < 0 {
		return fmt.Errorf("server.read_timeout: 负时长 %q 无意义", c.Server.ReadTimeout)
	}
	c.ServerReadTimeoutDur = d
	if c.Listen != "" && c.Listen[0] != ':' && !containsByte(c.Listen, ':') {
		c.Listen = ":" + c.Listen
	}
	return nil
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

func (c *Config) normalizePanelAuth() error {
	if c.PanelAuth.SessionHours <= 0 {
		c.PanelAuth.SessionHours = 72
	}
	if c.PanelAuth.MaxFailures <= 0 {
		c.PanelAuth.MaxFailures = 5
	}
	if c.PanelAuth.LockMinutes <= 0 {
		c.PanelAuth.LockMinutes = 15
	}
	if err := panelauth.ValidateUsers(c.PanelAuth.Users); err != nil {
		return fmt.Errorf("panel_auth.users: %w", err)
	}
	return nil
}

// envInt 读取整数环境变量（未设置/非法时返回 fallback）。
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

// dataPath 返回 DataDir 下的文件路径。
func (c *Config) dataPath(name string) string {
	return filepath.Join(c.DataDir, name)
}
