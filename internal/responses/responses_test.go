package responses

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestToChat 验证 Responses 请求 → chat 请求的翻译。
func TestRequestToChat(t *testing.T) {
	chat, stream, model, err := RequestToChat([]byte(`{
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
	}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if stream {
		t.Errorf("stream should be false")
	}
	if model != "glm-5.2" {
		t.Errorf("model=%q", model)
	}
	var got map[string]any
	if err := json.Unmarshal(chat, &got); err != nil {
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

// TestInputString 字符串 input → 单条 user message。
func TestInputString(t *testing.T) {
	chat, _, _, err := RequestToChat([]byte(`{"model":"m","input":"hello"}`))
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(chat, &got)
	msgs := got["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "hello" {
		t.Errorf("messages=%v", msgs)
	}
}

// TestChatResponseToResponses 非流式 chat → Responses。
func TestChatResponseToResponses(t *testing.T) {
	out, err := ChatResponseToResponses([]byte(`{
		"id":"chatcmpl-1","object":"chat.completion","created":1753600000,"model":"glm-5.2",
		"choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}
		]},"finish_reason":"tool_calls"}],
		"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}
	}`), "glm-5.2")
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
	tr := NewTranslator(rec, true, "m")
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
	tr := NewTranslator(rec, true, "m")
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
