// Package newapi adapts a New API (QuantumNous/new-api) relay — e.g.
// https://api.justwoker.icu — to the provider.Provider interface.
//
// Why a dedicated adapter instead of the generic OpenAI backend: a New API relay
// exposes a *user* surface (/api/user/*) that the gateway needs for the daily
// check-in (which grants a random quota) and for session refresh, and those are
// authenticated by a rotating access-token/session pair — NOT by the sk- API key
// that /v1/* requires. So one account carries three credentials:
//
//	AccessToken (Account.Cookie)         Bearer for /api/user/* (check-in, self)
//	SessionSID  (Account.RefreshToken)   X-Auth-Session for token rotation
//	APIKey      (Account.APIKey)         sk-... for /v1/chat/completions
//
// Chat is plain OpenAI, so this package only adds the auth/lifecycle bits on top
// of the same request shape the OpenAI backend uses.
package newapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

const userAgent = "Mozilla/5.0 (compatible; omnigate/1.0)"

// refreshLead is how long before the access token's stated expiry we rotate it.
const refreshLead = 5 * time.Minute

// UpstreamError carries an HTTP/upstream failure so the gateway can tell an auth
// failure (401) from a generic error.
type UpstreamError struct {
	Status  int
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("上游返回 %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// Unauthorized reports whether the error is an auth failure worth a refresh+retry.
func Unauthorized(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == 401 || ue.Status == 403
	}
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "invalid access token") ||
		strings.Contains(s, "auth_unauthorized") ||
		strings.Contains(s, "unauthorized")
}

// Client talks to one New API relay.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client. baseURL is the relay origin, e.g.
// https://api.justwoker.icu (no trailing /v1).
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// BaseURL returns the relay origin.
func (c *Client) BaseURL() string { return c.baseURL }

// SetProxy routes this client's outbound requests through u (nil = direct).
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
// selected per request by sel (nil = direct).
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

// envelope is the New API JSON wrapper: {success, message, data}.
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// doJSON performs one request and decodes the New API envelope. authHeader is
// the value for the Authorization header (already including "Bearer "), sidHeader
// is the value for X-Auth-Session, and cookie is an optional Cookie header value
// (empty = omit). A non-2xx status or success=false becomes an *UpstreamError so
// callers see the real message.
func (c *Client) doJSON(ctx context.Context, method, endpoint, authHeader, sidHeader, cookie string, body any) (json.RawMessage, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	if sidHeader != "" {
		req.Header.Set("X-Auth-Session", sidHeader)
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env envelope
	_ = json.Unmarshal(raw, &env)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := firstNonEmpty(strings.TrimSpace(env.Message), strings.TrimSpace(env.Code), clip(string(raw)))
		return nil, &UpstreamError{Status: resp.StatusCode, Message: msg}
	}
	if env.Code == "AUTH_UNAUTHORIZED" {
		return nil, &UpstreamError{Status: 401, Message: firstNonEmpty(env.Message, "unauthorized")}
	}
	if !env.Success {
		// success=false 但 200：业务失败（如「今天已经签到过了」），原样上报。
		return nil, &UpstreamError{Status: 0, Message: firstNonEmpty(strings.TrimSpace(env.Message), "上游返回 success=false")}
	}
	if len(env.Data) == 0 {
		return json.RawMessage("{}"), nil
	}
	return env.Data, nil
}

// Bundle is the rotating auth bundle returned by login/refresh.
type Bundle struct {
	AccessToken string
	TokenType   string
	ExpiresAt   int64
	SessionSID  string
	Username    string
	UserID      int64
	Role        int
	User        map[string]any
}

