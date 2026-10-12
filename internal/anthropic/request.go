package anthropic

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// 入站：Anthropic Messages 请求 → chat/completions 请求
// ---------------------------------------------------------------------------

// request 是 Anthropic Messages 请求的宽松解码形态（未知字段忽略；content 保持
// raw，以便按块 type 分支）。
type request struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        json.RawMessage `json:"system"`
	Messages      []message       `json:"messages"`
	Tools         []tool          `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	TopK          *int            `json:"top_k"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Thinking      json.RawMessage `json:"thinking"`
	Metadata      map[string]any  `json:"metadata"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// contentBlock 是 content 数组元素（按 Type 分支取用；不相关字段留零值）。
type contentBlock struct {
	Type string `json:"type"`

	// Type=text
	Text string `json:"text"`

	// Type=image / document
	Source *mediaSource `json:"source"`

	// Type=tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// Type=tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// mediaSource 是 image/document 的来源（base64 内联或 URL）。
type mediaSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
	URL       string `json:"url"`
}

// ChatRequest 是 Anthropic 请求翻译成 chat/completions 请求后的结果。
type ChatRequest struct {
	// Body 是序列化后的 chat/completions 请求体。
	Body []byte
	// Stream 报告客户端是否要求 SSE。
	Stream bool
	// Model 是客户端请求的模型名（原样透传，翻译层不做映射）。
	Model string
	// MaxTokens 是 Anthropic 必填的 max_tokens（0 = 客户端没给）。
	MaxTokens int
	// StopSequences 是客户端的停止串：响应侧据此把 finish_reason=stop 且正文以
	// 停止串结尾的响应判成 stop_reason=stop_sequence。
	StopSequences []string
	// InputTokens 是本地估算的输入 token 数（message_start.usage.input_tokens 的
	// 占位值，同时供 /v1/messages/count_tokens 使用）。
	InputTokens int
	// DroppedTools 是入站 tools 里缺 name 被丢弃的条目数（调用方记一行日志）。
	DroppedTools int
}

// chatBody 是出站 chat/completions 请求体。
type chatBody struct {
	Model       string           `json:"model"`
	Messages    []map[string]any `json:"messages"`
	Tools       []map[string]any `json:"tools,omitempty"`
	ToolChoice  any              `json:"tool_choice,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Temperature *float64         `json:"temperature,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stop        []string         `json:"stop,omitempty"`
	Stream      bool             `json:"stream"`
}

// RequestToChat 把 Anthropic Messages 请求体翻译成 chat/completions 请求。
//
// 映射要点：
//   - system（字符串或 text 块数组）→ messages[0] role=system；
//   - user 文本/图片块 → content 字符串（纯文本）或 parts 数组（带图）；
//   - tool_result 块 → 独立的 role=tool 消息（OpenAI 要求紧随 assistant.tool_calls，
//     故先于同一条 user 消息里的其余文本/图片发出）；
//   - assistant tool_use 块 → message.tool_calls（input 序列化成 arguments 字符串）；
//   - thinking / redacted_thinking 块丢弃（上游无对应物，签名也无法伪造）；
//   - tools → chat tools（缺 name 的条目丢弃并计数）；tool_choice 的 any/tool
//     分别映射成 required / 指定函数；
//   - top_k 丢弃（chat/completions 无对应参数）；thinking 丢弃（见包注释）。
func RequestToChat(raw []byte) (*ChatRequest, error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, ErrBadRequest("invalid JSON body: " + err.Error())
	}
	if strings.TrimSpace(req.Model) == "" {
		return nil, ErrBadRequest("model: field required")
	}
	if len(req.Messages) == 0 {
		return nil, ErrBadRequest("messages: field required and must be non-empty")
	}

	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if sys := systemText(req.System); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	for _, m := range req.Messages {
		out, err := translateMessage(m)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, out...)
	}
	msgs = mergeAdjacent(msgs)

	tools, dropped := translateTools(req.Tools)

	body, err := json.Marshal(chatBody{
		Model:       req.Model,
		Messages:    msgs,
		Tools:       tools,
		ToolChoice:  translateToolChoice(req.ToolChoice),
		MaxTokens:   req.MaxTokens,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		Stop:        req.StopSequences,
		Stream:      req.Stream,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	return &ChatRequest{
		Body:          body,
		Stream:        req.Stream,
		Model:         req.Model,
		MaxTokens:     req.MaxTokens,
		StopSequences: req.StopSequences,
		InputTokens:   estimateInputTokens(msgs, tools),
		DroppedTools:  dropped,
	}, nil
}

// translateMessage 把一条 Anthropic 消息翻成 1..N 条 chat 消息。
//
// 一条 Anthropic 消息可能展开成多条：user 消息里的 tool_result 块必须先于其余
// 文本/图片发出（OpenAI 要求 role=tool 紧随 assistant.tool_calls）。全空的消息
// 返回 nil（不产生任何 chat 消息）。
func translateMessage(m message) ([]map[string]any, error) {
	switch strings.TrimSpace(m.Role) {
	case "user":
		return translateUserMessage(m.Content)
	case "assistant":
		return translateAssistantMessage(m.Content)
	case "system":
		// Anthropic 规范用顶层 system；历史里出现 system 角色时按 system 处理。
		if s := systemText(m.Content); s != "" {
			return []map[string]any{{"role": "system", "content": s}}, nil
		}
		return nil, nil
	default:
		return nil, ErrBadRequest("messages[].role: unknown role " + strconv.Quote(strings.TrimSpace(m.Role)))
	}
}

// translateUserMessage 翻译 user 消息：文本/图片 → 一条 user 消息；tool_result →
// 若干条 role=tool 消息（排在同一条 user 消息的前面）。
func translateUserMessage(raw json.RawMessage) ([]map[string]any, error) {
	if s, ok := rawString(raw); ok {
		if s == "" {
			return nil, nil
		}
		return []map[string]any{{"role": "user", "content": s}}, nil
	}
	blocks, err := contentBlocks(raw)
	if err != nil {
		return nil, err
	}

	var toolMsgs []map[string]any
	var text strings.Builder
	parts := make([]map[string]any, 0, len(blocks))
	hasImage := false
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "image":
			if p := imagePart(b.Source); p != nil {
				hasImage = true
				parts = append(parts, p)
			}
		case "tool_result":
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      toolResultText(b.Content),
			})
		default:
			// document / search_result 等：能取到文本就并入文本，取不到就跳过。
			text.WriteString(b.Text)
		}
	}

	out := toolMsgs
	switch {
	case hasImage:
		// 带图：content 用 parts 数组（text part 在前，image part 在后）。
		arr := make([]map[string]any, 0, len(parts)+1)
		if text.Len() > 0 {
			arr = append(arr, map[string]any{"type": "text", "text": text.String()})
		}
		arr = append(arr, parts...)
		out = append(out, map[string]any{"role": "user", "content": arr})
	case text.Len() > 0:
		out = append(out, map[string]any{"role": "user", "content": text.String()})
	}
	return out, nil
}

