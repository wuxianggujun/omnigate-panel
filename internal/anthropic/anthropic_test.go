package anthropic

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// decodeChat 把 RequestToChat 的 body 解成通用 map 供断言。
func decodeChat(t *testing.T, raw string) map[string]any {
	t.Helper()
	cr, err := RequestToChat([]byte(raw))
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		t.Fatalf("body not json: %v body=%s", err, cr.Body)
	}
	return body
}

func msgsOf(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, _ := json.Marshal(body["messages"])
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("messages: %v", err)
	}
	return out
}

// TestRequestToChatSystemAndModel system 两种形态（字符串 / 块数组）都归一成
// role=system 消息；模型名原样透传（不做映射）。
func TestRequestToChatSystemAndModel(t *testing.T) {
	body := decodeChat(t, `{
		"model":"claude-opus-4-8",
		"max_tokens":1024,
		"system":[{"type":"text","text":"be terse","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}]
	}`)
	if body["model"] != "claude-opus-4-8" {
		t.Errorf("model=%v（必须原样透传）", body["model"])
	}
	if body["max_tokens"].(float64) != 1024 {
		t.Errorf("max_tokens=%v", body["max_tokens"])
	}
	msgs := msgsOf(t, body)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "be terse" {
		t.Errorf("system=%v", msgs[0])
	}
	if msgs[1]["role"] != "user" || msgs[1]["content"] != "hi" {
		t.Errorf("user=%v", msgs[1])
	}
}

// TestRequestToChatToolResultAndToolUse 工具双向映射：assistant.tool_use →
// tool_calls；user.tool_result → role=tool 且排在同一条 user 消息的前面。
func TestRequestToChatToolResultAndToolUse(t *testing.T) {
	body := decodeChat(t, `{
		"model":"m","max_tokens":100,
		"messages":[
			{"role":"assistant","content":[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"a.go"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"package main"}]},
				{"type":"text","text":"now explain"}
			]}
		]
	}`)
	msgs := msgsOf(t, body)
	if len(msgs) != 3 {
		t.Fatalf("messages=%v", msgs)
	}
	asst := msgs[0]
	if asst["role"] != "assistant" || asst["content"] != "calling" {
		t.Errorf("assistant=%v", asst)
	}
	calls, _ := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls=%v", asst["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool id=%v", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "Read" || fn["arguments"] != `{"file_path":"a.go"}` {
		t.Errorf("function=%v", fn)
	}
	// tool_result 必须先于同一条 user 消息里的文本发出。
	if msgs[1]["role"] != "tool" || msgs[1]["tool_call_id"] != "toolu_1" || msgs[1]["content"] != "package main" {
		t.Errorf("tool msg=%v", msgs[1])
	}
	if msgs[2]["role"] != "user" || msgs[2]["content"] != "now explain" {
		t.Errorf("user msg=%v", msgs[2])
	}
}

