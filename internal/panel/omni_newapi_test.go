package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

func TestNewAPICaptureSnippet(t *testing.T) {
	got := newAPICaptureSnippet("https://wb.example.com/", "STATE123", "/api/user/self", false)
	if !strings.Contains(got, "https://wb.example.com/panel/api/omni/newapi/capture?state=STATE123") {
		t.Fatalf("脚本应包含带 state 的回传地址，得到:\n%s", got)
	}
	// 脚本走站点前端自己的 bootstrap 路径：cookie-only 刷新 + 同名 Web Lock。
	for _, want := range []string{
		"credentials: 'include'",
		"/api/user/auth/refresh",
		"new-api:auth-refresh",
		"AUTH_REFRESH_RACE",
		"session_sid",
		"mode: 'cors'",
		"out.error",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("脚本缺少 %q", want)
		}
	}
	// 不该残留未转义的 Go 动词（%%c 已转成 %c）。
	if strings.Contains(got, "%%c") {
		t.Error("控制台颜色占位符转义失败")
	}
}

// TestNewAPICookieSnippet：Cookie 型站点（AnyRouter / AgentRouter）的脚本不回传
// access_token，只回传会话 Cookie + 数字用户 ID，并且明确要求非 httpOnly。
func TestNewAPICookieSnippet(t *testing.T) {
	got := newAPICaptureSnippet("https://anyrouter.top", "STATE9", "/api/user/self", true)
	if !strings.Contains(got, "https://anyrouter.top/panel/api/omni/newapi/capture?state=STATE9") {
		t.Fatalf("缺少回传地址:\n%s", got)
	}
	for _, want := range []string{
		"document.cookie",
		`"/api/user/self"`,
		"credentials: 'include'",
		"user_id",
		"httpOnly",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("cookie 脚本缺少 %q", want)
		}
	}
	// cookie 型站点没有 access_token 可刷新，脚本不该去调 refresh。
	if strings.Contains(got, "auth/refresh") {
		t.Error("cookie 脚本不该调用 auth/refresh")
	}
	// 自定义 self 路径要被用上（不同部署可能不同）。
	custom := newAPICaptureSnippet("https://x.test", "S1", "/api/user/info", true)
	if !strings.Contains(custom, `"/api/user/info"`) {
		t.Errorf("自定义 self 路径未生效:\n%s", custom)
	}
}

func TestPanelPublicBase(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://internal/panel/", nil)
	r.Host = "wb.example.com"
	r.Header.Set("X-Forwarded-Proto", "https")
	if got := panelPublicBase(r); got != "https://wb.example.com" {
		t.Fatalf("得到 %q", got)
	}
	// 本机（无点）默认 http。
	r2 := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:7863/panel/", nil)
	if got := panelPublicBase(r2); got != "http://127.0.0.1:7863" {
		t.Fatalf("得到 %q", got)
	}
}

// 自动命名的账号在回传后才落归属：普通 user 拿到标，别人的名字不抢，管理员保持共享。
func TestNewAPIAutoLabelClaim(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})

	p.rememberNewAPIClaim("s1", "justwoker", reqAs("alice", panelauth.RoleUser))
	p.takeNewAPIClaim("s1", "浣熊账号1")
	if got := p.ownership.Owner(omniOwnerKey("justwoker", "浣熊账号1")); got != "alice" {
		t.Fatalf("归属应为 alice，得到 %q", got)
	}
	// 一次性：同一个 state 不能重复落标。
	p.takeNewAPIClaim("s1", "别的名字")
	if got := p.ownership.Owner(omniOwnerKey("justwoker", "别的名字")); got != "" {
		t.Fatalf("claim 应一次性消费，得到 %q", got)
	}
	// 名字已被别人占用时不抢。
	p.ownership.Set(omniOwnerKey("justwoker", "bob"), "bob")
	p.rememberNewAPIClaim("s2", "justwoker", reqAs("alice", panelauth.RoleUser))
	p.takeNewAPIClaim("s2", "bob")
	if got := p.ownership.Owner(omniOwnerKey("justwoker", "bob")); got != "bob" {
		t.Fatalf("不该抢别人的账号，得到 %q", got)
	}
	// 管理员不落标（账号保持无主/共享）。
	p.rememberNewAPIClaim("s3", "justwoker", reqAs("root", panelauth.RoleAdmin))
	p.takeNewAPIClaim("s3", "shared")
	if got := p.ownership.Owner(omniOwnerKey("justwoker", "shared")); got != "" {
		t.Fatalf("管理员不该落标，得到 %q", got)
	}
}

func TestOmniResultLabel(t *testing.T) {
	if got := omniResultLabel(map[string]any{"label": " 浣熊 "}); got != "浣熊" {
		t.Fatalf("得到 %q", got)
	}
	if got := omniResultLabel(nil); got != "" {
		t.Fatalf("得到 %q", got)
	}
}
