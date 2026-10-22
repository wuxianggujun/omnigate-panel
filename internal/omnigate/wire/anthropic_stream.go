package wire

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// AnthropicStream normalizes an Anthropic Messages SSE stream into
// provider.Events.
//
// Anthropic's stream is a state machine, not a list of deltas: message_start,
// then content_block_start/delta/stop per block, then message_delta and
// message_stop. A consumer that only read `delta.text` would drop tool inputs
// entirely and would not know when a block ended — so this type keeps the block
// table and emits one normalized event per completed unit:
//
//	text block        → EventText per delta
//	thinking block    → EventReasoning per delta
//	tool_use block    → EventToolCalls with the assembled JSON input
//	message_delta     → the finish reason
//	message_stop      → EventUsage (when reported) then EventFinish
//	error event       → EventError
type AnthropicStream struct {
	rc io.ReadCloser
	sc *provider.SSEScanner

	// blocks maps content index → the block being accumulated.
	blocks map[int]*anthropicBlock
	// pending holds events produced while parsing one frame (a block_stop can
	// emit a tool call); Recv drains it before reading more bytes.
	pending []provider.Event

	finish   string
	usage    *provider.Usage
	sampled  []byte
	done     bool
	finished bool
}

// anthropicBlock accumulates one content block across its deltas.
type anthropicBlock struct {
	kind string
	text strings.Builder
	id   string
	name string
	// input accumulates input_json_delta fragments. The block_start frame also
	// carries an input value — normally the empty object, sometimes a complete
	// one — so it is kept apart and only used when no deltas arrived. Appending
	// both would produce "{}" + "{\"city\":…", which is not valid JSON.
	input      strings.Builder
	startInput string
}

// NewAnthropicStream wraps an Anthropic SSE body.
func NewAnthropicStream(rc io.ReadCloser) *AnthropicStream {
	return &AnthropicStream{
		rc:     rc,
		sc:     provider.NewSSEScanner(rc),
		blocks: map[int]*anthropicBlock{},
	}
}

// Close releases the body.
func (s *AnthropicStream) Close() error { return s.rc.Close() }

// UpstreamSample implements provider.Sampler: the frames that produced nothing
// usable, so a 200-with-junk upstream is still diagnosable.
func (s *AnthropicStream) UpstreamSample() []byte {
	out := append([]byte(nil), s.sc.Junk()...)
	out = append(out, s.sampled...)
	if len(out) > provider.MaxSample {
		out = out[:provider.MaxSample]
	}
	return out
}

// noteFrame records one frame we could not interpret.
func (s *AnthropicStream) noteFrame(frame string) {
	if len(s.sampled) >= provider.MaxSample {
		return
	}
	s.sampled = append(s.sampled, frame...)
	if len(s.sampled) < provider.MaxSample {
		s.sampled = append(s.sampled, '\n')
	}
}

