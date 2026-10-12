package fingerprint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ChromeVersion 描述当前伪装的 Chrome 版本。
type ChromeVersion struct {
	// Major 是主版本号（如 124），用于拼接 Sec-CH-UA 与 UA。
	Major int `json:"major"`
	// Full 是完整版本号（如 "124.0.6367.91"），用于拼接 UA。
	Full string `json:"full"`
	// ImpersonateTarget 是 TLS 指纹目标名（如 "chrome124"），tls.go 据此挑选
	// 最接近的 utls ClientHello。
	ImpersonateTarget string `json:"impersonate_target"`
}

// PlatformInfo 描述伪装的操作系统平台。
type PlatformInfo struct {
	OS        string `json:"os"`
	OSVersion string `json:"os_version"`
	Arch      string `json:"arch"`
}

// Config 是一份完整的指纹配置。JSON 字段名与 Python 参考实现保持一致，
// 便于两个项目共用同一份 data/fingerprint.json。
type Config struct {
	Chrome      ChromeVersion     `json:"chrome_version"`
	Platform    PlatformInfo      `json:"platform"`
	HeaderOrder []string          `json:"header_order"`
	Headers     map[string]string `json:"headers"`
	VersionMap  map[string]string `json:"version_map"`
	LastUpdated string            `json:"last_updated"`
}

// Clone 深拷贝一份配置，避免调用方拿到内部可变状态。
func (c *Config) Clone() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.HeaderOrder = append([]string(nil), c.HeaderOrder...)
	out.Headers = make(map[string]string, len(c.Headers))
	for k, v := range c.Headers {
		out.Headers[k] = v
	}
	out.VersionMap = make(map[string]string, len(c.VersionMap))
	for k, v := range c.VersionMap {
		out.VersionMap[k] = v
	}
	return &out
}

// DefaultConfig 返回内置的默认指纹（Chrome 124 / Windows 10 / x86_64），
// 与 Python 参考实现的 _default_data 等价。
func DefaultConfig() *Config {
	return &Config{
		Chrome: ChromeVersion{
			Major:             124,
			Full:              "124.0.6367.91",
			ImpersonateTarget: "chrome124",
		},
		Platform: PlatformInfo{OS: "Windows", OSVersion: "10.0", Arch: "x86_64"},
		HeaderOrder: []string{
			"sec-ch-ua",
			"sec-ch-ua-mobile",
			"sec-ch-ua-platform",
			"User-Agent",
			"X-Same-Domain",
			"Origin",
			"Sec-Fetch-Site",
			"Sec-Fetch-Mode",
			"Sec-Fetch-Dest",
			"Referer",
			"Accept-Encoding",
			"Accept-Language",
		},
		Headers: map[string]string{
			"Accept-Language": "en-US,en;q=0.9",
			"Accept-Encoding": "gzip, deflate, br, zstd",
		},
		VersionMap: map[string]string{
			"119": "chrome119",
			"120": "chrome120",
			"123": "chrome123",
			"124": "chrome124",
		},
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	}
}

// Manager 线程安全地持有指纹配置，支持磁盘持久化与热更新。
type Manager struct {
	path string

	mu  sync.RWMutex
	cfg *Config
}

// NewManager 创建管理器。path 为空表示纯内存模式（不落盘，测试友好）。
func NewManager(path string) *Manager {
	return &Manager{path: path}
}

// Path 返回配置文件的落盘路径（空 = 纯内存）。
func (m *Manager) Path() string { return m.path }

// Load 读取配置；文件缺失、损坏或结构异常时回退默认值并原子重写。
// 与 Python 参考实现一样「读容错」：一份被截断的 JSON 不该让客户端初始化崩溃。
func (m *Manager) Load() (*Config, error) {
	cfg, existed := m.readFile()
	if !existed {
		cfg = DefaultConfig()
		if err := m.persist(cfg); err != nil {
			return nil, err
		}
		m.set(cfg)
		return cfg.Clone(), nil
	}
	if cfg == nil {
		// 解析失败：回退默认并覆盖写回。
		cfg = DefaultConfig()
		if err := m.persist(cfg); err != nil {
			return nil, err
		}
	}
	m.set(cfg)
	return cfg.Clone(), nil
}

// Config 返回当前配置的副本；未加载时惰性加载。
func (m *Manager) Config() *Config {
	m.mu.RLock()
	cfg := m.cfg
	m.mu.RUnlock()
	if cfg != nil {
		return cfg.Clone()
	}
	loaded, err := m.Load()
	if err != nil || loaded == nil {
		return DefaultConfig()
	}
	return loaded
}

