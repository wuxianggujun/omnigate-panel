package server

// Responses API 兼容层：POST /v1/responses
//
// 上游（WorkBuddy 逆向网关）只实现 chat/completions；本文件让网关对外同时提供
// OpenAI Responses API。做法是「翻译 + 复用」：
//
//	Responses 请求 ──translate──▶ chat/completions 请求
//	                                    │
//	                          复用整条 chat 流水线
//	                  （选号/轮转/错误策略/请求日志/粘性…）
//	                                    │
//	  Responses 响应 ◀──translate── chat 响应（非流式 JSON / 流式 SSE）
//
// 复用方式：把转换后的 chat 请求在进程内直接投给 h.chatCompletions，用一个
// ResponseWriter 拦截器接住它的输出再翻译。好处是零重复实现——换号重试、429/6004
// 冷却、内容拦截降级、gateway_hint、请求日志等全部自动生效，未来 chat 侧任何改进
// 都同时惠及 /v1/responses。
//
// 兼容性：NewAPI（渠道侧）对 RelayModeResponses 走「原生透传」——它把上游 SSE
// 按 OpenAI Responses 事件原样转发给客户端，因此这里产出的必须是标准 Responses
// SSE（response.created / output_item.added / output_text.delta / … / response.completed）。

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// randID 生成带前缀的随机 ID（resp_/msg_/fc_/rs_/call_）。
func randID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}

// responses 处理 POST /v1/responses：翻译请求 → 复用 chat 流水线 → 翻译响应。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	chatBody, stream, model, err := responsesRequestToChat(raw)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 进程内投递：把转换后的 chat 请求交给既有 chatCompletions，复用全部逻辑。
	// 沿用同一 context（保留 requestTrace / 客户端取消信号），只替换 body 与 path。
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.URL = &url.URL{Path: "/v1/chat/completions"}
	r2.RequestURI = "/v1/chat/completions"
	r2.Body = io.NopCloser(bytes.NewReader(chatBody))
	r2.ContentLength = int64(len(chatBody))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")

	tw := newResponsesTranslator(w, stream, model)
	h.chatCompletions(tw, r2)
	tw.finish()
}

// ---------------------------------------------------------------------------
// 请求翻译：Responses → chat/completions
// ---------------------------------------------------------------------------

type responsesTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict"`
}

type responsesTextFormat struct {
	Format *struct {
		Type   string          `json:"type"`
		Name   string          `json:"name"`
		Schema json.RawMessage `json:"schema"`
		Strict *bool           `json:"strict"`
	} `json:"format"`
}

type responsesReasoning struct {
	Effort string `json:"effort"`
}

type responsesInputItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// responsesRequestToChat 把 Responses API 请求体翻译成 chat/completions 请求体。
// 返回转换后的 JSON、是否流式、请求模型名。
func responsesRequestToChat(raw []byte) (chat []byte, stream bool, model string, err error) {
	var req struct {
		Model             string               `json:"model"`
		Input             json.RawMessage      `json:"input"`
		Instructions      string               `json:"instructions"`
		Stream            bool                 `json:"stream"`
		MaxOutputTokens   *int                 `json:"max_output_tokens"`
		Temperature       *float64             `json:"temperature"`
		TopP              *float64             `json:"top_p"`
		ParallelToolCalls *bool                `json:"parallel_tool_calls"`
		Tools             []responsesTool      `json:"tools"`
		ToolChoice        json.RawMessage      `json:"tool_choice"`
		Reasoning         *responsesReasoning  `json:"reasoning"`
		Text              *responsesTextFormat `json:"text"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, false, "", fmt.Errorf("invalid JSON body: %w", err)
	}
	model = req.Model
	stream = req.Stream

	msgs := make([]map[string]any, 0, 4)
	if strings.TrimSpace(req.Instructions) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.Instructions})
	}
	inMsgs, err := responsesInputToMessages(req.Input)
	if err != nil {
		return nil, false, "", err
	}
	msgs = append(msgs, inMsgs...)
	if len(msgs) == 0 {
		return nil, false, "", fmt.Errorf("request has no input")
	}

	out := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if req.Stream {
		out["stream"] = true
	}
	if req.MaxOutputTokens != nil {
		out["max_tokens"] = *req.MaxOutputTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if tools := responsesToolsToChat(req.Tools); tools != nil {
		out["tools"] = tools
	}
	if tc := responsesToolChoiceToChat(req.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if req.Reasoning != nil && req.Reasoning.Effort != "" {
		out["reasoning_effort"] = req.Reasoning.Effort
	}
	if rf := responsesTextToChat(req.Text); rf != nil {
		out["response_format"] = rf
	}
	chat, err = json.Marshal(out)
	if err != nil {
		return nil, false, "", err
	}
	return chat, stream, model, nil
}

// responsesInputToMessages 把 Responses 的 input（字符串 / 输入项数组）翻译成 chat
// messages。支持 message / function_call / function_call_output；reasoning 项丢弃
// （chat 无对应表示）。
func responsesInputToMessages(input json.RawMessage) ([]map[string]any, error) {
	trimmed := bytes.TrimSpace(input)
	if len(trimmed) == 0 {
		return nil, nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		return []map[string]any{{"role": "user", "content": s}}, nil
	}
	if trimmed[0] != '[' {
		return nil, fmt.Errorf("input must be a string or an array of input items")
	}
	var items []responsesInputItem
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	msgs := make([]map[string]any, 0, len(items))
	for _, it := range items {
		switch it.Type {
		case "function_call":
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []map[string]any{{
					"id":   it.CallID,
					"type": "function",
					"function": map[string]any{
						"name":      it.Name,
						"arguments": it.Arguments,
					},
				}},
			})
		case "function_call_output":
			msgs = append(msgs, map[string]any{
				"role":         "tool",
				"tool_call_id": it.CallID,
				"content":      rawToString(it.Output),
			})
		case "reasoning":
			// 无 chat 对应表示，丢弃。
		default:
			role := it.Role
			if role == "" {
				role = "user"
			}
			content, err := responsesContentToChat(it.Content)
			if err != nil {
				return nil, err
			}
			msgs = append(msgs, map[string]any{"role": role, "content": content})
		}
	}
	return msgs, nil
}

// responsesContentToChat 翻译 message.content：字符串原样；部件数组映射为 chat 的
// text / image_url 部件。
func responsesContentToChat(raw json.RawMessage) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil, err
		}
		return s, nil
	}
	if trimmed[0] != '[' {
		return string(trimmed), nil
	}
	var parts []struct {
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		ImageURL json.RawMessage `json:"image_url"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "input_text", "text", "output_text":
			out = append(out, map[string]any{"type": "text", "text": p.Text})
		case "input_image":
			if u := rawToString(p.ImageURL); u != "" {
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": u},
				})
			}
		}
	}
	if len(out) == 0 {
		return "", nil
	}
	return out, nil
}

