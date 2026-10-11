// Package responses implements an OpenAI Responses API (POST /v1/responses)
// compatibility layer on top of a chat/completions pipeline.
//
// Upstream providers only speak chat/completions; this package lets a gateway
// expose the Responses API by translation:
//
//	Responses request ──RequestToChat──▶ chat/completions request
//	                                          │
//	                            (the caller runs its own chat pipeline)
//	                                          │
//	Responses response ◀──Translator───── chat response (JSON / SSE)
//
// The caller wraps its ResponseWriter with a Translator and runs the existing
// chat pipeline against it; the Translator intercepts the output and rewrites
// it to the Responses shape (non-streaming JSON or the Responses SSE event
// stream). This keeps rotation/retry/error-policy/logging in the caller with
// zero duplication.
package responses

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// RandID generates a prefixed random id (resp_/msg_/fc_/rs_/call_).
func RandID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// Request translation: Responses → chat/completions
// ---------------------------------------------------------------------------

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
	// Function 是 chat/completions 形态的嵌套工具定义。Responses 规范用的是扁平
	// 形态（name/description/parameters 与 type 同级），但大量客户端把「写给 chat
	// 端点的」tools 原样发到 /v1/responses。只认扁平形态会把它们整批静默丢掉：
	// 上游收不到 tools，模型只能干聊——实案里模型甚至自述 "no tool namespace
	// listed"（会话中永远没有工具调用）。两种形态都收。
	Function *nestedFunction `json:"function"`
}

