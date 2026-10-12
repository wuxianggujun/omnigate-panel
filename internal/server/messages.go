package server

// Anthropic Messages API 兼容端点：POST /v1/messages（+ POST /v1/messages/count_tokens）
//
// 上游（WorkBuddy 逆向网关）只实现 chat/completions；本文件让网关对外同时提供
// Anthropic Messages 协议，使 Claude Code 之类只会说 Anthropic 协议的客户端也能接入。
// 做法与 /v1/responses 完全同构（见 responses.go）：「翻译 + 复用」——把 Anthropic
// 请求翻译成 chat 请求，在进程内直接投给既有 chatCompletions（换号重试 / 429 冷却 /
// 内容拦截降级 / gateway_hint / 请求日志 / 积分记账全部自动生效），用一个
// ResponseWriter 拦截器接住输出再翻译回 Anthropic 形态。翻译细节见 internal/anthropic。
//
// 边界能力：文本 / 图片（base64 与 URL）/ 工具调用（tool_use ↔ tool_calls、
// tool_result ↔ role=tool）/ tool_choice / stop_sequences（含 stop_reason=stop_sequence
// 反推）/ 流式与非流式。thinking 不回吐（上游给不出 Anthropic 签名）。
//
// 模型名原样透传：本层不做 claude-* → 上游模型名的映射，客户端发什么就发什么，
// 由渠道/上游自己决定收哪些名字。

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"

	"github.com/wuxianggujun/omnigate-panel/internal/anthropic"
	"github.com/wuxianggujun/omnigate-panel/internal/httpauth"
)

// messages 处理 POST /v1/messages。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	cr, err := anthropic.RequestToChat(raw)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	if cr.DroppedTools > 0 {
		// 工具被静默丢弃 = 上游收不到 tools、模型只能干聊，是「会话里永远没有
		// 工具调用」的头号成因。留一行日志，别让用户报障成为唯一的发现途径。
		log.Printf("WARN: [server] /v1/messages: dropped %d tool definition(s) in translation (tools[].name is required)", cr.DroppedTools)
	}

	// 进程内投递：把转换后的 chat 请求交给既有 chatCompletions，复用全部逻辑。
	// 沿用同一 context（保留 requestTrace / 客户端取消信号），只替换 body 与 path。
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.URL = &url.URL{Path: "/v1/chat/completions"}
	r2.RequestURI = "/v1/chat/completions"
	r2.Body = io.NopCloser(bytes.NewReader(cr.Body))
	r2.ContentLength = int64(len(cr.Body))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")

	tw := anthropic.NewTranslator(w, cr.Stream, anthropic.Options{
		Model:         cr.Model,
		MaxTokens:     cr.MaxTokens,
		InputTokens:   cr.InputTokens,
		StopSequences: cr.StopSequences,
	})
	h.chatCompletions(tw, r2)
	tw.Finish()
}

// countTokens 处理 POST /v1/messages/count_tokens。
//
// 本地估算（不引 tokenizer、不打上游）：Anthropic 客户端用它做上下文预算，量级对
// 即可。复用 RequestToChat 的估算口径，保证与 /v1/messages 的 input_tokens 占位一致。
func (h *Handler) countTokens(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	cr, err := anthropic.RequestToChat(raw)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": cr.InputTokens})
}

// withAuthAnthropic 同 withAuth，但 401 用 Anthropic 错误形态（Anthropic 客户端直接
// 读 error.type/error.message，喂 OpenAI 形态会显示成看不懂的报错）。
func (h *Handler) withAuthAnthropic(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !httpauth.VerifyBearer(r, h.loadLive().APIKey) {
			anthropic.WriteError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		next(w, r)
	}
}

// writeAnthropicError 把翻译层错误写成 Anthropic 错误响应：*anthropic.Error 带
// status/type，其余一律按 400 invalid_request_error。
func writeAnthropicError(w http.ResponseWriter, err error) {
	var ae *anthropic.Error
	if errors.As(err, &ae) {
		anthropic.WriteError(w, ae.Status, ae.Type, ae.Message)
		return
	}
	anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
}