// translateAssistantMessage 翻译 assistant 消息：text 块拼接成 content，
// tool_use 块映射成 tool_calls，thinking 块丢弃。
func translateAssistantMessage(raw json.RawMessage) ([]map[string]any, error) {
	if s, ok := rawString(raw); ok {
		if s == "" {
			return nil, nil
		}
		return []map[string]any{{"role": "assistant", "content": s}}, nil
	}
	blocks, err := contentBlocks(raw)
	if err != nil {
		return nil, err
	}

	var text strings.Builder
	var calls []map[string]any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			id := b.ID
			if id == "" {
				id = RandID(toolIDPrefix)
			}
			calls = append(calls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": compactJSON(b.Input, "{}"),
				},
			})
		}
		// thinking / redacted_thinking 丢弃。
	}
	if text.Len() == 0 && len(calls) == 0 {
		return nil, nil
	}
	m := map[string]any{"role": "assistant"}
	if text.Len() > 0 {
		m["content"] = text.String()
	} else {
		// 带 tool_calls 的助手回合：OpenAI 规范 content 为 null。
		m["content"] = nil
	}
	if len(calls) > 0 {
		m["tool_calls"] = calls
	}
	return []map[string]any{m}, nil
}

// mergeAdjacent 合并相邻的同角色**纯文本**消息（user/assistant）。Anthropic 保证
// 角色交替，但展开 tool_result、客户端拼接等场景会产生连续同角色；上游对连续
// 同角色更宽容。带 tool_calls 的消息不参与合并（会破坏工具配对）。
func mergeAdjacent(msgs []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 && mergeable(out[n-1], m) {
			prev := out[n-1]
			prev["content"] = prev["content"].(string) + "\n\n" + m["content"].(string)
			continue
		}
		out = append(out, m)
	}
	return out
}

// mergeable 报告 a、b 是否可合并：同为 user/assistant、都不带 tool_calls、
// 且两侧 content 都是字符串。
func mergeable(a, b map[string]any) bool {
	role, _ := a["role"].(string)
	if role != "user" && role != "assistant" {
		return false
	}
	if b["role"] != role {
		return false
	}
	if _, ok := a["tool_calls"]; ok {
		return false
	}
	if _, ok := b["tool_calls"]; ok {
		return false
	}
	_, aok := a["content"].(string)
	_, bok := b["content"].(string)
	return aok && bok
}

