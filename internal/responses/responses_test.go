package responses

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRequestToChat 验证 Responses 请求 → chat 请求的翻译。
func TestRequestToChat(t *testing.T) {
	cr, err := RequestToChat([]byte(`{
		"model":"glm-5.2",
		"instructions":"be terse",
		"input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],
		"max_output_tokens":128,
		"temperature":0.5,
		"parallel_tool_calls":false,
		"tools":[{"type":"function","name":"f","description":"d","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"f"},
		"reasoning":{"effort":"high"},
		"text":{"format":{"type":"json_object"}}
	}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if cr.Stream {
		t.Errorf("stream should be false")
	}
	if cr.Model != "glm-5.2" {
		t.Errorf("model=%q", cr.Model)
	}
	var got map[string]any
	if err := json.Unmarshal(cr.Body, &got); err != nil {
		t.Fatalf("chat not json: %v", err)
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v (want system + user)", msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "be terse" {
		t.Errorf("messages[0]=%v", msgs[0])
	}
	user := msgs[1].(map[string]any)
	if user["role"] != "user" {
		t.Errorf("messages[1].role=%v", user["role"])
	}
	parts, _ := user["content"].([]any)
	if len(parts) != 1 || parts[0].(map[string]any)["type"] != "text" || parts[0].(map[string]any)["text"] != "hello" {
		t.Errorf("messages[1].content=%v", user["content"])
	}
	if got["max_tokens"].(float64) != 128 {
		t.Errorf("max_tokens=%v", got["max_tokens"])
	}
	if got["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v", got["reasoning_effort"])
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools=%v", got["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "f" {
		t.Errorf("tool function.name=%v", fn["name"])
	}
	tc := got["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["function"].(map[string]any)["name"] != "f" {
		t.Errorf("tool_choice=%v", tc)
	}
	if rf := got["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format=%v", rf)
	}
}

// 流式 Responses 请求必须向 chat 层要 include_usage：翻译层的 response.completed
// usage 来自 chat 流末尾的 usage chunk，而 OpenAI 规范下流式 usage 是 opt-in。
func TestRequestToChatStreamRequestsUsage(t *testing.T) {
	cr, err := RequestToChat([]byte(`{"model":"m","stream":true,"input":"hi"}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !cr.Stream {
		t.Fatalf("stream should be true")
	}
	var got map[string]any
	if err := json.Unmarshal(cr.Body, &got); err != nil {
		t.Fatalf("chat not json: %v", err)
	}
	so, ok := got["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options = %v, want {include_usage:true}", got["stream_options"])
	}
}

// TestInputString 字符串 input → 单条 user message。
func TestInputString(t *testing.T) {
	cr, err := RequestToChat([]byte(`{"model":"m","input":"hello"}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(cr.Body, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello" {
		t.Errorf("messages=%v", msgs)
	}
}

// TestInstructionsArray instructions 允许部件数组；metadata 解析进 ChatRequest。
func TestInstructionsArrayAndMetadata(t *testing.T) {
	cr, err := RequestToChat([]byte(`{
		"model":"m",
		"instructions":[{"type":"input_text","text":"a"},{"type":"input_text","text":"b"}],
		"input":"hi",
		"metadata":{"trace":"t1"}
	}`), nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(cr.Body, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages=%v", msgs)
	}
	if msgs[0].(map[string]any)["content"] != "a\nb" {
		t.Errorf("instructions=%v", msgs[0].(map[string]any)["content"])
	}
	if cr.Metadata["trace"] != "t1" {
		t.Errorf("metadata=%v", cr.Metadata)
	}
}

// TestRequestToChatChaining previous_response_id 续写历史；新 instructions 替换旧 system。
func TestRequestToChatChaining(t *testing.T) {
	store := NewStore(time.Minute, 10)
	store.Put("resp_prev", "m", []map[string]any{
		{"role": "system", "content": "sys1"},
		{"role": "user", "content": "hello"},
		{"role": "assistant", "content": "hi there"},
	})

	// 无新 instructions：保留历史 system。
	cr, err := RequestToChat([]byte(`{"model":"m","previous_response_id":"resp_prev","input":"q1"}`), store)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(cr.Body, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 (sys+hist+user), got %d: %v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["content"] != "sys1" ||
		msgs[1].(map[string]any)["content"] != "hello" ||
		msgs[2].(map[string]any)["content"] != "hi there" ||
		msgs[3].(map[string]any)["content"] != "q1" {
		t.Errorf("chained messages=%v", msgs)
	}
	if !cr.Store {
		t.Errorf("store should default to true")
	}

	// 有新 instructions：丢弃历史 system，只保留一条。
	cr2, err := RequestToChat([]byte(`{"model":"m","previous_response_id":"resp_prev","instructions":"sys2","input":"q2"}`), store)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got2 map[string]any
	_ = json.Unmarshal(cr2.Body, &got2)
	msgs2 := got2["messages"].([]any)
	if len(msgs2) != 4 {
		t.Fatalf("want 4 (sys2+hist-without-sys+user), got %d: %v", len(msgs2), msgs2)
	}
	if msgs2[0].(map[string]any)["content"] != "sys2" {
		t.Errorf("msgs2[0]=%v", msgs2[0])
	}
	for _, m := range msgs2[1:] {
		if m.(map[string]any)["role"] == "system" {
			t.Errorf("stored system should be dropped: %v", msgs2)
		}
	}
}

// TestRequestToChatUnknownPreviousID 未知 previous_response_id 软降级（不报错、不续写）。
func TestRequestToChatUnknownPreviousID(t *testing.T) {
	cr, err := RequestToChat([]byte(`{"model":"m","previous_response_id":"nope","input":"q"}`), NewStore(time.Minute, 10))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(cr.Body, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "q" {
		t.Errorf("messages=%v", msgs)
	}
}

// TestStoreExpiryAndCap 存储过期与容量上限。
func TestStoreExpiryAndCap(t *testing.T) {
	s := NewStore(time.Millisecond, 10)
	s.Put("a", "m", []map[string]any{{"role": "user", "content": "1"}})
	if _, ok := s.Get("a"); !ok {
		t.Fatal("a should exist")
	}
	time.Sleep(5 * time.Millisecond)
	if _, ok := s.Get("a"); ok {
		t.Fatal("a should have expired")
	}

	s2 := NewStore(0, 2)
	s2.Put("a", "m", []map[string]any{{"role": "user", "content": "1"}})
	s2.Put("b", "m", []map[string]any{{"role": "user", "content": "2"}})
	s2.Put("c", "m", []map[string]any{{"role": "user", "content": "3"}})
	if _, ok := s2.Get("a"); ok {
		t.Error("a should be evicted (cap 2)")
	}
	if _, ok := s2.Get("c"); !ok {
		t.Error("c should exist")
	}
}

// TestChatResponseToResponses 非流式 chat → Responses（含 id 与 metadata 回显）。
func TestChatResponseToResponses(t *testing.T) {
	out, err := ChatResponseToResponses([]byte(`{
		"id":"chatcmpl-1","object":"chat.completion","created":1753600000,"model":"glm-5.2",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}
	}`), "glm-5.2", "resp_fixed", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("envelope=%v", resp)
	}
	if resp["id"] != "resp_fixed" {
		t.Errorf("id=%v", resp["id"])
	}
	if resp["metadata"].(map[string]any)["k"] != "v" {
		t.Errorf("metadata=%v", resp["metadata"])
	}
	output := resp["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output=%v", output)
	}
	if output[0].(map[string]any)["type"] != "message" {
		t.Errorf("output[0]=%v", output[0])
	}
	fc := output[1].(map[string]any)
	if fc["type"] != "function_call" || fc["name"] != "f" || fc["arguments"] != `{"a":1}` {
		t.Errorf("function_call=%v", fc)
	}
	if resp["output_text"] != "hi" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
}

// TestStreamMultipleToolCallsOmittedIndex 一个 chunk 内多个 tool_call 且都省略
// index（OmniGate 引擎的形态）：必须拆成多个 function_call 输出项，而不是合并。
func TestStreamMultipleToolCallsOmittedIndex(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "m", nil)
	frame := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[` +
		`{"id":"call_1","type":"function","function":{"name":"a","arguments":"{\"x\":1}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"b","arguments":"{}"}},` +
		`{"id":"call_3","type":"function","function":{"name":"c","arguments":"{}"}}]}}]}`
	if _, err := tr.Write([]byte("data: " + frame + "\n\n")); err != nil {
		t.Fatal(err)
	}
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()
	body := rec.Body.String()
	if got := strings.Count(body, "event: response.output_item.added"); got != 3 {
		t.Errorf("want 3 output_item.added, got %d\n%s", got, body)
	}
	for _, want := range []string{
		`"name":"a"`, `"name":"b"`, `"name":"c"`,
		`"call_id":"call_1"`, `"call_id":"call_2"`, `"call_id":"call_3"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n%s", want, body)
		}
	}
}

// TestStreamIncrementalToolArgs 增量参数（首帧带 id，后续帧仅带 index）应合并到同一项。
func TestStreamIncrementalToolArgs(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "m", nil)
	f1 := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"f","arguments":"{\"a\""}}]}}]}`
	f2 := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]}}]}`
	_, _ = tr.Write([]byte("data: " + f1 + "\n\n"))
	_, _ = tr.Write([]byte("data: " + f2 + "\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()
	body := rec.Body.String()
	if got := strings.Count(body, "event: response.output_item.added"); got != 1 {
		t.Errorf("want 1 output_item.added, got %d\n%s", got, body)
	}
	if !strings.Contains(body, `"arguments":"{\"a\":1}"`) {
		t.Errorf("merged arguments missing\n%s", body)
	}
}

// TestAssistantMessageNonStream 非流式：可从 Translator 取回助手消息（供续写存储）。
func TestAssistantMessageNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, false, "m", nil)
	tr.WriteHeader(200)
	_, _ = tr.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hey"},"finish_reason":"stop"}]}`))
	tr.Finish()
	msg := tr.AssistantMessage()
	if msg == nil || msg["content"] != "hey" || msg["role"] != "assistant" {
		t.Fatalf("assistant message=%v", msg)
	}
}

