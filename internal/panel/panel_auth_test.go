package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// pwUser 造一个带真实哈希的账号（外部测试拿不到 panelauth 的低迭代内部函数）。
func pwUser(t *testing.T, name string, role panelauth.Role, pw string) panelauth.User {
	t.Helper()
	h, err := panelauth.HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	return panelauth.User{Username: name, Role: role, PasswordHash: h}
}

// newAuthPanel 造一个"已配置面板账号"的面板：账号存于内存 section，保存闭包做
// 校验 + 热重建 store（与 main 装配同构）。
func newAuthPanel(t *testing.T, users []panelauth.User, apiKey string) (*Panel, *PanelAuthSection) {
	t.Helper()
	section := &PanelAuthSection{SessionHours: 72, MaxFailures: 5, LockMinutes: 15, Users: users}
	store := panelauth.New(users, time.Hour, 5, time.Minute)
	p := New(Config{
		Version:   "test",
		APIKey:    apiKey,
		PanelAuth: store,
		LoadPanelAuth: func() (PanelAuthSection, error) {
			return *section, nil
		},
		SavePanelAuth: func(s PanelAuthSection) ([]string, error) {
			if err := panelauth.ValidateUsers(s.Users); err != nil {
				return nil, err
			}
			*section = s
			store.Reconfigure(s.Users, time.Duration(s.SessionHours)*time.Hour, s.MaxFailures,
				time.Duration(s.LockMinutes)*time.Minute)
			return nil, nil
		},
	})
	return p, section
}

// do 发一个请求，返回响应记录。
func do(p *Panel, method, path, body string, cookie *http.Cookie, headers map[string]string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	p.ServeHTTP(rec, req)
	return rec
}

func login(t *testing.T, p *Panel, user, pass string) *http.Cookie {
	t.Helper()
	rec := do(p, "POST", "/panel/api/session", `{"username":"`+user+`","password":"`+pass+`"}`, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %q: code=%d body=%s", user, rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			if !c.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if c.SameSite != http.SameSiteStrictMode {
				t.Error("session cookie must be SameSite=Strict")
			}
			return c
		}
	}
	t.Fatal("login response has no session cookie")
	return nil
}

func TestPanelAuthSessionLoginLogout(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")

	// 未登录：查询登录态 → authenticated=false, configured=true。
	rec := do(p, "GET", "/panel/api/session", "", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"configured":true`) ||
		!strings.Contains(rec.Body.String(), `"authenticated":false`) {
		t.Fatalf("session state (anon) = %d %s", rec.Code, rec.Body.String())
	}
	// 未登录访问受保护接口 → 401。
	if rec := do(p, "GET", "/panel/api/config", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon config = %d want 401", rec.Code)
	}
	// 错误密码 → 401（且不泄漏账号是否存在）。
	if rec := do(p, "POST", "/panel/api/session", `{"username":"admin","password":"nope"}`, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad password = %d want 401", rec.Code)
	}
	// 正确密码 → 会话 Cookie。
	ck := login(t, p, "admin", "pw123456")
	// 带 Cookie 访问受保护接口：通过鉴权层（无 LoadConfig → 501，而非 401）。
	if rec := do(p, "GET", "/panel/api/config", "", ck, nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("authed config = %d want 501 (auth passed)", rec.Code)
	}
	// 会话态查询显示已登录身份。
	rec = do(p, "GET", "/panel/api/session", "", ck, nil)
	if !strings.Contains(rec.Body.String(), `"authenticated":true`) || !strings.Contains(rec.Body.String(), `"admin"`) {
		t.Fatalf("session state (authed) = %s", rec.Body.String())
	}
	// 登出后会话立即失效。
	if rec := do(p, "DELETE", "/panel/api/session", "", ck, nil); rec.Code != 200 {
		t.Fatalf("logout = %d", rec.Code)
	}
	if rec := do(p, "GET", "/panel/api/config", "", ck, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("after logout config = %d want 401", rec.Code)
	}
}

// 配了面板账号后，网关 api_key 不再能打开面板。
func TestPanelAuthAPIKeyRejectedWhenUsersConfigured(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")

	if rec := do(p, "GET", "/panel/api/config", "", nil, map[string]string{"Authorization": "Bearer test-key"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("api_key with users configured = %d want 401", rec.Code)
	}
	if rec := do(p, "GET", "/panel/api/config", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no credentials = %d want 401", rec.Code)
	}
}

// 未配置面板账号时，api_key 门仍作为过渡可用（不会把管理员锁在外面）。
func TestPanelAuthLegacyFallbackWithoutUsers(t *testing.T) {
	p, _ := newAuthPanel(t, nil, "test-key")
	if rec := do(p, "GET", "/panel/api/config", "", nil, map[string]string{"Authorization": "Bearer test-key"}); rec.Code != http.StatusNotImplemented {
		t.Fatalf("legacy api_key = %d want 501 (auth passed)", rec.Code)
	}
	if rec := do(p, "GET", "/panel/api/config", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy anon = %d want 401", rec.Code)
	}
}

func TestPanelAuthRoleEnforcement(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "root", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "guest", panelauth.RoleViewer, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	admin := login(t, p, "root", "pw123456")
	viewer := login(t, p, "guest", "pw123456")

	// viewer 可读。
	if rec := do(p, "GET", "/panel/api/config", "", viewer, nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("viewer read = %d want 501", rec.Code)
	}
	// viewer 不可写：admin 中间件 403。
	rec := do(p, "POST", "/panel/api/config", `{}`, viewer, nil)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "admin_required") {
		t.Fatalf("viewer write = %d %s want 403 admin_required", rec.Code, rec.Body.String())
	}
	// viewer 不能进账号管理。
	if rec := do(p, "GET", "/panel/api/admin/users", "", viewer, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer admin/users = %d want 403", rec.Code)
	}
	// admin 可写（无 SaveConfig → 501，说明通过了 admin 中间件）。
	if rec := do(p, "POST", "/panel/api/config", `{}`, admin, nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("admin write = %d want 501", rec.Code)
	}
	// admin 可读账号列表。
	if rec := do(p, "GET", "/panel/api/admin/users", "", admin, nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"root"`) {
		t.Fatalf("admin list users = %d %s", rec.Code, rec.Body.String())
	}
}

