package anthropic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Options 是 Translator 的请求侧上下文（响应里要回显/推断的字段）。
type Options struct {
	// Model 是客户端请求的模型名，原样回显在 message.model（客户端按它做状态一致性）。
	Model string
	// MaxTokens 是客户端的 max_tokens（仅诊断用，不参与翻译）。
	MaxTokens int
	// InputTokens 是本地估算的输入 token（message_start.usage.input_tokens）。
	InputTokens int
	// StopSequences 用于把「finish_reason=stop 且正文以停止串结尾」判成
	// stop_reason=stop_sequence。
	StopSequences []string
}

// Translator 是一个 http.ResponseWriter 拦截器：包住真实 writer 交给既有 chat
// 流水线，接住它写出的 chat 响应（JSON 或 SSE）并改写成 Anthropic 形态。
// 用法与 internal/responses.Translator 一致：跑完流水线后必须调用 Finish。
type Translator struct {
	inner  http.ResponseWriter
	stream bool
	opts   Options

	id string

	hdr         http.Header
	status      int
	wroteHeader bool
	sentHeader  bool

	// body 在非流式路径缓冲完整响应；流式路径下只在「还没确认是 SSE」时留一份
	// 原文，供「客户端要流式、上游却回了非流式 JSON」兜底。
	body    []byte
	st      *streamState
	lineBuf []byte
}

// NewTranslator 构建 Translator。stream 取客户端请求的 stream 字段。
func NewTranslator(inner http.ResponseWriter, stream bool, opts Options) *Translator {
	return &Translator{
		inner:  inner,
		stream: stream,
		opts:   opts,
		id:     RandID(messageIDPrefix),
		hdr:    http.Header{},
	}
}

// Header implements http.ResponseWriter.
func (t *Translator) Header() http.Header { return t.hdr }

// WriteHeader implements http.ResponseWriter（记录状态，懒转发给 inner）。
func (t *Translator) WriteHeader(code int) {
	if t.wroteHeader {
		return
	}
	t.status = code
	t.wroteHeader = true
}

// Write implements http.ResponseWriter.
func (t *Translator) Write(p []byte) (int, error) {
	if !t.wroteHeader {
		t.status = http.StatusOK
		t.wroteHeader = true
	}
	// 非流式，或流式请求早期就失败（status>=400）：整体缓冲，Finish 时统一处理。
	if !t.stream || t.status >= 400 {
		t.body = append(t.body, p...)
		return len(p), nil
	}
	if t.st == nil && len(t.body) < maxFallbackBody {
		t.body = append(t.body, p...)
	}
	t.lineBuf = append(t.lineBuf, p...)
	t.processLines(false)
	return len(p), nil
}

// Flush implements http.Flusher（上游可能调用；真正的写出在 rawWrite）。
func (t *Translator) Flush() {}

