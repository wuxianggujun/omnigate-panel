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

// Done writes the SSE terminator.
func Done(w io.Writer) error {
	_, err := io.WriteString(w, "data: [DONE]\n\n")
	return err
}

// Completion builds a non-streaming chat.completion body.
func Completion(id, model string, created int64, msg Message, finish string) []byte {
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
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	b, _ := json.Marshal(obj)
	return b
}

// ErrorJSON builds an OpenAI-style error body.
func ErrorJSON(code, msg string) []byte {
	b, _ := json.Marshal(ErrorBody{Error: ErrorDetail{Message: msg, Type: "invalid_request_error", Code: code}})
	return b
}