// translateTools 把 Anthropic tools 翻成 chat tools。缺 name 的条目丢弃并计数
// （静默消失会让模型「永远不调这个工具」，调用方据此记日志）。input_schema 缺失
// 或非法时退化成空对象 schema。
func translateTools(tools []tool) ([]map[string]any, int) {
	out := make([]map[string]any, 0, len(tools))
	dropped := 0
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			dropped++
			continue
		}
		params := json.RawMessage(t.InputSchema)
		if len(bytes.TrimSpace(params)) == 0 || !json.Valid(params) {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		fn := map[string]any{"name": name, "parameters": params}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, dropped
}

// translateToolChoice 把 Anthropic tool_choice 映射成 chat tool_choice：
// auto→"auto"、any→"required"、none→"none"、tool→指定函数。未识别返回 nil
// （不带该字段，交由上游默认）。
func translateToolChoice(raw json.RawMessage) any {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		}
	}
	return nil
}

// estimateInputTokens 估算出站 chat 请求的输入 token：只计文本载荷（content 字符串、
// text part、tool_calls 的 name/arguments、system、tools 定义），图片按常数开销计——
// 避免把 base64 内联图片当成天量文本。
func estimateInputTokens(msgs []map[string]any, tools []map[string]any) int {
	var b strings.Builder
	images := 0
	for _, m := range msgs {
		b.WriteString(fmt.Sprint(m["role"]))
		switch c := m["content"].(type) {
		case string:
			b.WriteString(c)
		case []map[string]any:
			for _, p := range c {
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
				if p["type"] == "image_url" {
					images++
				}
			}
		}
		if tcs, ok := m["tool_calls"].([]map[string]any); ok {
			for _, tc := range tcs {
				if fn, ok := tc["function"].(map[string]any); ok {
					b.WriteString(fmt.Sprint(fn["name"]))
					b.WriteString(fmt.Sprint(fn["arguments"]))
				}
			}
		}
	}
	if len(tools) > 0 {
		if raw, err := json.Marshal(tools); err == nil {
			b.Write(raw)
		}
	}
	return EstimateTokens(b.String()) + images*imageTokenCost
}

// ---------------------------------------------------------------------------
// 归一化小工具
// ---------------------------------------------------------------------------

// rawString 判定 raw 是否为 JSON 字符串并解出内容（ok=false 表示不是字符串）。
func rawString(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(trimmed, &s) != nil {
		return "", false
	}
	return s, true
}

// contentBlocks 把 content 归一成块数组：字符串形态退化成单个 text 块。
func contentBlocks(raw json.RawMessage) ([]contentBlock, error) {
	if s, ok := rawString(raw); ok {
		if s == "" {
			return nil, nil
		}
		return []contentBlock{{Type: "text", Text: s}}, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	var blocks []contentBlock
	if err := json.Unmarshal(trimmed, &blocks); err != nil {
		return nil, ErrBadRequest("content: expected a string or an array of content blocks")
	}
	return blocks, nil
}

// systemText 归一 system 字段：字符串，或 [{type:text,text}] 块数组（cache_control
// 等字段忽略）。多块按顺序拼接；解析失败按空处理（不因 system 形态怪异而 400）。
func systemText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	if s, ok := rawString(raw); ok {
		return s
	}
	blocks, err := contentBlocks(raw)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		if blk.Type == "text" || blk.Type == "" {
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// toolResultText 把 tool_result 的 content 归一成字符串：字符串原样，块数组取
// text 块拼接。图片等非文本块在 chat 的 role=tool 消息里无处安放，替换成占位标记
// （不静默丢成空串，模型至少知道这里原本有张图）。
func toolResultText(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	if s, ok := rawString(raw); ok {
		return s
	}
	blocks, err := contentBlocks(raw)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, blk := range blocks {
		switch blk.Type {
		case "text":
			b.WriteString(blk.Text)
		case "image":
			b.WriteString("[image omitted]")
		}
	}
	return b.String()
}

// imagePart 把 Anthropic image 块翻成 chat 的 image_url part：base64 内联 →
// data: URL，url 形态原样。未知/空来源返回 nil（跳过该块）。
func imagePart(src *mediaSource) map[string]any {
	if src == nil {
		return nil
	}
	url := ""
	if src.Type == "url" && src.URL != "" {
		url = src.URL
	} else if src.Data != "" || src.URL != "" {
		// base64 形态；部分客户端省略 type 直接给 data/media_type。
		if src.Data != "" {
			mt := src.MediaType
			if mt == "" {
				mt = "image/png"
			}
			url = "data:" + mt + ";base64," + src.Data
		} else {
			url = src.URL
		}
	}
	if url == "" {
		return nil
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

// compactJSON 把 raw 压成紧凑 JSON 字符串；空/null/非法时返回 def。
func compactJSON(raw json.RawMessage, def string) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return def
	}
	var buf bytes.Buffer
	if json.Compact(&buf, trimmed) != nil {
		return def
	}
	return buf.String()
}
