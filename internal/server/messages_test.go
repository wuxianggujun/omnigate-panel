package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

// newMessagesHandler 构建一个 /v1/messages 可用的 handler（单号池 + fake 上游）。
func newMessagesHandler(t *testing.T) *Handler {
	t.Helper()
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	return NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
}

// TestMessagesNonStream 非流式：Anthropic 请求 → 既有 chat 流水线 → Anthropic message。
func TestMessagesNonStream(t *testing.T) {
	h := newMessagesHandler(t)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-4-8","max_tokens":64,"system":"be terse","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var msg map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("envelope=%v", msg)
	}
	if msg["model"] != "claude-opus-4-8" {
		t.Errorf("model 应回显客户端请求名：%v", msg["model"])
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content=%v", msg["content"])
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" || block["text"] != "你好" {
		t.Errorf("content[0]=%v", block)
	}
	if _, ok := msg["usage"].(map[string]any); !ok {
		t.Errorf("usage missing: %v", msg["usage"])
	}
}

// TestMessagesStream 流式：chat SSE 翻译成 Anthropic SSE 事件序列。
func TestMessagesStream(t *testing.T) {
	h := newMessagesHandler(t)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(
		`{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"event: message_start", "event: ping",
		"event: content_block_start", "event: content_block_delta",
		"event: content_block_stop", "event: message_delta", "event: message_stop",
		`"text_delta"`, "你好",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("SSE 缺少 %q\nbody=%s", want, body)
		}
	}
	// message_stop 必须是最后一个事件。
	if !strings.HasSuffix(strings.TrimSpace(body), `"type":"message_stop"}`) {
		t.Errorf("message_stop 不是最后一个事件：%s", body)
	}
}

// TestMessagesCountTokens 本地估算端点（不打上游）。
func TestMessagesCountTokens(t *testing.T) {
	h := newMessagesHandler(t)
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(
		`{"model":"claude-opus-4-8","max_tokens":64,"messages":[{"role":"user","content":"hello world"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	n, ok := out["input_tokens"].(float64)
	if !ok || n < 1 {
		t.Errorf("input_tokens=%v", out["input_tokens"])
	}
}

// TestMessagesAuth x-api-key（Claude Code 形态）与 Authorization: Bearer 都放行；
// 缺密钥回 401 且错误体是 Anthropic 形态。
func TestMessagesAuth(t *testing.T) {
	body := `{"model":"claude-opus-4-8","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

	cases := []struct {
		name    string
		set     func(*http.Request)
		want    int
		wantErr string
	}{
		{"x-api-key 放行", func(r *http.Request) { r.Header.Set("x-api-key", "secret") }, 200, ""},
		{"Bearer 放行", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret") }, 200, ""},
		{"缺密钥 401", func(r *http.Request) {}, 401, "authentication_error"},
		{"错密钥 401", func(r *http.Request) { r.Header.Set("x-api-key", "nope") }, 401, "authentication_error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
			h := NewHandler(Config{
				Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
				Upstream: up,
				APIKey:   "secret",
			})
			req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			c.set(req)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != c.want {
				t.Fatalf("code=%d want=%d body=%s", rec.Code, c.want, rec.Body)
			}
			if c.wantErr == "" {
				return
			}
			var e map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("not json: %v (%s)", err, rec.Body)
			}
			inner, _ := e["error"].(map[string]any)
			if e["type"] != "error" || inner["type"] != c.wantErr {
				t.Errorf("error body=%v", e)
			}
		})
	}
}

// TestMessagesBadRequest 入站翻译失败回 400 Anthropic 错误（不打上游）。
func TestMessagesBadRequest(t *testing.T) {
	h := newMessagesHandler(t)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"max_tokens":8}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 400 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	inner, _ := e["error"].(map[string]any)
	if inner["type"] != "invalid_request_error" {
		t.Errorf("error=%v", e)
	}
}

// TestRequestTracePath 请求记录记「客户端实际调用的端点」：翻译端点不再一律记成
// /v1/chat/completions（否则面板里分不清 OpenAI / Responses / Anthropic 流量）。
func TestRequestTracePath(t *testing.T) {
	if got := (&requestTrace{path: "/v1/messages", start: time.Now()}).event(200).Path; got != "/v1/messages" {
		t.Errorf("path=%q", got)
	}
	if got := (&requestTrace{path: "/v1/responses", start: time.Now()}).event(200).Path; got != "/v1/responses" {
		t.Errorf("path=%q", got)
	}
	// 未设 path（直接构造 trace 的调用方）回落 chat 端点。
	if got := (&requestTrace{start: time.Now()}).event(200).Path; got != "/v1/chat/completions" {
		t.Errorf("path=%q", got)
	}
}
