package gemini

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// maxFallbackBody caps the buffered copy kept for the "client asked for a stream,
// upstream answered with plain JSON" fallback.
const maxFallbackBody = 1 << 20

// Translator is an http.ResponseWriter that intercepts a chat/completions
// response and rewrites it into the Gemini format. Callers run their chat
// pipeline with a Translator as the writer, then call Finish.
type Translator struct {
	inner       http.ResponseWriter
	stream      bool
	model       string
	inputTokens int

	hdr         http.Header
	status      int
	wroteHeader bool
	sentHeader  bool

	// body buffers the non-streaming response, and — while streaming — the raw
	// prefix until we are sure the upstream is really speaking SSE.
	body    []byte
	st      *streamState
	lineBuf []byte
}

// NewTranslator builds a Translator wrapping inner. stream is the client's
// choice of streamGenerateContent; model and inputTokens are echoed in the
// response's usage metadata.
func NewTranslator(inner http.ResponseWriter, stream bool, model string, inputTokens int) *Translator {
	return &Translator{
		inner:       inner,
		stream:      stream,
		model:       model,
		inputTokens: inputTokens,
		hdr:         http.Header{},
	}
}

// Header implements http.ResponseWriter.
func (t *Translator) Header() http.Header { return t.hdr }

// WriteHeader implements http.ResponseWriter (recorded, forwarded lazily).
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
	// Non-streaming, or a streaming request that failed early (status>=400):
	// buffer and handle in Finish.
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

// Flush implements http.Flusher (upstream may call it); real writes happen in
// rawWrite.
func (t *Translator) Flush() {}

// processLines splits SSE by line; final=true handles the trailing partial line.
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

func (t *Translator) handleLine(line string) {
	s := strings.TrimRight(line, "\r")
	// 上游的保活 ping 是 SSE 注释（": ping"）。本翻译层输出的是 NDJSON（每行一个
	// 合法 JSON），没有注释语法，注入进去只会破坏 JSON——因此这里显式丢弃注释行。
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

// Finish must be called after the chat pipeline returns; it completes the
// translation and writes the result.
func (t *Translator) Finish() {
	if !t.stream || t.status >= 400 {
		t.flushNonStream()
		return
	}
	t.processLines(true)
	if t.st == nil {
		// Streaming request but not a single data: frame parsed. The upstream may
		// have answered with plain JSON; replay it as NDJSON so the client still
		// gets a well-formed stream instead of hanging.
		if cr, err := parseChatResponse(bytes.TrimSpace(t.body)); err == nil {
			t.replay(cr)
			return
		}
		t.st = newStreamState(t)
	}
	t.st.finish()
}

func (t *Translator) statusOrOK() int {
	if t.status == 0 {
		return http.StatusOK
	}
	return t.status
}

// flushNonStream handles a non-streaming response (or a streaming request that
// failed before any frame was written).
func (t *Translator) flushNonStream() {
	status := t.statusOrOK()
	t.hdr.Set("Content-Type", "application/json")
	if status >= 400 {
		// Error: translate the chat error body into Gemini's error shape, keeping
		// the status code (clients branch on it).
		t.copyHeadersToInner()
		t.inner.WriteHeader(status)
		_, _ = t.inner.Write(ErrorJSON(status, errorMessage(t.body)))
		return
	}
	converted, err := ChatResponseToGemini(t.body, t.inputTokens)
	if err != nil {
		t.copyHeadersToInner()
		t.inner.WriteHeader(http.StatusBadGateway)
		_, _ = t.inner.Write(ErrorJSON(http.StatusBadGateway,
			"upstream returned an unparseable chat response: "+truncateForError(string(t.body))))
		return
	}
	t.copyHeadersToInner()
	t.inner.WriteHeader(status)
	_, _ = t.inner.Write(converted)
}

// replay renders an aggregated chat response as a complete NDJSON stream (the
// fallback for a client that asked to stream and an upstream that answered with
// plain JSON). The chunk sequence matches the real streaming path.
func (t *Translator) replay(cr *chatResponse) {
	s := newStreamState(t)
	var content string
	var calls []openai.ToolCall
	if len(cr.Choices) > 0 {
		content = cr.Choices[0].Message.Content
		calls = cr.Choices[0].Message.ToolCalls
		s.finishReason = cr.Choices[0].FinishReason
	}
	if cr.Usage != nil {
		s.hasUsage = true
		if cr.Usage.PromptTokens > 0 {
			s.promptTokens = cr.Usage.PromptTokens
		}
		s.completionTokens = cr.Usage.CompletionTokens
	}
	s.start()
	if content != "" {
		s.textDelta(content)
	}
	for _, tc := range calls {
		s.addTool(tc)
	}
	s.finish()
}

// copyHeadersToInner merges local headers into the underlying writer (keeping
// any it already set, e.g. X-Request-Id).
func (t *Translator) copyHeadersToInner() {
	for k, vs := range t.hdr {
		for _, v := range vs {
			t.inner.Header().Set(k, v)
		}
	}
}

// sendHeader flushes headers to inner before the first streaming write.
func (t *Translator) sendHeader() {
	if t.sentHeader {
		return
	}
	t.sentHeader = true
	// streamGenerateContent answers with newline-delimited JSON, not SSE, so the
	// content type is application/json — same as the non-streaming endpoint. This
	// overrides whatever the chat pipeline set: it defaults to text/event-stream,
	// which would make a Gemini client try to parse SSE.
	t.hdr.Set("Content-Type", "application/json")
	t.copyHeadersToInner()
	t.inner.WriteHeader(t.statusOrOK())
}

// rawWrite writes directly to the underlying writer and flushes.
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

// writeNDJSON emits one response object followed by a newline.
func (t *Translator) writeNDJSON(v map[string]any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	t.rawWrite(append(raw, '\n'))
}

// maxErrorBody caps how much upstream text is carried in an error message.
const maxErrorBody = 600

// errorMessage extracts the upstream error text, falling back to a truncated
// copy of the body so the message is never empty.
func errorMessage(body []byte) string {
	var e struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil {
		if e.Error != nil && e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return truncateForError(string(body))
}

func truncateForError(s string) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "upstream request failed"
	}
	if len(s) > maxErrorBody {
		s = s[:maxErrorBody] + "…"
	}
	return s
}

