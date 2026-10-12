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

// TestGeminiRoute：路径解析必须区分两个动作（":streamGenerateContent" 也以
// "GenerateContent" 结尾，先匹配长的）。
func TestGeminiRoute(t *testing.T) {
	for _, tc := range []struct {
		path   string
		model  string
		stream bool
		ok     bool
	}{
		{"/v1beta/models/gemini-pro:generateContent", "gemini-pro", false, true},
		{"/v1beta/models/gemini-pro:streamGenerateContent", "gemini-pro", true, true},
		{"/v1beta/models/models/gemini-3-flash:generateContent", "models/gemini-3-flash", false, true},
		{"/v1beta/models/gemini-pro", "", false, false},
		{"/v1beta/models/", "", false, false},
		{"/v1beta/other", "", false, false},
	} {
		model, stream, ok := geminiRoute(tc.path)
		if ok != tc.ok || model != tc.model || stream != tc.stream {
			t.Errorf("geminiRoute(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tc.path, model, stream, ok, tc.model, tc.stream, tc.ok)
		}
	}
}

// TestGeminiAuthShape：/v1beta 未带密钥时必须回 Gemini 形态的 401（而不是 OpenAI
// 形态），同时证明路由已注册（不是 404）。
func TestGeminiAuthShape(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{{Key: "sk-full"}}}}
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/v1beta/models"},
		{http.MethodPost, "/v1beta/models/gemini-pro:generateContent"},
		{http.MethodPost, "/v1beta/models/gemini-pro:streamGenerateContent"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)

		if w.Code == http.StatusNotFound {
			t.Fatalf("%s %s 未注册", tc.method, tc.path)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status = %d, want 401", tc.method, tc.path, w.Code)
		}
		var body struct {
			Error struct {
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body not json: %v (%s)", err, w.Body.String())
		}
		if body.Error.Status != "UNAUTHENTICATED" || body.Error.Message == "" {
			t.Fatalf("body = %s, want gemini UNAUTHENTICATED", w.Body.String())
		}
	}
}

// TestGeminiRejectsScopedKeyModel：收窄密钥调用无权模型时必须是 403（Gemini 形态），
// 与 /v1/chat/completions 同口径，而不是静默改道。
func TestGeminiRejectsScopedKeyModel(t *testing.T) {
	cfg := &config.Config{APIKeys: config.APIKeyList{
		{Key: "sk-rc", Name: "浣熊专用", Providers: []string{"raccoon"}},
	}}
	// 空供应商的网关足够：ProviderOf 只看 cfg + providers，403 在触达上游之前发出。
	gw, err := gateway.New(cfg, nil, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, gw: gw}

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:generateContent", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer sk-rc")
	w := httptest.NewRecorder()

	s.handleGeminiGenerate(w, r)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d body=%s, want 403", w.Code, w.Body.String())
	}
	var got struct {
		Error struct {
			Status string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Status != "PERMISSION_DENIED" {
		t.Fatalf("body = %s, want PERMISSION_DENIED", w.Body.String())
	}
}

// TestGeminiBadBody：翻译层错误按 Gemini 400 形态回，而不是 OpenAI 形态或 panic。
func TestGeminiBadBody(t *testing.T) {
	cfg := &config.Config{APIKeys: config.APIKeyList{{Key: "sk-full"}}}
	gw, err := gateway.New(cfg, nil, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{cfg: cfg, gw: gw}

	r := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-pro:generateContent", strings.NewReader(`{`))
	r.Header.Set("Authorization", "Bearer sk-full")
	w := httptest.NewRecorder()

	s.handleGeminiGenerate(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s, want 400", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "INVALID_ARGUMENT") {
		t.Fatalf("body = %s, want INVALID_ARGUMENT", w.Body.String())
	}
}