// processLines 按行切分 SSE；final=true 时处理结尾的无换行残行。
func (t *Translator) processLines(final bool) {
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

// handleLine 处理一行 SSE：只认 "data:" 前缀，[DONE]/空行忽略。
func (t *Translator) handleLine(line string) {
	s := strings.TrimRight(line, "\r")
	if !strings.HasPrefix(s, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(s, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	if t.st == nil {
		t.st = newStreamState(t)
	}
	t.st.consume(payload)
}

// Finish 必须在 chat 流水线返回后调用；它完成翻译并写出结果。
func (t *Translator) Finish() {
	if !t.stream || t.status >= 400 {
		t.flushNonStream()
		return
	}
	t.processLines(true)
	if t.st == nil {
		// 流式请求但一帧 data: 都没解析到：上游可能降级成非流式 JSON。先试着重放，
		// 实在解析不出来也至少回一个完整的空 message（绝不让客户端挂住）。
		if cr, err := parseChatResponse(bytes.TrimSpace(t.body)); err == nil {
			t.replayMessage(cr)
			return
		}
		t.st = newStreamState(t)
	}
	t.st.finish()
}

// statusOrOK 返回已记录的状态码，未写过时为 200。
func (t *Translator) statusOrOK() int {
	if t.status == 0 {
		return http.StatusOK
	}
	return t.status
}

// flushNonStream 处理非流式响应（或流式请求的早期失败）。
func (t *Translator) flushNonStream() {
	status := t.statusOrOK()
	t.hdr.Set("Content-Type", "application/json")
	if status >= 400 {
		// 错误：把 chat 错误 JSON 翻成 Anthropic 错误 JSON（保留状态码，客户端按
		// 状态码/type 决定是否重试）。
		typ, msg := errorFromBody(t.body, status)
		t.copyHeadersToInner()
		t.inner.WriteHeader(status)
		_, _ = t.inner.Write(errorJSON(typ, msg))
		return
	}
	converted, err := ChatResponseToMessage(t.body, t.opts.Model, t.id, t.opts.StopSequences)
	if err != nil {
		// 200 但不可解析：按上游故障回 502（把原文带进 message，便于定位）。
		t.copyHeadersToInner()
		t.inner.WriteHeader(http.StatusBadGateway)
		_, _ = t.inner.Write(errorJSON("api_error",
			"upstream returned an unparseable chat response: "+truncateForError(string(t.body))))
		return
	}
	t.copyHeadersToInner()
	t.inner.WriteHeader(status)
	_, _ = t.inner.Write(converted)
}

// replayMessage 把一份聚合 chat 响应按 SSE 事件流重放（流式请求、上游给非流式
// JSON 时的兜底）。事件序列与真流式一致，客户端无感。
func (t *Translator) replayMessage(cr *chatResponse) {
	parts := messageFromChat(cr, t.opts.StopSequences)
	s := newStreamState(t)
	if in, ok := parts.Usage["input_tokens"].(int); ok && in > 0 {
		s.inputTokens = in
	}
	if out, ok := parts.Usage["output_tokens"].(int); ok {
		s.outputTokens = out
		s.hasUsage = true
	}
	s.start()
	for _, block := range parts.Content {
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			if text != "" {
				s.textDelta(text)
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			args := ""
			if raw, ok := block["input"].(json.RawMessage); ok {
				args = string(raw)
			}
			s.emitTool(id, name, args)
		}
	}
	s.stopReason = parts.StopReason
	s.stopSequence = parts.StopSequence
	s.finish()
}

// copyHeadersToInner 把本地头合并进底层 writer（保留它已有的头，如 X-Request-Id）。
func (t *Translator) copyHeadersToInner() {
	for k, vs := range t.hdr {
		for _, v := range vs {
			t.inner.Header().Set(k, v)
		}
	}
}

// sendHeader 在第一次流式写出前把响应头落到底层 writer。
func (t *Translator) sendHeader() {
	if t.sentHeader {
		return
	}
	t.sentHeader = true
	if t.hdr.Get("Content-Type") == "" {
		t.hdr.Set("Content-Type", "text/event-stream")
	}
	t.copyHeadersToInner()
	t.inner.WriteHeader(t.statusOrOK())
}

// rawWrite 直接写底层 writer 并 flush（SSE 逐帧刷新）。
func (t *Translator) rawWrite(p []byte) {
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

// maxErrorBody 错误 message 里携带上游原文的上限（避免把整页 HTML 塞进 message）。
const maxErrorBody = 600

// errorFromBody 从上游错误体里提取 (error.type, message)。message 优先取
// error.message，其次顶层 message，最后退化成原文截断（保证 message 永不为空）。
func errorFromBody(body []byte, status int) (string, string) {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error != nil && e.Error.Message != "" {
			return errorTypeForStatus(status), e.Error.Message
		}
		if e.Message != "" {
			return errorTypeForStatus(status), e.Message
		}
	}
	return errorTypeForStatus(status), truncateForError(string(body))
}

// truncateForError 把上游原文压成单行并截断到 maxErrorBody。
func truncateForError(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "upstream request failed"
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// 流式翻译：chat SSE 帧 → Anthropic SSE 事件
// ---------------------------------------------------------------------------

// toolCallDelta 是流式 delta.tool_calls 条目。
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatStreamChunk 是 chat/completions 的一帧 SSE。
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

// toolState 是一个流式工具调用的累积状态。
type toolState struct {
	id   string
	name string
	args strings.Builder
}

// streamState 把 chat SSE 帧累积成 Anthropic SSE 事件序列。
type streamState struct {
	t   *Translator
	id  string

	started  bool
	finished bool

	blockIndex int

	textOpen  bool
	textIndex int
	text      strings.Builder

	// tools 按首次出现顺序缓冲。Anthropic 的 content block 必须连续（块一旦
	// content_block_stop 就不能再追加 delta），而上游可能把多个 tool call 的
	// delta 交错下发；统一在收尾时按序整块发出，事件序列必然合法。
	toolsByIndex map[int]*toolState
	toolOrder    []int

	inputTokens  int
	outputTokens int
	hasUsage     bool

	finishReason string
	stopReason   string
	stopSequence any
}

// newStreamState 构建流式状态（id/模型取自 Translator 的请求侧上下文）。
func newStreamState(t *Translator) *streamState {
	return &streamState{
		t:             t,
		id:            t.id,
		inputTokens:   t.opts.InputTokens,
		toolsByIndex:  map[int]*toolState{},
	}
}

// consume 处理一帧 chat SSE payload。
func (s *streamState) consume(payload string) {
	if s.finished {
		return
	}
	// 上游错误帧（限流 / 内容拦截 / 审核）：转成 Anthropic 的 error 事件。
	var probe struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(payload), &probe) == nil && probe.Error != nil && probe.Error.Message != "" {
		s.fail(probe.Error.Message)
		return
	}

	var chunk chatStreamChunk
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return
	}
	s.start()
	if chunk.Usage != nil {
		s.hasUsage = true
		s.outputTokens = chunk.Usage.CompletionTokens
		if in, ok := usageFromChat(chunk.Usage)["input_tokens"].(int); ok {
			s.inputTokens = in
		}
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finishReason = *ch.FinishReason
		}
		// reasoning_content 有意忽略：Anthropic 的 thinking 块需要签名，回吐会让
		// 客户端下一轮因签名缺失报错。
		if ch.Delta.Content != "" {
			s.textDelta(ch.Delta.Content)
		}
		if len(ch.Delta.ToolCalls) > 0 {
			s.bufferTools(ch.Delta.ToolCalls)
		}
	}
}

// start 发出 message_start（+ 一个 ping，Anthropic 惯例）。幂等。
func (s *streamState) start() {
	if s.started {
		return
	}
	s.started = true
	s.t.sendHeader()
	s.emit("message_start", map[string]any{
		"message": map[string]any{
			"id":            s.id,
			"type":          "message",
			"role":          "assistant",
			"model":         s.t.opts.Model,
			"content":       []map[string]any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0},
		},
	})
	s.emit("ping", map[string]any{})
}

