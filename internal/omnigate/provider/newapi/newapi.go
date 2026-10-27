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

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/egress"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/wire"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// browserUA / acceptLanguage mimic the relay's own console, which is the only
// thing that ever legitimately hits /api/user/* and (same-origin) /v1/*. A
// scripted UA ("omnigate/1.0") plus a missing Accept-Language/Referer is a
// needless tell for anti-abuse heuristics, so every request this client makes
// carries the same shape a Chrome tab would send.
const (
	browserUA      = provider.BrowserUA
	acceptLanguage = provider.AcceptLanguage
)

// refreshLead is how long before the access token's stated expiry we rotate it.
const refreshLead = 5 * time.Minute

// UpstreamError carries an HTTP/upstream failure so the gateway can tell an auth
// failure (401) from a generic error.
type UpstreamError struct {
	Status int
	// Code 是上游 envelope 里的业务错误码（如 AUTH_REFRESH_RACE / AUTH_SESSION_MISMATCH），
	// 用于区分「并发刷新」这类可重试错误。
	Code    string
	Message string
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("上游返回 %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// UpstreamStatus exposes the HTTP status to the gateway's route-failure classifier.
func (e *UpstreamError) UpstreamStatus() int { return e.Status }

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
	eps     Endpoints
	egress  egress.Profile
}

// NewClient builds a client. baseURL is the relay origin, e.g.
// https://api.justwoker.icu (no trailing /v1).
func NewClient(baseURL string) *Client {
	return NewClientWithOptions(baseURL, Endpoints{}, egress.Profile{})
}

// NewClientWithOptions builds a client with an explicit endpoint surface and
// outbound identity. Zero values keep the legacy behaviour exactly (default
// paths, browser identity), so every existing config is unaffected.
func NewClientWithOptions(baseURL string, eps Endpoints, prof egress.Profile) *Client {
	c := &Client{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		eps:     eps.Normalized(),
		egress:  prof.Normalized(),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
	c.applyTLS()
	return c
}

// applyTLS installs the utls ClientHello the identity profile asks for. A CLI
// preset does this because the real Claude Code / Codex clients ship a non-Go TLS
// stack; presenting Go's ClientHello while claiming to be them is exactly the
// mismatch this layer exists to remove. No-op when no profile asks for it.
func (c *Client) applyTLS() {
	tr, ok := c.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	target := c.egress.ResolveWith(egress.PresetBrowser).TLSProfile
	imp := fingerprint.NewTLSImpersonator(target)
	if target == "" || !imp.Enabled() {
		tr.DialTLSContext = nil
		tr.CloseIdleConnections()
		return
	}
	imp.Apply(tr, &net.Dialer{Timeout: 20 * time.Second})
	tr.ForceAttemptHTTP2 = true
	tr.CloseIdleConnections()
}

// Endpoints returns the resolved endpoint surface.
func (c *Client) Endpoints() Endpoints { return c.eps }

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

// setBrowserHeaders stamps req with the headers a same-origin XHR from the
// relay's console would carry: browsers attach Origin to every non-GET/HEAD
// request, but Content-Type only when there is a body. hasBody marks the latter;
// accept overrides the default Accept header when non-empty.
//
// The identity itself comes from egress: with no configured profile this resolves
// to PresetBrowser, which is byte-for-byte what this client sent before. When the
// operator picks a CLI preset (claude-code / codex), the browser-only Referer and
// Origin are omitted — a CLI never sends them, so keeping them would make the
// impersonation internally inconsistent.
func (c *Client) setBrowserHeaders(req *http.Request, hasBody bool, accept string) {
	if req == nil {
		return
	}
	prof := egress.Profile{}
	if c != nil {
		prof = c.egress
	}
	resolved := prof.ResolveWith(egress.PresetBrowser)
	resolved.Apply(req)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	if c != nil && c.baseURL != "" && resolved.Preset == egress.PresetBrowser {
		req.Header.Set("Referer", c.baseURL+"/")
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			req.Header.Set("Origin", c.baseURL)
		}
	}
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
	return c.doUserJSON(ctx, method, endpoint, Auth{Token: authHeader, SessionSID: sidHeader, Cookie: cookie}, body)
}

// Auth carries every credential shape a New API relay's user API accepts.
//
// The adapter originally assumed one shape (a rotating bearer access token).
// AnyRouter and AgentRouter instead authenticate the user API with the browser
// session cookie plus a numeric identity header, and have no access token at all
// — so the shape has to be a parameter, not an assumption.
type Auth struct {
	// Token is the access token value (without "Bearer ") when known.
	Token string
	// SessionSID is sent as X-Auth-Session during rotation.
	SessionSID string
	// Cookie is the raw Cookie header value (session=…).
	Cookie string
	// UserID is the numeric user id, sent via Endpoints.UserHeader.
	UserID string
}

// doUserJSON performs one user-API request using a fully specified credential
// set. Authentication follows the configured AuthMode:
//   - bearer: Authorization: Bearer <token>
//   - cookie: Cookie: <cookie> (and no Authorization header at all)
//
// Endpoints.UserHeader is stamped whenever UserID is known, in either mode: the
// header is what New API deployments use to pick the account, independent of how
// the request was authenticated.
func (c *Client) doUserJSON(ctx context.Context, method, endpoint string, auth Auth, body any) (json.RawMessage, error) {
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
	c.setBrowserHeaders(req, body != nil, "application/json")
	switch c.eps.AuthMode {
	case AuthCookie:
		if auth.Cookie != "" {
			req.Header.Set("Cookie", auth.Cookie)
		} else if auth.SessionSID != "" {
			// A captured session id is usually the same value as the cookie's
			// "session=" payload; fall back to it so cookie-auth sites work from
			// a session-only capture.
			req.Header.Set("Cookie", "session="+auth.SessionSID)
		}
	default:
		if a := bearer(auth.Token); a != "" {
			req.Header.Set("Authorization", a)
		}
		if auth.SessionSID != "" {
			req.Header.Set("X-Auth-Session", auth.SessionSID)
		}
		if auth.Cookie != "" {
			req.Header.Set("Cookie", auth.Cookie)
		}
	}
	if c.eps.UserHeader != "" && auth.UserID != "" {
		req.Header.Set(c.eps.UserHeader, auth.UserID)
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
		return nil, &UpstreamError{Status: resp.StatusCode, Code: strings.TrimSpace(env.Code), Message: msg}
	}
	if env.Code == "AUTH_UNAUTHORIZED" {
		return nil, &UpstreamError{Status: 401, Code: env.Code, Message: firstNonEmpty(env.Message, "unauthorized")}
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
		AccessToken     string         `json:"access_token"`
		TokenType       string         `json:"token_type"`
		AccessExpiresAt json.Number    `json:"access_expires_at"`
		Session         map[string]any `json:"session"`
		User            map[string]any `json:"user"`
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
//
// session.sid 是**稳定**的（刷新只换 access_token，sid 不变；上游还会校验返回的
// sid 必须与请求一致），所以浏览器标签页与面板可以共用同一个 sid。唯一要处理的是
// 并发刷新：上游对同 session 的并发刷新回 409 AUTH_REFRESH_RACE，稍后重试即可。
//
// Cookie-auth relays (AnyRouter/AgentRouter) have no refresh endpoint: they keep
// the browser session alive as long as it is used, so Refresh reports a clear
// "not supported" instead of pretending to succeed.
func (c *Client) Refresh(ctx context.Context, sid, cookie, userID string) (*Bundle, error) {
	sid = strings.TrimSpace(sid)
	if c.eps.UsesCookieAuth() {
		if strings.TrimSpace(cookie) == "" && sid == "" {
			return nil, &UpstreamError{Status: 401, Message: "缺少浏览器会话 Cookie，请重新登录"}
		}
		return nil, &UpstreamError{Status: 501, Message: "该站点用浏览器 Cookie 鉴权，没有可轮换的 access_token"}
	}
	if sid == "" && strings.TrimSpace(cookie) == "" {
		return nil, &UpstreamError{Status: 401, Message: "缺少 session（X-Auth-Session），请重新登录"}
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 400 * time.Millisecond):
			}
		}
		data, err := c.doUserJSON(ctx, http.MethodPost, c.baseURL+c.eps.RefreshPath,
			Auth{SessionSID: sid, Cookie: cookie, UserID: userID}, nil)
		if err == nil {
			return parseBundle(data)
		}
		lastErr = err
		var ue *UpstreamError
		if errors.As(err, &ue) && ue.Code == "AUTH_REFRESH_RACE" {
			continue
		}
		return nil, err
	}
	return nil, lastErr
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

// AuthFor builds the user-API credential set for an account. It understands both
// credential shapes: a bearer access token (Cookie field for this adapter's
// historical layout) and a browser session cookie (SessionCookie).
func AuthFor(acc *provider.Account) Auth {
	if acc == nil {
		return Auth{}
	}
	a := Auth{
		Token:      stripBearer(acc.Cookie),
		SessionSID: stripBearer(acc.RefreshToken),
		Cookie:     strings.TrimSpace(acc.SessionCookie),
		UserID:     strings.TrimSpace(acc.UID),
	}
	return a
}

// Self fetches the account's own profile + quota.
func (c *Client) Self(ctx context.Context, acc *provider.Account) (*SelfResult, error) {
	data, err := c.doUserJSON(ctx, http.MethodGet, c.baseURL+c.eps.SelfPath, AuthFor(acc), nil)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", c.eps.SelfPath, err)
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

// Checkin performs the daily check-in (POST Endpoints.CheckinPath).
func (c *Client) Checkin(ctx context.Context, acc *provider.Account) (*CheckinResult, error) {
	data, err := c.doUserJSON(ctx, http.MethodPost, c.baseURL+c.eps.CheckinPath, AuthFor(acc), nil)
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+c.eps.ModelsPath, nil)
	if err != nil {
		return nil, err
	}
	c.setBrowserHeaders(req, false, "application/json")
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
	format wire.Format
}

// 编译期保证：模型目录要带账号（用其 API Key）才能拿到，实现可选能力。
var _ provider.AccountModelLister = (*Provider)(nil)

// Options configures a New API relay provider.
type Options struct {
	Name    string
	BaseURL string
	Models  []string
	// Endpoints overrides the relay's user-API surface. Zero = the legacy
	// defaults (api.justwoker.icu). AnyRouter/AgentRouter set sign_in paths,
	// New-Api-User and cookie auth here.
	Endpoints Endpoints
	// Client is the outbound identity (browser by default for this type).
	Client egress.Profile
	// Protocol is the upstream chat dialect: openai (default), anthropic or
	// responses. AnyRouter/AgentRouter only accept /v1/messages.
	Protocol string
}

// NewProvider builds a New API provider. baseURL is the relay origin; models is
// the fallback catalog used when no API key is available.
func NewProvider(name, baseURL string, models []string) *Provider {
	return NewProviderWithOptions(Options{Name: name, BaseURL: baseURL, Models: models})
}

// NewProviderWithOptions builds a provider with an explicit endpoint surface and
// outbound identity.
func NewProviderWithOptions(o Options) *Provider {
	return &Provider{
		name:   o.Name,
		client: NewClientWithOptions(o.BaseURL, o.Endpoints, o.Client),
		models: o.Models,
		format: wire.ParseFormat(o.Protocol),
	}
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

// StreamChat posts a chat completion using the account's API key and streams it
// back. The dialect follows the configured protocol: relays that only accept
// Claude Code traffic (AnyRouter) are spoken to in /v1/messages, everything else
// stays on /v1/chat/completions.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	if acc == nil || strings.TrimSpace(acc.APIKey) == "" {
		return nil, &UpstreamError{Status: 401, Message: "该账号未配置 API Key（sk-），无法聊天"}
	}
	format := p.format
	var (
		raw  []byte
		err  error
		path = p.client.eps.ChatPath
	)
	switch format {
	case wire.FormatAnthropic:
		raw, err = wire.EncodeAnthropic(in)
		path = "/v1/messages"
	case wire.FormatResponses:
		raw, err = wire.EncodeResponses(in)
		path = "/v1/responses"
	default:
		format = wire.FormatOpenAI
		raw, err = p.encodeOpenAI(in)
	}
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if format == wire.FormatAnthropic {
		// /v1/messages authenticates with x-api-key, and stays with the identity
		// the operator configured (Claude Code, normally).
		p.client.setBrowserHeaders(req, true, "text/event-stream")
		req.Header.Set("x-api-key", stripBearer(acc.APIKey))
		req.Header.Del("Authorization")
	} else {
		p.client.setBrowserHeaders(req, true, "text/event-stream")
		req.Header.Set("Authorization", "Bearer "+stripBearer(acc.APIKey))
	}
	resp, err := p.client.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, &UpstreamError{Status: resp.StatusCode, Message: strings.TrimSpace(string(b))}
	}
	switch format {
	case wire.FormatAnthropic:
		return wire.NewAnthropicStream(resp.Body), nil
	case wire.FormatResponses:
		return wire.NewResponsesStream(resp.Body), nil
	default:
		return provider.NewOpenAIStream(resp.Body), nil
	}
}

