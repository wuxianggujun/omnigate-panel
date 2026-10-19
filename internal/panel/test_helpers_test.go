package panel

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// pwUser 构造一个带哈希的面板用户（测试用）。
func pwUser(t *testing.T, username string, role panelauth.Role, password string) panelauth.User {
	t.Helper()
	hash, err := panelauth.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword(%s): %v", username, err)
	}
	return panelauth.User{Username: username, Role: role, PasswordHash: hash}
}

// newAuthPanel 构造一个只装配鉴权与给定 OmniGate 回调的面板。
func newAuthPanel(t *testing.T, users []panelauth.User, apiKey string) (*Panel, *panelauth.Store) {
	t.Helper()
	store := panelauth.New(users, time.Hour, 5, 15*time.Minute)
	section := PanelAuthSection{
		SessionHours: 1, MaxFailures: 5, LockMinutes: 15, Users: users,
	}
	p := New(Config{
		APIKey:        apiKey,
		Version:       "test",
		PanelAuth:     store,
		LoadPanelAuth: func() (PanelAuthSection, error) { return section, nil },
		SavePanelAuth: func(next PanelAuthSection) ([]string, error) {
			section = next
			store.Reconfigure(next.Users, time.Hour, 5, 15*time.Minute)
			return nil, nil
		},
	})
	return p, store
}

// mustPanel 构造带默认管理员的面板，并返回一个可用的 Bearer 会话令牌。
func mustPanel(t *testing.T, cfg Config) (*Panel, string) {
	t.Helper()
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	store := panelauth.New(users, time.Hour, 5, 15*time.Minute)
	section := PanelAuthSection{SessionHours: 1, MaxFailures: 5, LockMinutes: 15, Users: users}
	cfg.PanelAuth = store
	cfg.LoadPanelAuth = func() (PanelAuthSection, error) { return section, nil }
	cfg.SavePanelAuth = func(next PanelAuthSection) ([]string, error) {
		section = next
		store.Reconfigure(next.Users, time.Hour, 5, 15*time.Minute)
		return nil, nil
	}
	p := New(cfg)
	_, token, err := store.Login("admin", "pw123456", "test")
	if err != nil {
		t.Fatalf("login admin: %v", err)
	}
	return p, token
}

// reqAs 构造带面板身份的请求（供直接测试内部方法）。
func reqAs(username string, role panelauth.Role) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	ar := authResult{ok: true, mode: modeSession, session: panelauth.Session{Username: username, Role: role}}
	return r.WithContext(context.WithValue(r.Context(), authCtxKey, ar))
}

// do 发起一次面板请求；cookie 非 nil 时附带会话。
func do(p *Panel, method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}

// login 登录并返回会话 Cookie。
func login(t *testing.T, p *Panel, username, password string) *http.Cookie {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	rec := do(p, http.MethodPost, "/panel/api/session", body, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s = %d body=%s", username, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	t.Fatalf("login %s: no session cookie", username)
	return nil
}