// openText 打开一个 text 块（幂等）。
func (s *streamState) openText() {
	if s.textOpen {
		return
	}
	s.start()
	s.textOpen = true
	s.textIndex = s.blockIndex
	s.blockIndex++
	s.emit("content_block_start", map[string]any{
		"index":         s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// textDelta 追加一段正文（必要时自动开块）。
func (s *streamState) textDelta(delta string) {
	if delta == "" {
		return
	}
	s.openText()
	s.text.WriteString(delta)
	s.emit("content_block_delta", map[string]any{
		"index": s.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

// closeText 关闭已打开的 text 块（幂等）。
func (s *streamState) closeText() {
	if !s.textOpen {
		return
	}
	s.textOpen = false
	s.emit("content_block_stop", map[string]any{"index": s.textIndex})
}

// bufferTools 按 index 累积工具调用 delta。
func (s *streamState) bufferTools(deltas []toolCallDelta) {
	for _, d := range deltas {
		st, ok := s.toolsByIndex[d.Index]
		if !ok {
			st = &toolState{id: d.ID, name: d.Function.Name}
			s.toolsByIndex[d.Index] = st
			s.toolOrder = append(s.toolOrder, d.Index)
		}
		if d.ID != "" {
			st.id = d.ID
		}
		if d.Function.Name != "" {
			st.name = d.Function.Name
		}
		st.args.WriteString(d.Function.Arguments)
	}
}

// emitTool 完整发出一个 tool_use 块（start + 一段 input_json_delta + stop）。
func (s *streamState) emitTool(id, name, args string) {
	s.start()
	if id == "" {
		id = RandID(toolIDPrefix)
	}
	s.emit("content_block_start", map[string]any{
		"index": s.blockIndex,
		"content_block": map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": map[string]any{},
		},
	})
	if args != "" {
		s.emit("content_block_delta", map[string]any{
			"index": s.blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
	s.emit("content_block_stop", map[string]any{"index": s.blockIndex})
	s.blockIndex++
}

// finish 关闭所有块并发出 message_delta + message_stop。幂等。
func (s *streamState) finish() {
	if s.finished {
		return
	}
	s.finished = true
	s.start()
	s.closeText()
	for _, idx := range s.toolOrder {
		st := s.toolsByIndex[idx]
		// 归一成合法 JSON 对象：非法片段会让客户端解析工具入参失败（宁可给 {}，
		// 让工具以空参失败，也不给一段坏 JSON）。
		s.emitTool(st.id, st.name, string(jsonObject(st.args.String())))
	}
	reason := s.stopReason
	if reason == "" {
		reason = s.derivedStopReason()
	}
	usage := map[string]any{"output_tokens": s.outputTokens}
	if s.hasUsage {
		usage["input_tokens"] = s.inputTokens
	}
	s.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": reason, "stop_sequence": s.stopSequence},
		"usage": usage,
	})
	s.emit("message_stop", map[string]any{})
}

// derivedStopReason 从 finish_reason + 已缓冲的工具 + 停止串推断 stop_reason。
func (s *streamState) derivedStopReason() string {
	switch s.finishReason {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	}
	if len(s.toolOrder) > 0 {
		return "tool_use"
	}
	text := s.text.String()
	for _, sq := range s.t.opts.StopSequences {
		if sq != "" && strings.HasSuffix(text, sq) {
			s.stopSequence = sq
			return "stop_sequence"
		}
	}
	return "end_turn"
}

// fail 发出 Anthropic 的 error 事件并终止本流。
func (s *streamState) fail(msg string) {
	if s.finished {
		return
	}
	s.finished = true
	s.start()
	s.emit("error", map[string]any{
		"error": map[string]any{"type": "api_error", "message": msg},
	})
}

// emit 写出一条 Anthropic SSE 事件（event: <type>\ndata: <json>\n\n）。
func (s *streamState) emit(event string, payload map[string]any) {
	payload["type"] = event
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(event)
	b.WriteString("\ndata: ")
	b.Write(raw)
	b.WriteString("\n\n")
	s.t.rawWrite([]byte(b.String()))
}
