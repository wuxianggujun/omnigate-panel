// Package provider defines the upstream abstraction omnigate routes to.
package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// BrowserUA is the default outbound User-Agent for relays whose console — or
// whose Electron desktop client — is browser-shaped. These upstreams
// authenticate by token, not by UA, so a scripted UA ("omnigate/1.0") is a pure
// anti-abuse tell with no upside. Keep this close to a current desktop Chrome
// and bump the major version occasionally to stay plausible.
const BrowserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

// AcceptLanguage is the default Accept-Language sent alongside BrowserUA.
const AcceptLanguage = "zh-CN,zh;q=0.9,en;q=0.8"

// OutOfCredits reports whether err looks like an exhausted-credit failure, which
// triggers automatic account switching.
func OutOfCredits(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "out of credit") ||
		strings.Contains(s, "out_of_credit") ||
		strings.Contains(s, "insufficient credit") ||
		strings.Contains(s, "insufficient points") ||
		strings.Contains(s, "not enough points") ||
		strings.Contains(s, "积分不足") ||
		strings.Contains(s, "余额不足") ||
		strings.Contains(s, "积分已用完")
}

// Account is one credential set for a provider.
type Account struct {
	Label    string
	Cookie   string
	Email    string
	Password string
	// RefreshToken is used by token-based providers (e.g. raccoon) to mint a
	// fresh access token when Cookie expires. Empty for cookie-based providers.
	RefreshToken string
	// ExpiresAt is the access token's expiry (unix seconds) for token-based
	// providers that learn it from the token endpoint (TRAE). 0 = unknown
	// (fall back to reading the JWT).
	ExpiresAt int64
	// UID / MachineID / DeviceID / ApiHost carry TRAE-specific per-account
	// fingerprint fields. TRAE sends x-machine-id / x-device-id on every SOLO
	// request and requires a *distinct* deviceId per account for the daily
	// check-in, so these must be persisted per account. Empty for other types.
	UID       string
	MachineID string
	DeviceID  string
	ApiHost   string
	// APIKey is a second credential used by New API relays (newapi): the access
	// token drives /api/user/* (check-in, self) while this sk-... key drives
	// /v1/chat/completions. Empty for providers that use a single credential.
	APIKey string
	// SessionCookie is the optional session cookie captured alongside a New API
	// session id. Sent (as a Cookie header) during token rotation in case the
	// relay validates the cookie as well as the X-Auth-Session header.
	SessionCookie string
}

// ChatInput is a normalized chat request handed to a provider.
type ChatInput struct {
	// ChatID is a stable conversation id for providers that keep server-side
	// conversation state (Runable). Providers that are stateless ignore it.
	ChatID string
	Model  string
	// Prompt is the flattened prompt used by prompt-based providers.
	Prompt string
	// Messages is the structured form used by OpenAI-compatible providers.
	Messages []openai.Message
	Tools    []openai.Tool
	// Attachments are the caller's non-text message parts (images, files) in
	// order. Only a provider that can actually carry them uses this — geminiweb
	// uploads each one and injects the resulting id into the payload. A text-only
	// provider ignores it, and the prompt it receives already records that
	// something was dropped.
	Attachments []openai.Attachment
	// ReasoningEffort is the OpenAI thinking tier (minimal/low/medium/high)
	// forwarded to reasoning-capable upstreams; empty = not requested.
	ReasoningEffort string
	// Temperature / TopP / MaxTokens / Stop / ParallelToolCalls / ToolChoice /
	// ResponseFormat / User are the standard OpenAI sampling & structure controls.
	// Zero value = the client didn't send it. OpenAI-speaking providers merge them
	// into the request body via ApplySampling; providers with a bespoke protocol
	// (runable) ignore them.
	Temperature       *float64
	TopP              *float64
	MaxTokens         *int
	Stop              json.RawMessage
	ParallelToolCalls *bool
	ToolChoice        json.RawMessage
	ResponseFormat    json.RawMessage
	User              string
	// Incognito/Mode mirror the original gateway's upstream modes.
	Incognito bool
	Mode      string
	// GemID runs the turn on a saved custom Gem (geminiweb). Empty = a normal
	// model turn. The model in Model is the Gem's underlying base model.
	GemID string
	// PreferredAccount pins the turn to a specific account label. Set for a Gem
	// turn, because a gem_id is private to the account that owns it. Empty =
	// normal round-robin selection.
	PreferredAccount string
}

// ApplySampling merges the standard OpenAI sampling/structure controls from in
// into an OpenAI-compatible request body. Providers call it right after filling
// model/messages/stream, so a client's temperature / max_tokens / tool_choice /
// response_format / ... actually reach the upstream instead of being dropped when
// the gateway parses the request into openai.ChatRequest.
//
// tool_choice / response_format / stop are copied as raw JSON, so both the string
// and the object form stay valid. A provider whose upstream wants a different
// shape (trae) normalizes afterwards.
func ApplySampling(body map[string]any, in ChatInput) {
	if in.Temperature != nil {
		body["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		body["top_p"] = *in.TopP
	}
	if in.MaxTokens != nil {
		body["max_tokens"] = *in.MaxTokens
	}
	if in.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *in.ParallelToolCalls
	}
	if len(in.ToolChoice) > 0 {
		body["tool_choice"] = json.RawMessage(in.ToolChoice)
	}
	if len(in.ResponseFormat) > 0 {
		body["response_format"] = json.RawMessage(in.ResponseFormat)
	}
	if len(in.Stop) > 0 {
		body["stop"] = json.RawMessage(in.Stop)
	}
	if in.User != "" {
		body["user"] = in.User
	}
}

