package server

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/gemini"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// Gemini API (v1beta) 兼容端点：GET /v1beta/models、
// POST /v1beta/models/{model}:generateContent 与
// POST /v1beta/models/{model}:streamGenerateContent。
//
// 与 /v1/responses、/v1/messages 完全同构：「翻译 + 复用」——把 Gemini 请求翻成
// chat 请求，投给既有 HandleChat（供应商路由 / 换号重试 / 限流冷却 / 请求日志 /
// 用量记账全部自动生效），再用一个 ResponseWriter 拦截器把输出翻回 Gemini 形态。
// 翻译细节见 internal/gemini。
//
// 注意 streamGenerateContent 回的是**换行分隔 JSON**（NDJSON），不是 SSE：每行一个
// 完整响应对象，末行带 finishReason + usageMetadata，没有 data: 前缀也没有 [DONE]。

// handleGeminiModels 处理 GET /v1beta/models。返回的是**本网关真实可用**的模型
// （按密钥收窄过滤），而不是上游那份写死的三条清单——否则客户端会拿着调不通的名字。
func (s *Server) handleGeminiModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	k, _ := s.keyScope(r) // 已过 authorized
	models := s.gw.ListModelsFor(ctx, k.Providers)
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]any{
			"name":                       "models/" + m.ID,
			"displayName":                m.ID,
			"description":                "",
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}
	body, _ := json.Marshal(map[string]any{"models": out})
	writeJSON(w, http.StatusOK, body)
}

// handleGeminiGenerate 处理两个 generateContent 端点。
func (s *Server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request) {
	model, stream, ok := geminiRoute(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, gemini.ErrorJSON(http.StatusNotFound, "not found"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, gemini.ErrorJSON(http.StatusBadRequest, "failed to read body: "+err.Error()))
		return
	}
	cr, err := gemini.RequestToChat(model, body, stream)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, gemini.ErrorJSON(http.StatusBadRequest, err.Error()))
		return
	}
	if cr.DroppedTools > 0 {
		// 工具被丢弃 = 模型看不见它们，只能干聊。留一行日志：这类「工具静默消失」
		// 此前只能靠用户报障才发现。
		log.Printf("WARN: [omnigate] /v1beta: dropped %d tool declaration(s) (nameless declaration or image-generation intent); the model cannot call them", cr.DroppedTools)
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(cr.Body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, gemini.ErrorJSON(http.StatusBadRequest, "invalid chat body: "+err.Error()))
		return
	}
	req.Stream = cr.Stream
	if !s.allowModelGemini(w, r, req.Model) {
		return
	}
	tw := gemini.NewTranslator(w, cr.Stream, cr.Model, cr.InputTokens)
	s.gw.HandleChat(r.Context(), &req, tw)
	tw.Finish()
}

// geminiRoute splits /v1beta/models/{model}:generateContent into the model name
// and whether the client asked to stream. The action suffix is matched longest
// first (":streamGenerateContent" also ends with "GenerateContent").
func geminiRoute(path string) (model string, stream bool, ok bool) {
	rest, found := strings.CutPrefix(path, "/v1beta/models/")
	if !found || rest == "" {
		return "", false, false
	}
	switch {
	case strings.HasSuffix(rest, ":streamGenerateContent"):
		return strings.TrimSuffix(rest, ":streamGenerateContent"), true, true
	case strings.HasSuffix(rest, ":generateContent"):
		return strings.TrimSuffix(rest, ":generateContent"), false, true
	default:
		return "", false, false
	}
}

// authorizedGemini 同 authorized，但 401 用 Gemini 错误形态。
func (s *Server) authorizedGemini(w http.ResponseWriter, r *http.Request) bool {
	if s.keyOK(r) {
		return true
	}
	writeJSON(w, http.StatusUnauthorized, gemini.ErrorJSON(http.StatusUnauthorized, "invalid or missing API key"))
	return false
}

// allowModelGemini 同 allowModel，但越权错误用 Gemini 形态。
func (s *Server) allowModelGemini(w http.ResponseWriter, r *http.Request, model string) bool {
	k, _ := s.keyScope(r) // 已过 authorized
	prov := s.gw.ProviderFor(model, k.Providers)
	if prov != "" {
		return true
	}
	writeJSON(w, http.StatusForbidden, gemini.ErrorJSON(http.StatusForbidden,
		"api key is scoped to providers ["+strings.Join(k.Providers, ", ")+"]; requested model "+model+" has no target in scope"))
	return false
}
