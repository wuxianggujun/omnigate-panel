// Package raccoon adapts 办公小浣熊 (xiaohuanxiong.com) to the provider.Provider
// interface. It is a faithful port of the original APK's RaccoonProvider: the
// upstream LLM endpoint is already OpenAI-compatible, so this adapter mainly
// adds the OAuth authorization-code login, token refresh, balance query and the
// desktop "login points grant" check-in.
package raccoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// Default upstream bases, matching the values hard-coded in the original APK.
const (
	DefaultMainOrigin = "https://xiaohuanxiong.com"
	LLMPath           = "/api/web/llm/v2"
	AuthPath          = "/api/web/auth/v1"

	// AppName is the desktop client name sent on the authorize page.
	AppName = "办公小浣熊客户端"
	// CallbackPrefix is the custom-scheme redirect the authorize page uses.
	CallbackPrefix = "office-raccoon://auth/callback"

	// CodeExpired is the upstream code meaning "authorization code expired".
	CodeExpired = 200035
)

const userAgent = "Mozilla/5.0 (compatible; omnigate/1.0)"

// UpstreamError carries an HTTP/upstream failure.
type UpstreamError struct {
	Status  int
	Message string
	Code    int64
}

func (e *UpstreamError) Error() string {
	if e.Status > 0 {
		return fmt.Sprintf("上游返回 %d: %s", e.Status, e.Message)
	}
	return e.Message
}

// Unauthorized reports whether the error is an auth failure (401 / expired).
func Unauthorized(err error) bool {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == 401
	}
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "登录态已过期")
}

// Client talks to the Raccoon web API.
type Client struct {
	mainOrigin string
	llmBase    string
	authBase   string
	http       *http.Client
}

// NewClient builds a Raccoon client. Empty bases fall back to the defaults.
func NewClient(mainOrigin, llmBase, authBase string) *Client {
	mainOrigin = strings.TrimRight(strings.TrimSpace(mainOrigin), "/")
	if mainOrigin == "" {
		mainOrigin = DefaultMainOrigin
	}
	if llmBase == "" {
		llmBase = mainOrigin + LLMPath
	}
	if authBase == "" {
		authBase = mainOrigin + AuthPath
	}
	return &Client{
		mainOrigin: mainOrigin,
		llmBase:    strings.TrimRight(llmBase, "/"),
		authBase:   strings.TrimRight(authBase, "/"),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
}

// MainOrigin returns the site origin.
func (c *Client) MainOrigin() string { return c.mainOrigin }

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

// BuildAuthorizeURL returns the browser URL the user opens to log in.
func (c *Client) BuildAuthorizeURL(state string) string {
	q := url.Values{}
	q.Set("login_source", "desktop")
	q.Set("appname", AppName)
	q.Set("state", state)
	return c.mainOrigin + "/code/authorize?" + q.Encode()
}

// BuildAuthorizeURLWithRedirect returns a browser URL that, after the user logs
// in and passes the captcha, sends the authorization code to redirectURL (via
// window.open("<redirectURL>&authorization_code=<code>")) instead of the desktop
// custom scheme. This lets a server capture the code automatically — no manual
// callback paste. The authorize page picks this branch when login_source is not
// "desktop" and a "redirect" param is present.
func (c *Client) BuildAuthorizeURLWithRedirect(redirectURL string) string {
	q := url.Values{}
	q.Set("appname", AppName)
	q.Set("redirect", redirectURL)
	return c.mainOrigin + "/code/authorize?" + q.Encode()
}

// ParseCallback extracts (code, state) from an office-raccoon:// callback URL.
func ParseCallback(raw string) (code, state string, ok bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed != CallbackPrefix && !strings.HasPrefix(trimmed, CallbackPrefix+"?") {
		return "", "", false
	}
	rest := trimmed[len(CallbackPrefix):]
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimPrefix(rest, "?")
	values, err := url.ParseQuery(rest)
	if err != nil {
		return "", "", false
	}
	code = strings.TrimSpace(values.Get("code"))
	state = strings.TrimSpace(values.Get("state"))
	if code == "" || len(code) > 8192 || state == "" || len(state) > 512 {
		return "", "", false
	}
	return code, state, true
}

// ---------------------------------------------------------------------------
// JSON helpers

type rawResponse struct {
	status  int
	payload map[string]any
	body    string
}

func (c *Client) requestJSON(ctx context.Context, method, endpoint string, headers map[string]string, body []byte) (*rawResponse, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	out := &rawResponse{status: resp.StatusCode, body: string(raw)}
	if strings.TrimSpace(out.body) != "" {
		_ = json.Unmarshal(raw, &out.payload)
	}
	return out, nil
}

// unwrapData mirrors the APK's unwrapData: 401 -> auth error, non-2xx -> HTTP
// error, envelope code != 0 -> upstream error, otherwise return data (or the
// whole payload when there is no data object).
func unwrapData(r *rawResponse, structureMsg string) (map[string]any, error) {
	if r.status == 401 {
		return nil, &UpstreamError{Status: 401, Message: "登录态已过期"}
	}
	if r.status < 200 || r.status >= 300 {
		return nil, &UpstreamError{Status: r.status, Message: fmt.Sprintf("HTTP %d", r.status)}
	}
	if r.payload == nil {
		return nil, fmt.Errorf("%s", structureMsg)
	}
	if v, ok := r.payload["code"]; ok {
		if code := toInt64(v); code != 0 {
			msg := firstNonEmpty(strOf(r.payload, "message"), fmt.Sprintf("返回异常 code=%d", code))
			return nil, &UpstreamError{Message: msg, Code: code}
		}
	}
	if d, ok := r.payload["data"].(map[string]any); ok {
		return d, nil
	}
	return r.payload, nil
}

func strOf(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return n
	default:
		return 0
	}
}

