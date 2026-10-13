package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// 深度研究（Deep Research）端点，移植自参考实现 gemini2api 的
// app/routers/research.py。把一个研究问题拆成「规划子问题 → 逐条研究 → 汇总成报告」
// 三步，每一步都复用网关的聊天路径（gateway.CompleteText），所以能用任意已配置的
// 模型（含 geminiweb 的 Gem 别名）。提供同步、SSE 流式、以及交互式追问三种形态：
//
//	POST /gemini/v1beta/deepresearch          同步返回完整结果
//	POST /gemini/v1beta/deepresearch/stream   SSE 进度 + 结果
//	POST /gemini/v1beta/deepresearch/interact 交互式（可流式逐词回放）
//
// 同时接受 /v1beta/deepresearch* 别名，与本服务既有的 Gemini 方言路径一致。

// deepResearchRequest 对应参考实现的 DeepResearchRequest。
type deepResearchRequest struct {
	Query      string `json:"query"`
	Model      string `json:"model"`
	Language   string `json:"language"`
	MaxSources int    `json:"max_sources"`
}

// interactionRequest 对应参考实现的 InteractionRequest。
type interactionRequest struct {
	Input      string `json:"input"`
	Stream     bool   `json:"stream"`
	Language   string `json:"language"`
	MaxSources int    `json:"max_sources"`
}

// deepResearchResult 是同步端点的返回体（字段与参考实现一致）。
type deepResearchResult struct {
	TaskID         string   `json:"task_id"`
	Query          string   `json:"query"`
	Plan           string   `json:"plan"`
	SubQuestions   []string `json:"sub_questions"`
	Findings       []string `json:"findings"`
	Report         string   `json:"report"`
	ConversationID string   `json:"conversation_id"`
}

const (
	deepResearchDefaultLanguage = "en"
	deepResearchDefaultSources  = 10
	// deepResearchTimeout 给整条「规划 + 逐条研究 + 汇总」链路上限；子问题多、模型慢
	// 时可能跑很久，给足余量。
	deepResearchTimeout = 15 * time.Minute
)

// subQuestionPrefix 剥掉子问题的单层编号前缀（"1. " / "2) " / "- "），保留其余文本
// （含正文里的前导数字）。锚定正则，避免参考实现早期 lstrip 误删正文的坑。
var subQuestionPrefix = regexp.MustCompile(`^\s*[-*\d]+[.)]?\s+`)

func normalizeLanguage(l string) string {
	if strings.TrimSpace(l) == "" {
		return deepResearchDefaultLanguage
	}
	return l
}

func normalizeMaxSources(n int) int {
	if n <= 0 {
		return deepResearchDefaultSources
	}
	return n
}

func planPrompt(query, language string) string {
	return fmt.Sprintf(
		"You are a research assistant. Break down the following query into 3-5 specific "+
			"sub-questions that need to be answered to thoroughly research this topic. "+
			"Also provide a brief outline. Respond in %s.\n\n"+
			"Query: %s\n\n"+
			"Format your response as:\n"+
			"SUB-QUESTIONS:\n1. [question]\n2. [question]\n...\n\n"+
			"OUTLINE:\n[brief outline]", language, query)
}

func researchPrompt(subQuestion, language string) string {
	return fmt.Sprintf(
		"Research the following question thoroughly: '%s'. "+
			"Provide detailed findings with sources where possible. "+
			"Respond in %s.", subQuestion, language)
}

func synthesizePrompt(query, language string, findings []string) string {
	parts := make([]string, 0, len(findings))
	for i, f := range findings {
		parts = append(parts, fmt.Sprintf("Finding %d:\n%s", i+1, f))
	}
	findingsText := strings.Join(parts, "\n\n---\n\n")
	return fmt.Sprintf(
		"Based on the following research findings, write a comprehensive report "+
			"answering the original query: '%s'\n\n%s\n\n"+
			"Provide a well-structured report with citations. Respond in %s.",
		query, findingsText, language)
}

// extractSubQuestions 从规划响应里抽出子问题（对应参考实现 _extract_sub_questions）。
func extractSubQuestions(planText string) []string {
	var questions []string
	inSection := false
	for _, raw := range strings.Split(planText, "\n") {
		line := strings.TrimSpace(raw)
		upper := strings.ToUpper(line)
		if strings.Contains(upper, "SUB-QUESTIONS:") {
			inSection = true
			continue
		}
		if strings.Contains(upper, "OUTLINE:") {
			inSection = false
			continue
		}
		if inSection && line != "" {
			cleaned := strings.TrimSpace(subQuestionPrefix.ReplaceAllString(line, ""))
			if cleaned != "" {
				questions = append(questions, cleaned)
			}
		}
	}
	if len(questions) == 0 {
		questions = []string{"Provide comprehensive information about this topic."}
	}
	return questions
}

