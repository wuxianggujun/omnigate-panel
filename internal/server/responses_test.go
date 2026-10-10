package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

// TestResponsesRequestToChat 验证 Responses 请求 → chat 请求的翻译：
// instructions→system、input 部件→messages、max_output_tokens→max_tokens、
// tools/tool_choice 嵌套化、reasoning.effort→reasoning_effort、text.format→response_format。
func TestResponsesRequestToChat(t *testing.T) {
	chat, stream, model, err := responsesRequestToChat([]byte(`{
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

// TestResponsesInputString 字符串 input → 单条 user message。
func TestResponsesInputString(t *testing.T) {
	chat, _, _, err := responsesRequestToChat([]byte(`{"model":"m","input":"hello"}`))
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

// TestResponsesNonStream 非流式：chat 聚合响应翻译成 Responses 对象。
func TestResponsesNonStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" {
		t.Errorf("object=%v", resp["object"])
	}
	if resp["status"] != "completed" {
		t.Errorf("status=%v", resp["status"])
	}
	if resp["output_text"] != "你好" {
		t.Errorf("output_text=%v", resp["output_text"])
	}
	output, _ := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output=%v", resp["output"])
	}
	item := output[0].(map[string]any)
	if item["type"] != "message" || item["role"] != "assistant" {
		t.Errorf("output[0]=%v", item)
	}
	content := item["content"].([]any)[0].(map[string]any)
	if content["type"] != "output_text" || content["text"] != "你好" {
		t.Errorf("content=%v", content)
	}
	if _, ok := resp["usage"].(map[string]any); !ok {
		t.Errorf("usage missing: %v", resp["usage"])
	}
}

// TestResponsesStream 流式：chat SSE 翻译成 Responses SSE 事件序列。
func TestResponsesStream(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		`"delta":"你好"`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
		`"status":"completed"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q\n--- body ---\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("responses stream must not emit chat [DONE]:\n%s", body)
	}
}

// TestResponsesNoAccounts 无可用账号：错误 JSON 原样透传（503），不产出 Responses 对象。
func TestResponsesNoAccounts(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","input":"hi"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no_healthy_account") {
		t.Errorf("body=%s", rec.Body)
	}
}

// TestResponsesStreamToolCalls 流式工具调用：delta.tool_calls → function_call 输出项。
func TestResponsesStreamToolCalls(t *testing.T) {
	const sseTools = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_abc\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\"\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"sf\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":5,\"total_tokens\":8}}\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseTools, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"glm-5.2","stream":true,"input":"weather?","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`"type":"function_call"`,
		`"name":"get_weather"`,
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"arguments":"{\"city\":\"sf\"}"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %q\n--- body ---\n%s", want, body)
		}
	}
}