func toInt64Ptr(v any) (int64, bool) {
	if v == nil {
		return 0, false
	}
	switch v.(type) {
	case float64, int64, int, json.Number, string:
		return toInt64(v), true
	default:
		return 0, false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// JWT helpers

// StripBearer removes a leading case-insensitive "bearer " prefix.
func StripBearer(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= len("bearer") {
		return trimmed
	}
	if !strings.EqualFold(trimmed[:len("bearer")], "bearer") {
		return trimmed
	}
	rest := trimmed[len("bearer"):]
	if rest == "" || rest[0] == ' ' || rest[0] == '\t' {
		return strings.TrimSpace(rest)
	}
	return trimmed
}

func decodeBase64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// DecodeClaims decodes the (unverified) JWT payload.
func DecodeClaims(token string) map[string]any {
	parts := strings.Split(StripBearer(token), ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil
	}
	b, err := decodeBase64URL(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return nil
	}
	return claims
}

// TokenExpiry returns the token's exp time, or the zero time when unknown.
func TokenExpiry(token string) time.Time {
	claims := DecodeClaims(token)
	if claims == nil {
		return time.Time{}
	}
	exp, ok := claims["exp"]
	if !ok {
		return time.Time{}
	}
	var secs float64
	switch v := exp.(type) {
	case float64:
		secs = v
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return time.Time{}
		}
		secs = f
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return time.Time{}
		}
		secs = f
	default:
		return time.Time{}
	}
	if secs <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(secs), 0)
}

// ExtractUserID derives a stable account id from the token claims.
func ExtractUserID(claims map[string]any) string {
	for _, key := range []string{"sid", "iss"} {
		if v := asText(claims, key); v != "" {
			if d := hexToDecimal(v); d != "" {
				return d
			}
		}
	}
	for _, key := range []string{"sub", "id", "user_id"} {
		if v := strings.TrimSpace(asText(claims, key)); v != "" {
			return v
		}
	}
	return ""
}

// DisplayNameOf picks a human name from a claims/data object.
func DisplayNameOf(obj map[string]any) string {
	for _, key := range []string{"nickname", "nick_name", "user_name", "name", "username", "phone", "mobile"} {
		if v := strings.TrimSpace(asText(obj, key)); v != "" {
			return v
		}
	}
	return ""
}

