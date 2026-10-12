package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewAPICaptureSnippet(t *testing.T) {
	got := newAPICaptureSnippet("https://wb.example.com/", "STATE123")
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