// 会话 Cookie 的非安全方法必须校验同源，防 CSRF。
func TestPanelAuthCSRF(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")
	ck := login(t, p, "admin", "pw123456")

	// 跨站 Origin → 403。
	rec := do(p, "POST", "/panel/api/admin/users", `{}`, ck, map[string]string{"Origin": "https://evil.example"})
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "csrf") {
		t.Fatalf("cross-origin write = %d %s want 403 csrf", rec.Code, rec.Body.String())
	}
	// 跨站 Sec-Fetch-Site（无 Origin）→ 403。
	rec = do(p, "POST", "/panel/api/admin/users", `{}`, ck, map[string]string{"Sec-Fetch-Site": "cross-site"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-site fetch = %d want 403", rec.Code)
	}
	// 同源 Origin（匹配 Host=example.com）→ 放行（空 body → 400，但不是 CSRF 403）。
	rec = do(p, "POST", "/panel/api/admin/users", `{}`, ck, map[string]string{"Origin": "http://example.com"})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("same-origin write rejected as CSRF: %d %s", rec.Code, rec.Body.String())
	}
	// GET 不校验同源（非安全方法才校验）。
	if rec := do(p, "GET", "/panel/api/admin/users", "", ck, map[string]string{"Origin": "https://evil.example"}); rec.Code != 200 {
		t.Fatalf("cross-origin GET = %d want 200 (CSRF only guards unsafe methods)", rec.Code)
	}
}

