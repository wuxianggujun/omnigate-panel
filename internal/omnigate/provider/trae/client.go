// client.go TRAE SOLO 上游协议：请求改写 / 请求头 / 错误分类 / 短 JSON 调用
// （ExchangeToken、模型目录、签到、积分、用户信息）。
//
// 端口自 trae2api/internal/upstream（实测常量与头，勿改）：
//   - 对话走 trae-api-cn.mchost.guru，SSE 事件流是 SOLO 自定义方言；
//   - 签到 / 积分走 api.trae.cn（ug 系列），需要每个账号独立的 x-device-id；
//   - 换 token / 用户信息走 api.trae.com.cn，全程无签名（纯 JWT 头 + JSON）。
package trae

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// SOLO 上游技术常量（来自实测，禁止随意改动）。
const (
	DefaultAgentHost = "https://trae-api-cn.mchost.guru"
	DefaultUgHost    = "https://api.trae.cn"
	DefaultOAuthHost = "https://api.trae.com.cn"
	ConsoleHost      = "https://www.trae.cn"

	ClientID       = "en1oxy7wnw8j9n" // SOLO stable
	AppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	IdeVersion     = "0.1.52"
	IdeVersionCode = "20260811"
	DeviceBrand    = "Apple"
	OSVersion      = "macOS 15.7.4"
	// Function is the SOLO function name sent in the request body.
	Function = "solo_work_lite"

	// Endpoints.
	EpChat          = "/api/agent/v3/llm_utils_chat"
	EpModels        = "/api/ide/v1/get_detail_param"
	EpExchange      = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	EpUserInfo      = "/cloudide/api/v3/trae/GetUserInfo"
	EpCheckinStatus = "/trae/api/v2/ug/checkin_credits/status"
	EpCheckinClaim  = "/trae/api/v2/ug/checkin_credits/claim"
	EpEntUsage      = "/trae/api/v2/pay/ide_user_ent_usage"

	clientUA = "Trae/" + IdeVersion
)

// ErrKind classifies an upstream failure so the gateway can cool down / rotate.
type ErrKind string

const (
	ErrNone        ErrKind = "none"
	ErrSessionDead ErrKind = "session_dead" // 401：token 失效，刷新后重试
	ErrSoftRate    ErrKind = "soft_rate"    // 429 / 4011：限流
	ErrPlanLimit   ErrKind = "plan_limit"   // 1005 / 4008：权益/积分不足
	ErrNotFound    ErrKind = "not_found"
	ErrServer      ErrKind = "server"
	ErrClient      ErrKind = "client"
)

// Error is a classified upstream failure.
type Error struct {
	Kind   ErrKind
	Status int
	Msg    string
}

func (e *Error) Error() string {
	switch e.Kind {
	case ErrPlanLimit:
		// 文案命中 provider.OutOfCredits → 网关自动换号。
		return "积分不足（TRAE plan limit）: " + e.Msg
	case ErrSoftRate:
		// 文案命中 gateway.rateLimited → 该账号冷却。
		return "上游限流: " + e.Msg
	case ErrSessionDead:
		return "登录态已过期: " + e.Msg
	}
	return fmt.Sprintf("TRAE 上游 %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Unauthorized reports whether err is an auth failure worth a refresh+retry.
func Unauthorized(err error) bool {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind == ErrSessionDead
	}
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "登录态已过期")
}

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// Classify maps an HTTP status + body to an ErrKind.
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return ErrSessionDead
			}
		}
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

// Client is the TRAE upstream HTTP client.
type Client struct {
	// HTTP is for short JSON calls (ExchangeToken / models / check-in / usage).
	HTTP *http.Client
	// StreamHTTP is for SSE chat: no total timeout (long reasoning), with a
	// ResponseHeaderTimeout guard. Shares HTTP's transport.
	StreamHTTP *http.Client

	AgentHost string
	UgHost    string
	OAuthHost string
	ClientID  string
}

// NewClient builds a client with the default hosts.
func NewClient(agentHost, ugHost, oauthHost string) *Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 300 * time.Second,
		ForceAttemptHTTP2:     true,
	}
	c := &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		AgentHost:  strings.TrimRight(strings.TrimSpace(agentHost), "/"),
		UgHost:     strings.TrimRight(strings.TrimSpace(ugHost), "/"),
		OAuthHost:  strings.TrimRight(strings.TrimSpace(oauthHost), "/"),
		ClientID:   ClientID,
	}
	if c.AgentHost == "" {
		c.AgentHost = DefaultAgentHost
	}
	if c.UgHost == "" {
		c.UgHost = DefaultUgHost
	}
	if c.OAuthHost == "" {
		c.OAuthHost = DefaultOAuthHost
	}
	return c
}

// SetProxy routes this client's outbound requests through u (nil = direct).
func (c *Client) SetProxy(u *url.URL) {
	if u == nil {
		return
	}
	if tr, ok := c.HTTP.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(u)
		tr.CloseIdleConnections()
	}
}