// nestedFunction 是 chat 形态 tools[].function 的内容。
type nestedFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type textFormat struct {
	Format *struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"format"`
}

type reasoning struct {
	Effort string `json:"effort"`
}

type inputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// Echo 是 OpenAI 会在 response 对象里原样回显的请求侧字段：tools /
// tool_choice / parallel_tool_calls。客户端（含严格校验的 Codex 类工具）可能读
// 这几个字段，此前恒为 []/"auto"/true，属于「回显撒谎」。零值 = 客户端没传，
// 按规范给默认值（tools=[]、tool_choice="auto"、parallel_tool_calls=true）。
type Echo struct {
	// Tools 是规范化成 Responses 扁平形态的工具定义（只含真正转发给上游的那些）。
	Tools []map[string]any
	// ToolChoice 是规范化后的 tool_choice：字符串，或 {"type":"function","name":…}。
	ToolChoice any
	// ParallelToolCalls 为 nil 时按规范回显 true。
	ParallelToolCalls *bool
}

// toolsJSON 返回回显用的 tools，永不为 nil（nil 会序列化成 null，规范是 []）。
func (e Echo) toolsJSON() []map[string]any {
	if e.Tools == nil {
		return []map[string]any{}
	}
	return e.Tools
}

// toolChoiceJSON 返回回显用的 tool_choice（缺省 "auto"）。
func (e Echo) toolChoiceJSON() any {
	if e.ToolChoice == nil {
		return "auto"
	}
	return e.ToolChoice
}

// parallelJSON 返回回显用的 parallel_tool_calls（缺省 true）。
func (e Echo) parallelJSON() bool {
	if e.ParallelToolCalls == nil {
		return true
	}
	return *e.ParallelToolCalls
}

// ChatRequest is a Responses request translated to a chat/completions request,
// plus the bookkeeping the caller needs to serve and (optionally) persist it.
type ChatRequest struct {
	// Body is the marshalled chat/completions request body.
	Body []byte
	// Stream reports whether the client asked for SSE streaming.
	Stream bool
	// Model is the requested model, as sent by the client.
	Model string
	// Messages is the full chat-format message list sent upstream (system +
	// any chained history + new input). The caller appends the assistant reply
	// when persisting the turn for previous_response_id.
	Messages []map[string]any
	// PreviousID is the previous_response_id the client chained from ("" if none).
	PreviousID string
	// Store reports whether the caller should persist this turn for chaining.
	Store bool
	// Metadata is echoed back verbatim in the response ("metadata").
	Metadata map[string]any
	// Echo 是响应对象里要回显的请求侧字段（tools / tool_choice /
	// parallel_tool_calls），调用方把它交给 Translator。
	Echo Echo
	// DroppedTools 是入站 tools 里没能映射成 chat 函数、被丢弃的条目数（type 非
	// function，或缺 name）。非零 = 模型看不见这些工具，只能干聊——调用方必须
	// 记一行日志：这类「工具静默消失」此前只能靠用户报障才发现。
	DroppedTools int
}

// RequestToChat translates a Responses API request body into a chat/completions
// request. When store is non-nil and the client passed previous_response_id,
// the stored conversation (if any) is prepended; a missing/expired history is
// skipped rather than failing the request (this shim is stateless upstream).
func RequestToChat(raw []byte, store *Store) (*ChatRequest, error) {
	var req struct {
		Model              string          `json:"model"`
		Input              json.RawMessage `json:"input"`
		Instructions       json.RawMessage `json:"instructions"`
		Stream             bool            `json:"stream"`
		MaxOutputTokens    *int            `json:"max_output_tokens"`
		Temperature        *float64        `json:"temperature"`
		TopP               *float64        `json:"top_p"`
		ParallelToolCalls  *bool           `json:"parallel_tool_calls"`
		Tools              []tool          `json:"tools"`
		ToolChoice         json.RawMessage `json:"tool_choice"`
		Reasoning          *reasoning      `json:"reasoning"`
		Text               *textFormat     `json:"text"`
		PreviousResponseID string          `json:"previous_response_id"`
		Store              *bool           `json:"store"`
		Metadata           json.RawMessage `json:"metadata"`
		User               string          `json:"user"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}

	inMsgs, err := inputToMessages(req.Input)
	if err != nil {
		return nil, err
	}

	system := instructionsText(req.Instructions)
	msgs := make([]map[string]any, 0, len(inMsgs)+4)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": system})
	}
	// Chain onto a stored conversation when previous_response_id is supplied.
	if req.PreviousResponseID != "" && store != nil {
		if conv, ok := store.Get(req.PreviousResponseID); ok {
			hist := conv.Messages
			// Keep exactly one system message at the front: if this request
			// carries new instructions, drop the stored one.
			if system != "" && len(hist) > 0 && hist[0]["role"] == "system" {
				hist = hist[1:]
			}
			msgs = append(msgs, hist...)
		}
	}
	msgs = append(msgs, inMsgs...)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("request has no input")
	}

	meta := map[string]any{}
	if len(req.Metadata) > 0 {
		_ = json.Unmarshal(req.Metadata, &meta)
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if req.Stream {
		out["stream"] = true
		// Responses 流式响应的 usage 出现在 response.completed 事件里，翻译层靠
		// chat 流末尾的 usage chunk 取真实 token 数；而 OpenAI 规范下流式 usage 是
		// opt-in，所以这里必须显式向 chat 层要 include_usage。
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	chatTools, droppedTools := toolsToChat(req.Tools)
	if chatTools != nil {
		out["tools"] = chatTools
	}
	if tc := toolChoiceToChat(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	if rf := textToChat(req.Text); rf != nil {
		out["response_format"] = rf
	}
	if req.User != "" {
		out["user"] = req.User
	}
	body, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}

	return &ChatRequest{
		Body:         body,
		Stream:       req.Stream,
		Model:        req.Model,
		Messages:     msgs,
		PreviousID:   req.PreviousResponseID,
		Store:        req.Store == nil || *req.Store, // OpenAI defaults store to true
		Metadata:     meta,
		Echo: Echo{
			Tools:             toolsEcho(req.Tools),
			ToolChoice:        toolChoiceEcho(req.ToolChoice),
			ParallelToolCalls: req.ParallelToolCalls,
		},
		DroppedTools: droppedTools,
	}, nil
}

// instructionsText flattens instructions, which the Responses API accepts either
// as a plain string or as an array of {type,text} parts.
func instructionsText(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	if t[0] == '"' {
		var s string
		if json.Unmarshal(t, &s) == nil {
			return s
		}
		return ""
	}
	if t[0] != '[' {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(t, &parts) != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

// inputToMessages translates the Responses input (string / array of input
// items) into chat messages. message / function_call / function_call_output are
// supported; reasoning items are dropped (no chat equivalent).
func inputToMessages(input json.RawMessage) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		return []map[string]any{{"role": "user", "content": s}}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("input must be a string or an array of input items")
	}
	var items []inputItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	msgs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		switch it.Type {
		case "function_call":
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   it.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      it.Name,
						"arguments": it.Arguments,
					},
				}},
			})
		case "function_call_output":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      rawToString(it.Output),
			})
		case "reasoning":
			// No chat equivalent; drop (there is no encrypted_content
			// round-trip here — reasoning items are not replayed upstream).
		default:
			// Only role-bearing items are messages; other item types
			// (item_reference, computer_call_output, …) have no chat mapping.
			if it.Role == "" {
				continue
			}
			content, err := contentToChat(it.Content)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, map[string]any{"role": it.Role, "content": content})
		}
	}
	return msgs, nil
}