// TestRequestToChatToolsAndChoice tools/tool_choice 映射 + 缺 name 的工具被计数。
func TestRequestToChatToolsAndChoice(t *testing.T) {
	cr, err := RequestToChat([]byte(`{
		"model":"m","max_tokens":10,
		"messages":[{"role":"user","content":"x"}],
		"tools":[
			{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}},
			{"description":"no name"}
		],
		"tool_choice":{"type":"any"}
	}`))
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	if cr.DroppedTools != 1 {
		t.Errorf("DroppedTools=%d want 1", cr.DroppedTools)
	}
	var body map[string]any
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["tool_choice"] != "required" {
		t.Errorf("tool_choice=%v（any → required）", body["tool_choice"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", body["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Read" {
		t.Errorf("fn=%v", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Errorf("parameters 必须是对象 schema：%v", fn["parameters"])
	}
}

// TestRequestToChatToolChoiceTool 指定工具的 tool_choice 映射成 function 形态。
func TestRequestToChatToolChoiceTool(t *testing.T) {
	body := decodeChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"x"}],
		"tool_choice":{"type":"tool","name":"Bash"}}`)
	tc, ok := body["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice=%v", body["tool_choice"])
	}
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "Bash" {
		t.Errorf("tool_choice=%v", tc)
	}
}

// TestRequestToChatStopSequencesAndParams stop_sequences → stop；top_k 丢弃；
// temperature/top_p 透传。
func TestRequestToChatStopSequencesAndParams(t *testing.T) {
	cr, err := RequestToChat([]byte(`{
		"model":"m","max_tokens":1,"stream":true,
		"temperature":0.3,"top_p":0.9,"top_k":40,
		"stop_sequences":["</end>"],
		"messages":[{"role":"user","content":"x"}]
	}`))
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	if !cr.Stream {
		t.Error("stream 未透传")
	}
	if len(cr.StopSequences) != 1 || cr.StopSequences[0] != "</end>" {
		t.Errorf("StopSequences=%v", cr.StopSequences)
	}
	var body map[string]any
	_ = json.Unmarshal(cr.Body, &body)
	if _, ok := body["top_k"]; ok {
		t.Errorf("top_k 应被丢弃：%v", body["top_k"])
	}
	if body["stop"].([]any)[0] != "</end>" {
		t.Errorf("stop=%v", body["stop"])
	}
	if body["temperature"].(float64) != 0.3 || body["top_p"].(float64) != 0.9 {
		t.Errorf("temperature/top_p 未透传：%v %v", body["temperature"], body["top_p"])
	}
	if body["stream"] != true {
		t.Errorf("stream=%v", body["stream"])
	}
}

// TestRequestToChatImage base64 图片翻成 data: URL 的 image_url part。
func TestRequestToChatImage(t *testing.T) {
	body := decodeChat(t, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[
		{"type":"text","text":"what is this"},
		{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QUJD"}}
	]}]}`)
	msgs := msgsOf(t, body)
	parts, ok := msgs[0]["content"].([]any)
	if !ok {
		t.Fatalf("带图时 content 应是 parts 数组：%v", msgs[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts=%v", parts)
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("img=%v", img)
	}
	if img["image_url"].(map[string]any)["url"] != "data:image/jpeg;base64,QUJD" {
		t.Errorf("url=%v", img["image_url"])
	}
}

// TestRequestToChatValidation 缺 model / 空 messages / 坏 JSON 都回 400。
func TestRequestToChatValidation(t *testing.T) {
	for _, raw := range []string{
		`{"max_tokens":1,"messages":[{"role":"user","content":"x"}]}`,
		`{"model":"m","max_tokens":1,"messages":[]}`,
		`{`,
	} {
		_, err := RequestToChat([]byte(raw))
		if err == nil {
			t.Fatalf("raw=%s 应报错", raw)
		}
		var ae *Error
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || ae.Type != "invalid_request_error" {
			t.Fatalf("raw=%s err=%v", raw, err)
		}
	}
}

// TestChatResponseToMessage 非流式：text + tool_use、stop_reason 映射、usage 口径。
func TestChatResponseToMessage(t *testing.T) {
	raw := `{
		"id":"chatcmpl-1","model":"upstream-m",
		"choices":[{"message":{"role":"assistant","content":"hi there","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"command\":\"ls\"}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":100,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":40}}
	}`
	out, err := ChatResponseToMessage([]byte(raw), "claude-opus-4-8", "msg_fixed", nil)
	if err != nil {
		t.Fatalf("ChatResponseToMessage: %v", err)
	}
	var msg map[string]any
	if err := json.Unmarshal(out, &msg); err != nil {
		t.Fatal(err)
	}
	if msg["id"] != "msg_fixed" || msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("envelope=%v", msg)
	}
	if msg["model"] != "claude-opus-4-8" {
		t.Errorf("model 应回显客户端请求名：%v", msg["model"])
	}
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	if content[0].(map[string]any)["text"] != "hi there" {
		t.Errorf("text block=%v", content[0])
	}
	tu := content[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "Bash" {
		t.Errorf("tool_use=%v", tu)
	}
	if tu["input"].(map[string]any)["command"] != "ls" {
		t.Errorf("input=%v", tu["input"])
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"].(float64) != 60 { // 100 - 40 缓存命中
		t.Errorf("input_tokens=%v（应扣除缓存读）", usage["input_tokens"])
	}
	if usage["cache_read_input_tokens"].(float64) != 40 || usage["output_tokens"].(float64) != 7 {
		t.Errorf("usage=%v", usage)
	}
}

// TestStopReasonLength length → max_tokens；正文命中停止串 → stop_sequence。
func TestStopReasonMapping(t *testing.T) {
	if r, _ := stopReason("length", "abc", false, nil); r != "max_tokens" {
		t.Errorf("length → %s", r)
	}
	if r, seq := stopReason("stop", "done</end>", false, []string{"</end>"}); r != "stop_sequence" || seq != "</end>" {
		t.Errorf("stop_sequence → %s %v", r, seq)
	}
	if r, _ := stopReason("stop", "abc", false, []string{"</end>"}); r != "end_turn" {
		t.Errorf("end_turn → %s", r)
	}
	// 上游在工具回合常回 stop：有 tool_use 块时仍判 tool_use。
	if r, _ := stopReason("stop", "", true, nil); r != "tool_use" {
		t.Errorf("hasTools → %s", r)
	}
}

// ---------------------------------------------------------------------------
// Translator
// ---------------------------------------------------------------------------

type sseEvent struct {
	Event string
	Data  map[string]any
}

// parseEvents 解析 Anthropic SSE 输出。
func parseEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	for _, chunk := range strings.Split(body, "\n\n") {
		var ev string
		var data string
		for _, line := range strings.Split(chunk, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(data), &m); err != nil {
			t.Fatalf("event %s data 不是 JSON: %v (%s)", ev, err, data)
		}
		out = append(out, sseEvent{Event: ev, Data: m})
	}
	return out
}

