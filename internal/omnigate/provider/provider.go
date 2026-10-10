// Package provider defines the upstream abstraction omnigate routes to.
package provider

import (
	"bufio"
	"context"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

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
	// ReasoningEffort is the OpenAI thinking tier (minimal/low/medium/high)
	// forwarded to reasoning-capable upstreams; empty = not requested.
	ReasoningEffort string
	// Incognito/Mode mirror the original gateway's upstream modes.
	Incognito bool
	Mode      string
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

// SSEScanner reads Server-Sent Events and returns the concatenated data payload
// of each event. Comment lines and non-data fields are ignored.
type SSEScanner struct {
	r *bufio.Reader
}

// NewSSEScanner wraps r.
func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{r: bufio.NewReaderSize(r, 64*1024)}
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
			// event:, id:, retry: — ignored
		}
		if err != nil {
			if len(data) > 0 {
				return strings.Join(data, "\n"), nil
			}
			return "", err
		}
	}
}
