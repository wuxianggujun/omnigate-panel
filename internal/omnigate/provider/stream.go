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
			if err == io.EOF && len(s.calls) > 0 {
				c := s.calls
				s.calls = nil
				return Event{Type: EventToolCalls, ToolCalls: c}, nil
			}
			return Event{}, err
		}
		data = strings.TrimSpace(data)
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			if len(s.calls) > 0 {
				c := s.calls
				s.calls = nil
				return Event{Type: EventToolCalls, ToolCalls: c}, nil
			}
			return Event{}, io.EOF
		}
		var ch openAIChunk
		if err := json.Unmarshal([]byte(data), &ch); err != nil || len(ch.Choices) == 0 {
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
			if len(s.calls) > 0 {
				c := s.calls
				s.calls = nil
				s.pending = append(s.pending, Event{Type: EventFinish, Finish: MapOpenAIFinish(*c0.FinishReason)})
				return Event{Type: EventToolCalls, ToolCalls: c}, nil
			}
			return Event{Type: EventFinish, Finish: MapOpenAIFinish(*c0.FinishReason)}, nil
		}
	}
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
