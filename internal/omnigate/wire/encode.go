// Package wire turns a normalized provider.ChatInput into an upstream HTTP body
// and an upstream response into provider.Events.
//
// This is the layer that used to be implicit: each adapter hand-rolled
// "chat/completions + JSON" and that was the only dialect omnigate could speak
// upstream, even though it could *receive* three dialects (chat, Responses,
// Messages). A relay that only accepts /v1/messages — AnyRouter and
// AgentRouter both do — was therefore unreachable no matter what headers we
// sent.
//
// Encoding and decoding are deliberately separate from transport: the adapter
// owns the HTTP client, retries and key rotation, while this package owns the
// bytes. That keeps one codec usable by any future adapter.
package wire

import (
	"encoding/json"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// Format names the upstream dialect an adapter should speak.
type Format string

const (
	// FormatOpenAI is POST {base}/chat/completions with SSE chunks.
	FormatOpenAI Format = "openai"
	// FormatAnthropic is POST {base}/messages with Anthropic SSE events.
	FormatAnthropic Format = "anthropic"
	// FormatResponses is POST {base}/responses (OpenAI Responses API).
	FormatResponses Format = "responses"
)

// Path returns the endpoint suffix appended to the provider's base URL.
func (f Format) Path() string {
	switch f {
	case FormatAnthropic:
		return "/messages"
	case FormatResponses:
		return "/responses"
	default:
		return "/chat/completions"
	}
}

// Valid reports whether f is understood.
func (f Format) Valid() bool {
	switch f {
	case FormatOpenAI, FormatAnthropic, FormatResponses:
		return true
	}
	return false
}

// ParseFormat normalizes a configured string; unknown values yield FormatOpenAI
// so a hand-edited config never produces an unroutable provider.
func ParseFormat(s string) Format {
	switch Format(strings.ToLower(strings.TrimSpace(s))) {
	case FormatAnthropic:
		return FormatAnthropic
	case FormatResponses:
		return FormatResponses
	default:
		return FormatOpenAI
	}
}

// ---------------------------------------------------------------------------
// Anthropic Messages: encode
// ---------------------------------------------------------------------------

// anthropicRequest is the outbound /v1/messages body.
type anthropicRequest struct {
	Model         string             `json:"model"`
	MaxTokens     int                `json:"max_tokens"`
	System        string             `json:"system,omitempty"`
	Messages      []anthropicMessage `json:"messages"`
	Tools         []anthropicTool    `json:"tools,omitempty"`
	ToolChoice    any                `json:"tool_choice,omitempty"`
	Temperature   *float64           `json:"temperature,omitempty"`
	TopP          *float64           `json:"top_p,omitempty"`
	StopSequences []string           `json:"stop_sequences,omitempty"`
	Stream        bool               `json:"stream"`
	Metadata      map[string]any     `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicContentBlock is one element of a content array.
type anthropicContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	// image
	Source any `json:"source,omitempty"`
}

// EncodeAnthropic builds a /v1/messages request body from a normalized ChatInput.
//
// Translations, in the direction the gateway had no implementation for before:
//   - system message(s) → top-level "system" (Anthropic has one field, not a role)
//   - assistant tool_calls → tool_use blocks
//   - role=tool → a user message carrying a tool_result block (Anthropic pairs
//     results to uses via tool_use_id, so ordering is not significant)
//   - image_url parts → base64 / url image blocks
//   - tools[].function → tools[].{name,description,input_schema}
//
// max_tokens is mandatory upstream; when the client did not send one we use a
// conservative default rather than let Anthropic 400 the request.
func EncodeAnthropic(in provider.ChatInput) ([]byte, error) {
	const defaultMaxTokens = 8192

	req := anthropicRequest{Model: in.Model, Stream: true, MaxTokens: defaultMaxTokens}
	if in.MaxTokens != nil && *in.MaxTokens > 0 {
		req.MaxTokens = *in.MaxTokens
	}
	req.Temperature = in.Temperature
	req.TopP = in.TopP
	req.StopSequences = decodeStop(in.Stop)
	if len(in.Tools) > 0 {
		req.Tools = anthropicTools(in.Tools)
	}
	req.ToolChoice = anthropicToolChoice(in.ToolChoice)
	if in.User != "" {
		req.Metadata = map[string]any{"user_id": in.User}
	}

	var system []string
	var msgs []anthropicMessage
	for _, m := range in.Messages {
		switch strings.TrimSpace(m.Role) {
		case "system", "developer":
			if t := m.Content.Text; t != "" {
				system = append(system, t)
			}
		case "assistant":
			blocks := make([]anthropicContentBlock, 0, 1+len(m.ToolCalls))
			if m.Content.Text != "" {
				blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content.Text})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: json.RawMessage(jsonOrEmptyObject(tc.Function.Arguments)),
				})
			}
			if len(blocks) > 0 {
				msgs = append(msgs, anthropicMessage{Role: "assistant", Content: blocks})
			}
		case "tool":
			// One tool result per message; consecutive results are merged below.
			msgs = append(msgs, anthropicMessage{
				Role: "user",
				Content: []anthropicContentBlock{{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   m.Content.Text,
				}},
			})
		default: // user
			msgs = append(msgs, anthropicMessage{Role: "user", Content: userContent(m)})
		}
	}
	req.System = strings.Join(system, "\n\n")
	req.Messages = mergeToolResults(msgs)

	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// userContent builds the content of a user message: a plain string when there
// are no attachments, otherwise text + image blocks.
func userContent(m openai.Message) any {
	if len(m.Content.Attachments) == 0 {
		return m.Content.Text
	}
	blocks := make([]anthropicContentBlock, 0, len(m.Content.Attachments)+1)
	if m.Content.Text != "" {
		blocks = append(blocks, anthropicContentBlock{Type: "text", Text: m.Content.Text})
	}
	for _, att := range m.Content.Attachments {
		if src := imageSource(att); src != nil {
			blocks = append(blocks, anthropicContentBlock{Type: "image", Source: src})
		}
	}
	if len(blocks) == 0 {
		return m.Content.Text
	}
	return blocks
}

// imageSource maps an attachment to an Anthropic image source. Only real image
// MIME types are forwarded; a document would need the Files API, which this
// gateway does not have, so it is dropped rather than mistyped.
func imageSource(att openai.Attachment) any {
	mime := att.MIME
	if mime == "" {
		mime = "image/png"
	}
	if !strings.HasPrefix(mime, "image/") {
		return nil
	}
	if att.Inline() {
		return map[string]any{
			"type":       "base64",
			"media_type": mime,
			"data":       encodeBase64(att.Data),
		}
	}
	if att.URL != "" {
		return map[string]any{"type": "url", "url": att.URL}
	}
	return nil
}

func anthropicTools(tools []openai.Tool) []anthropicTool {
	out := make([]anthropicTool, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, anthropicTool{
			Name:        name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// anthropicToolChoice maps the OpenAI tool_choice to the Anthropic one:
// "required" → any, a named function → {type:tool,name}, "none" is dropped
// because Anthropic expresses "do not call" by omitting the tools.
func anthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "required":
			return map[string]any{"type": "any"}
		case "auto":
			return map[string]any{"type": "auto"}
		}
		return nil
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
		Name string `json:"name"`
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
	return map[string]any{"type": "tool", "name": name}
}

// mergeToolResults collapses consecutive user messages that consist only of
// tool_result blocks into one. Anthropic accepts a single user turn carrying
// several results (which is how parallel tool calls are answered), and a strict
// reader rejects two user turns in a row.
func mergeToolResults(in []anthropicMessage) []anthropicMessage {
	out := make([]anthropicMessage, 0, len(in))
	for _, m := range in {
		if n := len(out); n > 0 && isToolResultOnly(out[n-1]) && isToolResultOnly(m) {
			prev, _ := out[n-1].Content.([]anthropicContentBlock)
			cur, _ := m.Content.([]anthropicContentBlock)
			out[n-1].Content = append(prev, cur...)
			continue
		}
		out = append(out, m)
	}
	return out
}

func isToolResultOnly(m anthropicMessage) bool {
	blocks, ok := m.Content.([]anthropicContentBlock)
	if !ok || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// decodeStop accepts both the string and array forms of the OpenAI stop field.
func decodeStop(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		if one == "" {
			return nil
		}
		return []string{one}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		return many
	}
	return nil
}

// jsonOrEmptyObject returns s when it is a JSON object, else "{}".
func jsonOrEmptyObject(s string) string {
	t := strings.TrimSpace(s)
	if t != "" && json.Valid([]byte(t)) && strings.HasPrefix(t, "{") {
		return t
	}
	return "{}"
}
