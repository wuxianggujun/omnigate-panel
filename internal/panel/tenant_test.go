package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// ── 归属表 ────────────────────────────────────────────────────────────────

func TestOwnershipStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ownership.json")
	s := newOwnershipStore(path)
	if s.Owner(wbOwnerKey("u1")) != "" {
		t.Fatal("空表不应有任何归属")
	}
	s.Set(wbOwnerKey("u1"), "alice")
	s.Set(omniOwnerKey("raccoon", "浣熊1"), "alice")
	s.Set(wbOwnerKey("u2"), "bob")
	if got := len(s.KeysOf("alice")); got != 2 {
		t.Fatalf("alice 应拥有 2 个账号，得到 %d", got)
	}

	// 落盘 + 重载（原子写后应能读回）。
	s2 := newOwnershipStore(path)
	if got := s2.Owner(wbOwnerKey("u1")); got != "alice" {
		t.Fatalf("重载后 wb:u1 归属 = %q want alice", got)
	}
	if got := s2.Owner(omniOwnerKey("raccoon", "浣熊1")); got != "alice" {
		t.Fatalf("重载后 omni 归属 = %q want alice", got)
	}

	// 删单键：只影响该键。
	s2.Remove(omniOwnerKey("raccoon", "浣熊1"))
	if s2.Owner(omniOwnerKey("raccoon", "浣熊1")) != "" {
		t.Fatal("Remove 后应无归属")
	}
	if s2.Owner(wbOwnerKey("u1")) != "alice" {
		t.Fatal("Remove 不应影响其它键")
	}

	// 删用户：清空其全部归属，别人的不受影响。
	s2.DeleteUser("alice")
	if len(s2.KeysOf("alice")) != 0 {
		t.Fatal("DeleteUser 应清空该用户的全部归属")
	}
	if s2.Owner(wbOwnerKey("u2")) != "bob" {
		t.Fatal("DeleteUser 不应影响其它用户")
	}

	// 损坏文件退化为空表（不 panic）。
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := newOwnershipStore(bad).Owner("wb:x"); got != "" {
		t.Fatalf("损坏文件应退化为空表，得到 %q", got)
	}
}

// ── 视角（ownFilter / canSee*）───────────────────────────────────────────

// reqAs 造一个带（或不带）面板身份的请求，用于直接调用 ownFilter/canSee*。
func reqAs(username string, role panelauth.Role) *http.Request {
	r := httptest.NewRequest("GET", "/panel/api/overview", nil)
	if role == "" {
		return r.WithContext(context.WithValue(r.Context(), authCtxKey, authResult{ok: false}))
	}
	ar := authResult{ok: true, mode: modeSession,
		session: panelauth.Session{Username: username, Role: role}}
	return r.WithContext(context.WithValue(r.Context(), authCtxKey, ar))
}

func TestOwnFilterPerspectives(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	p.ownership.Set(wbOwnerKey("u1"), "alice")
	p.ownership.Set(omniOwnerKey("raccoon", "r1"), "alice")
	p.ownership.Set(wbOwnerKey("u2"), "bob")

	cases := []struct {
		name    string
		req     *http.Request
		all     bool
		ownKeys []string
	}{
		{"匿名只看统计", reqAs("", ""), false, nil},
		{"admin 不过滤", reqAs("admin", panelauth.RoleAdmin), true, nil},
		{"viewer 不过滤", reqAs("guest", panelauth.RoleViewer), true, nil},
		{"user 只看自己", reqAs("alice", panelauth.RoleUser), false, []string{wbOwnerKey("u1"), omniOwnerKey("raccoon", "r1")}},
	}
	for _, c := range cases {
		owned, all := p.ownFilter(c.req)
		if all != c.all {
			t.Fatalf("%s: all=%v want %v", c.name, all, c.all)
		}
		if all {
			continue
		}
		if len(owned) != len(c.ownKeys) {
			t.Fatalf("%s: owned=%v want %v", c.name, owned, c.ownKeys)
		}
		for _, k := range c.ownKeys {
			if !owned[k] {
				t.Fatalf("%s: 缺少归属键 %q（owned=%v）", c.name, k, owned)
			}
		}
	}

	// canSee*：匿名看不到任何账号；alice 看得到自己的、看不到别人的。
	anon := reqAs("", "")
	if p.canSeeWB(anon, "u1") || p.canSeeOmni(anon, "raccoon", "r1") {
		t.Fatal("匿名不应看到任何账号")
	}
	alice := reqAs("alice", panelauth.RoleUser)
	if !p.canSeeWB(alice, "u1") || !p.canSeeOmni(alice, "raccoon", "r1") {
		t.Fatal("alice 应看到自己的账号")
	}
	if p.canSeeWB(alice, "u2") {
		t.Fatal("alice 不应看到 bob 的账号")
	}
}

