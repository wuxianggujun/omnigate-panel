// Package gemini implements a Gemini API (v1beta) compatibility layer on top of a
// chat/completions pipeline.
//
// Upstream providers only speak chat/completions; this package lets the gateway
// also expose Google's generateContent / streamGenerateContent endpoints by
// translation:
//
//	Gemini request ──RequestToChat──▶ chat/completions request
//	                                      │
//	                      （调用方跑既有 chat 流水线）
//	                                      │
//	Gemini response ◀────Translator────── chat response (JSON / SSE)
//
// The caller wraps its ResponseWriter with a Translator and runs the existing
// chat pipeline against it; the Translator intercepts the output and rewrites it
// to the Gemini wire shape. Rotation / retry / error policy / logging all stay in
// the caller, with zero duplication.
//
// Two wire details are easy to get wrong and are handled here:
//
//   - The wire format is camelCase (systemInstruction / inlineData / functionCall
//     / finishReason / usageMetadata). Clients that send snake_case are accepted
//     too (the official SDK sends camelCase; some hand-rolled callers send
//     snake_case) — see rawAlt.
//   - streamGenerateContent returns newline-delimited JSON, NOT SSE. Each line is
//     a complete response object and the stream ends with a finishReason +
//     usageMetadata line. There is no "data:" prefix and no [DONE] sentinel.
package gemini

import "encoding/json"

// ErrorJSON renders a Gemini error body. The Gemini API wraps errors as
// {"error":{"code":<http status>,"message":"…","status":"<STATUS>"}} — an
// OpenAI-shaped body surfaces as an unintelligible error in Gemini clients.
func ErrorJSON(httpStatus int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"code":    httpStatus,
			"message": msg,
			"status":  statusName(httpStatus),
		},
	})
	return b
}

// statusName maps an HTTP status to the google.rpc.Code name Gemini uses.
func statusName(httpStatus int) string {
	switch httpStatus {
	case 400:
		return "INVALID_ARGUMENT"
	case 401:
		return "UNAUTHENTICATED"
	case 403:
		return "PERMISSION_DENIED"
	case 404:
		return "NOT_FOUND"
	case 429:
		return "RESOURCE_EXHAUSTED"
	case 500:
		return "INTERNAL"
	case 502:
		return "UNAVAILABLE"
	case 503:
		return "UNAVAILABLE"
	case 504:
		return "DEADLINE_EXCEEDED"
	default:
		return "UNKNOWN"
	}
}
