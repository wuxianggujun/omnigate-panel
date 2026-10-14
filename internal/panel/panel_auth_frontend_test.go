package panel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
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
		`id="btnLogin"`, `id="btnLogout"`, `id="paccBox"`, `id="loginErr"`,
		// 默认公开只读：未登录时顶部的「登录」入口。
		`id="btnTopLogin"`,
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
		// 默认公开只读：顶部登录入口 + 匿名也能看 OmniGate 模型（走面板侧代理，
		// 不再依赖需要 api_key 的 /omni/*）。
		"$('btnTopLogin').onclick",
		"api('omni/models')",
		"looksMasked",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js missing wiring: %q", needle)
		}
	}

	css := get("/panel/theme.css")
	if !strings.Contains(css, `body[data-role="viewer"] [data-admin]`) {
		t.Error("theme.css missing viewer [data-admin] gating rule")
	}
	// 未登录（public）与 viewer 一样隐藏写控件。
	if !strings.Contains(css, `body[data-role="public"] [data-admin]`) {
		t.Error("theme.css missing public (anonymous) [data-admin] gating rule")
	}
}

// TestPanelAuthFrontendRegisterEntry 回归：登录弹层必须有显眼的「注册」入口。
// 用户曾反馈「对话框里没有办法注册」——当时的入口是标题旁的小 tab，容易被忽略，
// 现在改成表单顶部的整宽两段式切换（.tabs.seg）。
func TestPanelAuthFrontendRegisterEntry(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	html := rec.Body.String()

	for _, needle := range []string{
		`id="authTabs"`,
		`class="tabs seg"`,
		`data-auth-mode="login"`,
		`data-auth-mode="register"`,
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html 登录弹层缺少注册入口: %q", needle)
		}
	}
	if !strings.Contains(html, `id="authTabs" hidden`) {
		t.Error("未开放自助注册时应默认隐藏切换条（由 app.js 按 allow_registration 打开）")
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/theme.css", nil))
	css := rec.Body.String()
	if !strings.Contains(css, ".dlg .tabs.seg") {
		t.Error("theme.css 缺少整宽两段式切换条样式 .dlg .tabs.seg")
	}

	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	js := rec.Body.String()
	for _, needle := range []string{
		// 密码框「显示 / 隐藏」由 app.js 统一注入。
		"function addPwEyes()",
		"addPwEyes();",
		"dataset.pwEye",
		// 401 统一收敛（不再每 3 秒把登录门顶回来）。
		"function authLost(",
		"function httpErr(",
		// 已打开页面的版本自检。
		"function checkBuild(",
		"function stickyToast(",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少接线: %q", needle)
		}
	}
}

// TestPanelAuthSessionExposesBuild /panel/api/session 必须带资源指纹 build：
// 已经打开的页面据此发现「服务端已更新」，提示刷新（否则用户会在旧 JS 上排查新功能）。
func TestPanelAuthSessionExposesBuild(t *testing.T) {
	if len(panelBuild) != 12 {
		t.Fatalf("panelBuild 应为 12 位十六进制，得到 %q", panelBuild)
	}
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")
	rec := do(p, "GET", "/panel/api/session", "", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("anon session = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"build":"`+panelBuild+`"`) {
		t.Fatalf("session 响应未带 build 指纹: %s", rec.Body.String())
	}
}