// ── 端点级归属校验 ────────────────────────────────────────────────────────

// 普通用户对别人的账号做运维 → 403 not_your_account；匿名 → 401。
func TestTenantGuardOnAccountOps(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	p.cfg.OmniCheckin = func(provider, label string) (any, error) { return map[string]any{"ok": true}, nil }
	p.cfg.OmniRemove = func(provider, label string) (any, error) { return map[string]any{"removed": true}, nil }
	p.ownership.Set(omniOwnerKey("raccoon", "mine"), "alice")
	p.ownership.Set(omniOwnerKey("raccoon", "others"), "bob")

	alice := login(t, p, "alice", "pw123456")
	admin := login(t, p, "admin", "pw123456")

	body := func(label string) string {
		return `{"provider":"raccoon","label":"` + label + `"}`
	}
	// 自己的账号：放行（闭包被调用）。
	if rec := do(p, "POST", "/panel/api/omni/account/checkin", body("mine"), alice, nil); rec.Code != 200 {
		t.Fatalf("alice checkin own = %d want 200", rec.Code)
	}
	// 别人的账号：403。
	if rec := do(p, "POST", "/panel/api/omni/account/checkin", body("others"), alice, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("alice checkin others = %d want 403", rec.Code)
	}
	// 匿名：401。
	if rec := do(p, "POST", "/panel/api/omni/account/checkin", body("mine"), nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon checkin = %d want 401", rec.Code)
	}
	// admin：全量放行。
	if rec := do(p, "POST", "/panel/api/omni/account/checkin", body("others"), admin, nil); rec.Code != 200 {
		t.Fatalf("admin checkin others = %d want 200", rec.Code)
	}

	// 平台配置：普通用户 403，匿名 401，viewer/admin 通过鉴权层。
	if rec := do(p, "GET", "/panel/api/config", "", alice, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("user read config = %d want 403", rec.Code)
	}
	if rec := do(p, "GET", "/panel/api/config", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon read config = %d want 401", rec.Code)
	}
	if rec := do(p, "GET", "/panel/api/config", "", admin, nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("admin read config = %d want 501（通过了 privileged 鉴权层）", rec.Code)
	}
}