// contentToChat translates message.content: a string passes through; an array
// of parts maps to chat text / image_url parts.
func contentToChat(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if trimmed[0] != '[' {
		return string(trimmed), nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			if u := rawToString(p.ImageURL); u != "" {
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": u},
				})
			}
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	return out, nil
}

// toolsToChat translates tools into the chat {type:"function",function:{...}}
// nesting. Both the Responses flat shape (name/description/parameters beside
// type) and the chat nested shape (function:{...}) are accepted — see the tool
// struct. Non-function tools are skipped.
//
// A missing parameters schema is filled with an empty object schema: the
// upstream only treats an entry as callable when it carries parameters, so a
// client that omits it (legal in some SDKs, required by the spec) would
// otherwise get tools that are silently invisible to the model.
//
// The second return is the number of dropped definitions; callers log it.
func toolsToChat(tools []tool) ([]map[string]any, int) {
	if len(tools) == 0 {
		return nil, 0
	}
	out := make([]map[string]any, 0, len(tools))
	dropped := 0
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			dropped++
			continue
		}
		name, desc, params, strict := t.Name, t.Description, t.Parameters, t.Strict
		if name == "" && t.Function != nil {
			name, desc, params, strict = t.Function.Name, t.Function.Description, t.Function.Parameters, t.Function.Strict
		}
		if name == "" {
			dropped++
			continue
		}
		fn := map[string]any{"name": name}
		if desc != "" {
			fn["description"] = desc
		}
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn["parameters"] = json.RawMessage(params)
		if strict != nil {
			fn["strict"] = *strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil, dropped
	}
	return out, dropped
}

// toolChoiceToChat translates tool_choice into the chat form: a string passes
// through; {type:"auto"|"none"|"required"} → that string; a named function
// (either the Responses flat {type:"function",name} or the chat nested
// {type:"function",function:{name}}) → {type:"function",function:{name}}.
func toolChoiceToChat(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil && s != "" {
			return s
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return nil
	}
	switch obj.Type {
	case "auto", "none", "required":
		return obj.Type
	}
	name := obj.Name
	if name == "" && obj.Function != nil {
		name = obj.Function.Name
	}
	if name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return nil
}

// toolsEcho normalizes the inbound tools into the Responses flat shape for the
// response object. OpenAI mirrors the request's tools back; both accepted
// inbound shapes collapse to the flat one, which is what a Responses client
// expects to read. Entries we did not forward (dropped by toolsToChat) are not
// echoed — echoing a tool the gateway cannot honor would be a lie.
func toolsEcho(tools []tool) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		name, desc, params, strict := t.Name, t.Description, t.Parameters, t.Strict
		if name == "" && t.Function != nil {
			name, desc, params, strict = t.Function.Name, t.Function.Description, t.Function.Parameters, t.Function.Strict
		}
		if name == "" {
			continue
		}
		item := map[string]any{"type": "function", "name": name}
		if desc != "" {
			item["description"] = desc
		}
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		item["parameters"] = json.RawMessage(params)
		if strict != nil {
			item["strict"] = *strict
		}
		out = append(out, item)
	}
	return out
}