func asText(obj map[string]any, key string) string {
	if obj == nil {
		return ""
	}
	v, ok := obj[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func hexToDecimal(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "0x")
	s = strings.TrimPrefix(s, "0X")
	if s == "" {
		return ""
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	n := new(big.Int)
	if _, ok := n.SetString(s, 16); !ok {
		return ""
	}
	return n.String()
}

// ---------------------------------------------------------------------------
// Auth

// ExchangeResult is the outcome of a successful authorization-code exchange.
type ExchangeResult struct {
	AccessToken  string
	RefreshToken string
	Identity     string
	OrgName      string
	OrgRole      string
	DisplayName  string
}

// ExchangeCode trades an authorization code for access/refresh tokens.
func (c *Client) ExchangeCode(ctx context.Context, code string) (*ExchangeResult, error) {
	body, _ := json.Marshal(map[string]any{"authorization_code": code})
	headers := map[string]string{"Content-Type": "application/json"}
	r, err := c.requestJSON(ctx, http.MethodPost, c.authBase+"/login_with_authorization_code", headers, body)
	if err != nil {
		return nil, err
	}
	payload := r.payload
	if payload == nil {
		payload = map[string]any{}
	}
	if v, ok := payload["code"]; ok {
		if toInt64(v) == CodeExpired {
			return nil, &UpstreamError{Status: 400, Message: "授权码已失效，请重新发起网页登录", Code: CodeExpired}
		}
	}
	if r.status < 200 || r.status >= 300 {
		msg := firstNonEmpty(strOf(payload, "message"), strOf(payload, "msg"),
			fmt.Sprintf("网页登录换取凭证失败（HTTP %d）", r.status))
		if msg == "internal_server_error" {
			msg = "登录服务异常，请稍后重试"
		}
		return nil, &UpstreamError{Status: r.status, Message: msg}
	}
	data, _ := payload["data"].(map[string]any)
	access := strOf(data, "access_token")
	refresh := strOf(data, "refresh_token")
	if access == "" || refresh == "" {
		return nil, &UpstreamError{Status: 502, Message: fmt.Sprintf("网页登录换取凭证失败（HTTP %d）", r.status)}
	}
	return &ExchangeResult{
		AccessToken:  access,
		RefreshToken: refresh,
		Identity:     strings.TrimSpace(strOf(data, "office_identity")),
		OrgName:      strings.TrimSpace(strOf(data, "office_org_name")),
		OrgRole:      strings.TrimSpace(strOf(data, "office_org_role")),
		DisplayName:  DisplayNameOf(data),
	}, nil
}

// RefreshResult is the outcome of a token refresh.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
}

// Refresh mints a fresh access token from a refresh token.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*RefreshResult, error) {
	body, _ := json.Marshal(map[string]any{"refresh_token": refreshToken})
	headers := map[string]string{"Content-Type": "application/json"}
	r, err := c.requestJSON(ctx, http.MethodPost, c.authBase+"/refresh", headers, body)
	if err != nil {
		return nil, err
	}
	payload := r.payload
	if payload == nil {
		payload = map[string]any{}
	}
	if r.status < 200 || r.status >= 300 {
		msg := firstNonEmpty(strOf(payload, "message"), strOf(payload, "msg"), "服务器未返回错误说明")
		status := 502
		if r.status == 401 {
			status = 401
		}
		return nil, &UpstreamError{Status: status, Message: fmt.Sprintf("小浣熊刷新接口失败（HTTP %d）: %s", r.status, msg)}
	}
	data, _ := payload["data"].(map[string]any)
	access := strOf(data, "access_token")
	if access == "" {
		code := firstNonEmpty(strOf(payload, "code"), "null")
		msg := firstNonEmpty(strOf(payload, "message"), strOf(payload, "msg"), "服务器未返回新 token")
		return nil, &UpstreamError{Status: 401, Message: fmt.Sprintf("小浣熊 token 刷新失败 code=%s msg=%s", code, msg)}
	}
	next := strOf(data, "refresh_token")
	if next == "" {
		next = refreshToken
	}
	return &RefreshResult{AccessToken: access, RefreshToken: next}, nil
}