// omniAccounts 按归属过滤：匿名空明细、user 只有自己的、admin 全部。
func TestOmniAccountsScoped(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	p.cfg.OmniAccounts = func(includeBalance bool) (any, error) {
		return map[string]any{"providers": []any{
			map[string]any{"name": "raccoon", "type": "raccoon", "accounts": []any{
				map[string]any{"label": "mine"},
				map[string]any{"label": "others"},
			}},
		}}, nil
	}
	p.ownership.Set(omniOwnerKey("raccoon", "mine"), "alice")
	p.ownership.Set(omniOwnerKey("raccoon", "others"), "bob")

	labels := func(rec *httptest.ResponseRecorder) []string {
		var m struct {
			Data struct {
				Providers []struct {
					Accounts []struct {
						Label string `json:"label"`
					} `json:"accounts"`
				} `json:"providers"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
			t.Fatalf("解析响应失败: %v (%s)", err, rec.Body.String())
		}
		var out []string
		for _, prov := range m.Data.Providers {
			for _, a := range prov.Accounts {
				out = append(out, a.Label)
			}
		}
		return out
	}

	if got := labels(do(p, "GET", "/panel/api/omni/accounts", "", nil, nil)); len(got) != 0 {
		t.Fatalf("匿名应看不到账号明细，得到 %v", got)
	}
	if got := labels(do(p, "GET", "/panel/api/omni/accounts", "", login(t, p, "alice", "pw123456"), nil)); len(got) != 1 || got[0] != "mine" {
		t.Fatalf("alice 应只看到 mine，得到 %v", got)
	}
	if got := labels(do(p, "GET", "/panel/api/omni/accounts", "", login(t, p, "admin", "pw123456"), nil)); len(got) != 2 {
		t.Fatalf("admin 应看到全部，得到 %v", got)
	}
}

// ── 自助注册 ──────────────────────────────────────────────────────────────

func TestRegisterFlow(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, section := newAuthPanel(t, users, "test-key")

	// 注册成功：强制 role=user（哪怕请求体里塞 admin）。
	rec := do(p, "POST", "/panel/api/register",
		`{"username":"newbie","password":"pw123456","role":"admin"}`, nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"role":"user"`) {
		t.Fatalf("register = %d %s want 200 role=user", rec.Code, rec.Body.String())
	}
	if len(section.Users) != 2 {
		t.Fatalf("注册后应有 2 个账号，得到 %d", len(section.Users))
	}
	// 新账号能登录且角色是 user。
	ck := login(t, p, "newbie", "pw123456")
	if rec := do(p, "GET", "/panel/api/session", "", ck, nil); !strings.Contains(rec.Body.String(), `"role":"user"`) {
		t.Fatalf("注册账号角色 = %s want user", rec.Body.String())
	}
	// 重名 → 409（绝不覆盖已有账号）。
	if rec := do(p, "POST", "/panel/api/register", `{"username":"admin","password":"pw123456"}`, nil, nil); rec.Code != http.StatusConflict {
		t.Fatalf("重名注册 = %d want 409", rec.Code)
	}
	// 弱密码 → 400。
	if rec := do(p, "POST", "/panel/api/register", `{"username":"weak","password":"123"}`, nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("弱密码注册 = %d want 400", rec.Code)
	}

	// 关闭注册开关 → 403 registration_disabled。
	off := false
	section.AllowRegistration = &off
	if rec := do(p, "POST", "/panel/api/register", `{"username":"nope","password":"pw123456"}`, nil, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("关闭注册后 = %d want 403", rec.Code)
	}
}

// 面板未配置任何账号时不允许注册（没有 store 也就没有面板账号体系）。
func TestRegisterRefusedWithoutUsers(t *testing.T) {
	p, _ := newAuthPanel(t, nil, "test-key")
	if rec := do(p, "POST", "/panel/api/register", `{"username":"x","password":"pw123456"}`, nil, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("未配置账号时注册 = %d want 400", rec.Code)
	}
}

// omniAccountAdd：服务端定向追加配置账号；label 被他人占用 → 409。
func TestOmniAccountAddOwnershipGuard(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	saved := ""
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{"providers": []any{
			map[string]any{"name": "runable", "type": "runable", "accounts": []any{
				map[string]any{"label": "taken", "cookie": "c0"},
			}},
		}}, nil
	}
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) {
		saved = string(raw)
		return nil, nil
	}
	p.ownership.Set(omniOwnerKey("runable", "taken"), "bob")

	alice := login(t, p, "alice", "pw123456")
	// 认领别人的 label → 409，且不写盘。
	if rec := do(p, "POST", "/panel/api/omni/account/add",
		`{"provider":"runable","label":"taken","cookie":"x"}`, alice, nil); rec.Code != http.StatusConflict {
		t.Fatalf("认领他人 label = %d want 409", rec.Code)
	}
	if saved != "" {
		t.Fatal("被拒的添加不应写盘")
	}
	// 自己的新 label → 200 且写盘，归属记录写入。
	if rec := do(p, "POST", "/panel/api/omni/account/add",
		`{"provider":"runable","label":"fresh","cookie":"ck"}`, alice, nil); rec.Code != 200 {
		t.Fatalf("新增账号 = %d want 200", rec.Code)
	}
	if saved == "" || !strings.Contains(saved, `"fresh"`) || !strings.Contains(saved, "ck") {
		t.Fatalf("写盘内容不含新账号：%s", saved)
	}
	if p.ownership.Owner(omniOwnerKey("runable", "fresh")) != "alice" {
		t.Fatal("新增账号应归属 alice")
	}
	// 匿名不能添加 → 401。
	if rec := do(p, "POST", "/panel/api/omni/account/add",
		`{"provider":"runable","label":"anon","cookie":"x"}`, nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("匿名添加 = %d want 401", rec.Code)
	}
}

