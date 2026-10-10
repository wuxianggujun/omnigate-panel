// trae.go adapts TRAE SOLO (trae.cn) to provider.Provider.
//
// TRAE's llm_utils_chat endpoint speaks a custom SSE dialect (event:output /
// event:token_usage / event:done) rather than OpenAI SSE, so StreamChat rewrites
// the OpenAI request into SOLO's shape and translates the event stream back into
// normalized provider events.
//
// Auth: a JWT access token (sent as "Cloud-IDE-JWT <token>") plus a rotating
// refresh token exchanged at ExchangeToken. Each account also carries its own
// machine/device fingerprint — the daily check-in rejects a shared deviceId.
package trae

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// Auth is the per-account TRAE credential + fingerprint set. It is built from a
// provider.Account on every call, so the account's fields stay the source of
// truth (and the gateway can persist refreshed tokens).
type Auth struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // unix seconds; 0 = unknown
	UID          string
	MachineID    string
	DeviceID     string
	ApiHost      string
}

// AuthOf maps a provider account onto a TRAE Auth. The access token lives in
// Account.Cookie (the same slot raccoon uses) so the gateway's token-refresh
// plumbing works unchanged.
func AuthOf(acc *provider.Account) *Auth {
	return &Auth{
		AccessToken:  acc.Cookie,
		RefreshToken: acc.RefreshToken,
		ExpiresAt:    acc.ExpiresAt,
		UID:          acc.UID,
		MachineID:    acc.MachineID,
		DeviceID:     acc.DeviceID,
		ApiHost:      acc.ApiHost,
	}
}

// Provider implements provider.Provider for TRAE SOLO.
type Provider struct {
	name   string
	client *Client
}

// 编译期保证：TRAE 的模型目录需要鉴权，实现可选能力 AccountModelLister。
var _ provider.AccountModelLister = (*Provider)(nil)

// NewProvider builds a TRAE provider. Empty hosts fall back to the defaults.
func NewProvider(name, agentHost, ugHost, oauthHost string) *Provider {
	return &Provider{name: name, client: NewClient(agentHost, ugHost, oauthHost)}
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "trae" }

// Client exposes the underlying client (used by the gateway's login/check-in).
func (p *Provider) Client() *Client { return p.client }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) { p.client.SetProxy(u) }

// SetProxySelector installs the proxy-pool selector (with connection failover).
func (p *Provider) SetProxySelector(sel outbound.Selector) { p.client.SetProxySelector(sel) }

// ListModels fetches the upstream catalog, falling back to built-in defaults.
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	return p.ListModelsWithAccount(ctx, nil)
}

// ListModelsWithAccount lists the SOLO model configurations. The endpoint needs
// auth (anonymous → 401), so the gateway passes one usable account.
func (p *Provider) ListModelsWithAccount(ctx context.Context, acc *provider.Account) ([]openai.Model, error) {
	if acc == nil || strings.TrimSpace(acc.Cookie) == "" {
		return fallbackModels(p.name), nil
	}
	infos, err := p.client.FetchModels(ctx, AuthOf(acc))
	if err != nil {
		// 降级目录 + 错误：网关优先沿用最近一次成功缓存，无缓存才用这份。
		return fallbackModels(p.name), err
	}
	out := make([]openai.Model, 0, len(infos))
	for _, m := range infos {
		out = append(out, openai.Model{
			ID:            m.ID,
			Object:        "model",
			OwnedBy:       p.name,
			ContextWindow: m.ContextWindow,
			MaxTokens:     m.MaxTokens,
		})
	}
	if len(out) == 0 {
		return fallbackModels(p.name), errors.New("trae: empty model catalog")
	}
	return out, nil
}

// StreamChat posts the OpenAI-shaped request and streams SOLO events back.
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
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	rc, status, respBody, err := p.client.ChatStream(ctx, AuthOf(acc), raw)
	if err != nil {
		return nil, err
	}
	if rc == nil {
		return nil, &Error{Kind: Classify(status, string(respBody)), Status: status, Msg: truncate(string(respBody), 200)}
	}
	return newSOLOStream(rc), nil
}