// parseBundle decodes {access_token, token_type, access_expires_at, session, user}.
func parseBundle(data json.RawMessage) (*Bundle, error) {
	var m struct {
		AccessToken     string          `json:"access_token"`
		TokenType       string          `json:"token_type"`
		AccessExpiresAt json.Number     `json:"access_expires_at"`
		Session         map[string]any  `json:"session"`
		User            map[string]any  `json:"user"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析鉴权响应失败: %w", err)
	}
	if strings.TrimSpace(m.AccessToken) == "" {
		return nil, &UpstreamError{Status: 401, Message: "上游未返回 access_token"}
	}
	b := &Bundle{
		AccessToken: strings.TrimSpace(m.AccessToken),
		TokenType:   strings.TrimSpace(m.TokenType),
		SessionSID:  asText(m.Session, "sid"),
		User:        m.User,
	}
	if n, err := m.AccessExpiresAt.Int64(); err == nil {
		b.ExpiresAt = n
	}
	b.Username = asText(m.User, "username")
	if v, ok := toInt64(m.User["id"]); ok {
		b.UserID = v
	}
	if v, ok := toInt64(m.User["role"]); ok {
		b.Role = int(v)
	}
	return b, nil
}

// Refresh rotates the access token using the stored session id (and, when the
// capture managed to read it, the session cookie).
func (c *Client) Refresh(ctx context.Context, sid, cookie string) (*Bundle, error) {
	sid = strings.TrimSpace(sid)
	if sid == "" && strings.TrimSpace(cookie) == "" {
		return nil, &UpstreamError{Status: 401, Message: "缺少 session（X-Auth-Session），请重新登录"}
	}
	data, err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/api/user/auth/refresh", "", sid, cookie, nil)
	if err != nil {
		return nil, err
	}
	return parseBundle(data)
}

// SelfResult is the normalized /api/user/self snapshot.
type SelfResult struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	UserID      int64  `json:"id"`
	Quota       int64  `json:"quota"`
	UsedQuota   int64  `json:"used_quota"`
	Group       string `json:"group,omitempty"`
}

// Self fetches the account's own profile + quota.
func (c *Client) Self(ctx context.Context, accessToken string) (*SelfResult, error) {
	data, err := c.doJSON(ctx, http.MethodGet, c.baseURL+"/api/user/self", bearer(accessToken), "", "", nil)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析 /api/user/self 失败: %w", err)
	}
	out := &SelfResult{
		Username:    asText(m, "username"),
		DisplayName: firstNonEmpty(asText(m, "display_name"), asText(m, "username")),
		Group:       firstNonEmpty(asText(m, "group"), asText(m, "user_group")),
	}
	if v, ok := toInt64(m["id"]); ok {
		out.UserID = v
	}
	if v, ok := toInt64(m["quota"]); ok {
		out.Quota = v
	}
	if v, ok := toInt64(m["used_quota"]); ok {
		out.UsedQuota = v
	}
	return out, nil
}

// CheckinResult is the outcome of the daily check-in.
type CheckinResult struct {
	Success bool   `json:"success"`
	Message string `json:"msg"`
	// Awarded is the quota granted this time (0 when the relay does not report it
	// or when today's reward was already claimed).
	Awarded int64 `json:"awarded"`
	// AlreadyClaimed is true when the relay reports today's reward was taken —
	// not an error, just a neutral "已签到".
	AlreadyClaimed bool `json:"already_claimed"`
}

// Checkin performs the daily check-in (POST /api/user/checkin).
func (c *Client) Checkin(ctx context.Context, accessToken string) (*CheckinResult, error) {
	data, err := c.doJSON(ctx, http.MethodPost, c.baseURL+"/api/user/checkin", bearer(accessToken), "", "", nil)
	if err != nil {
		msg := err.Error()
		if isAlreadyClaimed(msg) {
			return &CheckinResult{Success: false, AlreadyClaimed: true, Message: msg}, nil
		}
		return nil, err
	}
	res := &CheckinResult{Success: true, Message: "签到成功"}
	var m map[string]any
	_ = json.Unmarshal(data, &m)
	for _, key := range []string{"quota_awarded", "quota", "reward", "amount"} {
		if v, ok := toInt64(m[key]); ok && v > 0 {
			res.Awarded = v
			break
		}
	}
	if msg := asText(m, "message"); msg != "" {
		res.Message = msg
	}
	if res.Awarded > 0 {
		res.Message = fmt.Sprintf("签到成功，获得额度 %d", res.Awarded)
	}
	if isAlreadyClaimed(res.Message) {
		res.AlreadyClaimed = true
	}
	return res, nil
}

// isAlreadyClaimed reports whether a message means "today's reward was already
// taken" (a neutral state, not a failure).
func isAlreadyClaimed(msg string) bool {
	s := strings.ToLower(msg)
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "已经签到") ||
		strings.Contains(s, "already") ||
		strings.Contains(s, "repeated") ||
		strings.Contains(s, "请勿重复")
}

// FetchModels lists the relay's models for a chat API key.
func (c *Client) FetchModels(ctx context.Context, apiKey string) ([]openai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")
	if k := stripBearer(apiKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &UpstreamError{Status: resp.StatusCode, Message: clip(string(raw))}
	}
	var ml openai.ModelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ml); err != nil {
		return nil, err
	}
	out := make([]openai.Model, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.Object == "" {
			m.Object = "model"
		}
		if m.OwnedBy == "" {
			m.OwnedBy = "newapi"
		}
		out = append(out, m)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Provider

// Provider implements provider.Provider for a New API relay.
type Provider struct {
	name   string
	client *Client
	models []string
}

// 编译期保证：模型目录要带账号（用其 API Key）才能拿到，实现可选能力。
var _ provider.AccountModelLister = (*Provider)(nil)

// NewProvider builds a New API provider. baseURL is the relay origin; models is
// the fallback catalog used when no API key is available.
func NewProvider(name, baseURL string, models []string) *Provider {
	return &Provider{name: name, client: NewClient(baseURL), models: models}
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "newapi" }

// Client exposes the underlying client.
func (p *Provider) Client() *Client { return p.client }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) { p.client.SetProxy(u) }

// SetProxySelector routes this provider's outbound requests through the proxy pool.
func (p *Provider) SetProxySelector(sel outbound.Selector) { p.client.SetProxySelector(sel) }

// ListModels fetches the catalog anonymously (falls back to the configured list).
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	return p.ListModelsWithAccount(ctx, nil)
}

// ListModelsWithAccount fetches the catalog using the account's chat API key. The
// /v1/models endpoint needs a valid sk- key, so with no key we return the
// configured fallback list (no error — the relay may simply have no key yet).
func (p *Provider) ListModelsWithAccount(ctx context.Context, acc *provider.Account) ([]openai.Model, error) {
	if acc != nil && strings.TrimSpace(acc.APIKey) != "" {
		models, err := p.client.FetchModels(ctx, acc.APIKey)
		if err != nil {
			return p.fallback(), err
		}
		if len(models) > 0 {
			for i := range models {
				if models[i].OwnedBy == "newapi" {
					models[i].OwnedBy = p.name
				}
			}
			return models, nil
		}
	}
	return p.fallback(), nil
}

func (p *Provider) fallback() []openai.Model {
	out := make([]openai.Model, 0, len(p.models))
	for _, id := range p.models {
		out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: p.name})
	}
	return out
}

// StreamChat posts an OpenAI-compatible chat completion using the account's API
// key and streams it back.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	if acc == nil || strings.TrimSpace(acc.APIKey) == "" {
		return nil, &UpstreamError{Status: 401, Message: "该账号未配置 API Key（sk-），无法聊天"}
	}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.baseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Authorization", "Bearer "+stripBearer(acc.APIKey))
	resp, err := p.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	return provider.NewOpenAIStream(resp.Body), nil
}

// TokenExpired reports whether acc's access token is missing or about to expire.
// When the expiry is unknown we optimistically say "valid" and rely on the
// reactive refresh in the gateway (Unauthorized → RefreshAccount → retry).
func (p *Provider) TokenExpired(acc *provider.Account) bool {
	if acc == nil || strings.TrimSpace(acc.Cookie) == "" {
		return true
	}
	if acc.ExpiresAt > 0 {
		return time.Now().Add(refreshLead).Unix() >= acc.ExpiresAt
	}
	if t := tokenExpiry(acc.Cookie); !t.IsZero() {
		return time.Now().Add(refreshLead).After(t)
	}
	return false
}

// RefreshAccount rotates the access token from the stored session id and updates
// acc in place (access token, session id, expiry).
func (p *Provider) RefreshAccount(ctx context.Context, acc *provider.Account) error {
	if acc == nil {
		return fmt.Errorf("账号为空")
	}
	if strings.TrimSpace(acc.RefreshToken) == "" && strings.TrimSpace(acc.SessionCookie) == "" {
		return fmt.Errorf("账号「%s」缺少 session，请重新登录", acc.Label)
	}
	b, err := p.client.Refresh(ctx, acc.RefreshToken, acc.SessionCookie)
	if err != nil {
		return err
	}
	acc.Cookie = b.AccessToken
	if b.SessionSID != "" {
		acc.RefreshToken = b.SessionSID
	}
	if b.ExpiresAt > 0 {
		acc.ExpiresAt = b.ExpiresAt
	}
	return nil
}

// Unauthorized reports whether err is an auth failure worth a refresh+retry.
func (p *Provider) Unauthorized(err error) bool { return Unauthorized(err) }

// ---------------------------------------------------------------------------
// helpers

func bearer(token string) string {
	t := stripBearer(token)
	if t == "" {
		return ""
	}
	return "Bearer " + t
}

// stripBearer removes a leading case-insensitive "bearer " prefix.
func stripBearer(v string) string {
	s := strings.TrimSpace(v)
	if len(s) > len("bearer") && strings.EqualFold(s[:len("bearer")], "bearer") {
		rest := s[len("bearer"):]
		if rest[0] == ' ' || rest[0] == '\t' {
			return strings.TrimSpace(rest)
		}
	}
	return s
}

// tokenExpiry best-effort reads a JWT's exp claim; zero time when unknown.
func tokenExpiry(token string) time.Time {
	parts := strings.Split(stripBearer(token), ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	raw, err := base64URLDecode(payload)
	if err != nil {
		return time.Time{}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return time.Time{}
	}
	if v, ok := toInt64(claims["exp"]); ok && v > 0 {
		return time.Unix(v, 0)
	}
	return time.Time{}
}

func base64URLDecode(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

func asText(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

func toInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case float64:
		return int64(t), true
	case int64:
		return t, true
	case int:
		return int64(t), true
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