func eventNames(evs []sseEvent) []string {
	names := make([]string, 0, len(evs))
	for _, e := range evs {
		names = append(names, e.Event)
	}
	return names
}

// TestTranslatorNonStream 非流式：chat 聚合响应 → Anthropic message JSON。
func TestTranslatorNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, false, Options{Model: "claude-x"})
	tr.WriteHeader(http.StatusOK)
	tr.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"你好"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`))
	tr.Finish()

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("not json: %v (%s)", err, rec.Body)
	}
	if msg["type"] != "message" || msg["stop_reason"] != "end_turn" {
		t.Errorf("msg=%v", msg)
	}
	if msg["content"].([]any)[0].(map[string]any)["text"] != "你好" {
		t.Errorf("content=%v", msg["content"])
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type=%q", ct)
	}
}

// TestTranslatorNonStreamError 非流式错误：状态码保留，错误体翻成 Anthropic 形态。
func TestTranslatorNonStreamError(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, false, Options{Model: "claude-x"})
	tr.WriteHeader(http.StatusTooManyRequests)
	tr.Write([]byte(`{"error":{"message":"rate limited by upstream","type":"api_error","code":"soft_rate"}}`))
	tr.Finish()

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d", rec.Code)
	}
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e["type"] != "error" {
		t.Errorf("envelope=%v", e)
	}
	inner := e["error"].(map[string]any)
	if inner["type"] != "rate_limit_error" || inner["message"] != "rate limited by upstream" {
		t.Errorf("error=%v", inner)
	}
}

// TestTranslatorStreamText 流式文本：事件序列为 message_start → ping →
// content_block_start/delta/stop → message_delta → message_stop。
func TestTranslatorStreamText(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x", InputTokens: 11})
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你\"}}]}\n\n"))
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"好\"}}]}\n\n"))
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2}}\n\n"))
	tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type=%q", ct)
	}
	evs := parseEvents(t, rec.Body.String())
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_delta", "content_block_stop",
		"message_delta", "message_stop",
	}
	if got := eventNames(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want=%v\nbody=%s", got, want, rec.Body)
	}
	// message_start 的 input_tokens 用请求侧估算（真值到达前就要给一个）。
	mu := evs[0].Data["message"].(map[string]any)["usage"].(map[string]any)
	if mu["input_tokens"].(float64) != 11 {
		t.Errorf("message_start usage=%v", mu)
	}
	// message_delta 用上游真值覆盖 output_tokens，并回填真实 input_tokens。
	du := evs[6].Data["usage"].(map[string]any)
	if du["output_tokens"].(float64) != 2 || du["input_tokens"].(float64) != 11 {
		t.Errorf("message_delta usage=%v", du)
	}
	if evs[6].Data["delta"].(map[string]any)["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", evs[6].Data["delta"])
	}
}

// TestTranslatorStreamTool 流式工具调用：text 块后接一个完整 tool_use 块，
// stop_reason=tool_use。
func TestTranslatorStreamTool(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x"})
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"let me look\"}}]}\n\n"))
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Read\",\"arguments\":\"\"}}]}}]}\n\n"))
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"file_path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n"))
	tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":9}}\n\n"))
	tr.Finish()

	evs := parseEvents(t, rec.Body.String())
	want := []string{
		"message_start", "ping",
		"content_block_start", "content_block_delta", "content_block_stop", // text 块
		"content_block_start", "content_block_delta", "content_block_stop", // tool_use 块
		"message_delta", "message_stop",
	}
	if got := eventNames(evs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%v want=%v\nbody=%s", got, want, rec.Body)
	}
	// text 块 index 0；tool_use 块 index 1（Anthropic 块序号连续）。
	if evs[2].Data["index"].(float64) != 0 {
		t.Errorf("text index=%v", evs[2].Data["index"])
	}
	tu := evs[5].Data["content_block"].(map[string]any)
	if evs[5].Data["index"].(float64) != 1 {
		t.Errorf("tool index=%v", evs[5].Data["index"])
	}
	if tu["type"] != "tool_use" || tu["id"] != "call_1" || tu["name"] != "Read" {
		t.Errorf("content_block=%v", tu)
	}
	if evs[6].Data["delta"].(map[string]any)["partial_json"] != `{"file_path":"a.go"}` {
		t.Errorf("partial_json=%v", evs[6].Data["delta"])
	}
	if evs[8].Data["delta"].(map[string]any)["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v", evs[8].Data["delta"])
	}
}

// TestTranslatorStreamErrorFrame 上游 error 帧 → Anthropic error 事件，且不再补
// message_delta/message_stop。
func TestTranslatorStreamErrorFrame(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x"})
	tr.Write([]byte("data: {\"error\":{\"message\":\"content blocked\",\"type\":\"api_error\"}}\n\n"))
	tr.Finish()

	evs := parseEvents(t, rec.Body.String())
	if len(evs) != 3 || evs[2].Event != "error" {
		t.Fatalf("events=%v\nbody=%s", eventNames(evs), rec.Body)
	}
	inner := evs[2].Data["error"].(map[string]any)
	if inner["message"] != "content blocked" {
		t.Errorf("error=%v", inner)
	}
}

// TestTranslatorStreamNonStreamFallback 客户端要流式、上游却回非流式 JSON：
// 按事件流重放，内容不丢。
func TestTranslatorStreamNonStreamFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x"})
	tr.WriteHeader(http.StatusOK)
	tr.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"fallback"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":1}}`))
	tr.Finish()

	evs := parseEvents(t, rec.Body.String())
	if evs[0].Event != "message_start" || evs[len(evs)-1].Event != "message_stop" {
		t.Fatalf("events=%v\nbody=%s", eventNames(evs), rec.Body)
	}
	found := false
	for _, e := range evs {
		if e.Event == "content_block_delta" {
			if e.Data["delta"].(map[string]any)["text"] == "fallback" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("重放丢了正文：%s", rec.Body)
	}
}

