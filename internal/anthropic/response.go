package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// 出站：chat/completions 响应 → Anthropic message
// ---------------------------------------------------------------------------

// chatUsage 是 chat/completions 的 usage（与 internal/responses 同字段口径）。
type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// chatToolCall 是聚合响应里的 message.tool_calls 条目。
type chatToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatResponse 是聚合（非流式）chat/completions 响应。
type chatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string         `json:"content"`
			ToolCalls []chatToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// messageParts 是一次响应的结构化内容（非流式路径与流式兜底重放共用）。
type messageParts struct {
	// Content 是 Anthropic content 块数组（text / tool_use）。
	Content []map[string]any
	// StopReason 是 end_turn / max_tokens / stop_sequence / tool_use。
	StopReason string
	// StopSequence 是命中的停止串（StopReason=stop_sequence 时非 nil）。
	StopSequence any
	// Usage 是 Anthropic usage 对象。
	Usage map[string]any
}

// parseChatResponse 解析聚合 chat/completions 响应；无 choices 视为不可解析。
func parseChatResponse(raw []byte) (*chatResponse, error) {
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil {
		return nil, err
	}
	if len(cr.Choices) == 0 {
		return nil, fmt.Errorf("chat response carries no choices")
	}
	return &cr, nil
}

// messageFromChat 把聚合响应转成 Anthropic message 的组成部分。
func messageFromChat(cr *chatResponse, stopSequences []string) *messageParts {
	msg := cr.Choices[0].Message
	parts := &messageParts{Content: make([]map[string]any, 0, 1+len(msg.ToolCalls))}

	if msg.Content != "" {
		parts.Content = append(parts.Content, map[string]any{"type": "text", "text": msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		id := tc.ID
		if id == "" {
			id = RandID(toolIDPrefix)
		}
		parts.Content = append(parts.Content, map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  tc.Function.Name,
			"input": jsonObject(tc.Function.Arguments),
		})
	}
	parts.StopReason, parts.StopSequence = stopReason(
		cr.Choices[0].FinishReason, msg.Content, len(msg.ToolCalls) > 0, stopSequences)
	parts.Usage = usageFromChat(cr.Usage)
	return parts
}

// stopReason 推断 Anthropic 的 stop_reason。
//
// 优先看上游 finish_reason；上游在工具回合常回 "stop"（而不是 "tool_calls"），
// 所以「有 tool_use 块」本身也算 tool_use 信号。stop_sequence 只能靠正文后缀
// 反推（上游不会告诉我们命中了哪个停止串）。
func stopReason(finish, text string, hasTools bool, stops []string) (string, any) {
	switch finish {
	case "length":
		return "max_tokens", nil
	case "tool_calls", "function_call":
		return "tool_use", nil
	case "content_filter":
		return "end_turn", nil
	}
	if hasTools {
		return "tool_use", nil
	}
	for _, s := range stops {
		if s != "" && strings.HasSuffix(text, s) {
			return "stop_sequence", s
		}
	}
	return "end_turn", nil
}

// usageFromChat 把 chat usage 翻成 Anthropic usage。
//
// input_tokens 要**减去**缓存命中：Anthropic 的 input_tokens 不含缓存读部分，
// 缓存命中单列在 cache_read_input_tokens。客户端（Claude Code）按
// input + cache_read + cache_creation 汇总上下文，照搬 prompt_tokens 会重复计数。
func usageFromChat(u *chatUsage) map[string]any {
	in, out, cached := 0, 0, 0
	if u != nil {
		in, out = u.PromptTokens, u.CompletionTokens
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
	}
	uncached := in - cached
	if uncached < 0 {
		uncached = 0
	}
	return map[string]any{
		"input_tokens":                uncached,
		"output_tokens":               out,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cached,
	}
}

// jsonObject 把 arguments 字符串归一成可嵌入响应的 JSON 对象；空/非法/非对象时
// 退化成 {}（Anthropic 要求 tool_use.input 是对象，非法片段会让客户端整条报错）。
func jsonObject(s string) json.RawMessage {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return json.RawMessage(`{}`)
	}
	var probe map[string]any
	if json.Unmarshal([]byte(trimmed), &probe) != nil {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(trimmed)
}

// ChatResponseToMessage 把非流式 chat 响应翻成 Anthropic message JSON。
// id 为空时生成。
func ChatResponseToMessage(chatResp []byte, model, id string, stopSequences []string) ([]byte, error) {
	cr, err := parseChatResponse(chatResp)
	if err != nil {
		return nil, err
	}
	return marshalMessage(messageFromChat(cr, stopSequences), model, id)
}

// marshalMessage 组装 Anthropic message 对象（非流式响应体）。
func marshalMessage(p *messageParts, model, id string) ([]byte, error) {
	if id == "" {
		id = RandID(messageIDPrefix)
	}
	content := p.Content
	if len(content) == 0 {
		// Anthropic 的 content 至少一个块；空回复给一个空 text 块。
		content = []map[string]any{{"type": "text", "text": ""}}
	}
	return json.Marshal(map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   p.StopReason,
		"stop_sequence": p.StopSequence,
		"usage":         p.Usage,
	})
}
