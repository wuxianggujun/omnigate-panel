package provider

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// OpenAIStream normalizes an OpenAI-compatible SSE chat-completion stream into
// provider.Events. It is shared by the "openai" and "raccoon" adapters because
// both upstreams speak the OpenAI wire format (chat/completions + data: chunks).
type OpenAIStream struct {
	rc      io.ReadCloser
	sc      *SSEScanner
	calls   []openai.ToolCall
	byIndex map[int]int
	pending []Event

	// usage is the trailing usage block (held until end so it can be emitted
	// before the buffered finish, letting the caller record it).
	usage *Usage
	// pendingFinish buffers the finish_reason until the stream ends, so a
	// trailing usage chunk is not lost (upstreams send finish then usage).
	pendingFinish *string
}

// NewOpenAIStream wraps an OpenAI-compatible SSE response body.
func NewOpenAIStream(rc io.ReadCloser) *OpenAIStream {
	return &OpenAIStream{rc: rc, sc: NewSSEScanner(rc)}
}

// Close releases the underlying body.
func (s *OpenAIStream) Close() error { return s.rc.Close() }

type openAIToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIChunk struct {
	Choices []struct {
		Delta struct {
			Content          string                `json:"content"`
			ReasoningContent string                `json:"reasoning_content"`
			ToolCalls        []openAIToolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage"`
	// Error is the top-level error frame OpenAI-compatible upstreams send on a
	// failed stream (rate limit / content filter / moderation). Such a frame has
	// no choices, so without this field it would be silently skipped.
	Error *openAIError `json:"error"`
}

// openAIError is an upstream error frame body: {"error":{"message":...}}.
type openAIError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// openAIUsage is the OpenAI-format usage block. Upstreams (raccoon included)
// send it in a trailing chunk after the finish_reason chunk.
type openAIUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	PromptTokensDetails     *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u *openAIUsage) toUsage() *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptTokensDetails != nil {
		out.CachedTokens = u.PromptTokensDetails.CachedTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
	}
	return out
}

// Recv returns the next normalized event, or io.EOF when the stream ends.
func (s *OpenAIStream) Recv() (Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		data, err := s.sc.Next()
		if err != nil {
			return s.end(err)
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			return s.end(io.EOF)
		}
		var ch openAIChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil {
			continue
		}
		// 上游以 error 帧报错（限流 / 内容拦截 / 审核）：OpenAI 兼容服务把它发成
		// 顶层 {"error":{...}}，没有 choices——此前被下面 len(choices)==0 的
		// continue 静默吞掉，一次失败的流在请求记录里被记成 200 success。这里显式
		// 转成 EventError，交给 gateway 收敛为失败（与 WorkBuddy 侧同语义）。
		if ch.Error != nil {
			msg := strings.TrimSpace(ch.Error.Message)
			if msg == "" {
				msg = strings.TrimSpace(data)
			}
			return Event{Type: EventError, Text: msg}, nil
		}
		if ch.Usage != nil {
			s.usage = ch.Usage.toUsage()
		}
		if len(ch.Choices) == 0 {
			continue
		}
		c0 := ch.Choices[0]
		if c0.Delta.Content != "" {
			return Event{Type: EventText, Text: c0.Delta.Content}, nil
		}
		if c0.Delta.ReasoningContent != "" {
			return Event{Type: EventReasoning, Text: c0.Delta.ReasoningContent}, nil
		}
		for _, tc := range c0.Delta.ToolCalls {
			s.addCall(tc)
		}
		if c0.FinishReason != nil {
			fin := MapOpenAIFinish(*c0.FinishReason)
			// Hold the finish until the stream ends: upstreams report usage in
			// a trailing chunk right after it.
			s.pendingFinish = &fin
			if len(s.calls) > 0 {
				c := s.calls
				s.calls = nil
				return Event{Type: EventToolCalls, ToolCalls: c}, nil
			}
			continue
		}
	}
}

// end finalizes the stream: it queues any captured usage before the buffered
// finish (so callers that stop on EventFinish still see usage), flushes pending
// tool calls, then returns the next queued event or err.
func (s *OpenAIStream) end(err error) (Event, error) {
	if s.usage != nil {
		u := s.usage
		s.usage = nil
		s.pending = append(s.pending, Event{Type: EventUsage, Usage: u})
	}
	if s.pendingFinish != nil {
		f := *s.pendingFinish
		s.pendingFinish = nil
		s.pending = append(s.pending, Event{Type: EventFinish, Finish: f})
	}
	if len(s.calls) > 0 {
		c := s.calls
		s.calls = nil
		s.pending = append(s.pending, Event{Type: EventToolCalls, ToolCalls: c})
	}
	if len(s.pending) > 0 {
		e := s.pending[0]
		s.pending = s.pending[1:]
		return e, nil
	}
	return Event{}, err
}

func (s *OpenAIStream) addCall(tc openAIToolCallDelta) {
	if s.byIndex == nil {
		s.byIndex = map[int]int{}
	}
	if idx, ok := s.byIndex[tc.Index]; ok {
		c := &s.calls[idx]
		if tc.ID != "" {
			c.ID = tc.ID
		}
		c.Function.Name += tc.Function.Name
		c.Function.Arguments += tc.Function.Arguments
		return
	}
	s.byIndex[tc.Index] = len(s.calls)
	typ := tc.Type
	if typ == "" {
		typ = "function"
	}
	s.calls = append(s.calls, openai.ToolCall{
		Index: tc.Index,
		ID:    tc.ID,
		Type:  typ,
		Function: openai.ToolFunction{
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		},
	})
}

// MapOpenAIFinish maps an upstream finish_reason to the gateway's vocabulary.
func MapOpenAIFinish(s string) string {
	switch s {
	case "tool_calls":
		return "tool_calls"
	case "length":
		return "length"
	case "content_filter":
		return "content_filter"
	default:
		return "stop"
	}
}
