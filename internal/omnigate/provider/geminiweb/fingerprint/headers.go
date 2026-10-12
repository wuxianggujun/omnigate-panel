package fingerprint

import (
	"net/http"
	"strconv"
	"strings"
)

// Header 是一个有序请求头。Go 的 net/http 在写包时会把请求头按字典序排序，
// 无法保序；这里仍然按配置顺序产出，一是把「顺序」这一逻辑忠实移植过来便于
// 校验，二是让调用方在需要时（自定义传输层）能够拿到真实顺序。
type Header struct {
	Name  string
	Value string
}

// HeaderBuilder 依据指纹配置动态构建请求头。
type HeaderBuilder struct {
	mgr *Manager
}

// NewHeaderBuilder 创建一个绑定到 mgr 的构建器。
func NewHeaderBuilder(mgr *Manager) *HeaderBuilder {
	if mgr == nil {
		mgr = NewManager("")
	}
	return &HeaderBuilder{mgr: mgr}
}

// Build 按配置顺序构建一组请求头。method 决定 Sec-Fetch-Mode / Sec-Fetch-Dest
// 的取值（POST = cors/empty，其余 = navigate/document），contentType 非空时才会
// 输出 Content-Type。
func (b *HeaderBuilder) Build(url, method, contentType string) []Header {
	cfg := b.mgr.Config()
	out := make([]Header, 0, len(cfg.HeaderOrder))
	for _, name := range cfg.HeaderOrder {
		v, ok := b.resolve(name, cfg, method, contentType)
		if !ok {
			continue
		}
		out = append(out, Header{Name: name, Value: v})
	}
	return out
}

// Apply 把构建出的请求头写入 req；skip 里的头名（大小写不敏感）会被跳过。
func (b *HeaderBuilder) Apply(req *http.Request, method, contentType string, skip ...string) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[strings.ToLower(s)] = true
	}
	for _, h := range b.Build(req.URL.String(), method, contentType) {
		if skipSet[strings.ToLower(h.Name)] {
			continue
		}
		req.Header.Set(h.Name, h.Value)
	}
}

// GetImpersonateTarget 返回当前配置的 TLS 指纹目标名。
func (b *HeaderBuilder) GetImpersonateTarget() string {
	return b.mgr.Config().Chrome.ImpersonateTarget
}

func (b *HeaderBuilder) resolve(name string, cfg *Config, method, contentType string) (string, bool) {
	post := strings.EqualFold(method, http.MethodPost)
	switch strings.ToLower(name) {
	case "user-agent":
		return buildUA(cfg), true
	case "sec-ch-ua":
		return buildSecCHUA(cfg), true
	case "sec-ch-ua-mobile":
		return "?0", true
	case "sec-ch-ua-platform":
		return `"` + cfg.Platform.OS + `"`, true
	case "origin":
		return "https://gemini.google.com", true
	case "referer":
		return "https://gemini.google.com/", true
	case "x-same-domain":
		return "1", true
	case "sec-fetch-site":
		return "same-origin", true
	case "sec-fetch-mode":
		if post {
			return "cors", true
		}
		return "navigate", true
	case "sec-fetch-dest":
		if post {
			return "empty", true
		}
		return "document", true
	case "content-type":
		if contentType == "" {
			return "", false
		}
		return contentType, true
	case "content-length", "host", "connection":
		// 由 net/http 自行管理，绝不手写。
		return "", false
	}
	if v, ok := cfg.Headers[name]; ok && v != "" {
		return v, true
	}
	return "", false
}

func buildUA(cfg *Config) string {
	osStr := cfg.Platform.OS + " NT " + cfg.Platform.OSVersion + "; Win64; x64"
	return "Mozilla/5.0 (" + osStr + ") AppleWebKit/537.36 (KHTML, like Gecko) Chrome/" +
		cfg.Chrome.Full + " Safari/537.36"
}

func buildSecCHUA(cfg *Config) string {
	major := strconv.Itoa(cfg.Chrome.Major)
	return `"Google Chrome";v="` + major + `", "Chromium";v="` + major + `", "Not_A Brand";v="24"`
}