// Event is one normalized stream event.
type Event struct {
	Type      string // "text" | "reasoning" | "finish" | "error" | "tool_calls" | "usage"
	Text      string
	Finish    string
	ToolCalls []openai.ToolCall
	// Usage carries the upstream's OpenAI-format token accounting when it
	// reports one (providers that emit a trailing usage chunk). nil = not
	// reported.
	Usage *Usage
}

// Usage is the OpenAI-format token accounting an upstream reports for a
// completion. Zero fields mean "not reported".
type Usage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     int
	ReasoningTokens  int
}

// Event type constants.
const (
	EventText      = "text"
	EventReasoning = "reasoning"
	EventFinish    = "finish"
	EventError     = "error"
	EventToolCalls = "tool_calls"
	EventUsage     = "usage"
)

// Stream yields normalized events until io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// Provider is implemented by each upstream adapter.
type Provider interface {
	Name() string
	Type() string
	ListModels(ctx context.Context) ([]openai.Model, error)
	StreamChat(ctx context.Context, acc *Account, in ChatInput) (Stream, error)
}

// AccountModelLister is an optional Provider capability: some upstreams (raccoon)
// only return their model catalog — and its billing metadata — to an
// authenticated request. When a Provider implements it, the gateway supplies a
// live account so the catalog (and per-model credit price) can be fetched;
// otherwise the gateway falls back to ListModels(ctx).
type AccountModelLister interface {
	ListModelsWithAccount(ctx context.Context, acc *Account) ([]openai.Model, error)
}

// CookieRotator is an optional Provider capability: the upstream rotates the
// account's credential cookie on its responses (geminiweb). When a Provider
// implements it and RotatesCookie reports true, the gateway persists acc.Cookie
// after each call so a restart does not lose the rotation.
type CookieRotator interface {
	RotatesCookie() bool
}

// imageBaseKey carries the public address the caller used to reach the gateway.
type imageBaseKey struct{}

// WithImageBase attaches the gateway's public base URL to ctx so a provider that
// produces image links (geminiweb) can build a URL the client can actually
// fetch. It is set by the inbound server, which is the only layer that knows how
// it was addressed. Absent = no links are emitted.
func WithImageBase(ctx context.Context, base string) context.Context {
	return context.WithValue(ctx, imageBaseKey{}, base)
}

// ImageBase returns the public base URL attached by WithImageBase ("" if none).
func ImageBase(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(imageBaseKey{}).(string)
	return s
}

// MaxSample bounds how many upstream bytes a stream keeps for diagnostics.
const MaxSample = 600

// Sampler is implemented by streams that can report a bounded sample of the
// upstream bytes that produced no usable event: non-SSE junk lines (a JSON error
// body sent with HTTP 200) plus frames we could not interpret (empty deltas, an
// unknown field shape). The gateway reads it only when a 200 stream yielded 0
// valid frames, so the upstream's real message is surfaced instead of vanishing
// behind a bare "empty upstream stream".
type Sampler interface {
	UpstreamSample() []byte
}

// SSEScanner reads Server-Sent Events and returns the concatenated data payload
// of each event. Comment lines and non-data fields are ignored.
type SSEScanner struct {
	r *bufio.Reader
	// junk 保存「不是 SSE 字段」的原始行（最多 MaxSample 字节）。上游用 200 回一个
	// 普通 JSON 错误体时，这些行是全空 data 流里唯一的线索，此前被静默丢掉。
	junk []byte
}

// NewSSEScanner wraps r.
func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{r: bufio.NewReaderSize(r, 64*1024)}
}

// Junk returns a bounded sample of the non-SSE lines seen so far (diagnostics).
func (s *SSEScanner) Junk() []byte { return s.junk }

// note appends one raw line to the junk sample, capped at MaxSample bytes.
func (s *SSEScanner) note(line string) {
	if len(s.junk) >= MaxSample {
		return
	}
	s.junk = append(s.junk, line...)
	if len(s.junk) < MaxSample {
		s.junk = append(s.junk, '\n')
	}
	if len(s.junk) > MaxSample {
		s.junk = s.junk[:MaxSample]
	}
}

// Next returns the next event's data payload, or io.EOF when the stream ends.
func (s *SSEScanner) Next() (string, error) {
	var data []string
	for {
		line, err := s.r.ReadString('\n')
		if len(line) == 0 && err != nil {
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
		case strings.HasPrefix(line, ":"):
			// comment
		case strings.HasPrefix(line, "data:"):
			v := strings.TrimPrefix(line[5:], " ")
			data = append(data, v)
		default:
			// event: / id: / retry: 是合法 SSE 字段；其余（典型：非 SSE 的 JSON
			// 错误体）记进 junk 供诊断——否则 0 帧失败时毫无线索。
			if !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") && !strings.HasPrefix(line, "retry:") {
				s.note(line)
			}
		}
		if err != nil {
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			return "", err
		}
	}
}