// ---------------------------------------------------------------------------
// Balance

// Wallet is one points bucket.
type Wallet struct {
	Type        string `json:"type"`
	DisplayName string `json:"displayName"`
	Balance     int64  `json:"balance"`
}

// Balance is the normalized points/subscription snapshot.
type Balance struct {
	Available       int64    `json:"available"`
	Unit            string   `json:"unit"`
	Wallets         []Wallet `json:"wallets"`
	Subscription    any      `json:"subscription"`
	SubscriptionErr string   `json:"subscriptionError,omitempty"`
}

// QueryBalance fetches points balance (and, best-effort, subscription info).
func (c *Client) QueryBalance(ctx context.Context, token string) (*Balance, error) {
	bearer := "Bearer " + StripBearer(token)
	headers := map[string]string{"Accept": "application/json", "Authorization": bearer}

	points, err := c.requestJSON(ctx, http.MethodGet, c.mainOrigin+"/api/web/points/v1/balance", headers, nil)
	if err != nil {
		return nil, err
	}
	data, err := unwrapData(points, "积分余额响应结构异常")
	if err != nil {
		return nil, err
	}
	out := &Balance{Unit: "积分"}
	if v, ok := toInt64Ptr(data["available_points"]); ok {
		out.Available = v
	}
	labels := []struct{ key, label string }{
		{"daily_points", "每日积分"},
		{"monthly_points", "每月会员积分"},
		{"reward_points", "奖励积分"},
		{"topup_points", "充值积分"},
	}
	for _, w := range labels {
		if v, ok := toInt64Ptr(data[w.key]); ok {
			out.Wallets = append(out.Wallets, Wallet{Type: w.key, DisplayName: w.label, Balance: v})
		}
	}
	if ent, err := c.requestJSON(ctx, http.MethodGet, c.mainOrigin+"/api/web/auth/v1/entitlement_info", headers, nil); err == nil {
		if ed, err := unwrapData(ent, "订阅响应结构异常"); err == nil {
			out.Subscription = normalizeSubscription(ed)
		} else {
			out.SubscriptionErr = err.Error()
		}
	} else {
		out.SubscriptionErr = err.Error()
	}
	return out, nil
}

func normalizeSubscription(data map[string]any) any {
	office, _ := data["office"].(map[string]any)
	codeEnt, _ := data["code"].(map[string]any)
	if office == nil && codeEnt == nil {
		return nil
	}
	proEnabled := false
	if office != nil {
		proEnabled, _ = office["pro_enable"].(bool)
	} else if codeEnt != nil {
		proEnabled, _ = codeEnt["pro_enable"].(bool)
	}
	planType := pick(office, codeEnt, "plan_type")
	if active, ok := pick(office, codeEnt, "active_plan").(map[string]any); ok {
		if t := active["type"]; t != nil {
			planType = t
		}
	}
	expireAt := pick(office, codeEnt, "pro_expired_time")
	if expireAt == nil {
		if active, ok := pick(office, codeEnt, "active_plan").(map[string]any); ok {
			expireAt = active["subscription_expired_at"]
		}
	}
	planName := ""
	if s, ok := planType.(string); ok {
		planName = s
	}
	status := "none"
	if proEnabled {
		status = "active"
	}
	return map[string]any{"planName": planName, "status": status, "expireAt": expireAt}
}

