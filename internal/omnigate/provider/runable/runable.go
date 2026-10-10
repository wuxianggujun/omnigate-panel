// Package runable adapts the Runable upstream (api.runable.com) to the
// provider.Provider interface. It is a faithful port of the original APK's
// RunableApi class.
package runable

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
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

const userAgent = "Mozilla/5.0 (compatible; omnigate/1.0)"

// Client talks to the Runable API.
type Client struct {
	baseURL      string
	deviceHeader string
	http         *http.Client
}

// New builds a Runable client.
func New(baseURL, deviceHeader string) *Client {
	return &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		deviceHeader: deviceHeader,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// SetProxy routes this client's outbound requests through u (nil = direct).
// Used by the gateway to honor the panel's per-provider outbound proxy config.
func (c *Client) SetProxy(u *url.URL) {
	if u == nil {
		return
	}
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(u)
		tr.CloseIdleConnections()
	}
}

// SetProxySelector routes this client's outbound requests through the proxy pool
// selected per request by sel (nil = direct). Unlike SetProxy it adds
// connection-level failover (retry the next proxy on a connection error).
func (c *Client) SetProxySelector(sel outbound.Selector) {
	if c == nil || c.http == nil {
		return
	}
	base, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	c.http.Transport = outbound.WrapTransport(base, sel)
}

// UpstreamError carries an HTTP failure from the upstream.
type UpstreamError struct {
	Status int
	Body   string
}

func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream HTTP %d: %s", e.Status, e.Body)
}

// OutOfCredits reports whether err looks like an exhausted-credit failure.
func OutOfCredits(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "out of credit") ||
		strings.Contains(s, "out_of_credit") ||
		strings.Contains(s, "insufficient credit")
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", userAgent)
	return c.http.Do(req)
}

// SignIn performs email/password login and returns the session cookie.
func (c *Client) SignIn(ctx context.Context, email, password string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"email":      email,
		"password":   password,
		"rememberMe": true,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/sign-in/email", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://runable.com")
	req.Header.Set("Referer", "https://runable.com/")
	resp, err := c.do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	for _, sc := range resp.Header.Values("Set-Cookie") {
		kv := strings.TrimSpace(strings.SplitN(sc, ";", 2)[0])
		if strings.Contains(kv, "session_token") {
			return kv, nil
		}
	}
	return "", fmt.Errorf("sign-in succeeded but no session cookie returned")
}

// Session is the result of a get-session probe.
type Session struct {
	Alive bool
	Email string
	Name  string
}

// FetchSession validates a cookie against /api/auth/get-session.
func (c *Client) FetchSession(ctx context.Context, cookie string) Session {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/auth/get-session", nil)
	if err != nil {
		return Session{}
	}
	req.Header.Set("Cookie", cookie)
	resp, err := c.do(req)
	if err != nil {
		return Session{}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return Session{}
	}
	s := string(raw)
	if !strings.Contains(s, `"session"`) || strings.Contains(s, `"session":null`) {
		return Session{}
	}
	var obj struct {
		User struct {
			Email string `json:"email"`
			Name  string `json:"name"`
		} `json:"user"`
	}
	_ = json.Unmarshal(raw, &obj)
	return Session{Alive: true, Email: obj.User.Email, Name: obj.User.Name}
}

type modelInfo struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	IsFree        bool     `json:"isFree"`
	Description   string   `json:"description"`
	ContextWindow int64    `json:"context_window"`
	MaxTokens     int64    `json:"max_tokens"`
	Tags          []string `json:"tags"`
	Created       int64    `json:"created"`
	OwnedBy       string   `json:"owned_by"`

	// 计费：credits 是每百万 token 消耗的积分（creditsPerMillionTokens=true 时），
	// pricing 是上游给出的每 token 美元价。
	Credits                 float64       `json:"credits"`
	CreditsPerMillionTokens bool          `json:"creditsPerMillionTokens"`
	Pricing                 *modelPricing `json:"pricing"`
}

// modelPricing 是 runable 上游的每 token 美元价。
type modelPricing struct {
	Input           float64 `json:"input"`
	Output          float64 `json:"output"`
	InputCacheRead  float64 `json:"input_cache_read"`
	InputCacheWrite float64 `json:"input_cache_write"`
}