// researchModel 决定用哪个模型：请求里给了就用它，否则用默认模型，再否则取目录里
// 第一个（对应参考实现「model 缺省取 models[0]」）。
func (s *Server) researchModel(ctx context.Context, requested string) (string, error) {
	if m := strings.TrimSpace(requested); m != "" {
		return m, nil
	}
	if s.cfg != nil {
		if m := strings.TrimSpace(s.cfg.DefaultModel); m != "" {
			return m, nil
		}
	}
	for _, m := range s.gw.ListModels(ctx) {
		if m.ID != "" {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("没有可用的模型")
}

// withImageBase 复刻 serveLogged 的图片基址注入：研究链路里若用到生图类 provider，
// 拼出的图片 URL 才能被客户端取到。
func (s *Server) withImageBase(r *http.Request) *http.Request {
	return r.WithContext(provider.WithImageBase(r.Context(), util.PublicBase(r)+omniMountPrefix))
}

func decodeResearchBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeBody(r, v); err != nil {
		writeJSON(w, http.StatusBadRequest, openai.ErrorJSON("invalid_body", err.Error()))
		return false
	}
	return true
}

// performDeepResearch 执行完整的三步流程（对应参考实现 _perform_deep_research）。
func (s *Server) performDeepResearch(ctx context.Context, query, model, language string, maxSources int) (*deepResearchResult, error) {
	taskID := util.UUID()

	planText, err := s.gw.CompleteText(ctx, model, planPrompt(query, language))
	if err != nil {
		return nil, err
	}
	subQuestions := extractSubQuestions(planText)
	if len(subQuestions) > maxSources {
		subQuestions = subQuestions[:maxSources]
	}

	findings := make([]string, 0, len(subQuestions))
	for _, q := range subQuestions {
		text, err := s.gw.CompleteText(ctx, model, researchPrompt(q, language))
		if err != nil {
			return nil, err
		}
		findings = append(findings, text)
	}

	report, err := s.gw.CompleteText(ctx, model, synthesizePrompt(query, language, findings))
	if err != nil {
		return nil, err
	}

	return &deepResearchResult{
		TaskID:       taskID,
		Query:        query,
		Plan:         planText,
		SubQuestions: subQuestions,
		Findings:     findings,
		Report:       report,
	}, nil
}

// sseWriter 把事件写成 SSE 帧（"data: <json>\n\n"）并即时 Flush。
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (s *sseWriter) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = s.w.Write([]byte("data: "))
	_, _ = s.w.Write(b)
	_, _ = s.w.Write([]byte("\n\n"))
	if s.f != nil {
		s.f.Flush()
	}
}

func startSSE(w http.ResponseWriter) *sseWriter {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

// handleDeepResearch 处理同步深度研究。
func (s *Server) handleDeepResearch(w http.ResponseWriter, r *http.Request) {
	var req deepResearchRequest
	if !decodeResearchBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSON(w, http.StatusBadRequest, openai.ErrorJSON("invalid_request", "query 不能为空"))
		return
	}
	model, err := s.researchModel(r.Context(), req.Model)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, openai.ErrorJSON("no_model", err.Error()))
		return
	}
	if !s.allowModelGemini(w, r, model) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), deepResearchTimeout)
	defer cancel()
	res, err := s.performDeepResearch(ctx, req.Query, model, normalizeLanguage(req.Language), normalizeMaxSources(req.MaxSources))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, openai.ErrorJSON("research_failed", err.Error()))
		return
	}
	s.writeValue(w, http.StatusOK, res)
}

