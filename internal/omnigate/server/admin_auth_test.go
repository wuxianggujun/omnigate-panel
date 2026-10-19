package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
)

// 收窄密钥只能调模型接口，不能进任何 /admin/* 管理端点。
func TestAdminEndpointsRejectScopedKey(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{
		{Key: "sk-full"},
		{Key: "sk-scoped", Name: "客户专用", Providers: []string{"raccoon"}},
	}}}
	paths := []string{
		"/admin/model-mapping",
		"/admin/raccoon/accounts",
		"/admin/trae/accounts",
		"/admin/geminiweb/gems",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("Authorization", "Bearer sk-scoped")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("scoped key %s = %d want 403 (body=%s)", path, w.Code, w.Body.String())
			}
		})
	}

	// 未带密钥仍然是 401，而不是 403——两者语义不同。
	r := httptest.NewRequest(http.MethodGet, "/admin/model-mapping", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous admin = %d want 401", w.Code)
	}
}