// encodeOpenAI builds the standard chat/completions body.
func (p *Provider) encodeOpenAI(in provider.ChatInput) ([]byte, error) {
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
	return json.Marshal(body)
}

// SetProtocol switches the upstream dialect (hot reload).
func (p *Provider) SetProtocol(protocol string) {
	p.format = wire.ParseFormat(protocol)
}

// Protocol returns the upstream dialect name.
func (p *Provider) Protocol() string { return string(p.format) }

// TokenExpired reports whether acc's user-API credentials are missing or about to
// expire. Cookie-auth relays (AnyRouter/AgentRouter) have no access token at all,
// so a session cookie is a valid, non-expiring credential from our point of view.
func (p *Provider) TokenExpired(acc *provider.Account) bool {
	if acc == nil {
		return true
	}
	if strings.TrimSpace(acc.Cookie) == "" {
		// Cookie-mode accounts: the session cookie is the credential.
		return strings.TrimSpace(acc.SessionCookie) == ""
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
//
// Cookie-auth relays (AnyRouter / AgentRouter) have no access token to rotate:
// the browser session cookie *is* the credential, and refreshing it is the
// browser's job. Rotating there would either 404 or, worse, invalidate the
// session the user is still using — so it is a successful no-op.
func (p *Provider) RefreshAccount(ctx context.Context, acc *provider.Account) error {
	if acc == nil {
		return fmt.Errorf("账号为空")
	}
	if p.client.eps.UsesCookieAuth() && strings.TrimSpace(acc.Cookie) == "" {
		if strings.TrimSpace(acc.SessionCookie) == "" {
			return fmt.Errorf("账号「%s」缺少会话 Cookie，请重新登录", acc.Label)
		}
		return nil
	}
	if strings.TrimSpace(acc.RefreshToken) == "" && strings.TrimSpace(acc.SessionCookie) == "" {
		return fmt.Errorf("账号「%s」缺少 session，请重新登录", acc.Label)
	}
	b, err := p.client.Refresh(ctx, acc.RefreshToken, acc.SessionCookie, acc.UID)
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
