package openai

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// NewID returns a chat-completion id.
func NewID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "chatcmpl-000000000000000000000000"
	}
	return "chatcmpl-" + hex.EncodeToString(b[:])
}

// writeChunk marshals and writes a single SSE chunk.
func writeChunk(w io.Writer, id, model string, created int64, delta any, finish any) error {
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finish,
		}},
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// EmitRole writes the initial assistant role delta.
func EmitRole(w io.Writer, id, model string, created int64) error {
	return writeChunk(w, id, model, created, map[string]any{"role": "assistant", "content": ""}, nil)
}

// EmitContent writes a content delta.
func EmitContent(w io.Writer, id, model string, created int64, text string) error {
	return writeChunk(w, id, model, created, map[string]any{"content": text}, nil)
}

// KeepAliveField 是保活帧的顶层标记字段。真实上游不会发它；网关用它让下游各
// 方言的翻译层（Anthropic / Responses / Gemini）识别出「这一帧只为防读超时」，
// 从而改写成该方言自己的空转帧。普通 OpenAI 客户端会忽略未知字段。
const KeepAliveField = "omnigate_keepalive"

// EmitKeepAlive writes a *semantically inert but structurally valid* chunk: an
// empty delta with no finish_reason, tagged with KeepAliveField. It carries no
// content, so every OpenAI client ignores it, yet it is a real `data:` frame.
//
// 为什么不能用 SSE 注释帧（": ping"）：注释帧只在「客户端直连网关」时才有效。
// 一旦中间还有一层 SSE 再编码器（例如 new-api 的 StreamScannerHandler），那层会
// 只转发 `data:` / `[DONE]` 行、把注释行直接丢弃，于是客户端在长静默期一个字节
// 都收不到，最终按自己的读超时断连（线上 499 / context canceled 的成因）。
// 真实上游在思考阶段也正是发这种空 delta 帧，客户端对此完全兼容。
func EmitKeepAlive(w io.Writer, id, model string, created int64) error {
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": nil,
		}},
		KeepAliveField: true,
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// EmitReasoning writes a reasoning_content delta.
func EmitReasoning(w io.Writer, id, model string, created int64, text string) error {
	return writeChunk(w, id, model, created, map[string]any{"reasoning_content": text}, nil)
}

// EmitToolCalls writes a tool_calls delta (one chunk containing all calls).
func EmitToolCalls(w io.Writer, id, model string, created int64, calls []ToolCall) error {
	return writeChunk(w, id, model, created, map[string]any{"tool_calls": calls}, nil)
}

// EmitFinish writes the terminal chunk with a finish_reason.
func EmitFinish(w io.Writer, id, model string, created int64, reason string) error {
	return writeChunk(w, id, model, created, map[string]any{}, reason)
}

// EmitUsage writes a terminal usage-only chunk (empty choices), matching the
// OpenAI streaming shape. Upstreams report usage in a trailing chunk after the
// finish_reason chunk; forwarding it keeps clients (and the /v1/responses
// translation) on real token counts instead of zeros.
func EmitUsage(w io.Writer, id, model string, created int64, usage map[string]any) error {
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []any{},
		"usage":   usage,
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// Done writes the SSE terminator.
func Done(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// ZeroUsage is the placeholder usage block used when an upstream reports none.
func ZeroUsage() map[string]any {
	return map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
}

// Completion builds a non-streaming chat.completion body. usage may be nil, in
// which case a zero-usage block is emitted.
func Completion(id, model string, created int64, msg Message, finish string, usage map[string]any) []byte {
	if usage == nil {
		usage = ZeroUsage()
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": created,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	b, _ := json.Marshal(obj)
	return b
}

// ErrorJSON builds an OpenAI-style error body.
func ErrorJSON(code, msg string) []byte {
	b, _ := json.Marshal(ErrorBody{Error: ErrorDetail{Message: msg, Type: "invalid_request_error", Code: code}})
	return b
}
