// Package openai contains the subset of the OpenAI HTTP schema omnigate speaks,
// plus a streaming (SSE) emitter.
package openai

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
)

// Attachment is one non-text content part of a message: an image or a file the
// caller attached.
//
// Content flattens text parts into Text and keeps everything else here, so a
// provider that can actually carry an attachment (geminiweb uploads it upstream
// and injects the resulting id into the payload) forwards it instead of dropping
// it. Only request bodies are ever parsed into this — our own replies are
// text-only, and Content still marshals back to a plain string.
type Attachment struct {
	// Type is the part type as the caller sent it ("image_url", "file", …).
	Type string
	// MIME and Filename describe the payload; both may be empty.
	MIME     string
	Filename string
	// Data holds the decoded bytes of an inline part (a data: URL). Nil when the
	// part points at a URL instead.
	Data []byte
	// URL is where a non-inline part lives ("" for inline parts).
	URL string
}

// Inline reports whether the attachment carries its own bytes.
func (a Attachment) Inline() bool { return len(a.Data) > 0 }

// Content accepts either a plain string or an array of parts. Text parts are
// flattened into Text; non-text parts are collected into Attachments. It always
// marshals back to a plain string, matching how most clients read it.
type Content struct {
	Text string
	// Attachments are the non-text parts of an array-form content (see
	// Attachment). Always empty for the plain string form.
	Attachments []Attachment
}

// UnmarshalJSON implements json.Unmarshaler.
func (c *Content) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	c.Text, c.Attachments = "", nil
	if len(b) == 0 || string(b) == "null" {
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
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil {
		// Unknown shape: keep the raw text so nothing is silently lost.
		c.Text = string(b)
		return nil
	}
	var sb strings.Builder
	parsed := 0
	for _, raw := range parts {
		var p struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		parsed++
		if p.Type == "" || p.Type == "text" || p.Type == "input_text" {
			sb.WriteString(p.Text)
			continue
		}
		if att, ok := parseAttachment(p.Type, raw); ok {
			c.Attachments = append(c.Attachments, att)
		}
	}
	if parsed == 0 && len(parts) > 0 {
		// Nothing in the array looked like a part object; keep the raw JSON
		// rather than replacing it with an empty string.
		c.Text, c.Attachments = string(b), nil
		return nil
	}
	c.Text = sb.String()
	return nil
}

// parseAttachment decodes one non-text part into an Attachment. It understands
// the shapes our three inbound dialects can produce: the OpenAI image_url/file
// parts (what the Anthropic and Responses translators emit too).
func parseAttachment(typ string, raw json.RawMessage) (Attachment, bool) {
	switch typ {
	case "image_url", "input_image":
		return parseImagePart(typ, raw)
	case "file", "input_file":
		return parseFilePart(typ, raw)
	}
	return Attachment{}, false
}

// parseImagePart reads {"image_url":{"url":…}} and the bare-string variant. A
// data: URL carries its own bytes, so it is decoded here and the URL cleared.
func parseImagePart(typ string, raw json.RawMessage) (Attachment, bool) {
	var p struct {
		ImageURL json.RawMessage `json:"image_url"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.ImageURL) == 0 {
		return Attachment{}, false
	}
	att := Attachment{Type: typ}
	if p.ImageURL[0] == '"' {
		if json.Unmarshal(p.ImageURL, &att.URL) != nil {
			return Attachment{}, false
		}
	} else {
		var obj struct {
			URL      string `json:"url"`
			MIMEType string `json:"mime_type"`
			Filename string `json:"filename"`
		}
		if json.Unmarshal(p.ImageURL, &obj) != nil {
			return Attachment{}, false
		}
		att.URL, att.MIME, att.Filename = obj.URL, obj.MIMEType, obj.Filename
	}
	if att.URL == "" {
		return Attachment{}, false
	}
	if data, mime, ok := decodeDataURL(att.URL); ok {
		att.Data, att.URL = data, ""
		if att.MIME == "" {
			att.MIME = mime
		}
	}
	return att, true
}

// parseFilePart reads {"file":{"filename":…,"file_data":"data:…"}}. A part that
// only references a file_id is skipped: nothing here has a file store to resolve
// it against, and uploading an empty part would be worse than dropping it.
func parseFilePart(typ string, raw json.RawMessage) (Attachment, bool) {
	var p struct {
		File json.RawMessage `json:"file"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.File) == 0 {
		return Attachment{}, false
	}
	var obj struct {
		Filename string `json:"filename"`
		FileData string `json:"file_data"`
	}
	if json.Unmarshal(p.File, &obj) != nil {
		return Attachment{}, false
	}
	data, mime, ok := decodeDataURL(obj.FileData)
	if !ok {
		return Attachment{}, false
	}
	att := Attachment{Type: typ, Filename: obj.Filename, Data: data}
	if mime != "" {
		att.MIME = mime
	}
	return att, true
}

// decodeDataURL decodes "data:<mime>;base64,<payload>".
func decodeDataURL(s string) ([]byte, string, bool) {
	if !strings.HasPrefix(s, "data:") {
		return nil, "", false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, "", false
	}
	meta := s[len("data:"):comma]
	if !strings.HasSuffix(meta, ";base64") {
		return nil, "", false
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s[comma+1:]))
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	return data, strings.TrimSuffix(meta, ";base64"), true
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
	// Strict 请求上游严格按 schema 生成参数（OpenAI structured function calling）。
	// 此前没有字段，客户端传了会被静默丢掉。
	Strict *bool `json:"strict,omitempty"`
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

	// 以下标准采样/结构控制字段此前**没有字段**：请求体里带了也会在解析这一步被
	// 静默丢掉（/omni/v1/chat/completions 与 /omni/v1/responses 都中招——Responses
	// 翻译层明明产出了 temperature/max_tokens/tool_choice/response_format，一转
	// struct 就没了）。它们被原样交给 provider（见 provider.ApplySampling）。
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls,omitempty"`
	ToolChoice          json.RawMessage `json:"tool_choice,omitempty"`
	ResponseFormat      json.RawMessage `json:"response_format,omitempty"`
	User                string          `json:"user,omitempty"`
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
