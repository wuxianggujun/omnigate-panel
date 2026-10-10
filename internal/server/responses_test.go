package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

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
	const sseTools = `data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]}}]}

data: {"id":"c1","object":"chat.completion.chunk","created":1753600000,"model":"glm-5.2","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"total_tokens":8}}

data: [DONE]

`
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