// handleDeepResearchStream 处理 SSE 流式深度研究，事件序列与参考实现一致：
// progress / step / result / done（出错则 error）。
func (s *Server) handleDeepResearchStream(w http.ResponseWriter, r *http.Request) {
	var req deepResearchRequest
	if !decodeResearchBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Query) == "" {
		writeJSON(w, http.StatusBadRequest, openai.ErrorJSON("invalid_request", "query 不能为空"))
		return
	}
	model, err := s.researchModel(r.Context(), req.Model)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, openai.ErrorJSON("no_model", err.Error()))
		return
	}
	if !s.allowModelGemini(w, r, model) {
		return
	}

	language := normalizeLanguage(req.Language)
	maxSources := normalizeMaxSources(req.MaxSources)
	ctx := r.Context()
	sw := startSSE(w)

	taskID := util.UUID()
	sw.send(map[string]any{"type": "progress", "percentage": 0, "message": "Planning research..."})

	planText, err := s.gw.CompleteText(ctx, model, planPrompt(req.Query, language))
	if err != nil {
		sw.send(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	subQuestions := extractSubQuestions(planText)
	if len(subQuestions) > maxSources {
		subQuestions = subQuestions[:maxSources]
	}
	sw.send(map[string]any{
		"type": "step",
		"step": "plan",
		"data": map[string]any{
			"plan":            planText,
			"sub_questions":   subQuestions,
			"total_questions": len(subQuestions),
		},
	})
	sw.send(map[string]any{
		"type":       "progress",
		"percentage": 20,
		"message":    fmt.Sprintf("Generated %d research questions", len(subQuestions)),
	})

	total := len(subQuestions)
	findings := make([]string, 0, total)
	for i, q := range subQuestions {
		progress := 20
		if total > 0 {
			progress = 20 + int(float64(i)/float64(total)*60)
		}
		sw.send(map[string]any{
			"type":       "progress",
			"percentage": progress,
			"message":    fmt.Sprintf("Researching question %d/%d...", i+1, total),
		})
		text, err := s.gw.CompleteText(ctx, model, researchPrompt(q, language))
		if err != nil {
			sw.send(map[string]any{"type": "error", "message": err.Error()})
			return
		}
		findings = append(findings, text)
		sw.send(map[string]any{
			"type": "step",
			"step": "research",
			"data": map[string]any{
				"question_index": i,
				"question":       q,
				"finding":        text,
			},
		})
	}

	sw.send(map[string]any{"type": "progress", "percentage": 80, "message": "Synthesizing final report..."})
	report, err := s.gw.CompleteText(ctx, model, synthesizePrompt(req.Query, language, findings))
	if err != nil {
		sw.send(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	sw.send(map[string]any{"type": "step", "step": "synthesize", "data": map[string]any{"report": report}})
	sw.send(map[string]any{"type": "progress", "percentage": 100, "message": "Research complete"})
	sw.send(map[string]any{
		"type":            "result",
		"task_id":         taskID,
		"query":           req.Query,
		"report":          report,
		"conversation_id": "",
	})
	sw.send(map[string]any{"type": "done"})
}

// handleDeepResearchInteract 处理交互式追问：把输入当作一次直问，可选逐词流式回放
// （对应参考实现 /deepresearch/interact）。
func (s *Server) handleDeepResearchInteract(w http.ResponseWriter, r *http.Request) {
	var req interactionRequest
	if !decodeResearchBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Input) == "" {
		writeJSON(w, http.StatusBadRequest, openai.ErrorJSON("invalid_request", "input 不能为空"))
		return
	}
	model, err := s.researchModel(r.Context(), "")
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, openai.ErrorJSON("no_model", err.Error()))
		return
	}
	if !s.allowModelGemini(w, r, model) {
		return
	}
	prompt := fmt.Sprintf(
		"You are a research assistant. Answer the following question thoroughly "+
			"with detailed information and sources where possible. "+
			"Respond in %s.\n\nQuestion: %s", normalizeLanguage(req.Language), req.Input)

	if !req.Stream {
		ctx, cancel := context.WithTimeout(r.Context(), deepResearchTimeout)
		defer cancel()
		text, err := s.gw.CompleteText(ctx, model, prompt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, openai.ErrorJSON("research_failed", err.Error()))
			return
		}
		s.writeValue(w, http.StatusOK, map[string]any{"response": text, "conversation_id": ""})
		return
	}

	ctx := r.Context()
	sw := startSSE(w)
	text, err := s.gw.CompleteText(ctx, model, prompt)
	if err != nil {
		sw.send(map[string]any{"type": "error", "message": err.Error()})
		return
	}
	// 逐词回放（文本此刻已完整拿到，这里只是把参考实现的伪流式照搬过来）。
	words := strings.Fields(text)
	for i, word := range words {
		chunk := word + " "
		if i == len(words)-1 {
			chunk = word
		}
		sw.send(map[string]any{"type": "chunk", "text": chunk})
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Millisecond):
		}
	}
	sw.send(map[string]any{"type": "done", "conversation_id": ""})
}
