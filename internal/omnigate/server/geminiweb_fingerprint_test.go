package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// fingerprintTestServer builds a Server with one geminiweb provider whose
// fingerprint layer is wired exactly like the production gateway path
// (gateway.New → SetFingerprint).
func fingerprintTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	tlsOn := true
	cfg := &config.Config{
		DataDir:   dir,
		Providers: []config.ProviderConfig{{Name: "gw", Type: "geminiweb"}},
		Fingerprint: &config.FingerprintConfig{
			ConfigPath: filepath.Join(dir, "fingerprint.json"),
			CookieDir:  filepath.Join(dir, "cookies"),
			TLSEnabled: &tlsOn,
		},
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, gw: gw}
}

func fingerprintBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body struct {
		OK          bool           `json:"ok"`
		Fingerprint map[string]any `json:"fingerprint"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	return body.Fingerprint
}

// TestGeminiWebFingerprintGet：GET 返回当前指纹状态（含默认 Chrome 版本与可用
// TLS 目标），证明配置经 gateway.New 一路注入到了 provider 运行时。
func TestGeminiWebFingerprintGet(t *testing.T) {
	s := fingerprintTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/admin/geminiweb/fingerprint", nil)
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	fp := fingerprintBody(t, w)
	if fp["chrome_major"] != float64(124) {
		t.Fatalf("chrome_major = %v want 124", fp["chrome_major"])
	}
	if fp["impersonate_target"] != "chrome124" {
		t.Fatalf("impersonate_target = %v", fp["impersonate_target"])
	}
	if fp["tls_enabled"] != true {
		t.Fatalf("tls_enabled = %v want true", fp["tls_enabled"])
	}
	targets, ok := fp["tls_supported"].([]any)
	if !ok || len(targets) == 0 {
		t.Fatalf("tls_supported = %v", fp["tls_supported"])
	}
	if fp["cookie_dir"] != filepath.Join(s.cfg.DataDir, "cookies") {
		t.Fatalf("cookie_dir = %v", fp["cookie_dir"])
	}
}

// TestGeminiWebFingerprintSetTarget：POST 切换 TLS 目标后，返回的状态里目标已更新。
func TestGeminiWebFingerprintSetTarget(t *testing.T) {
	s := fingerprintTestServer(t)

	body := `{"provider":"gw","target":"chrome120"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/geminiweb/fingerprint", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	fp := fingerprintBody(t, w)
	if fp["impersonate_target"] != "chrome120" {
		t.Fatalf("impersonate_target = %v want chrome120", fp["impersonate_target"])
	}
}

// TestGeminiWebFingerprintRejectsBadTarget：非法目标必须报错，而不是静默接受。
func TestGeminiWebFingerprintRejectsBadTarget(t *testing.T) {
	s := fingerprintTestServer(t)

	body := `{"provider":"gw","target":"netscape"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/geminiweb/fingerprint", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)

	if w.Code == http.StatusOK {
		t.Fatalf("bad target accepted: %s", w.Body.String())
	}
}