func TestPanelAuthUserManagement(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, section := newAuthPanel(t, users, "test-key")
	admin := login(t, p, "admin", "pw123456")

	// 新增 viewer。
	if rec := do(p, "POST", "/panel/api/admin/users", `{"username":"ops","role":"viewer","password":"secret1"}`, admin, nil); rec.Code != 200 {
		t.Fatalf("add user = %d %s", rec.Code, rec.Body.String())
	}
	if _, _, ok := panelauth.FindUser(section.Users, "ops"); !ok {
		t.Fatal("ops not persisted")
	}
	// 已有账号：密码留空 → 只改角色，哈希保持。
	before, _, _ := panelauth.FindUser(section.Users, "ops")
	if rec := do(p, "POST", "/panel/api/admin/users", `{"username":"ops","role":"admin","password":""}`, admin, nil); rec.Code != 200 {
		t.Fatalf("role change = %d %s", rec.Code, rec.Body.String())
	}
	after, _, _ := panelauth.FindUser(section.Users, "ops")
	if after.Role != panelauth.RoleAdmin || after.PasswordHash != before.PasswordHash {
		t.Fatalf("role change mutated hash or role: %+v", after)
	}
	// 新密码可登录。
	ops := login(t, p, "ops", "secret1")
	if rec := do(p, "GET", "/panel/api/admin/users", "", ops, nil); rec.Code != 200 {
		t.Fatalf("promoted ops cannot list users = %d", rec.Code)
	}
	// 重置他人密码。
	if rec := do(p, "POST", "/panel/api/admin/users/password", `{"username":"ops","password":"rotated1"}`, admin, nil); rec.Code != 200 {
		t.Fatalf("reset password = %d %s", rec.Code, rec.Body.String())
	}
	// 不能用过短密码。
	if rec := do(p, "POST", "/panel/api/admin/users", `{"username":"weak","role":"viewer","password":"123"}`, admin, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("weak password = %d want 400", rec.Code)
	}
	// 不能删除自己。
	if rec := do(p, "POST", "/panel/api/admin/users/delete", `{"username":"admin"}`, admin, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete self = %d want 400", rec.Code)
	}
	// 删除 ops：成功，且不能在只剩一个管理员时删掉最后管理员。
	if rec := do(p, "POST", "/panel/api/admin/users/delete", `{"username":"ops"}`, admin, nil); rec.Code != 200 {
		t.Fatalf("delete ops = %d %s", rec.Code, rec.Body.String())
	}
	if len(section.Users) != 1 {
		t.Fatalf("users=%d want 1", len(section.Users))
	}
}

func TestPanelAuthChangeOwnPassword(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, section := newAuthPanel(t, users, "test-key")
	ck := login(t, p, "admin", "pw123456")

	// 当前密码错误 → 401。
	if rec := do(p, "POST", "/panel/api/password", `{"current_password":"wrong","new_password":"newpw123"}`, ck, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current = %d want 401", rec.Code)
	}
	// 正确 → 200，且新密码生效。
	if rec := do(p, "POST", "/panel/api/password", `{"current_password":"pw123456","new_password":"newpw123"}`, ck, nil); rec.Code != 200 {
		t.Fatalf("change = %d %s", rec.Code, rec.Body.String())
	}
	u, _, _ := panelauth.FindUser(section.Users, "admin")
	if !panelauth.VerifyPassword(u.PasswordHash, "newpw123") {
		t.Fatal("new password does not verify after change")
	}
	if rec := do(p, "POST", "/panel/api/session", `{"username":"admin","password":"pw123456"}`, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password still works = %d want 401", rec.Code)
	}
	login(t, p, "admin", "newpw123")
}

// 登录失败达到阈值即锁定（429），并且面板不回显账号是否存在。
func TestPanelAuthLockout(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")
	// MaxFailures=5：连错 5 次后第 6 次即使密码正确也 429。
	for i := 0; i < 5; i++ {
		do(p, "POST", "/panel/api/session", `{"username":"admin","password":"bad"}`, nil, nil)
	}
	rec := do(p, "POST", "/panel/api/session", `{"username":"admin","password":"pw123456"}`, nil, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("lockout = %d want 429 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("lockout response missing Retry-After")
	}
}

// resolveAuth 的边界：未知会话令牌不等于已登录；空令牌不鉴权。
func TestPanelAuthResolveAuthEdges(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")
	bogus := &http.Cookie{Name: sessionCookieName, Value: "not-a-real-token"}
	if rec := do(p, "GET", "/panel/api/config", "", bogus, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus cookie = %d want 401", rec.Code)
	}
	// 会话令牌也可经 Bearer 头传（自动化友好）。
	ck := login(t, p, "admin", "pw123456")
	if rec := do(p, "GET", "/panel/api/config", "", nil, map[string]string{"Authorization": "Bearer " + ck.Value}); rec.Code != http.StatusNotImplemented {
		t.Fatalf("bearer session token = %d want 501", rec.Code)
	}
}

// 校验 panelauth 的错误语义在 HTTP 层被正确翻译。
func TestPanelAuthErrorMapping(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")
	admin := login(t, p, "admin", "pw123456")
	// 非法角色。
	rec := do(p, "POST", "/panel/api/admin/users", `{"username":"x","role":"root","password":"secret1"}`, admin, nil)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), panelauth.ErrBadRole.Error()) {
		t.Fatalf("bad role = %d %s", rec.Code, rec.Body.String())
	}
}