// anthropicEvent is the union of every stream event's fields.
type anthropicEvent struct {
	Type  string `json:"type"`
	Index int    `json:"index"`

	// message_start
	Message *struct {
		Usage *anthropicUsage `json:"usage"`
	} `json:"message"`

	// content_block_start
	ContentBlock *struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`

	// content_block_delta
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`

	// message_delta
	Usage *anthropicUsage `json:"usage"`

	// error
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Recv returns the next normalized event, io.EOF after the terminal events.
func (s *AnthropicStream) Recv() (provider.Event, error) {
	for {
		if len(s.pending) > 0 {
			e := s.pending[0]
			s.pending = s.pending[1:]
			return e, nil
		}
		if s.done && s.finished {
			return provider.Event{}, io.EOF
		}
		if s.done {
			// Terminal frame already seen but the events it enqueued have been
			// drained: nothing left.
			return provider.Event{}, io.EOF
		}
		data, err := s.sc.Next()
		if err != nil {
			if err == io.EOF {
				// Upstream closed without message_stop: surface whatever we have
				// instead of hanging the caller forever.
				s.complete()
				continue
			}
			return provider.Event{}, err
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		if s.consume(data) {
			s.complete()
		}
	}
}

// complete enqueues the terminal events exactly once: the usage snapshot (when
// the upstream reported one) and then the finish reason.
//
// They are separate events because the gateway accounts tokens from EventUsage
// and closes the client stream on EventFinish; emitting only one of them leaves
// either the ledger or the client waiting.
func (s *AnthropicStream) complete() {
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
	s.pending = append(s.pending, provider.Event{Type: provider.EventFinish, Finish: mapAnthropicStop(reason)})
}

// consume parses one SSE payload; it reports whether the stream reached its
// terminal frame.
//
// A payload can carry several data lines when the upstream omitted the blank
// separator between events. Each line is a complete JSON object, so they are
// parsed individually instead of being handed to the JSON decoder joined by
// newlines.
func (s *AnthropicStream) consume(payload string) bool {
	terminal := false
	for _, line := range strings.Split(payload, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "[DONE]" {
			continue
		}
		if s.consumeOne(line) {
			terminal = true
		}
	}
	return terminal
}

// consumeOne parses a single event object; it reports whether the stream is
// complete after it.
func (s *AnthropicStream) consumeOne(payload string) bool {
	var ev anthropicEvent
	if err := json.Unmarshal([]byte(payload), &ev); err != nil {
		s.noteFrame(payload)
		return false
	}
	switch ev.Type {
	case "message_start":
		if ev.Message != nil && ev.Message.Usage != nil {
			s.usage = &provider.Usage{
				PromptTokens: ev.Message.Usage.InputTokens,
				TotalTokens:  ev.Message.Usage.InputTokens,
			}
		}
	case "content_block_start":
		if ev.ContentBlock != nil {
			b := &anthropicBlock{kind: ev.ContentBlock.Type, id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			if raw := strings.TrimSpace(string(ev.ContentBlock.Input)); raw != "" && raw != "{}" {
				b.startInput = raw
			}
			s.blocks[ev.Index] = b
			if ev.ContentBlock.Type == "text" && ev.ContentBlock.Text != "" {
				b.text.WriteString(ev.ContentBlock.Text)
			}
		}
	case "content_block_delta":
		b := s.blocks[ev.Index]
		if b == nil || ev.Delta == nil {
			s.noteFrame(payload)
			return false
		}
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" {
				b.text.WriteString(ev.Delta.Text)
				s.pending = append(s.pending, provider.Event{Type: provider.EventText, Text: ev.Delta.Text})
			}
		case "thinking_delta":
			if ev.Delta.Thinking != "" {
				s.pending = append(s.pending, provider.Event{Type: provider.EventReasoning, Text: ev.Delta.Thinking})
			}
		case "input_json_delta":
			b.input.WriteString(ev.Delta.PartialJSON)
		}
	case "content_block_stop":
		b := s.blocks[ev.Index]
		delete(s.blocks, ev.Index)
		if b != nil && b.kind == "tool_use" {
			s.pending = append(s.pending, provider.Event{
				Type: provider.EventToolCalls,
				ToolCalls: []openai.ToolCall{{
					ID:   b.id,
					Type: "function",
					Function: openai.ToolFunction{
						Name:      b.name,
						Arguments: b.arguments(),
					},
				}},
			})
			s.finish = "tool_use"
		}
	case "message_delta":
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			s.finish = ev.Delta.StopReason
		}
		if ev.Usage != nil {
			if s.usage == nil {
				s.usage = &provider.Usage{}
			}
			s.usage.CompletionTokens = ev.Usage.OutputTokens
			s.usage.TotalTokens = s.usage.PromptTokens + ev.Usage.OutputTokens
		}
	case "message_stop":
		return true
	case "error":
		msg := "upstream stream error"
		if ev.Error != nil {
			if ev.Error.Message != "" {
				msg = ev.Error.Message
			} else if ev.Error.Type != "" {
				msg = ev.Error.Type
			}
		}
		s.pending = append(s.pending, provider.Event{Type: provider.EventError, Text: msg})
		return true
	}
	return false
}

// arguments returns the tool input as valid JSON.
func (b *anthropicBlock) arguments() string {
	if frag := strings.TrimSpace(b.input.String()); frag != "" {
		if json.Valid([]byte(frag)) {
			return frag
		}
		// Truncated stream: keep the request from failing on a half-written
		// object, and let the tool call fail on its own terms instead.
		return "{}"
	}
	if b.startInput != "" && json.Valid([]byte(b.startInput)) {
		return b.startInput
	}
	return "{}"
}

// mapAnthropicStop maps Anthropic stop_reason to the OpenAI finish_reason the
// rest of the pipeline (and the client-facing chat response) expects.
func mapAnthropicStop(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	}
	return "stop"
}

// encodeBase64 is the std base64 encoder used for inline images.
func encodeBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
