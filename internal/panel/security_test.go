package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestPanel() *Panel {
	// 未注入面板鉴权 store（= 未配置账号）：所有 /panel/api/* 一律 401，
	// 不进入依赖 Pool/Upstream 的 handler。
	return New(Config{Version: "test", APIKey: "test-key"})
}

// 面板安全响应头必须覆盖：页面、静态脚本、鉴权失败响应。
func TestSecurityHeadersOnAllPanelResponses(t *testing.T) {
	p := newTestPanel()
	paths := []struct{ method, path string }{
		{"GET", "/panel/"},
		{"GET", "/panel/app.js"},
		{"GET", "/panel/theme.css"},
		{"GET", "/panel/api/overview"}, // 401（未提供 key）
		{"POST", "/panel/api/config"},  // 401
		{"GET", "/panel/api/nonexistent"},
	}
	for _, c := range paths {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		h := rec.Header()
		if got := h.Get("Content-Security-Policy"); got == "" {
			t.Errorf("%s %s: missing CSP", c.method, c.path)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s %s: X-Content-Type-Options=%q", c.method, c.path, h.Get("X-Content-Type-Options"))
		}
		if h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s %s: X-Frame-Options=%q", c.method, c.path, h.Get("X-Frame-Options"))
		}
		if h.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("%s %s: Referrer-Policy=%q", c.method, c.path, h.Get("Referrer-Policy"))
		}
	}
}

// CSP 必须禁止内联脚本与 iframe 嵌套（严格策略的核心约束）。
func TestCSPDisallowsInlineScriptAndFraming(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	csp := rec.Header().Get("Content-Security-Policy")

	for _, must := range []string{
		"script-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"default-src 'none'",
	} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q; got: %s", must, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Errorf("CSP must not allow unsafe-inline scripts; got: %s", csp)
	}
}

// 页面必须引用外部脚本（内联脚本会被上面的 CSP 拦掉，页面将完全不可用）。
func TestIndexReferencesExternalScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	if !strings.Contains(body, `<script src="app.js"></script>`) {
		t.Error("index.html must load app.js externally (inline script is blocked by CSP)")
	}
	// 反例保护：出现内联 <script>...</script> 内容块即为回归
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("index.html still contains an inline <script> block; CSP would block it")
	}
}

// app.js 必须能作为同源脚本取到且类型正确（否则页面白屏）。
func TestAppScriptServed(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/app.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type=%q want javascript", ct)
	}
	if !strings.Contains(rec.Body.String(), "'use strict'") {
		t.Error("app.js body looks wrong")
	}
}

// 页面与静态资源必须带 Cache-Control（no-store + no-cache）：否则浏览器会启发式
// 复用旧 app.js——服务端已更新却仍跑旧前端（模型筛选失效、看不到域徽标）。
func TestStaticAssetsNoCache(t *testing.T) {
	p := newTestPanel()
	for _, path := range []string{"/panel/", "/panel/app.js", "/panel/theme.css"} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		cc := rec.Header().Get("Cache-Control")
		if !strings.Contains(cc, "no-store") || !strings.Contains(cc, "no-cache") {
			t.Errorf("%s Cache-Control=%q want no-store + no-cache", path, cc)
		}
	}
}

// 统一主题 + OmniGate 并入主面板：主面板是唯一页面，必须外链同一份 theme.css
// （设计令牌 + 组件）且不含内联 <style>/<script>（严格 CSP）。旧的 OmniGate 独立
// 子页已并入主面板，其 URL 302 到主面板账号池。
func TestUnifiedThemeAndMergedOmni(t *testing.T) {
	p := newTestPanel()

	// theme.css 可取到、类型正确、含明暗两套令牌。
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/theme.css", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("theme.css code=%d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("theme.css Content-Type=%q want css", ct)
	}
	css := rec.Body.String()
	for _, must := range []string{":root", `[data-theme="light"]`, ".shell", ".box", ".tag", ".og-hint"} {
		if !strings.Contains(css, must) {
			t.Errorf("theme.css missing %q (shared design system)", must)
		}
	}

	// 主面板（唯一页面）要 <link> 引入 theme.css，且无内联样式/脚本。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `href="/panel/theme.css"`) {
		t.Error("/panel/ must link /panel/theme.css (unified theme)")
	}
	if strings.Contains(body, "<style>") {
		t.Error("/panel/ still contains an inline <style> block; theme must come from theme.css")
	}
	if strings.Contains(body, "<script>\n") || strings.Contains(body, "<script> ") {
		t.Error("/panel/ contains an inline <script> block (blocked by CSP)")
	}
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Error("/panel/ must declare data-theme (shared theme tokens)")
	}
	// OmniGate 的供应商 / 出站代理视图与统一账号池的来源筛选，都并入主面板。
	for _, must := range []string{`id="view-providers"`, `id="view-outbound"`, `id="accSource"`, `id="accBody"`} {
		if !strings.Contains(body, must) {
			t.Errorf("/panel/ must embed merged OmniGate view markup %q", must)
		}
	}
	// 域优先级卡片（realm_routing）并入模型与档位视图：默认优先级 + 逐模型优先域表。
	for _, must := range []string{`id="rrOrder"`, `id="rrPrefer"`, `id="btnRrSave"`, `id="rrBody"`, `id="rrSearch"`} {
		if !strings.Contains(body, must) {
			t.Errorf("/panel/ must embed realm-routing card markup %q", must)
		}
	}

	// 旧的 OmniGate 独立子页 URL → 302 到主面板账号池。
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/omni/", nil))
	if rec.Code != http.StatusFound {
		t.Errorf("/panel/omni/ code=%d want 302 (merged into main panel)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/panel/#accounts" {
		t.Errorf("/panel/omni/ Location=%q want /panel/#accounts", loc)
	}
}

// UID 白名单：拒绝路径穿越与异常字符，放行真实 UUID 形态。
func TestValidUID(t *testing.T) {
	ok := []string{
		"248890d9-bb26-4131-87a7-4ec74d472344",
		"abc_123-XYZ",
		"a",
	}
	bad := []string{
		"",
		"../../evil",
		"x/../../y",
		`..\..\evil`,
		"a/b",
		"a\\b",
		"uid with space",
		"uid\nnewline",
		"uid\x00null",
		"café",
		strings.Repeat("a", 65), // 超长
	}
	for _, u := range ok {
		if !validUID(u) {
			t.Errorf("validUID(%q) = false, want true", u)
		}
	}
	for _, u := range bad {
		if validUID(u) {
			t.Errorf("validUID(%q) = true, want false", u)
		}
	}
}

// 未登录的 API 请求一律 401；网关 api_key 已不能打开面板（与面板鉴权彻底解耦）。
func TestAuthLayerBehavior(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/api/overview", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: code=%d want 401", rec.Code)
	}
	// 网关 api_key（哪怕就是本进程的 api_key）不再放行面板接口。
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/panel/api/overview", nil)
	req2.Header.Set("Authorization", "Bearer test-key")
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("api_key must not open panel: code=%d want 401", rec2.Code)
	}
}