// ---------------------------------------------------------------------------
// Streaming translation: chat SSE frames → NDJSON response objects
// ---------------------------------------------------------------------------

// toolCallDelta is a streaming delta.tool_calls entry.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// chatStreamChunk is one chat/completions SSE frame.
type chatStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
}

// toolState accumulates one streamed tool call.
type toolState struct {
	name string
	args strings.Builder
}

// streamState accumulates chat SSE frames and emits NDJSON response objects.
type streamState struct {
	t        *Translator
	started  bool
	finished bool

	text strings.Builder

	toolsByIndex map[int]*toolState
	toolOrder    []int

	finishReason string

	promptTokens     int
	completionTokens int
	hasUsage         bool
}

func newStreamState(t *Translator) *streamState {
	return &streamState{
		t:            t,
		promptTokens: t.inputTokens,
		toolsByIndex: map[int]*toolState{},
	}
}

// consume handles one chat SSE payload.
func (s *streamState) consume(payload string) {
	if s.finished {
		return
	}
	// Upstream error frame (rate limit / content block / moderation): surface it
	// the way the upstream does, as an NDJSON error object.
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
	if json.Unmarshal([]byte(payload), &chunk) != nil {
		return
	}
	s.start()
	if chunk.Usage != nil {
		s.hasUsage = true
		if chunk.Usage.PromptTokens > 0 {
			s.promptTokens = chunk.Usage.PromptTokens
		}
		s.completionTokens = chunk.Usage.CompletionTokens
	}
	for _, ch := range chunk.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			s.finishReason = *ch.FinishReason
		}
		// reasoning_content is deliberately ignored: Gemini has no signed
		// thinking part, and echoing unsigned thoughts would break clients on the
		// next turn.
		if ch.Delta.Content != "" {
			s.textDelta(ch.Delta.Content)
		}
		if len(ch.Delta.ToolCalls) > 0 {
			s.bufferTools(ch.Delta.ToolCalls)
		}
	}
}

// start flushes the response headers. Idempotent.
func (s *streamState) start() {
	if s.started {
		return
	}
	s.started = true
	s.t.sendHeader()
}

// textDelta appends a text fragment and emits it immediately.
func (s *streamState) textDelta(delta string) {
	if delta == "" {
		return
	}
	s.start()
	s.text.WriteString(delta)
	s.chunk([]map[string]any{{"text": delta}})
}

// chunk emits one NDJSON object carrying parts for the single candidate.
func (s *streamState) chunk(parts []map[string]any) {
	s.t.writeNDJSON(map[string]any{
		"candidates": []map[string]any{{
			"content": map[string]any{"parts": parts, "role": "model"},
			"index":   0,
		}},
	})
}

// bufferTools accumulates streamed tool-call deltas by index.
func (s *streamState) bufferTools(deltas []toolCallDelta) {
	for _, d := range deltas {
		st, ok := s.toolsByIndex[d.Index]
		if !ok {
			st = &toolState{}
			s.toolsByIndex[d.Index] = st
			s.toolOrder = append(s.toolOrder, d.Index)
		}
		if d.Function.Name != "" {
			st.name = d.Function.Name
		}
		st.args.WriteString(d.Function.Arguments)
	}
}

// addTool appends a complete tool call (used by the non-stream replay path).
func (s *streamState) addTool(tc openai.ToolCall) {
	idx := len(s.toolOrder)
	st := &toolState{name: tc.Function.Name}
	st.args.WriteString(tc.Function.Arguments)
	s.toolsByIndex[idx] = st
	s.toolOrder = append(s.toolOrder, idx)
}

// finish closes the stream: any buffered tool calls first, then the terminal
// object carrying finishReason and usageMetadata.
func (s *streamState) finish() {
	if s.finished {
		return
	}
	s.finished = true
	s.start()

	// Tool calls go out as complete parts just before the terminal object: the
	// upstream may interleave deltas from several calls, so emitting them whole
	// keeps the part sequence stable.
	if len(s.toolOrder) > 0 {
		parts := make([]map[string]any, 0, len(s.toolOrder))
		for _, idx := range s.toolOrder {
			st := s.toolsByIndex[idx]
			parts = append(parts, functionCallPart(openai.ToolCall{
				Function: openai.ToolFunction{Name: st.name, Arguments: st.args.String()},
			}))
		}
		s.chunk(parts)
	}

	prompt := s.promptTokens
	completion := s.completionTokens
	if !s.hasUsage || completion == 0 {
		completion = estimateTokens([]byte(s.text.String()))
	}
	s.t.writeNDJSON(map[string]any{
		"candidates": []map[string]any{{
			"content":      map[string]any{"parts": []map[string]any{{"text": ""}}, "role": "model"},
			"finishReason": geminiFinishReason(s.finishReason),
			"index":        0,
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     prompt,
			"candidatesTokenCount": completion,
			"totalTokenCount":      prompt + completion,
		},
	})
}

// fail emits an NDJSON error object and ends the stream.
func (s *streamState) fail(msg string) {
	if s.finished {
		return
	}
	s.finished = true
	s.start()
	raw, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": msg, "type": "api_error"},
	})
	if err != nil {
		return
	}
	s.t.rawWrite(append(raw, '\n'))
}