// TokenExpired reports whether the access token is missing or near expiry.
func (p *Provider) TokenExpired(acc *provider.Account) bool {
	if acc == nil || acc.Cookie == "" {
		return true
	}
	if acc.ExpiresAt > 0 {
		return time.Now().Add(60 * time.Second).Unix() >= acc.ExpiresAt
	}
	// 无明确过期时间：交给 401 重试路径（与 raccoon 同口径），避免每次重启都轮换。
	return false
}

// RefreshAccount mints a fresh access token and updates acc in place.
func (p *Provider) RefreshAccount(ctx context.Context, acc *provider.Account) error {
	if acc.RefreshToken == "" {
		return fmt.Errorf("账号「%s」缺少 refresh_token，请重新授权", acc.Label)
	}
	a := AuthOf(acc)
	if err := p.client.RefreshAccount(ctx, a); err != nil {
		return err
	}
	acc.Cookie = a.AccessToken
	acc.RefreshToken = a.RefreshToken
	acc.ExpiresAt = a.ExpiresAt
	return nil
}

// Unauthorized reports whether err is an auth failure (implements the gateway's
// generic TokenRefresher).
func (p *Provider) Unauthorized(err error) bool { return Unauthorized(err) }

// fallbackModels is a minimal static catalog used when the authenticated model
// listing is unavailable (cold start / upstream error with no cache).
func fallbackModels(owner string) []openai.Model {
	ids := []string{
		"claude-4.5-sonnet", "claude-4-sonnet", "gpt-5", "gpt-4.1",
		"gemini-2.5-pro", "deepseek-v3.1",
	}
	out := make([]openai.Model, 0, len(ids))
	for _, id := range ids {
		out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: owner})
	}
	return out
}

// ---------------------------------------------------------------------------
// SOLO SSE → provider.Event

// SOLOEvent is one normalized SOLO SSE event.
type SOLOEvent struct {
	Event        string
	Response     string
	Reasoning    string
	ToolCalls    json.RawMessage
	Usage        map[string]any
	FinishReason string
	ErrorCode    int64
	ErrorMessage string
}

// ParseSOLOLine parses one event (eventName from the "event:" line, dataLine
// from the "data:" line).
func ParseSOLOLine(eventName, dataLine string) (*SOLOEvent, error) {
	ev := &SOLOEvent{Event: strings.TrimSpace(eventName)}
	if dataLine == "" {
		return ev, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
		return nil, err
	}
	switch ev.Event {
	case "output":
		if v, ok := raw["response"].(string); ok {
			ev.Response = v
		}
		if v, ok := raw["reasoning_content"].(string); ok {
			ev.Reasoning = v
		}
		if tc, ok := raw["tool_calls"]; ok && tc != nil {
			ev.ToolCalls, _ = json.Marshal(tc)
		}
	case "token_usage":
		ev.Usage = raw
	case "done":
		if v, ok := raw["finish_reason"].(string); ok {
			ev.FinishReason = v
		}
	case "error":
		if v, ok := raw["code"].(float64); ok {
			ev.ErrorCode = int64(v)
		}
		if v, ok := raw["message"].(string); ok {
			ev.ErrorMessage = v
		}
	}
	return ev, nil
}

// sseState accumulates the event/data lines of one SSE frame.
type sseState struct {
	event string
	data  strings.Builder
}

func (s *sseState) reset() {
	s.event = ""
	s.data.Reset()
}

// scanLine feeds one line; it returns a parsed event at the frame boundary.
func scanLine(st *sseState, line string) *SOLOEvent {
	switch {
	case line == "":
		if st.event == "" {
			st.reset()
			return nil
		}
		ev, err := ParseSOLOLine(st.event, st.data.String())
		st.reset()
		if err != nil {
			return nil
		}
		return ev
	case strings.HasPrefix(line, "event:"):
		st.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	case strings.HasPrefix(line, "data:"):
		st.data.WriteString(strings.TrimPrefix(line, "data:"))
	case strings.HasPrefix(line, ":"):
		// comment
	}
	return nil
}