func pick(office, codeEnt map[string]any, key string) any {
	if office != nil {
		if v, ok := office[key]; ok && v != nil {
			return v
		}
	}
	if codeEnt != nil {
		if v, ok := codeEnt[key]; ok && v != nil {
			return v
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Check-in

// Grant is one points credit today.
type Grant struct {
	Name   string `json:"name"`
	Points int64  `json:"points"`
	At     string `json:"at"`
}

// CheckinResult is the outcome of the desktop login-reward grant.
type CheckinResult struct {
	Success        bool    `json:"success"`
	Message        string  `json:"msg"`
	GrantsToday    []Grant `json:"grantsToday"`
	DesktopGranted bool    `json:"desktopGranted"`
	DesktopError   string  `json:"desktopError,omitempty"`
	GrantsError    string  `json:"grantsError,omitempty"`
}

// Checkin claims the desktop login points grant, then reconciles today's bills.
func (c *Client) Checkin(ctx context.Context, token string) (*CheckinResult, error) {
	bearer := "Bearer " + StripBearer(token)
	grantHeaders := map[string]string{
		"Accept":            "application/json",
		"Authorization":     bearer,
		"X-Client-Platform": "desktop-windows",
		"X-Client-Version":  "v1.0.0",
	}

	res := &CheckinResult{}
	if r, err := c.requestJSON(ctx, http.MethodPost, c.mainOrigin+"/api/web/desktop/v1/login/points/grant", grantHeaders, nil); err == nil {
		if data, err := unwrapData(r, "签到响应结构异常"); err == nil {
			res.DesktopGranted, _ = data["granted"].(bool)
		} else {
			res.DesktopError = err.Error()
		}
	} else {
		res.DesktopError = err.Error()
	}

	today := time.Now().Format("2006-01-02")
	billHeaders := map[string]string{"Accept": "application/json", "Authorization": bearer}
	billsURL := c.mainOrigin + "/api/web/points/v1/bills?paging.limit=20&paging.offset=0"
	var todayPoints int64
	if r, err := c.requestJSON(ctx, http.MethodGet, billsURL, billHeaders, nil); err == nil {
		if data, err := unwrapData(r, "账单响应结构异常"); err == nil {
			if items, ok := data["items"].([]any); ok {
				for _, it := range items {
					item, ok := it.(map[string]any)
					if !ok {
						continue
					}
					points, ok := toInt64Ptr(item["points"])
					if !ok || points <= 0 {
						continue
					}
					created := strOf(item, "created_at")
					if len(created) < 10 || created[:10] != today {
						continue
					}
					name := strOf(item, "event_name")
					if name == "" {
						name = "积分发放"
					}
					res.GrantsToday = append(res.GrantsToday, Grant{
						Name:   name,
						Points: points,
						At:     clipClock(created),
					})
					todayPoints += points
				}
			}
		} else {
			res.GrantsError = err.Error()
		}
	} else {
		res.GrantsError = err.Error()
	}

	notes := []string{}
	if res.DesktopGranted {
		notes = append(notes, "桌面登录奖励已发放")
	}
	switch {
	case todayPoints > 0:
		notes = append(notes, fmt.Sprintf("今日积分 +%d", todayPoints))
	case res.GrantsError != "":
		notes = append(notes, "未能核对今日入账")
	default:
		notes = append(notes, "今日未见积分入账（可能已领取）")
	}
	if res.GrantsError != "" {
		notes = append(notes, "账单核对失败："+res.GrantsError)
	} else if res.DesktopError != "" {
		notes = append(notes, "桌面登录奖励未领取")
	}
	res.Success = todayPoints > 0
	res.Message = strings.Join(notes, "；")
	return res, nil
}

func clipClock(created string) string {
	s := strings.ReplaceAll(created, "T", " ")
	if len(s) >= 19 {
		return s[11:19]
	}
	return s
}

// ---------------------------------------------------------------------------
// Models

// FetchModelCatalog lists upstream models for a token (token may be empty).
func (c *Client) FetchModelCatalog(ctx context.Context, token string) ([]openai.Model, error) {
	headers := map[string]string{"Accept": "application/json"}
	if t := StripBearer(token); t != "" {
		headers["Authorization"] = "Bearer " + t
	}
	r, err := c.requestJSON(ctx, http.MethodGet, c.llmBase+"/model_catalog", headers, nil)
	if err != nil {
		return nil, err
	}
	if r.status < 200 || r.status >= 300 {
		return nil, &UpstreamError{Status: r.status, Message: fmt.Sprintf("上游返回 HTTP %d", r.status)}
	}
	return parseCatalog(r.payload), nil
}

func parseCatalog(payload map[string]any) []openai.Model {
	data := payload
	if d, ok := payload["data"].(map[string]any); ok {
		data = d
	}
	categories, _ := data["categories"].([]any)
	seen := map[string]bool{}
	out := []openai.Model{}
	for _, c := range categories {
		category, ok := c.(map[string]any)
		if !ok {
			continue
		}
		entries, _ := category["models"].([]any)
		for _, e := range entries {
			entry, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id := strings.TrimSpace(firstNonEmpty(strOf(entry, "name"), strOf(entry, "model_name")))
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			params, _ := entry["params"].(map[string]any)
			out = append(out, openai.Model{
				ID:            id,
				Object:        "model",
				OwnedBy:       "raccoon",
				ContextWindow: pickInt(entry, params, "context_window", "contextWindow"),
				MaxTokens:     pickInt(entry, params, "max_tokens", "maxTokens"),
			})
		}
	}
	return out
}

func pickInt(entry, params map[string]any, keys ...string) int64 {
	for _, key := range keys {
		if v, ok := toInt64Ptr(entry[key]); ok {
			return v
		}
		if params != nil {
			if v, ok := toInt64Ptr(params[key]); ok {
				return v
			}
		}
	}
	return 0
}

// fallbackModels mirror the APK's hard-coded defaults.
func fallbackModels(owner string) []openai.Model {
	specs := []struct {
		id, ctx, max int64
		ids          string
	}{
		{ids: "raccoon-8c4485", ctx: 1000000, max: 100000},
		{ids: "raccoon-19b265", ctx: 1000000, max: 100000},
		{ids: "raccoon-405a1c", ctx: 1000000, max: 100000},
		{ids: "raccoon-chat-ml-5-5", ctx: 180000, max: 80000},
		{ids: "sn-sensenova-6-8-flash-lite", ctx: 256000, max: 63999},
	}
	out := make([]openai.Model, 0, len(specs))
	for _, s := range specs {
		out = append(out, openai.Model{ID: s.ids, Object: "model", OwnedBy: owner, ContextWindow: s.ctx, MaxTokens: s.max})
	}
	return out
}

// ---------------------------------------------------------------------------
// Provider

// Provider implements provider.Provider for Raccoon.
type Provider struct {
	name   string
	client *Client
}

// NewProvider builds a Raccoon provider.
func NewProvider(name, mainOrigin, llmBase, authBase string) *Provider {
	return &Provider{name: name, client: NewClient(mainOrigin, llmBase, authBase)}
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "raccoon" }

// Client exposes the underlying client.
func (p *Provider) Client() *Client { return p.client }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) { p.client.SetProxy(u) }

// ListModels fetches the upstream catalog, falling back to built-in defaults.
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	models, err := p.client.FetchModelCatalog(ctx, "")
	if err != nil || len(models) == 0 {
		return fallbackModels(p.name), nil
	}
	for i := range models {
		if models[i].OwnedBy == "raccoon" {
			models[i].OwnedBy = p.name
		}
	}
	return models, nil
}

// StreamChat posts an OpenAI-compatible chat completion and streams it back.
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.client.llmBase+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+StripBearer(acc.Cookie))
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
func (p *Provider) TokenExpired(acc *provider.Account) bool {
	if acc == nil || acc.Cookie == "" {
		return true
	}
	exp := TokenExpiry(acc.Cookie)
	if exp.IsZero() {
		return false // not a JWT we can read; assume still valid
	}
	return time.Now().Add(60 * time.Second).After(exp)
}

// RefreshAccount mints a fresh access token and updates acc in place.
func (p *Provider) RefreshAccount(ctx context.Context, acc *provider.Account) error {
	if acc.RefreshToken == "" {
		return fmt.Errorf("账号「%s」缺少 refresh_token，请重新授权", acc.Label)
	}
	res, err := p.client.Refresh(ctx, acc.RefreshToken)
	if err != nil {
		return err
	}
	acc.Cookie = res.AccessToken
	acc.RefreshToken = res.RefreshToken
	return nil
}