// SetProxySelector adds proxy-pool failover at the transport level.
func (c *Client) SetProxySelector(sel outbound.Selector) {
	if c == nil || c.HTTP == nil {
		return
	}
	base, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		return
	}
	tr := outbound.WrapTransport(base, sel)
	c.HTTP.Transport = tr
	c.StreamHTTP.Transport = tr
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

// ---------------------------------------------------------------------------
// Headers

// soloHeaders sets the SOLO chat / models headers.
func soloHeaders(req *http.Request, a *Auth, stream bool) {
	req.Header.Set("Content-Type", "application/json")
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	req.Header.Set("User-Agent", clientUA)
	at := a.AccessToken
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+at)
	req.Header.Set("X-Cloudide-Token", at)
	req.Header.Set("X-Ide-Token", at)
	if a.UID != "" {
		req.Header.Set("X-Uid", a.UID)
	}
	req.Header.Set("X-App-Id", AppID)
	req.Header.Set("X-App-Version", "default")
	req.Header.Set("X-Ide-Version", IdeVersion)
	req.Header.Set("X-Ide-Version-Code", IdeVersionCode)
	req.Header.Set("X-Ide-Version-Type", "stable")
	req.Header.Set("X-Device-Type", "macos")
	req.Header.Set("X-OS-Version", OSVersion)
	req.Header.Set("X-Device-Brand", DeviceBrand)
	req.Header.Set("Request-Traffic-Type", "prod")
	if a.MachineID != "" {
		req.Header.Set("X-Machine-Id", a.MachineID)
	}
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// ugHeaders sets the check-in / usage (api.trae.cn) headers.
func ugHeaders(req *http.Request, a *Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+a.AccessToken)
	req.Header.Set("X-User-Region", "CN")
	if a.DeviceID != "" {
		req.Header.Set("X-Device-Id", a.DeviceID)
	}
}

// oauthHeaders sets the ExchangeToken / GetUserInfo headers (no signing).
func oauthHeaders(req *http.Request) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", clientUA)
}

// ---------------------------------------------------------------------------
// Body rewrite

// DefaultConfigName is the fallback SOLO model when the request carries none.
const DefaultConfigName = "glm-5.2"

// PrepareBody rewrites an OpenAI chat body into SOLO's llm_utils_chat shape:
//
//	OpenAI: {model, messages, stream, tools, tool_choice, ...}
//	SOLO:   {messages, function:"solo_work_lite", stream:true,
//	         config_name:<model>, model:<model>}
//
// Rules: content string -> [{type:text,text}]; assistant tool_calls' function ->
// function_call (name required); model -> config_name + model; tools[].function.
// parameters (object) -> JSON string; tool_choice normalized to a string.
func PrepareBody(src []byte) []byte {
	if len(src) == 0 {
		return src
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src
	}
	obj["stream"] = true
	obj["function"] = Function

	if msgs, ok := obj["messages"].([]any); ok {
		for _, mi := range msgs {
			m, ok := mi.(map[string]any)
			if !ok {
				continue
			}
			content, present := m["content"]
			role, _ := m["role"].(string)

			// assistant 回传 tool_calls：OpenAI function → 上游 function_call
			if role == "assistant" {
				if tcs, ok := m["tool_calls"].([]any); ok {
					kept := make([]any, 0, len(tcs))
					for _, tci := range tcs {
						tc, ok := tci.(map[string]any)
						if !ok {
							continue
						}
						if fn, ok := tc["function"].(map[string]any); ok {
							tc["function_call"] = fn
							delete(tc, "function")
						}
						// 上游要求 FunctionCall.Name 必填：无 name 的剔除。
						if fc, ok := tc["function_call"].(map[string]any); ok {
							name, _ := fc["name"].(string)
							if strings.TrimSpace(name) == "" {
								continue
							}
						}
						kept = append(kept, tc)
					}
					if len(kept) == 0 {
						delete(m, "tool_calls")
					} else {
						m["tool_calls"] = kept
					}
				}
			}

			if !present || content == nil {
				continue
			}
			if s, ok := content.(string); ok {
				m["content"] = []any{map[string]any{"type": "text", "text": s}}
			}
		}
	}

	model, _ := obj["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = DefaultConfigName
	}
	obj["config_name"] = model
	obj["model"] = model

	normalizeToolChoice(obj)
	normalizeTools(obj)

	out, err := json.Marshal(obj)
	if err != nil {
		return src
	}
	return out
}

// normalizeToolChoice rewrites OpenAI tool_choice into the upstream's string
// form: "none" drops tools; auto/required stay; a function becomes its name.
func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ := strings.ToLower(strings.TrimSpace(strOf(v["type"])))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name, _ := v["function"].(map[string]any)
			n := ""
			if name != nil {
				n = strOf(name["name"])
			}
			if strings.TrimSpace(n) == "" {
				n = strOf(v["name"])
			}
			if n = strings.TrimSpace(n); n != "" {
				obj["tool_choice"] = n
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}