// soloStream translates a SOLO SSE body into provider events.
type soloStream struct {
	rc      io.ReadCloser
	br      *bufio.Reader
	st      *sseState
	pending []provider.Event
	done    bool
}

func newSOLOStream(rc io.ReadCloser) *soloStream {
	return &soloStream{rc: rc, br: bufio.NewReaderSize(rc, 64*1024), st: &sseState{}}
}

// Recv returns the next normalized event, or io.EOF at end of stream.
func (s *soloStream) Recv() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if s.done {
			return provider.Event{}, io.EOF
		}
		line, err := s.br.ReadString('\n')
		if len(line) > 0 {
			if ev := scanLine(s.st, strings.TrimRight(line, "\r\n")); ev != nil {
				s.mapEvent(ev)
			}
		}
		if err != nil {
			s.done = true
			if len(s.pending) == 0 {
				if err == io.EOF {
					return provider.Event{}, io.EOF
				}
				return provider.Event{}, err
			}
		}
	}
}

// Close releases the upstream body.
func (s *soloStream) Close() error {
	if s.rc != nil {
		return s.rc.Close()
	}
	return nil
}

func (s *soloStream) mapEvent(ev *SOLOEvent) {
	switch ev.Event {
	case "output":
		if ev.Reasoning != "" {
			s.pending = append(s.pending, provider.Event{Type: provider.EventReasoning, Text: ev.Reasoning})
		}
		if ev.Response != "" {
			s.pending = append(s.pending, provider.Event{Type: provider.EventText, Text: ev.Response})
		}
		if len(ev.ToolCalls) > 0 && string(ev.ToolCalls) != "null" {
			if calls := parseToolCalls(ev.ToolCalls); len(calls) > 0 {
				s.pending = append(s.pending, provider.Event{Type: provider.EventToolCalls, ToolCalls: calls})
			}
		}
	case "token_usage":
		if u := parseUsage(ev.Usage); u != nil {
			s.pending = append(s.pending, provider.Event{Type: provider.EventUsage, Usage: u})
		}
	case "done":
		finish := ev.FinishReason
		if finish == "" {
			finish = "stop"
		}
		s.pending = append(s.pending, provider.Event{Type: provider.EventFinish, Finish: finish})
	case "error":
		s.pending = append(s.pending, provider.Event{
			Type: provider.EventError,
			Text: soloErrorMessage(ev.ErrorCode, ev.ErrorMessage),
		})
	}
}

// soloErrorMessage maps a SOLO in-stream error code onto a message the gateway's
// classification can act on: 1005/4008 → out-of-credits (rotate account),
// 4011 → rate limit (cooldown).
func soloErrorMessage(code int64, msg string) string {
	switch code {
	case 1005, 4008:
		return fmt.Sprintf("积分不足（TRAE code=%d）: %s", code, msg)
	case 4011:
		return fmt.Sprintf("上游限流（TRAE code=%d）: %s", code, msg)
	}
	return fmt.Sprintf("TRAE 上游错误 code=%d msg=%s", code, msg)
}

// parseToolCalls tolerantly decodes SOLO's tool_calls payload into OpenAI tool
// calls (it may be a single object or an array).
func parseToolCalls(raw json.RawMessage) []openai.ToolCall {
	var arr []openai.ToolCall
	if err := json.Unmarshal(raw, &arr); err == nil && len(arr) > 0 {
		return arr
	}
	var one openai.ToolCall
	if err := json.Unmarshal(raw, &one); err == nil && (one.Function.Name != "" || one.ID != "") {
		return []openai.ToolCall{one}
	}
	return nil
}

func parseUsage(m map[string]any) *provider.Usage {
	if m == nil {
		return nil
	}
	u := &provider.Usage{
		PromptTokens:     intOf(m["prompt_tokens"]),
		CompletionTokens: intOf(m["completion_tokens"]),
		TotalTokens:      intOf(m["total_tokens"]),
		ReasoningTokens:  intOf(m["reasoning_tokens"]),
	}
	if u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 {
		return nil
	}
	return u
}

func intOf(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case int64:
		return int(t)
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	default:
		return 0
	}
}
