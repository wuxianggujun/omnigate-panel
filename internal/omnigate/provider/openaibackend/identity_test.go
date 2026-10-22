package openaibackend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/egress"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// capture 记录一次上游请求的关键事实：路径、鉴权头、客户端身份头、请求体。
type capture struct {
	path   string
	auth   string
	apiKey string
	ua     string
	xApp   string
	beta   string
	accept string
	ctype  string
	body   map[string]any
}

func captureServer(t *testing.T, sse string) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.apiKey = r.Header.Get("x-api-key")
		got.ua = r.Header.Get("User-Agent")
		got.xApp = r.Header.Get("x-app")
		got.beta = r.Header.Get("anthropic-beta")
		got.accept = r.Header.Get("Accept")
		got.ctype = r.Header.Get("Content-Type")
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func drain(t *testing.T, s provider.Stream) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if _, err := s.Recv(); err != nil {
			return
		}
	}
	t.Fatal("流没有终止")
}

// TestAnthropicProtocolWire 锁住 Anthropic 方言的三件事：端点换成
// /messages、鉴权换成 x-api-key（not Bearer）、请求体是 messages 形态。
// 这三件缺一件，AnyRouter 一类只收 Claude Code 流量的中转站就会拒。
func TestAnthropicProtocolWire(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")
	srv, got := captureServer(t, sse)

	p := NewWithOptions(Options{
		Name:     "anyrouter",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "sk-test",
		Protocol: "anthropic",
	})
	s, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)

	if got.path != "/v1/messages" {
		t.Errorf("path = %q", got.path)
	}
	if got.apiKey != "sk-test" {
		t.Errorf("x-api-key = %q", got.apiKey)
	}
	if got.auth != "" {
		t.Errorf("Anthropic 方言不该带 Authorization，得到 %q", got.auth)
	}
	if got.body["model"] != "m" {
		t.Errorf("body.model = %#v", got.body["model"])
	}
	if _, ok := got.body["messages"]; !ok {
		t.Errorf("body 缺 messages：%#v", got.body)
	}
	if _, ok := got.body["max_tokens"]; !ok {
		t.Errorf("Anthropic 强制要求 max_tokens：%#v", got.body)
	}
}

// TestClaudeCodeIdentity 证明 provider.client 预设真的把身份写到了上游可见层：
// UA、x-app、anthropic-beta、Accept。缺任何一个，上游都能看出不是官方客户端。
func TestClaudeCodeIdentity(t *testing.T) {
	srv, got := captureServer(t, "data: {\"type\":\"message_stop\"}\n\n")

	p := NewWithOptions(Options{
		Name:     "anyrouter",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "sk-test",
		Protocol: "anthropic",
		Client:   egress.Profile{Preset: egress.PresetClaudeCode},
	})
	s, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)

	if !strings.HasPrefix(got.ua, "claude-cli/") {
		t.Errorf("User-Agent = %q", got.ua)
	}
	if got.xApp != "cli" {
		t.Errorf("x-app = %q", got.xApp)
	}
	if got.beta == "" {
		t.Error("anthropic-beta 没带上")
	}
	if got.accept != "text/event-stream" {
		t.Errorf("Accept = %q", got.accept)
	}
	if got.ctype != "application/json" {
		t.Errorf("Content-Type = %q", got.ctype)
	}
}

// TestDefaultIdentityIsHonest：不配置身份时如实申报，不能悄悄伪装。
func TestDefaultIdentityIsHonest(t *testing.T) {
	srv, got := captureServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")

	p := NewWithOptions(Options{Name: "plain", BaseURL: srv.URL + "/v1", APIKey: "sk-test"})
	s, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)

	if !strings.HasPrefix(got.ua, "omnigate/") {
		t.Errorf("User-Agent = %q", got.ua)
	}
	if got.path != "/v1/chat/completions" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if got.apiKey != "" {
		t.Errorf("openai 方言不该带 x-api-key，得到 %q", got.apiKey)
	}
}

// TestResponsesProtocolWire：Codex 方言走 /responses，body 是 instructions 形态。
func TestResponsesProtocolWire(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"ok"}`,
		``,
		`data: {"type":"response.completed","response":{"status":"completed"}}`,
		``,
	}, "\n")
	srv, got := captureServer(t, sse)

	p := NewWithOptions(Options{
		Name:     "codex",
		BaseURL:  srv.URL + "/v1",
		APIKey:   "sk-test",
		Protocol: "responses",
		Client:   egress.Profile{Preset: egress.PresetCodex},
	})
	s, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)

	if got.path != "/v1/responses" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer sk-test" {
		t.Errorf("Authorization = %q", got.auth)
	}
	if _, ok := got.body["input"]; !ok {
		t.Errorf("body 缺 input：%#v", got.body)
	}
	if !strings.HasPrefix(got.ua, "codex_cli_rs/") {
		t.Errorf("User-Agent = %q", got.ua)
	}
}

// TestSetProtocolHotReload：热重载切换方言后，下一次请求立刻换端点与鉴权。
func TestSetProtocolHotReload(t *testing.T) {
	srv, got := captureServer(t, "data: {\"type\":\"message_stop\"}\n\n")
	p := NewWithOptions(Options{Name: "p", BaseURL: srv.URL + "/v1", APIKey: "sk-test"})

	s, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)
	if got.path != "/v1/chat/completions" {
		t.Fatalf("默认方言 path = %q", got.path)
	}

	p.SetProtocol("anthropic")
	if p.Protocol() != "anthropic" {
		t.Fatalf("Protocol() = %q", p.Protocol())
	}
	s, err = p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	drain(t, s)
	if got.path != "/v1/messages" {
		t.Errorf("切换后 path = %q", got.path)
	}

	// 未知方言退回 openai，而不是让供应商不可路由。
	p.SetProtocol("garbage")
	if p.Protocol() != "openai" {
		t.Errorf("未知方言应退回 openai，得到 %q", p.Protocol())
	}
}
