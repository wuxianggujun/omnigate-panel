package wire

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// ---------------------------------------------------------------------------
// OpenAI Responses API: encode
// ---------------------------------------------------------------------------

// responsesRequest is the outbound POST /responses body.
type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions,omitempty"`
	Input        []any           `json:"input"`
	Tools        []responsesTool `json:"tools,omitempty"`
	ToolChoice   any             `json:"tool_choice,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	TopP         *float64        `json:"top_p,omitempty"`
	MaxTokens    *int            `json:"max_output_tokens,omitempty"`
	Stream       bool            `json:"stream"`
	// Store=false keeps the upstream from persisting the conversation: omnigate
	// is stateless and a stored response costs the operator quota.
	Store bool `json:"store"`
}

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// EncodeResponses builds a Responses API request from a normalized ChatInput.
//
// The Responses API flattens what chat keeps nested: assistant tool calls become
// top-level function_call items and tool results become function_call_output
// items, with system messages promoted to "instructions". Getting that shape
// wrong makes the upstream reject the whole request, so it is done here once
// instead of in every adapter.
func EncodeResponses(in provider.ChatInput) ([]byte, error) {
	req := responsesRequest{Model: in.Model, Stream: true}
	req.Temperature = in.Temperature
	req.TopP = in.TopP
	req.MaxTokens = in.MaxTokens
	if tc := responsesToolChoice(in.ToolChoice); tc != nil {
		req.ToolChoice = tc
	}
	for _, t := range in.Tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		params := t.Function.Parameters
		if len(params) == 0 || !json.Valid(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		req.Tools = append(req.Tools, responsesTool{
			Type:        "function",
			Name:        name,
			Description: t.Function.Description,
			Parameters:  params,
			Strict:      t.Function.Strict,
		})
	}

	var instructions []string
	input := make([]any, 0, len(in.Messages))
	for _, m := range in.Messages {
		switch strings.TrimSpace(m.Role) {
		case "system", "developer":
			if m.Content.Text != "" {
				instructions = append(instructions, m.Content.Text)
			}
		case "assistant":
			if m.Content.Text != "" {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": m.Content.Text}},
				})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   tc.ID,
					"name":      tc.Function.Name,
					"arguments": args,
				})
			}
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  m.Content.Text,
			})
		default:
			input = append(input, map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{map[string]any{
					"type": "input_text",
					"text": m.Content.Text,
				}},
			})
		}
	}
	req.Instructions = strings.Join(instructions, "\n\n")
	req.Input = input

	return json.Marshal(req)
}

// responsesToolChoice maps OpenAI's tool_choice to the flat Responses form.
func responsesToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto", "required", "none":
			return s
		}
		return nil
	}
	var obj struct {
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	name := obj.Name
	if name == "" {
		name = obj.Function.Name
	}
	if name == "" {
		return nil
	}
	return map[string]any{"type": "function", "name": name}
}

// ---------------------------------------------------------------------------
// OpenAI Responses API: decode
// ---------------------------------------------------------------------------

// ResponsesStream normalizes a Responses API SSE stream into provider.Events.
//
// The wire format is far chattier than chat/completions: text arrives as
// response.output_text.delta, tool arguments as
// response.function_call_arguments.delta plus a final ...done, and usage only in
// response.completed. This type collapses all of it into the same event vocabulary
// the chat codec produces, so the gateway needs no per-format branch.
type ResponsesStream struct {
	rc io.ReadCloser
	sc *provider.SSEScanner

	pending []provider.Event
	calls   map[string]*responsesCall // item id → accumulating tool call
	order   []string

	finish   string
	usage    *provider.Usage
	sampled  []byte
	done     bool
	finished bool
}

type responsesCall struct {
	id   string
	name string
	args strings.Builder
}

// NewResponsesStream wraps a Responses SSE body.
func NewResponsesStream(rc io.ReadCloser) *ResponsesStream {
	return &ResponsesStream{
		rc:    rc,
		sc:    provider.NewSSEScanner(rc),
		calls: map[string]*responsesCall{},
	}
}

// Close releases the body.
func (s *ResponsesStream) Close() error { return s.rc.Close() }

// UpstreamSample implements provider.Sampler.
func (s *ResponsesStream) UpstreamSample() []byte {
	out := append([]byte(nil), s.sc.Junk()...)
	out = append(out, s.sampled...)
	if len(out) > provider.MaxSample {
		out = out[:provider.MaxSample]
	}
	return out
}

func (s *ResponsesStream) noteFrame(frame string) {
	if len(s.sampled) >= provider.MaxSample {
		return
	}
	s.sampled = append(s.sampled, frame...)
	if len(s.sampled) < provider.MaxSample {
		s.sampled = append(s.sampled, '\n')
	}
}

// responsesItem is one output item (a message or a function call).
type responsesItem struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

// responsesEvent is the union of the stream event payloads we consume.
type responsesEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	// output_text.delta / reasoning delta share "delta"; item_id identifies the
	// tool call an arguments delta belongs to.
	ItemID string `json:"item_id"`

	Item *responsesItem `json:"item"`

	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`

	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// Recv returns the next normalized event, io.EOF after the terminal events.
