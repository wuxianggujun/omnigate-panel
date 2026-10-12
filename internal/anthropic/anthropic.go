// Package anthropic 在 chat/completions 流水线之上实现 Anthropic Messages API
// （POST /v1/messages）。
//
// 形制与 internal/responses 完全一致：本包只做**纯翻译**，不碰账号池、轮转、
// 日志、积分记账——
//
//	Anthropic Messages 请求 ──RequestToChat──▶ chat/completions 请求
//	                                              │
//	                              （调用方跑既有 chat 流水线）
//	                                              │
//	Anthropic 响应 ◀────────Translator────────── chat 响应（JSON / SSE）
//
// 调用方用一个 Translator 包住 ResponseWriter 再跑既有 chat 流水线，Translator
// 接住输出并改写成 Anthropic 形态（非流式 message JSON 或 Anthropic SSE 事件流）。
// 换号重试 / 429 冷却 / 内容拦截降级 / gateway_hint / 请求日志 / 积分记账全部留在
// 调用方，零重复实现。
//
// 与 justdowork-proxy 的差异（均为有意为之，见各文件注释）：
//   - 模型名原样透传，不做重命名/映射：客户端发什么模型名就是什么。
//   - 不向客户端回吐 thinking 块：上游给不出 Anthropic 签名，回吐会让客户端下一轮
//     因签名缺失报错；上游的 reasoning_content 在流式路径被忽略。
//   - 工具名不重命名成 read/write/edit/bash：本网关上游原生支持 tools。
//   - tool_use 块在收尾时统一发出：Anthropic 的 content block 必须连续，而上游可能
//     把多个 tool call 的 delta 交错，统一缓冲可保证事件序列始终合法。
package anthropic

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
)

// id 前缀：Anthropic SDK 按前缀分流（msg_ 消息、toolu_ 工具调用）。
const (
	messageIDPrefix = "msg_"
	toolIDPrefix    = "toolu_"
)

// imageTokenCost 单张图片的 token 估算开销（无 tokenizer 时的粗口径：典型图片
// 约 1k token）。只影响 message_start 的 input_tokens 占位值。
const imageTokenCost = 1000

// maxFallbackBody 流式请求下为「上游其实返回了非流式 JSON」兜底保留的原文上限。
const maxFallbackBody = 1 << 20

// RandID 生成带前缀的随机 id（与 internal/responses.RandID 同形制）。
func RandID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// Error 是翻译层可判定的入站错误（携带 Anthropic 错误响应所需的 status/type）。
type Error struct {
	Status  int
	Type    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// ErrBadRequest 构造 400 invalid_request_error。
func ErrBadRequest(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: msg}
}

// WriteError 按 Anthropic 错误形态写响应：
//
//	{"type":"error","error":{"type":"<type>","message":"<msg>"}}
//
// status 为 0 时按 type 推断（见 statusForErrorType）。
func WriteError(w http.ResponseWriter, status int, typ, msg string) {
	if status == 0 {
		status = statusForErrorType(typ)
	}
	writeErrorJSON(w, status, typ, msg)
}

// writeErrorJSON 写出错误响应体（WriteError 与流式 error 事件共用同一形态）。
func writeErrorJSON(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(errorJSON(typ, msg))
}

// errorJSON 序列化 Anthropic 错误体。
func errorJSON(typ, msg string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
	return raw
}

// errorTypeForStatus 把 HTTP 状态映射到 Anthropic error.type。客户端按 type 决定
// 是否重试（429/529 可重试，4xx 不可），所以这里必须按语义分档而不是一刀切。
func errorTypeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	case http.StatusInternalServerError:
		return "api_error"
	}
	if status >= 500 {
		return "api_error"
	}
	if status >= 400 {
		return "invalid_request_error"
	}
	return "api_error"
}

// statusForErrorType 是 errorTypeForStatus 的反向（WriteError 未给 status 时用）。
func statusForErrorType(typ string) int {
	switch typ {
	case "invalid_request_error", "request_too_large":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// EstimateTokens 是本地 token 估算（不引 tokenizer 依赖）。
//
// 口径：ASCII 约 4 字符/token；CJK 等宽字符约 1.5 字符/token（中英混排下偏保守）。
// 只用于 message_start.usage.input_tokens 的**占位值**与 count_tokens 端点——
// 上游真值只在流末尾到达，届时以 message_delta 覆盖 output_tokens；input 无真值
// 可回填（Anthropic 协议把 input_tokens 放在 message_start）。
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range s {
		if r < 0x80 {
			ascii++
		} else {
			wide++
		}
	}
	n := ascii/4 + (wide*2)/3
	if n < 1 {
		n = 1
	}
	return n
}