// toolChoiceEcho normalizes the inbound tool_choice into the Responses shape for
// the response object. Absent/unparseable → "auto" (the spec default).
func toolChoiceEcho(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "auto"
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil && s != "" {
			return s
		}
		return "auto"
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if json.Unmarshal(trimmed, &obj) != nil {
		return "auto"
	}
	switch obj.Type {
	case "auto", "none", "required":
		return obj.Type
	}
	name := obj.Name
	if name == "" && obj.Function != nil {
		name = obj.Function.Name
	}
	if name != "" {
		return map[string]any{"type": "function", "name": name}
	}
	return "auto"
}

// textToChat translates text.format → response_format.
func textToChat(text *textFormat) any {
	if text == nil || text.Format == nil {
		return nil
	}
	switch text.Format.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{}
		if text.Format.Name != "" {
			js["name"] = text.Format.Name
		}
		if len(text.Format.Schema) > 0 {
			js["schema"] = json.RawMessage(text.Format.Schema)
		}
		if text.Format.Strict != nil {
			js["strict"] = *text.Format.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}

// rawToString converts a JSON raw value to a string: strings are dereferenced;
// other shapes (object/array/number) are returned verbatim.
func rawToString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

// ---------------------------------------------------------------------------
// Response translation (non-streaming): chat → Responses
// ---------------------------------------------------------------------------

type chatUsage struct {
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

// usageToResponses translates chat usage into Responses usage.
func usageToResponses(u *chatUsage) map[string]any {
	in, out, total, cached, reasoning := 0, 0, 0, 0, 0
	if u != nil {
		in, out, total = u.PromptTokens, u.CompletionTokens, u.TotalTokens
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			reasoning = u.CompletionTokensDetails.ReasoningTokens
		}
	}
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"input_tokens":          in,
		"output_tokens":         out,
		"total_tokens":          total,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
}

// ChatResponseToResponses translates a non-streaming chat response into a
// Responses response. id, when empty, is generated; metadata is echoed back;
// echo carries the request-side fields OpenAI mirrors back (tools /
// tool_choice / parallel_tool_calls).
func ChatResponseToResponses(chatResp []byte, fallbackModel, id string, metadata map[string]any, echo Echo) ([]byte, error) {
	if id == "" {
		id = RandID("resp_")
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	var cr struct {
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(chatResp, &cr); err != nil {
		return nil, err
	}
	created := cr.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	model := cr.Model
	if model == "" {
		model = fallbackModel
	}

	output := make([]map[string]any, 0, 3)
	outputText := ""
	status := "completed"
	var incomplete any

	if len(cr.Choices) > 0 {
		msg := cr.Choices[0].Message
		if strings.TrimSpace(msg.ReasoningContent) != "" {
			output = append(output, map[string]any{
				"type": "reasoning",
				"id":   RandID("rs_"),
				"summary": []map[string]any{
					{"type": "summary_text", "text": msg.ReasoningContent},
				},
			})
		}
		if msg.Content != "" {
			outputText = msg.Content
			output = append(output, map[string]any{
				"type":   "message",
				"id":     RandID("msg_"),
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": msg.Content, "annotations": []any{}},
				},
			})
		}
		for _, tc := range msg.ToolCalls {
			callID := tc.ID
			if callID == "" {
				callID = RandID("call_")
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        RandID("fc_"),
				"call_id":   callID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			})
		}
		if cr.Choices[0].FinishReason == "length" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		}
	}

	out := map[string]any{
		"id":                  id,
		"object":              "response",
		"created_at":          created,
		"status":              status,
		"model":               model,
		"output":              output,
		"output_text":         outputText,
		"usage":               usageToResponses(cr.Usage),
		"metadata":            metadata,
		"tools":               echo.toolsJSON(),
		"tool_choice":         echo.toolChoiceJSON(),
		"parallel_tool_calls": echo.parallelJSON(),
	}
	if incomplete != nil {
		out["incomplete_details"] = incomplete
	}
	return json.Marshal(out)
}

