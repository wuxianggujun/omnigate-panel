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
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// Provider is an OpenAI-compatible upstream.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	models  []string
	http    *http.Client
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

// Type returns the provider type.
func (p *Provider) Type() string { return "openai" }

func (p *Provider) auth(req *http.Request) {
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
}

// ListModels fetches /models, falling back to the configured model list.
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err == nil {
		p.auth(req)
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

// StreamChat sends a streaming chat completion and normalizes the SSE.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	body := map[string]any{
		"model":    in.Model,
		"messages": in.Messages,
		"stream":   true,
	}
	if len(in.Tools) > 0 {
		body["tools"] = in.Tools
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	p.auth(req)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, string(b))
	}
	return provider.NewOpenAIStream(resp.Body), nil
}
