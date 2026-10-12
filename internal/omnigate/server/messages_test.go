package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
)

// TestMessagesRouteAuthShape：OmniGate 侧的 /v1/messages 未带密钥时必须回
// Anthropic 形态的 401（Claude Code 直接读 error.type/error.message，喂 OpenAI
// 形态会显示成看不懂的报错），同时证明路由已注册（不是 404）。
func TestMessagesRouteAuthShape(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{{Key: "sk-full"}}}}
	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)

			if w.Code == http.StatusNotFound {
				t.Fatalf("%s 未注册", path)
			}
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
			var body struct {
				Type  string `json:"type"`
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not json: %v (%s)", err, w.Body.String())
			}
			if body.Type != "error" || body.Error.Type != "authentication_error" {
				t.Fatalf("body = %s, want anthropic authentication_error", w.Body.String())
			}
		})
	}
}

// TestCountTokensLocalEstimate：count_tokens 是本地估算，不打上游、不需要账号池，
// 所以能脱离 gateway 单独验证翻译链路（anthropic.RequestToChat）确实接通。
func TestCountTokensLocalEstimate(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{{Key: "sk-full"}}}}
	body := `{"model":"gemini-pro","max_tokens":16,"messages":[{"role":"user","content":"hello world, this is a test"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-full")
	w := httptest.NewRecorder()

	s.handleCountTokens(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	n, _ := got["input_tokens"].(float64)
	if n <= 0 {
		t.Fatalf("input_tokens = %v, want > 0", got["input_tokens"])
	}
}

// TestCountTokensBadJSON：翻译层错误要按 Anthropic 400 形态回，而不是 panic 或
// OpenAI 形态。
func TestCountTokensBadJSON(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{{Key: "sk-full"}}}}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{`))
	r.Header.Set("Authorization", "Bearer sk-full")
	w := httptest.NewRecorder()

	s.handleCountTokens(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if body.Type != "error" || body.Error.Type == "" {
		t.Fatalf("body = %s, want anthropic error shape", w.Body.String())
	}
}

// TestMessagesRouteRejectsScopedKeyModel：收窄密钥调用无权模型时必须是 403
// model_not_allowed（与 /v1/chat/completions 同口径），而不是静默改道。
func TestMessagesRouteRejectsScopedKeyModel(t *testing.T) {
	cfg := &config.Config{APIKeys: config.APIKeyList{
		{Key: "sk-rc", Name: "浣熊专用", Providers: []string{"raccoon"}},
	}}
	// 空供应商的网关足够：ProviderOf 只看 cfg + providers，403 在触达上游之前发出。
	gw, err := gateway.New(cfg, nil, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, gw: gw}
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"gemini-pro","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer sk-rc")
	w := httptest.NewRecorder()

	s.handleMessages(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "model_not_allowed") {
		t.Fatalf("body = %s, want model_not_allowed", w.Body.String())
	}
}