// ---------------------------------------------------------------------------
// ResponseWriter interceptor: catch the chat pipeline output and translate it
// ---------------------------------------------------------------------------

// Translator is an http.ResponseWriter that intercepts a chat/completions
// response and rewrites it into the Responses format. Callers run their chat
// pipeline with a Translator as the writer, then call Finish.
type Translator struct {
	inner    http.ResponseWriter
	stream   bool
	model    string
	id       string
	metadata map[string]any
	echo     Echo

	hdr         http.Header
	status      int
	wroteHeader bool
	sentHeader  bool

	body []byte // non-streaming (or streaming error passthrough) buffer

	st      *streamState
	lineBuf []byte
}

// NewTranslator builds a Translator wrapping inner. metadata (may be nil) is
// echoed back in the response envelope; echo carries the request-side fields
// OpenAI mirrors back (tools / tool_choice / parallel_tool_calls).
func NewTranslator(inner http.ResponseWriter, stream bool, model string, metadata map[string]any, echo Echo) *Translator {
	return &Translator{
		inner:    inner,
		stream:   stream,
		model:    model,
		id:       RandID("resp_"),
		metadata: metadata,
		echo:     echo,
		hdr:      http.Header{},
	}
}

// ResponseID returns the id assigned to the response being produced.
func (t *Translator) ResponseID() string { return t.id }