// UpdateChromeVersion 把伪装版本提升到 major/full。仅当 version_map 里存在
// 该主版本的 impersonate 目标时才更新（与 Python 一致：无目标就跳过，不硬造）。
func (m *Manager) UpdateChromeVersion(major int, full string) (bool, error) {
	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		_, _ = m.Load()
		m.mu.Lock()
	}
	cfg := m.cfg
	if cfg == nil {
		m.mu.Unlock()
		return false, fmt.Errorf("fingerprint: 配置未加载")
	}
	if cfg.Chrome.Major == major {
		m.mu.Unlock()
		return false, nil
	}
	target := cfg.VersionMap[strconv.Itoa(major)]
	if target == "" {
		m.mu.Unlock()
		return false, nil
	}
	cfg.Chrome = ChromeVersion{Major: major, Full: full, ImpersonateTarget: target}
	cfg.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	snapshot := cfg.Clone()
	m.mu.Unlock()

	if err := m.persist(snapshot); err != nil {
		return false, err
	}
	return true, nil
}

// SetVersionMap 注册一个「主版本 → impersonate 目标」映射（版本同步用）。
func (m *Manager) SetVersionMap(major int, target string) error {
	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		_, _ = m.Load()
		m.mu.Lock()
	}
	if m.cfg == nil {
		m.mu.Unlock()
		return fmt.Errorf("fingerprint: 配置未加载")
	}
	if m.cfg.VersionMap == nil {
		m.cfg.VersionMap = map[string]string{}
	}
	m.cfg.VersionMap[strconv.Itoa(major)] = target
	snapshot := m.cfg.Clone()
	m.mu.Unlock()
	return m.persist(snapshot)
}

// SetChromeTarget 直接改写 impersonate 目标（面板手动切换指纹用）。
func (m *Manager) SetChromeTarget(target string) error {
	m.mu.Lock()
	if m.cfg == nil {
		m.mu.Unlock()
		_, _ = m.Load()
		m.mu.Lock()
	}
	if m.cfg == nil {
		m.mu.Unlock()
		return fmt.Errorf("fingerprint: 配置未加载")
	}
	m.cfg.Chrome.ImpersonateTarget = target
	m.cfg.LastUpdated = time.Now().UTC().Format(time.RFC3339)
	snapshot := m.cfg.Clone()
	m.mu.Unlock()
	return m.persist(snapshot)
}

// VersionKeys 返回 version_map 里所有主版本号，降序排列。
func (c *Config) VersionKeys() []int {
	out := make([]int, 0, len(c.VersionMap))
	for k := range c.VersionMap {
		if n, err := strconv.Atoi(k); err == nil {
			out = append(out, n)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(out)))
	return out
}

func (m *Manager) set(cfg *Config) {
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

// readFile 读取并解析配置文件。返回值 (cfg, existed)：existed=false 表示文件不
// 存在（不是错误）；existed=true 且 cfg=nil 表示文件存在但无法解析。
func (m *Manager) readFile() (*Config, bool) {
	if m.path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(m.path)
	if err != nil {
		return nil, false
	}
	var parsed Config
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, true
	}
	normalizeConfig(&parsed)
	return &parsed, true
}

// normalizeConfig 补齐解析后缺失的字段，避免零值把指纹打成「空 UA」。
func normalizeConfig(c *Config) {
	def := DefaultConfig()
	if c.Chrome.Full == "" {
		c.Chrome.Full = def.Chrome.Full
	}
	if c.Chrome.Major == 0 {
		c.Chrome.Major = def.Chrome.Major
	}
	if c.Chrome.ImpersonateTarget == "" {
		c.Chrome.ImpersonateTarget = def.Chrome.ImpersonateTarget
	}
	if c.Platform.OS == "" {
		c.Platform.OS = def.Platform.OS
	}
	if c.Platform.OSVersion == "" {
		c.Platform.OSVersion = def.Platform.OSVersion
	}
	if c.Platform.Arch == "" {
		c.Platform.Arch = def.Platform.Arch
	}
	if len(c.HeaderOrder) == 0 {
		c.HeaderOrder = def.HeaderOrder
	}
	if c.Headers == nil {
		c.Headers = def.Headers
	}
	if c.VersionMap == nil {
		c.VersionMap = def.VersionMap
	}
}

func (m *Manager) persist(cfg *Config) error {
	if m.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(m.path, raw, 0o644)
}

// writeFileAtomic 临时文件 + rename 落盘，避免写一半崩溃把 JSON 截成半截。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".fp-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