// ListModels returns the language models offered by Runable.
func (c *Client) ListModels(ctx context.Context) ([]openai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/rpc/public/models/getAIChatModels", strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	var root struct {
		JSON struct {
			Models []modelInfo `json:"models"`
		} `json:"json"`
		Models []modelInfo `json:"models"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	infos := root.JSON.Models
	if infos == nil {
		infos = root.Models
	}
	out := make([]openai.Model, 0, len(infos))
	for _, m := range infos {
		if m.ID == "" {
			continue
		}
		if m.Type != "" && m.Type != "language" {
			continue
		}
		out = append(out, runableModel(m))
	}
	return out, nil
}

// runableModel 把上游模型条目映射为 openai.Model（含积分价 / 美元价 / 免费标记）。
func runableModel(m modelInfo) openai.Model {
	owned := m.OwnedBy
	if owned == "" {
		owned = "runable"
	}
	model := openai.Model{
		ID:            m.ID,
		Object:        "model",
		Created:       m.Created,
		OwnedBy:       owned,
		ContextWindow: m.ContextWindow,
		MaxTokens:     m.MaxTokens,
		IsFree:        m.IsFree,
	}
	if m.Credits > 0 {
		c := m.Credits
		model.Credits = &c
		model.CreditUnit = "credits_per_million_tokens"
	}
	if m.Pricing != nil {
		model.Pricing = &openai.ModelPricing{
			Input:           m.Pricing.Input,
			Output:          m.Pricing.Output,
			InputCacheRead:  m.Pricing.InputCacheRead,
			InputCacheWrite: m.Pricing.InputCacheWrite,
		}
	}
	return model
}

// Credits is the parsed billing balance.
type Credits struct {
	OK      bool
	Daily   int64
	Monthly int64
	Total   int64
	Error   string
}

// FetchCredits reads the account balance.
func (c *Client) FetchCredits(ctx context.Context, cookie string) Credits {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/rpc/billing/getCustomer", strings.NewReader("{}"))
	if err != nil {
		return Credits{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", "https://runable.com")
	req.Header.Set("Referer", "https://runable.com/")
	resp, err := c.do(req)
	if err != nil {
		return Credits{Error: err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return Credits{Error: fmt.Sprintf("HTTP %d", resp.StatusCode)}
	}
	return parseCredits(raw)
}

// parseCredits is a direct port of RunableApi.parseCredits.
func parseCredits(raw []byte) Credits {
	cr := Credits{}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		cr.Error = err.Error()
		return cr
	}
	obj := root
	if j, ok := root["json"].(map[string]any); ok {
		obj = j
	}
	if obj["balances"] == nil {
		if cust, ok := obj["customer"].(map[string]any); ok {
			obj = cust
		}
	}
	balances, _ := obj["balances"].(map[string]any)
	if balances == nil {
		cr.Error = "响应中没有 balances"
		return cr
	}
	if rc, ok := balances["runable_credits"].(map[string]any); ok {
		cr.Monthly = maxI64(toI64(rc["remaining"]), 0)
	}
	if dc, ok := balances["daily_credits"].(map[string]any); ok {
		if bd, ok := dc["breakdown"].([]any); ok {
			for _, it := range bd {
				m, _ := it.(map[string]any)
				if m == nil {
					continue
				}
				if reset, ok := m["reset"].(map[string]any); ok && reset["interval"] == "day" {
					cr.Daily += toI64(m["remaining"])
				}
			}
		}
	}
	if cr.Daily < 0 {
		cr.Daily = 0
	}
	cr.Total = cr.Monthly + cr.Daily
	cr.OK = true
	return cr
}

// ChatStream opens an SSE chat stream. mode is one of text/agent/incognito and
// becomes the URL path segment, matching the original client.
func (c *Client) ChatStream(ctx context.Context, cookie, chatID, model, prompt string, incognito bool, mode string) (io.ReadCloser, error) {
	if mode == "" {
		mode = "text"
	}
	msg := map[string]any{
		"id":    util.UUID(),
		"role":  "user",
		"parts": []any{map[string]any{"type": "text", "text": prompt}},
	}
	payload := map[string]any{
		"id":      chatID,
		"trigger": "submit-message",
		"modelId": model,
	}
	if incognito {
		payload["messages"] = []any{msg}
	} else {
		payload["message"] = msg
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat/"+mode, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cookie", cookie)
	req.Header.Set("x-runable-device", c.deviceHeader)
	req.Header.Set("Origin", "https://runable.com")
	req.Header.Set("Referer", "https://runable.com/")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp.Body, nil
}

// Provider implements provider.Provider for Runable.
type Provider struct {
	name   string
	client *Client
}

// NewProvider builds a Runable provider.
func NewProvider(name, baseURL, deviceHeader string) *Provider {
	return &Provider{name: name, client: New(baseURL, deviceHeader)}
}

// Name returns the configured provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "runable" }

// Client exposes the underlying client for account checks.
func (p *Provider) Client() *Client { return p.client }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) { p.client.SetProxy(u) }

// SetProxySelector routes this provider's outbound requests through the proxy pool
// selected per request by sel (nil = direct). The gateway installs this via a
// type assertion on the Provider (not the Client), so it must live here.
func (p *Provider) SetProxySelector(sel outbound.Selector) { p.client.SetProxySelector(sel) }

// ListModels lists Runable language models.
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	return p.client.ListModels(ctx)
}

// StreamChat opens the upstream chat stream.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	rc, err := p.client.ChatStream(ctx, acc.Cookie, in.ChatID, in.Model, in.Prompt, in.Incognito, in.Mode)
	if err != nil {
		return nil, err
	}
	return &stream{rc: rc, sc: provider.NewSSEScanner(rc)}, nil
}

type stream struct {
	rc io.ReadCloser
	sc *provider.SSEScanner
}

func (s *stream) Close() error { return s.rc.Close() }

func (s *stream) Recv() (provider.Event, error) {
	for {
		data, err := s.sc.Next()
		if err != nil {
			return provider.Event{}, err
		}
		if strings.TrimSpace(data) == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue
		}
		typ := rawString(obj["type"])
		if typ == "" {
			// The original client skips objects that carry a runId but no type.
			continue
		}
		switch typ {
		case "text-delta":
			return provider.Event{Type: provider.EventText, Text: firstNonEmpty(rawString(obj["delta"]), rawString(obj["textDelta"]))}, nil
		case "reasoning-delta":
			return provider.Event{Type: provider.EventReasoning, Text: rawString(obj["delta"])}, nil
		case "finish":
			return provider.Event{Type: provider.EventFinish, Finish: mapFinish(rawString(obj["finishReason"]))}, nil
		case "abort":
			return provider.Event{Type: provider.EventFinish, Finish: "stop"}, nil
		case "error":
			return provider.Event{Type: provider.EventError, Text: firstNonEmpty(rawString(obj["errorText"]), "model error")}, nil
		}
	}
}

// mapFinish is a port of OpenAiBridge.mapFinish.
func mapFinish(s string) string {
	switch s {
	case "length":
		return "length"
	case "tool-calls":
		return "tool_calls"
	case "content-filter":
		return "content_filter"
	default:
		return "stop"
	}
}

func rawString(r json.RawMessage) string {
	if len(r) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func toI64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func maxI64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
