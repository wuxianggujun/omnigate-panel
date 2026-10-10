// Package outbound 提供「命名出站代理 + 目标路由」的统一模型，供面板上游
// （WorkBuddy）与内置 OmniGate 供应商复用。
//
// 一个 Config 里定义若干命名代理（http / https / socks5），再用 Routes 把
// 「目标」映射到代理名：
//
//	workbuddy            → 面板自身的 WorkBuddy 上游
//	omnigate:<provider>  → 内置 OmniGate 的某个供应商
//
// 未配置（或映射到空）的目标一律直连。代理地址支持省略 scheme（按 http 处理），
// 也支持在 URL 里内嵌 userinfo，或用 Username/Password 字段单独给出（后者优先）。
package outbound

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// TargetWorkbuddy 是面板上游（WorkBuddy）的出站目标名。
const TargetWorkbuddy = "workbuddy"

// OmniTarget 返回内置 OmniGate 某供应商的出站目标名。
func OmniTarget(provider string) string { return "omnigate:" + provider }

// Proxy 一个命名出站代理。它有两种形态：
//
//	单代理：URL（可内嵌或单独给 Username/Password）
//	代理池：Pool（显式列表）+ PoolURL（远程 API，定期拉取），二者可同时用
//
// 只要 Pool 或 PoolURL 非空就按「池」处理（URL 被忽略）。
type Proxy struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`

	// 代理池
	Pool       []string `json:"pool,omitempty"`        // 显式代理列表（host:port 或完整 URL）
	PoolURL    string   `json:"pool_url,omitempty"`    // 远程代理池 API（定期拉取）
	PoolScheme string   `json:"pool_scheme,omitempty"` // 池内 host:port 的协议，默认 http
	RefreshSec int      `json:"refresh_sec,omitempty"` // 池刷新间隔秒，默认 300
	ProbeURL   string   `json:"probe_url,omitempty"`   // 探活目标，默认 gstatic/generate_204
	Strict     bool     `json:"strict,omitempty"`      // true = 池无可用代理时不回退直连（直接失败）

	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// IsPool 报告该代理是否按代理池处理（显式列表或远程 API 任一非空）。
func (p Proxy) IsPool() bool {
	return len(p.Pool) > 0 || strings.TrimSpace(p.PoolURL) != ""
}

// Config 代理定义 + 目标路由。
type Config struct {
	Proxies []Proxy           `json:"proxies,omitempty"`
	Routes  map[string]string `json:"routes,omitempty"`
}

// Normalize 校验并归一化：去空白、校验协议、查重、校验路由引用的代理存在。
// 空路由（映射到空字符串）表示直连，会被直接删除。
func (c *Config) Normalize() error {
	seen := map[string]bool{}
	for i := range c.Proxies {
		p := &c.Proxies[i]
		if err := p.normalize(); err != nil {
			return fmt.Errorf("outbound.proxies[%d]: %w", i, err)
		}
		if seen[p.Name] {
			return fmt.Errorf("outbound.proxies: 代理名 %q 重复", p.Name)
		}
		seen[p.Name] = true
	}
	for target, name := range c.Routes {
		name = strings.TrimSpace(name)
		if name == "" {
			delete(c.Routes, target)
			continue
		}
		c.Routes[target] = name
		if !seen[name] {
			return fmt.Errorf("outbound.routes[%q]: 引用了不存在的代理 %q", target, name)
		}
	}
	return nil
}

// normalize 校验并归一化单个代理（去空白、补默认值、校验协议）。
func (p *Proxy) normalize() error {
	p.Name = strings.TrimSpace(p.Name)
	p.URL = strings.TrimSpace(p.URL)
	p.Username = strings.TrimSpace(p.Username)
	p.PoolURL = strings.TrimSpace(p.PoolURL)
	p.PoolScheme = strings.ToLower(strings.TrimSpace(p.PoolScheme))
	p.ProbeURL = strings.TrimSpace(p.ProbeURL)
	if p.Name == "" {
		return fmt.Errorf("name 不能为空")
	}

	if p.IsPool() {
		if p.PoolScheme == "" {
			p.PoolScheme = "http"
		}
		switch p.PoolScheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("pool_scheme 不支持 %q（支持 http/https/socks5/socks5h）", p.PoolScheme)
		}
		out := make([]string, 0, len(p.Pool))
		for j, e := range p.Pool {
			ne, err := normalizeEntry(p.PoolScheme, e)
			if err != nil {
				return fmt.Errorf("pool[%d]: %w", j, err)
			}
			out = append(out, ne)
		}
		p.Pool = out
		if p.PoolURL != "" {
			u, err := url.Parse(p.PoolURL)
			if err != nil {
				return fmt.Errorf("pool_url 解析失败: %w", err)
			}
			if u.Scheme != "http" && u.Scheme != "https" {
				return fmt.Errorf("pool_url 仅支持 http/https")
			}
			if u.Host == "" {
				return fmt.Errorf("pool_url 缺少 host")
			}
		}
		if p.RefreshSec == 0 {
			p.RefreshSec = 300
		}
		if p.RefreshSec < 30 {
			p.RefreshSec = 30
		}
		if p.RefreshSec > 86400 {
			p.RefreshSec = 86400
		}
		if p.ProbeURL == "" {
			p.ProbeURL = DefaultPoolProbeURL
		}
		if u, err := url.Parse(p.ProbeURL); err != nil || u.Host == "" {
			return fmt.Errorf("probe_url 非法: %q", p.ProbeURL)
		}
		return nil
	}

	// 单代理
	p.Pool = nil
	if p.URL == "" {
		return fmt.Errorf("url 不能为空（或填 pool/pool_url 建代理池）")
	}
	if _, err := p.parse(); err != nil {
		return err
	}
	return nil
}

// normalizeEntry 把一条池内代理归一化成完整 URL（缺 scheme 时按 scheme 补）。
func normalizeEntry(scheme, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("空代理地址")
	}
	if !strings.Contains(raw, "://") {
		if scheme == "" {
			scheme = "http"
		}
		raw = scheme + "://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("解析失败: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("缺少 host: %q", raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", fmt.Errorf("不支持协议 %q", u.Scheme)
	}
	return raw, nil
}

// parse 把 Proxy 归一化成 *url.URL（补 scheme、合并认证）。
func (p Proxy) parse() (*url.URL, error) {
	raw := p.URL
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("解析代理地址失败: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("代理地址缺少 host: %q", p.URL)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("不支持的代理协议 %q（支持 http/https/socks5/socks5h）", u.Scheme)
	}
	if p.Username != "" {
		if p.Password != "" {
			u.User = url.UserPassword(p.Username, p.Password)
		} else {
			u.User = url.User(p.Username)
		}
	}
	return u, nil
}

// Resolve 返回代理名对应的 URL；名为空或找不到返回 nil（= 直连）。
func (c Config) Resolve(name string) *url.URL {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	for i := range c.Proxies {
		if c.Proxies[i].Name == name {
			if u, err := c.Proxies[i].parse(); err == nil {
				return u
			}
			return nil
		}
	}
	return nil
}

// For 返回目标路由到的代理 URL；未配置返回 nil（= 直连）。
func (c Config) For(target string) *url.URL {
	return c.Resolve(c.Routes[target])
}

// HasProxy 报告目标是否配置了代理。
func (c Config) HasProxy(target string) bool { return c.For(target) != nil }

// ProxyFunc 返回 target 对应的 http.Transport.Proxy 函数；直连返回 nil。
func (c Config) ProxyFunc(target string) func(*http.Request) (*url.URL, error) {
	u := c.For(target)
	if u == nil {
		return nil
	}
	return http.ProxyURL(u)
}

// Apply 把 target 的代理应用到 tr（nil 代理 = 直连），并清空空闲连接池，让变更
// 立即对后续新建连接生效。tr 为 nil 时静默跳过。
func (c Config) Apply(tr *http.Transport, target string) {
	if tr == nil {
		return
	}
	tr.Proxy = c.ProxyFunc(target)
	tr.CloseIdleConnections()
}

// Names 返回全部代理名（按定义顺序，供 UI 展示）。
func (c Config) Names() []string {
	out := make([]string, 0, len(c.Proxies))
	for i := range c.Proxies {
		out = append(out, c.Proxies[i].Name)
	}
	return out
}
