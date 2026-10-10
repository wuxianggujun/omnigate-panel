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
