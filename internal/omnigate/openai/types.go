// Package openai contains the subset of the OpenAI HTTP schema omnigate speaks,
// plus a streaming (SSE) emitter.
package openai

import (
	"bytes"
	"encoding/json"
	"strings"
)

// Content accepts either a plain string or an array of {type,text} parts and
// always marshals back to a plain string, matching how most clients read it.
type Content struct {
	Text string
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || string(b) == "null" {
		c.Text = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		c.Text = s
		return nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &parts); err != nil {
		// Unknown shape: keep the raw text so nothing is silently lost.
		c.Text = string(b)
		return nil
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "" || p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	c.Text = sb.String()
	return nil
}

// MarshalJSON implements json.Marshaler.
func (c Content) MarshalJSON() ([]byte, error) { return json.Marshal(c.Text) }

// Message is a chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    Content    `json:"content"`
	Name       string     `json:"name,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolFunction carries a tool call's name and JSON-encoded arguments.
type ToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
}

// ToolCall is a function call emitted by (or requested of) the model.
type ToolCall struct {
	Index    int          `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function ToolFunction `json:"function"`
}

// ToolDef describes a callable function.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Tool is a tool definition in a chat request.
type Tool struct {
	Type     string  `json:"type"`
	Function ToolDef `json:"function"`
}

// StreamOptions mirrors OpenAI's stream_options. Only include_usage is honored:
// when true (and stream=true) the gateway emits an extra usage-only chunk before
// [DONE], exactly like OpenAI. Absent or false → no usage chunk (the spec
// default; clients that never asked for usage should not receive one).
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// ChatRequest is the incoming /v1/chat/completions body.
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Stream   bool      `json:"stream"`
	Tools    []Tool    `json:"tools,omitempty"`
	// ReasoningEffort is the OpenAI thinking tier (minimal/low/medium/high).
	// Forwarded to reasoning-capable upstreams (raccoon honors it) and recorded
	// in the request log's 思考 column.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// StreamOptions gates the trailing usage chunk (see StreamOptions). Usage is
	// still captured for the request log regardless of this flag.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// Model is one entry in GET /v1/models. The credit/price fields below are
// non-standard extensions: they are omitted when the upstream doesn't report
// them, and standard OpenAI clients ignore unknown fields, so compatibility is
// unaffected.
type Model struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	Created       int64  `json:"created,omitempty"`
	OwnedBy       string `json:"owned_by,omitempty"`
	ContextWindow int64  `json:"context_window,omitempty"`
	MaxTokens     int64  `json:"max_tokens,omitempty"`

	// Credits 是统一后的「积分消耗价」：raccoon 为计费倍率（×），runable 为每
	// 百万 token 消耗的积分。CreditUnit 解释单位。
	Credits    *float64 `json:"credits,omitempty"`
	CreditUnit string   `json:"credit_unit,omitempty"` // "multiplier" | "credits_per_million_tokens"
	// IsFree 上游标记的当前免费模型（runable）。
	IsFree bool `json:"is_free,omitempty"`
	// Pricing 上游给出的每 token 美元价（runable）。
	Pricing *ModelPricing `json:"pricing,omitempty"`
	// Billing 上游原始计费元数据（raccoon）。
	Billing *ModelBilling `json:"billing,omitempty"`
}

// ModelPricing 是上游给出的每 token 美元价（runable）。
type ModelPricing struct {
	Input           float64 `json:"input,omitempty"`
	Output          float64 `json:"output,omitempty"`
	InputCacheRead  float64 `json:"input_cache_read,omitempty"`
	InputCacheWrite float64 `json:"input_cache_write,omitempty"`
}

// ModelBilling 是上游原始计费元数据（raccoon 的 billing_* 字段）。
type ModelBilling struct {
	Category   string           `json:"category,omitempty"`
	Status     string           `json:"status,omitempty"`
	Note       string           `json:"status_note,omitempty"`
	Multiplier float64          `json:"multiplier,omitempty"`
	Effective  float64          `json:"effective_multiplier,omitempty"`
	Discounts  []map[string]any `json:"discounts,omitempty"`
}

// ModelList is the GET /v1/models response.
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ErrorBody is the OpenAI error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner error object.
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
