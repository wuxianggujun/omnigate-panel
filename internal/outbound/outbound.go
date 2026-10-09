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

// Proxy 一个命名出站代理。
type Proxy struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
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
		p.Name = strings.TrimSpace(p.Name)
		p.URL = strings.TrimSpace(p.URL)
		p.Username = strings.TrimSpace(p.Username)
		if p.Name == "" {
			return fmt.Errorf("outbound.proxies[%d].name 不能为空", i)
		}
		if seen[p.Name] {
			return fmt.Errorf("outbound.proxies: 代理名 %q 重复", p.Name)
		}
		seen[p.Name] = true
		if p.URL == "" {
			return fmt.Errorf("outbound.proxies[%q].url 不能为空", p.Name)
		}
		if _, err := p.parse(); err != nil {
			return fmt.Errorf("outbound.proxies[%q]: %w", p.Name, err)
		}
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
