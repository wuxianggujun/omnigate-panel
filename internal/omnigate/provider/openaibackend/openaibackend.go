// Package openaibackend adapts any OpenAI-compatible upstream (OpenAI, DeepSeek,
// GLM, Kimi, OpenRouter, Ollama, ...) to the provider.Provider interface. This is
// how omnigate "supports other" upstreams without touching the core.
package openaibackend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// Provider is an OpenAI-compatible upstream.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	models  []string
	http    *http.Client

	// 上游密钥池（可选）。非空时按请求轮询：某个 key 拿到 401/403（key 不可用）
	// 或 429（限流）就冷却并自动切下一个。空 = 只用单个 api_key（旧写法不变）。
	mu   sync.Mutex
	keys []string
	next int
	bad  map[string]time.Time
}

// New builds an OpenAI-compatible provider. baseURL should include the version
// prefix, e.g. https://api.openai.com/v1.
func New(name, baseURL, apiKey string, models []string) *Provider {
	return &Provider{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		models:  models,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// 密钥池冷却时长：鉴权失败（key 被吊销 / 无权限）冷得久一点，限流（429）只短暂
// 绕开——上游恢复后这把 key 还能用。
const (
	keyCooldownAuth = 10 * time.Minute
	keyCooldownRate = time.Minute
)

// SetAPIKeys 启用上游密钥池（多密钥轮询）。空池 / 单项池等于没启轮询；调用会重置
// 轮询游标与冷却状态（面板保存配置后热重载会重建 provider，这里主要供测试与
// 运行时替换）。
func (p *Provider) SetAPIKeys(keys []string) {
	if p == nil {
		return
	}
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = clean
	p.next = 0
	p.bad = nil
}

// candidates 本 provider 可用的候选 key：密钥池优先，否则回落到单个 api_key。
func (p *Provider) candidates() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) > 0 {
		out := make([]string, len(p.keys))
		copy(out, p.keys)
		return out
	}
	if p.apiKey != "" {
		return []string{p.apiKey}
	}
	return nil
}

// pick 取下一个 key：第一轮只挑「没在冷却、本次没试过」的；全池冷却时第二轮退让
// 到「只要本次没试过」（宁可冒险也别直接失败）。tried 为 nil 表示不排除任何 key。
func (p *Provider) pick(tried map[string]bool) (string, bool) {
	cands := p.candidates()
	if len(cands) == 0 {
		return "", false
	}
	if len(cands) == 1 {
		if tried[cands[0]] {
			return "", false
		}
		return cands[0], true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(cands)
	for pass := 0; pass < 2; pass++ {
		for j := 0; j < n; j++ {
			i := (p.next + j) % n
			k := cands[i]
			if tried[k] {
				continue
			}
			if pass == 0 {
				if until, bad := p.bad[k]; bad && now.Before(until) {
					continue
				}
			}
			p.next = (i + 1) % n
			return k, true
		}
	}
	return "", false
}

// markBad 把 key 放进冷却；markGood 清除它的冷却。
func (p *Provider) markBad(key string, d time.Duration) {
	if key == "" || d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	p.bad[key] = time.Now().Add(d)
}

func (p *Provider) markGood(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.bad, key)
}

// keyCooldownFor 按上游状态码决定冷却时长；0 = 不参与轮询（直接报错）。
func keyCooldownFor(status int) time.Duration {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return keyCooldownAuth
	case http.StatusTooManyRequests:
		return keyCooldownRate
	}
	return 0
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) {
	if u == nil {
		return
	}
	if tr, ok := p.http.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(u)
		tr.CloseIdleConnections()
	}
}

// SetProxySelector routes this provider's outbound requests through the proxy pool
// selected per request by sel (nil = direct). Unlike SetProxy it adds
// connection-level failover (retry the next proxy on a connection error).
func (p *Provider) SetProxySelector(sel outbound.Selector) {
	if p == nil || p.http == nil {
		return
	}
	base, ok := p.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	p.http.Transport = outbound.WrapTransport(base, sel)
}

// Type returns the provider type.
func (p *Provider) Type() string { return "openai" }

// auth 给请求带上某把上游 key 的 Authorization 头（key 为空则不带，兼容无鉴权
// 上游）。
func (p *Provider) auth(req *http.Request, key string) {
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
}

// ListModels fetches /models, falling back to the configured model list.
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err == nil {
		key, _ := p.pick(nil)
		p.auth(req, key)
		if resp, err := p.http.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode < 400 {
				var ml openai.ModelList
				if json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ml) == nil {
					out := make([]openai.Model, 0, len(ml.Data))
					for _, m := range ml.Data {
						if m.Object == "" {
							m.Object = "model"
						}
						if m.OwnedBy == "" {
							m.OwnedBy = p.name
						}
						out = append(out, m)
					}
					if len(out) > 0 {
						return out, nil
					}
				}
			}
		}
	}
	out := make([]openai.Model, 0, len(p.models))
	for _, id := range p.models {
		out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: p.name})
	}
	return out, nil
}

// StreamChat sends a streaming chat completion and normalizes the SSE. When an
// upstream key pool is configured it rotates on auth/rate failures, so one dead
// or throttled key doesn't take the whole provider down.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	body := map[string]any{
		"model":    in.Model,
		"messages": in.Messages,
		"stream":   true,
	}
	if len(in.Tools) > 0 {
		body["tools"] = in.Tools
	}
	if in.ReasoningEffort != "" {
		body["reasoning_effort"] = in.ReasoningEffort
	}
	provider.ApplySampling(body, in)
	raw, _ := json.Marshal(body)

	tried := map[string]bool{}
	var lastErr error
	for {
		key, ok := p.pick(tried)
		if !ok {
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("no upstream api key available")
		}
		tried[key] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "text/event-stream")
		p.auth(req, key)
		resp, err := p.http.Do(req)
		if err != nil {
			// 网络层错误与 key 无关：不轮换，否则一次网络抖动会把整池 key 冷却掉。
			return nil, err
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			lastErr = fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, string(b))
			// 只有「这把 key 的问题」才换 key 重试：401/403（失效/无权限）、
			// 429（限流）。单 key 或其余错误码直接上报，行为与从前一致。
			if d := keyCooldownFor(resp.StatusCode); d > 0 && len(p.candidates()) > 1 {
				p.markBad(key, d)
				continue
			}
			return nil, lastErr
		}
		p.markGood(key)
		return provider.NewOpenAIStream(resp.Body), nil
	}
}
