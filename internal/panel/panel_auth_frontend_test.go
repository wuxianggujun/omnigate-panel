package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPanelAuthFrontendWiring 前端鉴权接线回归：登录控件存在、登录按钮已绑定
// 点击、启动走 /panel/api/session、viewer 有 [data-admin] 隐藏规则。
//
// 这些是"离线静态冒烟跑不出、真机才暴露"的接线。典型事故：登录按钮在 <form>
// 之外，只绑了 form 的 submit——顶层求值不报错、语法校验全绿，但点按钮毫无反应。
// 用静态断言把这类接线钉死。
func TestPanelAuthFrontendWiring(t *testing.T) {
	p := newTestPanel()
	get := func(path string) string {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		return rec.Body.String()
	}

	html := get("/panel/")
	for _, id := range []string{
		`id="loginVeil"`, `id="loginForm"`, `id="loginUser"`, `id="loginPass"`,
		`id="btnLogin"`, `id="whoami"`, `id="btnLogout"`, `id="paccBox"`, `id="loginErr"`,
	} {
		if !strings.Contains(html, id) {
			t.Errorf("index.html missing %s", id)
		}
	}

	js := get("/panel/app.js")
	for _, needle := range []string{
		// 登录按钮点击必须绑定（回归：不再只依赖 form submit）。
		"$('btnLogin').addEventListener('click', doLogin)",
		"$('loginForm').addEventListener('submit'",
		// 启动判定与登入登出。
		"fetch('/panel/api/session')",
		"api('session', { method: 'POST'",
		"api('session', { method: 'DELETE'",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js missing wiring: %q", needle)
		}
	}

	css := get("/panel/theme.css")
	if !strings.Contains(css, `body[data-role="viewer"] [data-admin]`) {
		t.Error("theme.css missing viewer [data-admin] gating rule")
	}
}