func (s *ResponsesStream) Recv() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if s.done && s.finished {
			return provider.Event{}, io.EOF
		}
		data, err := s.sc.Next()
		if err != nil {
			if err == io.EOF {
				s.complete()
				continue
			}
			return provider.Event{}, err
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		s.consume(data)
	}
}

// complete enqueues the terminal usage/finish events exactly once (see the
// Anthropic decoder for why they must both be emitted).
func (s *ResponsesStream) complete() {
	if s.finished {
		return
	}
	s.finished = true
	s.done = true
	if s.usage != nil {
		u := *s.usage
		s.usage = nil
		s.pending = append(s.pending, provider.Event{Type: provider.EventUsage, Usage: &u})
	}
	reason := s.finish
	if reason == "" {
		reason = "stop"
	}
	s.finish = ""
	s.pending = append(s.pending, provider.Event{Type: provider.EventFinish, Finish: reason})
}

// consume parses one SSE payload, splitting multi-line payloads (an upstream may
// omit the blank separator between events).
func (s *ResponsesStream) consume(payload string) {
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		s.consumeOne(line)
	}
	if s.done && !s.finished {
		s.complete()
	}
}

func (s *ResponsesStream) consumeOne(payload string) {
	var ev responsesEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		s.noteFrame(payload)
		return
	}
	switch ev.Type {
	case "response.output_text.delta":
		if ev.Delta != "" {
			s.pending = append(s.pending, provider.Event{Type: provider.EventText, Text: ev.Delta})
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if ev.Delta != "" {
			s.pending = append(s.pending, provider.Event{Type: provider.EventReasoning, Text: ev.Delta})
		}
	case "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			c := s.callFor(ev.Item)
			c.name = ev.Item.Name
			if ev.Item.Arguments != "" {
				c.args.WriteString(ev.Item.Arguments)
			}
		}
	case "response.function_call_arguments.delta":
		c := s.calls[ev.ItemID]
		if c != nil {
			c.args.WriteString(ev.Delta)
		}
	case "response.function_call_arguments.done":
		// Arguments are emitted as one tool_calls event when the item completes;
		// some upstreams only send ...done with the full text in item.arguments.
		if c := s.calls[ev.ItemID]; c != nil && ev.Delta != "" && c.args.Len() == 0 {
			c.args.WriteString(ev.Delta)
		}
	case "response.output_item.done":
		if ev.Item == nil {
			return
		}
		if ev.Item.Type == "function_call" {
			c := s.callFor(ev.Item)
			if ev.Item.Name != "" {
				c.name = ev.Item.Name
			}
			if c.args.Len() == 0 && ev.Item.Arguments != "" {
				c.args.WriteString(ev.Item.Arguments)
			}
			s.emitCall(c)
		}
	case "response.completed", "response.incomplete":
		if ev.Response != nil {
			if ev.Response.Usage != nil {
				s.usage = &provider.Usage{
					PromptTokens:     ev.Response.Usage.InputTokens,
					CompletionTokens: ev.Response.Usage.OutputTokens,
					TotalTokens:      ev.Response.Usage.TotalTokens,
				}
			}
			if ev.Response.Status == "incomplete" {
				s.finish = "length"
			}
		}
		s.done = true
	case "response.failed", "error":
		msg := ev.Message
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		}
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		}
		if msg == "" {
			msg = "upstream stream error"
		}
		s.pending = append(s.pending, provider.Event{Type: provider.EventError, Text: msg})
		s.done = true
	}
}

// callFor returns (creating if needed) the accumulator for one function_call
// item. The map is keyed by item id because arguments deltas reference item_id,
// while the emitted tool call must carry call_id.
func (s *ResponsesStream) callFor(item *responsesItem) *responsesCall {
	key := item.ID
	if key == "" {
		key = item.CallID
	}
	c, ok := s.calls[key]
	if !ok {
		c = &responsesCall{id: item.CallID}
		if c.id == "" {
			c.id = item.ID
		}
		c.name = item.Name
		s.calls[key] = c
		s.order = append(s.order, key)
	}
	return c
}

// emitCall releases one completed tool call.
func (s *ResponsesStream) emitCall(c *responsesCall) {
	args := strings.TrimSpace(c.args.String())
	if args == "" {
		args = "{}"
	}
	s.pending = append(s.pending, provider.Event{
		Type: provider.EventToolCalls,
		ToolCalls: []openai.ToolCall{{
			ID:       c.id,
			Type:     "function",
			Function: openai.ToolFunction{Name: c.name, Arguments: args},
		}},
	})
	s.finish = "tool_calls"
}
