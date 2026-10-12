package server

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/wuxianggujun/omnigate-panel/internal/anthropic"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// Anthropic Messages API 兼容端点：POST /v1/messages（+ POST /v1/messages/count_tokens）。
//
// 与 WorkBuddy 侧（internal/server/messages.go）完全同构，同样「翻译 + 复用」：
// 把 Anthropic 请求翻成 chat 请求，在进程内投给既有 HandleChat（供应商路由 / 换号
// 重试 / 限流冷却 / 请求日志 / 用量记账全部自动生效），再用一个 ResponseWriter 拦截器
// 把输出翻回 Anthropic 形态。翻译细节见 internal/anthropic。
//
// 有了这一层，任何 OmniGate 供应商（raccoon / trae / newapi / geminiweb / runable）
// 都能被只会说 Anthropic 协议的客户端（Claude Code 等）直接使用，而不只是
// WorkBuddy 账号池。模型名原样透传，本层不做 claude-* 映射。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body: "+err.Error())
		return
	}
	cr, err := anthropic.RequestToChat(body)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	if cr.DroppedTools > 0 {
		// 工具被静默丢弃 = 上游收不到 tools、模型只能干聊，是「会话里永远没有工具
		// 调用」的头号成因。留一行日志，别让用户报障成为唯一的发现途径。
		log.Printf("WARN: [omnigate] /v1/messages: dropped %d tool definition(s) in translation (tools[].name is required)", cr.DroppedTools)
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(cr.Body, &req); err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", "invalid chat body: "+err.Error())
		return
	}
	req.Stream = cr.Stream
	if !s.allowModel(w, r, req.Model) {
		return
	}
	tw := anthropic.NewTranslator(w, cr.Stream, anthropic.Options{
		Model:         cr.Model,
		MaxTokens:     cr.MaxTokens,
		InputTokens:   cr.InputTokens,
		StopSequences: cr.StopSequences,
	})
	s.gw.HandleChat(r.Context(), &req, tw)
	tw.Finish()
}

// handleCountTokens 处理 POST /v1/messages/count_tokens：本地估算（不引 tokenizer、
// 不打上游），口径与 /v1/messages 的 input_tokens 占位值一致。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		anthropic.WriteError(w, http.StatusBadRequest, "invalid_request_error", "failed to read body: "+err.Error())
		return
	}
	cr, err := anthropic.RequestToChat(body)
	if err != nil {
		writeAnthropicError(w, err)
		return
	}
	out, _ := json.Marshal(map[string]any{"input_tokens": cr.InputTokens})
	writeJSON(w, http.StatusOK, out)
}

// authorizedAnthropic 同 authorized，但 401 用 Anthropic 错误形态：Anthropic 客户端
// 直接读 error.type/error.message，喂 OpenAI 形态会显示成看不懂的报错。
func (s *Server) authorizedAnthropic(w http.ResponseWriter, r *http.Request) bool {
	if s.keyOK(r) {
		return true
	}
	anthropic.WriteError(w, http.StatusUnauthorized, "authentication_error", "invalid or missing API key")
	return false
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