// TestWriteErrorShape WriteError 的错误体形态。
func TestWriteErrorShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e["type"] != "error" {
		t.Errorf("type=%v", e["type"])
	}
	inner := e["error"].(map[string]any)
	if inner["type"] != "authentication_error" || inner["message"] != "missing or invalid API key" {
		t.Errorf("error=%v", inner)
	}
}

// TestEstimateTokens 估算单调且有下界。
func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 {
		t.Error("空串应为 0")
	}
	if n := EstimateTokens("hello world"); n < 1 {
		t.Errorf("ascii=%d", n)
	}
	if EstimateTokens(strings.Repeat("好", 300)) <= EstimateTokens(strings.Repeat("好", 30)) {
		t.Error("估算应随长度单调增")
	}
}

// TestRequestToChatMergeAdjacent 连续同角色纯文本消息合并，带 tool_calls 的不合并。
func TestRequestToChatMergeAdjacent(t *testing.T) {
	body := decodeChat(t, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":"b"},
		{"role":"assistant","content":"c"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{}}]}
	]}`)
	msgs := msgsOf(t, body)
	if len(msgs) != 3 {
		t.Fatalf("messages=%v", msgs)
	}
	if msgs[0]["content"] != "a\n\nb" {
		t.Errorf("合并结果=%v", msgs[0]["content"])
	}
	if msgs[1]["content"] != "c" {
		t.Errorf("assistant 文本=%v", msgs[1])
	}
	if _, ok := msgs[2]["tool_calls"]; !ok {
		t.Errorf("带 tool_calls 的消息不应被合并掉：%v", msgs[2])
	}
}