// AssistantMessage returns the assistant reply in chat format (role / content /
// reasoning_content / tool_calls) so the caller can persist the turn and serve
// a later previous_response_id. It returns nil when no usable reply was
// produced (error status, empty body, or a failed stream).
func (t *Translator) AssistantMessage() map[string]any {
	if t.status >= 400 {
		return nil
	}
	if t.stream {
		if t.st == nil || !t.st.finished || t.st.failed {
			return nil
		}
		return t.st.assistantMessage()
	}
	var cr struct {
		Choices []struct {
			Message map[string]any `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(t.body, &cr) != nil || len(cr.Choices) == 0 {
		return nil
	}
	m := cr.Choices[0].Message
	if m == nil {
		return nil
	}
	if _, ok := m["content"]; !ok {
		m["content"] = nil
	}
	if m["content"] == nil && m["tool_calls"] == nil && m["reasoning_content"] == nil {
		return nil
	}
	return m
}

// Header implements http.ResponseWriter.
func (t *Translator) Header() http.Header { return t.hdr }

// WriteHeader implements http.ResponseWriter (recorded, forwarded lazily).
func (t *Translator) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.status = code
	t.wroteHeader = true
}

// Write implements http.ResponseWriter.
func (t *Translator) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.status = http.StatusOK
		t.wroteHeader = true
	}
	// Non-streaming, or a streaming request that failed early (status>=400):
	// buffer and handle in Finish.
	if !t.stream || t.status >= 400 {
		t.body = append(t.body, p...)
		return len(p), nil
	}
	t.lineBuf = append(t.lineBuf, p...)
	t.processLines(false)
	return len(p), nil
}

// Flush implements http.Flusher (upstream may call it); real writes happen in
// rawWrite.
func (t *Translator) Flush() {}

// processLines splits SSE by line; final=true handles the trailing partial line.
func (t *Translator) processLines(final bool) {
	for {
		idx := bytes.IndexByte(t.lineBuf, '\n')
		if idx < 0 {
			break
		}
		line := t.lineBuf[:idx]
		t.lineBuf = t.lineBuf[idx+1:]
		t.handleLine(string(line))
	}
	if final && len(t.lineBuf) > 0 {
		t.handleLine(string(t.lineBuf))
		t.lineBuf = nil
	}
}

func (t *Translator) handleLine(line string) {
	s := strings.TrimRight(line, "\r")
	if !strings.HasPrefix(s, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	if t.st == nil {
		t.st = newStreamState(t, t.model)
	}
	t.st.consume(payload)
}

// Finish must be called after the chat pipeline returns; it completes the
// translation and writes the result.
func (t *Translator) Finish() {
	if !t.stream || t.status >= 400 {
		t.flushNonStream()
		return
	}
	t.processLines(true)
	if t.st == nil {
		// Streaming with no data frames at all: still emit a complete
		// response.created + response.completed so clients never hang.
		t.st = newStreamState(t, t.model)
	}
	t.st.finish()
}

func (t *Translator) flushNonStream() {
	// Error path (status>=400): pass the error JSON through verbatim.
	if t.status >= 400 {
		t.copyHeadersToInner()
		t.inner.WriteHeader(t.status)
		_, _ = t.inner.Write(t.body)
		return
	}
	converted, err := ChatResponseToResponses(t.body, t.model, t.id, t.metadata, t.echo)
	if err != nil {
		// Unparseable: pass the original body through (best effort).
		t.copyHeadersToInner()
		t.inner.WriteHeader(t.status)
		_, _ = t.inner.Write(t.body)
		return
	}
	t.hdr.Set("Content-Type", "application/json")
	t.copyHeadersToInner()
	t.inner.WriteHeader(t.status)
	_, _ = t.inner.Write(converted)
}

// copyHeadersToInner merges local headers into the underlying writer (keeping
// any it already set, e.g. X-Request-Id).
func (t *Translator) copyHeadersToInner() {
	for k, vs := range t.hdr {
		for _, v := range vs {
			t.inner.Header().Set(k, v)
		}
	}
}

// sendHeader flushes headers to inner before the first streaming write.
func (t *Translator) sendHeader() {
	if t.sentHeader {
		return
	}
	t.sentHeader = true
	if t.hdr.Get("Content-Type") == "" {
		t.hdr.Set("Content-Type", "text/event-stream")
	}
	t.copyHeadersToInner()
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	t.inner.WriteHeader(status)
}

// rawWrite writes directly to the underlying writer and flushes.
func (t *Translator) rawWrite(p []byte) {
	if !t.sentHeader {
		t.sendHeader()
	}
	if len(p) > 0 {
		_, _ = t.inner.Write(p)
	}
	if f, ok := t.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// Streaming translation: chat SSE frames → Responses SSE events
// ---------------------------------------------------------------------------

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string          `json:"role"`
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type toolState struct {
	index  int
	itemID string
	callID string
	name   string
	args   strings.Builder
}

type streamState struct {
	t        *Translator
	model    string
	id       string
	created  int64
	seq      int
	metadata map[string]any
	echo     Echo

	started  bool
	finished bool
	failed   bool

	outputIndex int

	msgStarted bool
	msgIndex   int
	msgID      string
	msgText    strings.Builder

	reasonStarted bool
	reasonIndex   int
	reasonID      string
	reasonText    strings.Builder

	toolsByID    map[string]*toolState
	toolsByIndex map[int]*toolState
	toolOrder    []*toolState

	usage *chatUsage

	// finishReason 上游给出的收尾原因（stop / length / tool_calls / content_filter…）。
	// 只有 length 会改变终局状态：与非流式路径一致把 status 标成 incomplete。
	finishReason string
}

func newStreamState(t *Translator, model string) *streamState {
	return &streamState{
		t:            t,
		model:        model,
		id:           t.id,
		created:      time.Now().Unix(),
		metadata:     t.metadata,
		echo:         t.echo,
		toolsByID:    map[string]*toolState{},
		toolsByIndex: map[int]*toolState{},
	}
}

// assistantMessage assembles the streamed reply into a chat-format assistant
// message, so the caller can persist the turn for previous_response_id.
func (s *streamState) assistantMessage() map[string]any {
	msg := map[string]any{"role": "assistant"}
	if text := s.msgText.String(); text != "" {
		msg["content"] = text
	} else {
		msg["content"] = nil
	}
	if s.reasonText.Len() > 0 {
		msg["reasoning_content"] = s.reasonText.String()
	}
	if len(s.toolOrder) > 0 {
		tcs := make([]map[string]any, 0, len(s.toolOrder))
		for _, st := range s.toolOrder {
			tcs = append(tcs, map[string]any{
				"id":   st.callID,
				"type": "function",
				"function": map[string]any{
					"name":      st.name,
					"arguments": st.args.String(),
				},
			})
		}
		msg["tool_calls"] = tcs
	}
	if msg["content"] == nil && len(s.toolOrder) == 0 {
		return nil
	}
	return msg
}

// consume handles one chat SSE frame payload.
func (s *streamState) consume(payload string) {
	if s.finished {
		return
	}
	// Upstream error frame (rate limit / content filter / moderation): surface
	// it to the client in Responses form.
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &probe) == nil && probe.Error != nil && probe.Error.Message != "" {
		s.start()
		s.fail(probe.Error.Type, probe.Error.Message)
		return
	}
	var chunk chatStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	s.start()
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finishReason = *ch.FinishReason
		}
		if ch.Delta.ReasoningContent != "" {
			s.onReasoning(ch.Delta.ReasoningContent)
		}
		if ch.Delta.Content != "" {
			s.onText(ch.Delta.Content)
		}
		if len(ch.Delta.ToolCalls) > 0 {
			s.onToolCalls(ch.Delta.ToolCalls)
		}
	}
}

func (s *streamState) start() {
	if s.started {
		return
	}
	s.started = true
	s.t.sendHeader()
	s.emit("response.created", map[string]any{"response": s.envelope("in_progress", nil)})
	s.emit("response.in_progress", map[string]any{"response": s.envelope("in_progress", nil)})
}

func (s *streamState) onText(delta string) {
	if !s.msgStarted {
		s.msgStarted = true
		s.msgIndex = s.outputIndex
		s.outputIndex++
		s.msgID = RandID("msg_")
		s.emit("response.output_item.added", map[string]any{
			"output_index": s.msgIndex,
			"item": map[string]any{
				"id": s.msgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		s.emit("response.content_part.added", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	s.msgText.WriteString(delta)
	s.emit("response.output_text.delta", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
		"delta": delta,
	})
}

func (s *streamState) onReasoning(delta string) {
	if !s.reasonStarted {
		s.reasonStarted = true
		s.reasonIndex = s.outputIndex
		s.outputIndex++
		s.reasonID = RandID("rs_")
		s.emit("response.output_item.added", map[string]any{
			"output_index": s.reasonIndex,
			"item":         map[string]any{"id": s.reasonID, "type": "reasoning", "summary": []any{}},
		})
		s.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	s.reasonText.WriteString(delta)
	s.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
		"delta": delta,
	})
}

func (s *streamState) onToolCalls(tcs []toolCallDelta) {
	for _, tc := range tcs {
		st := s.lookupTool(tc)
		if st == nil {
			st = &toolState{
				index:  s.outputIndex,
				itemID: RandID("fc_"),
				callID: tc.ID,
				name:   tc.Function.Name,
			}
			if st.callID == "" {
				st.callID = RandID("call_")
			}
			s.outputIndex++
			s.toolOrder = append(s.toolOrder, st)
			s.emit("response.output_item.added", map[string]any{
				"output_index": st.index,
				"item": map[string]any{
					"id": st.itemID, "type": "function_call", "status": "in_progress",
					"call_id": st.callID, "name": st.name, "arguments": "",
				},
			})
		}
		if tc.ID != "" {
			st.callID = tc.ID
			s.toolsByID[tc.ID] = st
		}
		s.toolsByIndex[tc.Index] = st
		if tc.Function.Name != "" {
			st.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			st.args.WriteString(tc.Function.Arguments)
			s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": st.itemID, "output_index": st.index,
				"delta": tc.Function.Arguments,
			})
		}
	}
}

// lookupTool finds the tool state a delta belongs to. It keys by id when the
// provider supplies one (handles providers that emit several calls in one chunk
// with index omitted), and falls back to index for incremental argument deltas.
func (s *streamState) lookupTool(tc toolCallDelta) *toolState {
	if tc.ID != "" {
		if st, ok := s.toolsByID[tc.ID]; ok {
			return st
		}
	}
	if st, ok := s.toolsByIndex[tc.Index]; ok {
		if tc.ID == "" || st.callID == tc.ID {
			return st
		}
	}
	return nil
}

// finish closes every output item and emits the terminal response event.
func (s *streamState) finish() {
	if s.finished {
		return
	}
	s.finished = true
	s.start() // guarantee response.created was emitted

	output := make([]map[string]any, 0, 3)
	if s.reasonStarted {
		s.closeReasoning(&output)
	}
	if s.msgStarted {
		s.closeMessage(&output)
	}
	for _, st := range s.toolOrder {
		s.closeTool(st, &output)
	}
	// 上游按 max_output_tokens 截断时，与**非流式**路径保持一致：status 标成
	// incomplete 并带上 incomplete_details，客户端据此能提示「回答被截断」。
	// 终止事件名仍用 response.completed —— OpenAI 的三种终止事件
	// （completed / incomplete / failed）SDK 都认，但只等 completed 的手写客户端
	// 会因为换成 response.incomplete 一直挂住；这里取「状态正确 + 不挂客户端」的折中。
	status := "completed"
	if s.finishReason == "length" {
		status = "incomplete"
	}
	env := s.envelopeWithUsage(status, output)
	if status == "incomplete" {
		env["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	s.emit("response.completed", map[string]any{"response": env})
}

func (s *streamState) closeMessage(output *[]map[string]any) {
	text := s.msgText.String()
	s.emit("response.output_text.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := map[string]any{
		"id": s.msgID, "type": "message", "status": "completed", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.msgIndex, "item": item})
	*output = append(*output, item)
}

func (s *streamState) closeReasoning(output *[]map[string]any) {
	text := s.reasonText.String()
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0, "text": text,
	})
	s.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	})
	item := map[string]any{
		"id": s.reasonID, "type": "reasoning",
		"summary": []map[string]any{{"type": "summary_text", "text": text}},
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.reasonIndex, "item": item})
	*output = append(*output, item)
}

func (s *streamState) closeTool(st *toolState, output *[]map[string]any) {
	args := st.args.String()
	s.emit("response.function_call_arguments.done", map[string]any{
		"item_id": st.itemID, "output_index": st.index, "arguments": args,
	})
	item := map[string]any{
		"id": st.itemID, "type": "function_call", "status": "completed",
		"call_id": st.callID, "name": st.name, "arguments": args,
	}
	s.emit("response.output_item.done", map[string]any{"output_index": st.index, "item": item})
	*output = append(*output, item)
}

// fail emits response.failed (upstream error frame).
func (s *streamState) fail(code, msg string) {
	s.finished = true
	s.failed = true
	env := s.envelope("failed", []map[string]any{})
	env["error"] = map[string]any{"code": code, "message": msg}
	s.emit("response.failed", map[string]any{"response": env})
}

func (s *streamState) envelope(status string, output []map[string]any) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	meta := s.metadata
	if meta == nil {
		meta = map[string]any{}
	}
	return map[string]any{
		"id":                  s.id,
		"object":              "response",
		"created_at":          s.created,
		"status":              status,
		"model":               s.model,
		"output":              output,
		"usage":               nil,
		"parallel_tool_calls": s.echo.parallelJSON(),
		"tool_choice":         s.echo.toolChoiceJSON(),
		"tools":               s.echo.toolsJSON(),
		"error":               nil,
		"incomplete_details":  nil,
		"metadata":            meta,
	}
}

func (s *streamState) envelopeWithUsage(status string, output []map[string]any) map[string]any {
	env := s.envelope(status, output)
	env["usage"] = usageToResponses(s.usage)
	return env
}

// emit writes one Responses SSE event (event: <type>\ndata: <json>\n\n).
func (s *streamState) emit(eventType string, payload map[string]any) {
	payload["type"] = eventType
	payload["sequence_number"] = s.seq
	s.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(eventType)
	b.WriteString("\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	s.t.rawWrite([]byte(b.String()))
}