// 面板账号被删除时，其名下账号归属一并清空（账号变无主，不会跟着残留）。
func TestDeletePanelUserClearsOwnership(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	p.ownership.Set(wbOwnerKey("u1"), "alice")
	p.ownership.Set(wbOwnerKey("u2"), "bob")

	admin := login(t, p, "admin", "pw123456")
	if rec := do(p, "POST", "/panel/api/admin/users/delete", `{"username":"alice"}`, admin, nil); rec.Code != 200 {
		t.Fatalf("删除面板账号 = %d want 200", rec.Code)
	}
	if p.ownership.Owner(wbOwnerKey("u1")) != "" {
		t.Fatal("被删用户的账号归属应清空")
	}
	if p.ownership.Owner(wbOwnerKey("u2")) != "bob" {
		t.Fatal("其它用户的归属不应受影响")
	}
}

// ── OmniGate 管理代理 ─────────────────────────────────────────────────────

// 上游 401（网关 api_key 不匹配）必须改写成 502，绝不能原样回传：前端把 401 当
// 「面板会话失效」处理并弹登录门，而授权轮询每 3 秒一次 → 弹窗反复被顶回来。
func TestOmniAdminProxyMapsUpstream401(t *testing.T) {
	p := New(Config{
		Version: "test",
		APIKey:  "k",
		OmniAdmin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != "Bearer k" {
				t.Errorf("未注入网关 api_key，得到 %q", got)
			}
			if r.URL.Path != "/admin/raccoon/accounts" {
				t.Errorf("转发路径 = %q want /admin/raccoon/accounts", r.URL.Path)
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"unauthorized"}}`))
		}),
	})
	r := reqAs("alice", panelauth.RoleUser)
	r.Method = "GET"
	r.URL.Path = "/panel/api/omni/admin/raccoon/accounts"
	r.URL.RawQuery = "provider=raccoon"
	w := httptest.NewRecorder()
	p.omniAdminProxy(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("上游 401 应改写成 502，得到 %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "omnigate_unauthorized") {
		t.Fatalf("响应体应带 omnigate_unauthorized，得到 %q", w.Body.String())
	}
}

// 白名单外的子路径一律 404（不转发给 OmniGate）。
func TestOmniAdminProxyRejectsUnknownSubpath(t *testing.T) {
	called := false
	p := New(Config{Version: "test", APIKey: "k",
		OmniAdmin: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true })})
	r := reqAs("alice", panelauth.RoleUser)
	r.URL.Path = "/panel/api/omni/admin/raccoon/redirect"
	w := httptest.NewRecorder()
	p.omniAdminProxy(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("白名单外子路径应 404，得到 %d", w.Code)
	}
	if called {
		t.Fatal("白名单外子路径不应被转发")
	}
}