// TestAssistantMessageStream 流式：聚合出的助手消息含正文与工具调用。
func TestAssistantMessageStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "m", nil)
	f := `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`
	_, _ = tr.Write([]byte("data: " + f + "\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()
	msg := tr.AssistantMessage()
	if msg == nil || msg["content"] != "hi" {
		t.Fatalf("assistant message=%v", msg)
	}
}

// TestAssistantMessageFailed 流式失败：不产出可存储的助手消息。
func TestAssistantMessageFailed(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "m", nil)
	_, _ = tr.Write([]byte("data: {\"error\":{\"message\":\"boom\",\"type\":\"upstream_error\"}}\n\n"))
	tr.Finish()
	if msg := tr.AssistantMessage(); msg != nil {
		t.Fatalf("failed stream should not yield an assistant message, got %v", msg)
	}
}

// TestTranslatorResponseID id 稳定：Translator.ResponseID 与响应体一致。
func TestTranslatorResponseID(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, false, "m", map[string]any{"a": "b"})
	tr.WriteHeader(200)
	_, _ = tr.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"x"}}]}`))
	tr.Finish()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if resp["id"] != tr.ResponseID() {
		t.Errorf("body id=%v ResponseID=%v", resp["id"], tr.ResponseID())
	}
	if resp["metadata"].(map[string]any)["a"] != "b" {
		t.Errorf("metadata=%v", resp["metadata"])
	}
}