// responsesToolsToChat 翻译 tools：Responses 的扁平 function 形态 → chat 的
// {type:"function",function:{...}} 嵌套形态。非 function 工具（web_search 等）跳过。
func responsesToolsToChat(tools []responsesTool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Name == "" {
			continue
		}
		fn := map[string]any{"name": t.Name}
		if t.Description != "" {
			fn["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			fn["parameters"] = json.RawMessage(t.Parameters)
		}
		if t.Strict != nil {
			fn["strict"] = *t.Strict
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// responsesToolChoiceToChat 翻译 tool_choice：字符串原样；{type:"function",name} →
// chat 的 {type:"function",function:{name}}。
func responsesToolChoiceToChat(raw json.RawMessage) any {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil && s != "" {
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(trimmed, &obj) == nil && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// responsesTextToChat 翻译 text.format → response_format。
func responsesTextToChat(text *responsesTextFormat) any {
	if text == nil || text.Format == nil {
		return nil
	}
	switch text.Format.Type {
	case "json_object":
		return map[string]any{"type": "json_object"}
	case "json_schema":
		js := map[string]any{}
		if text.Format.Name != "" {
			js["name"] = text.Format.Name
		}
		if len(text.Format.Schema) > 0 {
			js["schema"] = json.RawMessage(text.Format.Schema)
		}
		if text.Format.Strict != nil {
			js["strict"] = *text.Format.Strict
		}
		return map[string]any{"type": "json_schema", "json_schema": js}
	}
	return nil
}

// rawToString 把 JSON 原始值转成字符串：字符串解引用；其余（对象/数组/数字）返回原文。
func rawToString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
	}
	return string(trimmed)
}

// ---------------------------------------------------------------------------
// 响应翻译（非流式）：chat → Responses
// ---------------------------------------------------------------------------

type chatUsage struct {
	PromptTokens            int `json:"prompt_tokens"`
	CompletionTokens        int `json:"completion_tokens"`
	TotalTokens             int `json:"total_tokens"`
	PromptTokensDetails     *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// responsesUsage 把 chat usage 翻译成 Responses usage。
func responsesUsage(u *chatUsage) map[string]any {
	in, out, total, cached, reasoning := 0, 0, 0, 0, 0
	if u != nil {
		in, out, total = u.PromptTokens, u.CompletionTokens, u.TotalTokens
		if u.PromptTokensDetails != nil {
			cached = u.PromptTokensDetails.CachedTokens
		}
		if u.CompletionTokensDetails != nil {
			reasoning = u.CompletionTokensDetails.ReasoningTokens
		}
	}
	if total == 0 {
		total = in + out
	}
	return map[string]any{
		"input_tokens":          in,
		"output_tokens":         out,
		"total_tokens":          total,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens_details": map[string]any{"reasoning_tokens": reasoning},
	}
}

// chatResponseToResponses 把非流式 chat 响应翻译成 Responses 响应。
func chatResponseToResponses(chatResp []byte, fallbackModel string) ([]byte, error) {
	var cr struct {
		Created int64  `json:"created"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role             string `json:"role"`
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				ToolCalls        []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *chatUsage `json:"usage"`
	}
	if err := json.Unmarshal(chatResp, &cr); err != nil {
		return nil, err
	}
	created := cr.Created
	if created == 0 {
		created = time.Now().Unix()
	}
	model := cr.Model
	if model == "" {
		model = fallbackModel
	}

	output := make([]map[string]any, 0, 3)
	outputText := ""
	status := "completed"
	var incomplete any

	if len(cr.Choices) > 0 {
		msg := cr.Choices[0].Message
		if strings.TrimSpace(msg.ReasoningContent) != "" {
			output = append(output, map[string]any{
				"type": "reasoning",
				"id":   randID("rs_"),
				"summary": []map[string]any{
					{"type": "summary_text", "text": msg.ReasoningContent},
				},
			})
		}
		if msg.Content != "" {
			outputText = msg.Content
			output = append(output, map[string]any{
				"type":   "message",
				"id":     randID("msg_"),
				"status": "completed",
				"role":   "assistant",
				"content": []map[string]any{
					{"type": "output_text", "text": msg.Content, "annotations": []any{}},
				},
			})
		}
		for _, tc := range msg.ToolCalls {
			callID := tc.ID
			if callID == "" {
				callID = randID("call_")
			}
			output = append(output, map[string]any{
				"type":      "function_call",
				"id":        randID("fc_"),
				"call_id":   callID,
				"name":      tc.Function.Name,
				"arguments": tc.Function.Arguments,
				"status":    "completed",
			})
		}
		if cr.Choices[0].FinishReason == "length" {
			status = "incomplete"
			incomplete = map[string]any{"reason": "max_output_tokens"}
		}
	}

	out := map[string]any{
		"id":          randID("resp_"),
		"object":      "response",
		"created_at":  created,
		"status":      status,
		"model":       model,
		"output":      output,
		"output_text": outputText,
		"usage":       responsesUsage(cr.Usage),
	}
	if incomplete != nil {
		out["incomplete_details"] = incomplete
	}
	return json.Marshal(out)
}

// ---------------------------------------------------------------------------
// ResponseWriter 拦截器：接住 chat 流水线的输出并翻译
// ---------------------------------------------------------------------------

type responsesTranslator struct {
	inner  http.ResponseWriter
	stream bool
	model  string

	hdr         http.Header
	status      int
	wroteHeader bool
	sentHeader  bool

	body []byte // 非流式（或流式错误透传）累积

	st      *responsesStream // 流式状态
	lineBuf []byte
}

func newResponsesTranslator(inner http.ResponseWriter, stream bool, model string) *responsesTranslator {
	return &responsesTranslator{inner: inner, stream: stream, model: model, hdr: http.Header{}}
}

func (t *responsesTranslator) Header() http.Header { return t.hdr }

func (t *responsesTranslator) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.status = code
	t.wroteHeader = true
}

func (t *responsesTranslator) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.status = http.StatusOK
		t.wroteHeader = true
	}
	// 非流式请求，或流式请求但流水线提前报错（status>=400）→ 原样缓冲，finish 处理。
	if !t.stream || t.status >= 400 {
		t.body = append(t.body, p...)
		return len(p), nil
	}
	t.lineBuf = append(t.lineBuf, p...)
	t.processLines(false)
	return len(p), nil
}

// Flush 满足 http.Flusher（上游 StreamHint 可能调用）；真正的写出在 rawWrite 里做。
func (t *responsesTranslator) Flush() {}

// processLines 按行切分 SSE；final=true 时处理最后一行残包。
func (t *responsesTranslator) processLines(final bool) {
	for {
		idx := bytes.IndexByte(t.lineBuf, '\n')
		if idx < 0 {
			break
		}
		line := t.lineBuf[:idx]
		t.lineBuf = t.lineBuf[idx+1:]
		t.handleLine(string(line))
	}
	if final && len(t.lineBuf) > 0 {
		t.handleLine(string(t.lineBuf))
		t.lineBuf = nil
	}
}

func (t *responsesTranslator) handleLine(line string) {
	s := strings.TrimRight(line, "\r")
	if !strings.HasPrefix(s, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	if t.st == nil {
		t.st = newResponsesStream(t, t.model)
	}
	t.st.consume(payload)
}

// finish 在 chat 流水线返回后调用：完成翻译并写出。
func (t *responsesTranslator) finish() {
	if !t.stream || t.status >= 400 {
		t.flushNonStream()
		return
	}
	t.processLines(true)
	if t.st == nil {
		// 流式但没有任何 data 帧（上游空流）：起流后立即收尾，至少给客户端一个
		// 完整的 response.created + response.completed，避免连接悬挂。
		t.st = newResponsesStream(t, t.model)
	}
	t.st.finish()
}

func (t *responsesTranslator) flushNonStream() {
	// 错误路径（status>=400）：原样透传错误 JSON。
	if t.status >= 400 {
		t.copyHeadersToInner()
		t.inner.WriteHeader(t.status)
		_, _ = t.inner.Write(t.body)
		return
	}
	converted, err := chatResponseToResponses(t.body, t.model)
	if err != nil {
		// 解析失败：原样透传（尽力而为，客户端至少能看到上游原文）。
		t.copyHeadersToInner()
		t.inner.WriteHeader(t.status)
		_, _ = t.inner.Write(t.body)
		return
	}
	t.hdr.Set("Content-Type", "application/json")
	t.copyHeadersToInner()
	t.inner.WriteHeader(t.status)
	_, _ = t.inner.Write(converted)
}

// copyHeadersToInner 把本地 header 合入底层 writer（保留底层已有的 X-Request-Id 等）。
func (t *responsesTranslator) copyHeadersToInner() {
	for k, vs := range t.hdr {
		for _, v := range vs {
			t.inner.Header().Set(k, v)
		}
	}
}

// sendHeader 首次写出前把 header 落到 inner（流式）。
func (t *responsesTranslator) sendHeader() {
	if t.sentHeader {
		return
	}
	t.sentHeader = true
	if t.hdr.Get("Content-Type") == "" {
		t.hdr.Set("Content-Type", "text/event-stream")
	}
	t.copyHeadersToInner()
	status := t.status
	if status == 0 {
		status = http.StatusOK
	}
	t.inner.WriteHeader(status)
}

// rawWrite 直接写底层 writer 并 flush（流式事件）。
func (t *responsesTranslator) rawWrite(p []byte) {
	if !t.sentHeader {
		t.sendHeader()
	}
	if len(p) > 0 {
		_, _ = t.inner.Write(p)
	}
	if f, ok := t.inner.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// 流式翻译：chat SSE 帧 → Responses SSE 事件
// ---------------------------------------------------------------------------

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatStreamChunk struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role             string          `json:"role"`
			Content          string          `json:"content"`
			ReasoningContent string          `json:"reasoning_content"`
			ToolCalls        []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

type respToolState struct {
	index   int
	itemID  string
	callID  string
	name    string
	args    strings.Builder
	started bool
}

type responsesStream struct {
	t       *responsesTranslator
	model   string
	id      string
	created int64
	seq     int

	started  bool
	finished bool

	outputIndex int

	msgStarted bool
	msgIndex   int
	msgID      string
	msgText    strings.Builder

	reasonStarted bool
	reasonIndex   int
	reasonID      string
	reasonText    strings.Builder

	tools     map[int]*respToolState
	toolOrder []int

	usage *chatUsage
}

func newResponsesStream(t *responsesTranslator, model string) *responsesStream {
	return &responsesStream{
		t:       t,
		model:   model,
		id:      randID("resp_"),
		created: time.Now().Unix(),
		tools:   map[int]*respToolState{},
	}
}

// consume 处理一条 chat SSE 帧的 payload。
func (s *responsesStream) consume(payload string) {
	// 上游 error 帧（6004 限流/内容拦截/审核）——chat 流水线已按其内容处置账号，
	// 这里只需把错误透传给客户端（Responses 形态）。
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &probe) == nil && probe.Error != nil && probe.Error.Message != "" {
		s.start()
		s.fail(probe.Error.Type, probe.Error.Message)
		return
	}
	var chunk chatStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	s.start()
	if chunk.Model != "" {
		s.model = chunk.Model
	}
	if chunk.Usage != nil {
		s.usage = chunk.Usage
	}
	for _, ch := range chunk.Choices {
		if ch.Delta.ReasoningContent != "" {
			s.onReasoning(ch.Delta.ReasoningContent)
		}
		if ch.Delta.Content != "" {
			s.onText(ch.Delta.Content)
		}
		if len(ch.Delta.ToolCalls) > 0 {
			s.onToolCalls(ch.Delta.ToolCalls)
		}
	}
}

func (s *responsesStream) start() {
	if s.started {
		return
	}
	s.started = true
	s.t.sendHeader()
	s.emit("response.created", map[string]any{"response": s.envelope("in_progress", nil)})
	s.emit("response.in_progress", map[string]any{"response": s.envelope("in_progress", nil)})
}

func (s *responsesStream) onText(delta string) {
	if !s.msgStarted {
		s.msgStarted = true
		s.msgIndex = s.outputIndex
		s.outputIndex++
		s.msgID = randID("msg_")
		s.emit("response.output_item.added", map[string]any{
			"output_index": s.msgIndex,
			"item": map[string]any{
				"id": s.msgID, "type": "message", "status": "in_progress",
				"role": "assistant", "content": []any{},
			},
		})
		s.emit("response.content_part.added", map[string]any{
			"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
		})
	}
	s.msgText.WriteString(delta)
	s.emit("response.output_text.delta", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
		"delta": delta,
	})
}

func (s *responsesStream) onReasoning(delta string) {
	if !s.reasonStarted {
		s.reasonStarted = true
		s.reasonIndex = s.outputIndex
		s.outputIndex++
		s.reasonID = randID("rs_")
		s.emit("response.output_item.added", map[string]any{
			"output_index": s.reasonIndex,
			"item":         map[string]any{"id": s.reasonID, "type": "reasoning", "summary": []any{}},
		})
		s.emit("response.reasoning_summary_part.added", map[string]any{
			"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": ""},
		})
	}
	s.reasonText.WriteString(delta)
	s.emit("response.reasoning_summary_text.delta", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
		"delta": delta,
	})
}

func (s *responsesStream) onToolCalls(tcs []toolCallDelta) {
	for _, tc := range tcs {
		st, ok := s.tools[tc.Index]
		if !ok {
			st = &respToolState{
				index:  s.outputIndex,
				itemID: randID("fc_"),
				callID: tc.ID,
				name:   tc.Function.Name,
			}
			if st.callID == "" {
				st.callID = randID("call_")
			}
			s.outputIndex++
			s.tools[tc.Index] = st
			s.toolOrder = append(s.toolOrder, tc.Index)
			s.emit("response.output_item.added", map[string]any{
				"output_index": st.index,
				"item": map[string]any{
					"id": st.itemID, "type": "function_call", "status": "in_progress",
					"call_id": st.callID, "name": st.name, "arguments": "",
				},
			})
		}
		if tc.ID != "" {
			st.callID = tc.ID
		}
		if tc.Function.Name != "" {
			st.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			st.args.WriteString(tc.Function.Arguments)
			s.emit("response.function_call_arguments.delta", map[string]any{
				"item_id": st.itemID, "output_index": st.index,
				"delta": tc.Function.Arguments,
			})
		}
	}
}

// finish 收尾：关闭各 output item 并发出 response.completed。
func (s *responsesStream) finish() {
	if s.finished {
		return
	}
	s.finished = true
	s.start() // 若此前无任何帧，保证已发出 response.created

	output := make([]map[string]any, 0, 3)
	if s.reasonStarted {
		s.closeReasoning(&output)
	}
	if s.msgStarted {
		s.closeMessage(&output)
	}
	for _, idx := range s.toolOrder {
		s.closeTool(s.tools[idx], &output)
	}
	s.emit("response.completed", map[string]any{"response": s.envelopeWithUsage("completed", output)})
}

func (s *responsesStream) closeMessage(output *[]map[string]any) {
	text := s.msgText.String()
	s.emit("response.output_text.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0, "text": text,
	})
	s.emit("response.content_part.done", map[string]any{
		"item_id": s.msgID, "output_index": s.msgIndex, "content_index": 0,
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	item := map[string]any{
		"id": s.msgID, "type": "message", "status": "completed", "role": "assistant",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.msgIndex, "item": item})
	*output = append(*output, item)
}

func (s *responsesStream) closeReasoning(output *[]map[string]any) {
	text := s.reasonText.String()
	s.emit("response.reasoning_summary_text.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0, "text": text,
	})
	s.emit("response.reasoning_summary_part.done", map[string]any{
		"item_id": s.reasonID, "output_index": s.reasonIndex, "summary_index": 0,
		"part": map[string]any{"type": "summary_text", "text": text},
	})
	item := map[string]any{
		"id": s.reasonID, "type": "reasoning",
		"summary": []map[string]any{{"type": "summary_text", "text": text}},
	}
	s.emit("response.output_item.done", map[string]any{"output_index": s.reasonIndex, "item": item})
	*output = append(*output, item)
}

func (s *responsesStream) closeTool(st *respToolState, output *[]map[string]any) {
	args := st.args.String()
	s.emit("response.function_call_arguments.done", map[string]any{
		"item_id": st.itemID, "output_index": st.index, "arguments": args,
	})
	item := map[string]any{
		"id": st.itemID, "type": "function_call", "status": "completed",
		"call_id": st.callID, "name": st.name, "arguments": args,
	}
	s.emit("response.output_item.done", map[string]any{"output_index": st.index, "item": item})
	*output = append(*output, item)
}

// fail 发出 response.failed（上游 error 帧）。
func (s *responsesStream) fail(code, msg string) {
	s.finished = true
	env := s.envelope("failed", []map[string]any{})
	env["error"] = map[string]any{"code": code, "message": msg}
	s.emit("response.failed", map[string]any{"response": env})
}

func (s *responsesStream) envelope(status string, output []map[string]any) map[string]any {
	if output == nil {
		output = []map[string]any{}
	}
	return map[string]any{
		"id":                  s.id,
		"object":              "response",
		"created_at":          s.created,
		"status":              status,
		"model":               s.model,
		"output":              output,
		"usage":               nil,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"error":               nil,
		"incomplete_details":  nil,
		"metadata":            map[string]any{},
	}
}

func (s *responsesStream) envelopeWithUsage(status string, output []map[string]any) map[string]any {
	env := s.envelope(status, output)
	env["usage"] = responsesUsage(s.usage)
	return env
}

// emit 写出一个 Responses SSE 事件（event: <type>\ndata: <json>\n\n）。
func (s *responsesStream) emit(eventType string, payload map[string]any) {
	payload["type"] = eventType
	payload["sequence_number"] = s.seq
	s.seq++
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(eventType)
	b.WriteString("\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	s.t.rawWrite([]byte(b.String()))
}
