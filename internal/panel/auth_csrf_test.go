package panel

import (
	"net/http"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// 登录与注册是公开端点，但写操作仍必须来自同源，防「跨站把受害者登入攻击者账号」。
func TestPublicAuthEndpointsRejectCrossOrigin(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")

	cases := []struct {
		name string
		path string
		body string
	}{
		{"login", "/panel/api/session", `{"username":"admin","password":"pw123456"}`},
		{"register", "/panel/api/register", `{"username":"evil","password":"pw123456"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(p, http.MethodPost, tc.path, tc.body, nil, map[string]string{
				"Origin": "https://evil.example",
			})
			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s cross-origin = %d want 403 body=%s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}