// normalizeTools serializes each tool's parameters object into a JSON string
// (the upstream declares it as a string) and drops malformed entries.
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

// ---------------------------------------------------------------------------
// JSON calls

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: Classify(resp.StatusCode, string(raw)), Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

// RefreshAccount exchanges the rotating refresh token for a fresh access token
// and updates a in place. On any failure a is left untouched (the old refresh
// token stays retryable).
func (c *Client) RefreshAccount(ctx context.Context, a *Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body, _ := json.Marshal(map[string]any{
		"ClientID":     c.ClientID,
		"RefreshToken": a.RefreshToken,
		"ClientSecret": "-",
		"UserID":       "",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+EpExchange, bytes.NewReader(body))
	if err != nil {
		return err
	}
	oauthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	switch {
	case resp.Result.TokenExpireAt > 0:
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	case resp.Result.TokenExpireDuration > 0:
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}

// normalizeExpiresAt normalizes ExchangeToken's TokenExpireAt to unix seconds
// (the upstream returns milliseconds).
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

// ModelInfo is one SOLO model configuration.
type ModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64
	MaxTokens     int64
}

// FetchModels lists the SOLO model configurations (get_detail_param).
func (c *Client) FetchModels(ctx context.Context, a *Auth) ([]ModelInfo, error) {
	body, _ := json.Marshal(map[string]any{
		"function":            Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	soloHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	out := make([]ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		out = append(out, ModelInfo{ID: cfg.ConfigName, Name: cfg.DisplayConfig.DisplayName})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

// CheckinStatus queries today's check-in state.
func (c *Client) CheckinStatus(ctx context.Context, a *Auth) (checkedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ugBase()+EpCheckinStatus, strings.NewReader("{}"))
	if err != nil {
		return false, 0, false, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, false, err
	}
	var resp struct {
		CheckedIn bool  `json:"checked_in"`
		Credits   int64 `json:"credits"`
		Enable    bool  `json:"enable"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return false, 0, false, fmt.Errorf("checkin status parse: %w", err)
	}
	return resp.CheckedIn, resp.Credits, resp.Enable, nil
}

// CheckinClaim performs today's check-in.
func (c *Client) CheckinClaim(ctx context.Context, a *Auth) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ugBase()+EpCheckinClaim, strings.NewReader(`{"req_source":2}`))
	if err != nil {
		return err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &resp); err == nil && resp.Code != 0 {
		return fmt.Errorf("%d %s", resp.Code, resp.Message)
	}
	return nil
}

// EntUsage returns (remain, limit, used, packs) credits from ide_user_ent_usage.
func (c *Client) EntUsage(ctx context.Context, a *Auth) (remain, limit, used int64, packs int, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.ugBase()+EpEntUsage, strings.NewReader("{}"))
	if err != nil {
		return 0, 0, 0, 0, err
	}
	ugHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	var resp struct {
		UserEntitlementPackList []struct {
			EntitlementBaseInfo struct {
				Quota struct {
					CreditsLimit int64 `json:"credits_limit"`
				} `json:"quota"`
			} `json:"entitlement_base_info"`
			Usage struct {
				CreditsAmount float64 `json:"credits_amount"`
			} `json:"usage"`
		} `json:"user_entitlement_pack_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("ent usage parse: %w", err)
	}
	for _, p := range resp.UserEntitlementPackList {
		l := p.EntitlementBaseInfo.Quota.CreditsLimit
		if l <= 0 {
			continue
		}
		u := int64(p.Usage.CreditsAmount)
		limit += l
		used += u
		remain += l - u
		packs++
	}
	return remain, limit, used, packs, nil
}

// GetUserInfo returns (uid, nickname, enterpriseID) for the account.
func (c *Client) GetUserInfo(ctx context.Context, a *Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+EpUserInfo, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	oauthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.AccessToken)
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

// ChatStream posts the SOLO chat request and returns the raw SSE body. Non-2xx
// returns rc=nil with the response body for classification; only transport
// errors return err.
func (c *Client) ChatStream(ctx context.Context, a *Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	soloHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ---------------------------------------------------------------------------
// Fingerprints

// NewMachineID returns a 32-hex-char machine id (matching real clients).
func NewMachineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewCheckinDeviceID returns a 16-digit numeric device id (first digit non-zero).
// TRAE's check-in requires a distinct, stable deviceId per account; two accounts
// sharing one deviceId makes the second report "already checked in today", and an
// empty deviceId fails with 9004.
func NewCheckinDeviceID() (string, error) {
	digits := make([]byte, 16)
	first, err := rand.Int(rand.Reader, big.NewInt(9))
	if err != nil {
		return "", err
	}
	digits[0] = byte('1' + first.Int64())
	for i := 1; i < 16; i++ {
		d, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[i] = byte('0' + d.Int64())
	}
	return string(digits), nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
