package panel

import (
	"os"
	"strings"
	"testing"
)

// TestAppJSHideIdleProviders 钉住「没账号 / 没密钥的供应商默认折叠」。
//
// 用户原话：「当前上游供应商，我都没有添加账号的供应商为什么要显示，这不是有问题吗？」
// 线上 7 家里 5 家 accounts 为空（raccoon / runable / trae / justwoker / deepseek），
// 其中 justwoker / deepseek 连 api_key 都没有；真正能服务的只有 atria 与 workbuddy。
//
// 三条硬要求：
//  1. 判定口径必须与 provBadgeHtml 同源——徽标标「缺 api_key / 无账号」的，正是要
//     隐藏的那些。两套口径打架就会出现「徽章说正常却被藏了」这种自相矛盾。
//  2. 是折叠不是删除：说明行要点名是哪几家、给「展开全部 N 家」按钮，
//     偏好落 localStorage。用户随时能看全，也不会误以为配置丢了。
//  3. 索引必须回填成 omniCfg.providers 的原始下标。过滤后直接用新 i 会错位——
//     openProvEdit / delProvFromDialog 都按原始下标取值，错位就改错供应商。
func TestAppJSHideIdleProviders(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, needle := range []string{
		"function provIsIdle(p) {",                 // 判定函数存在
		"provTypeHasKey(type) && !hasKey",          // 与徽标同源：key 型缺 key
		"provTypeHasAccounts(type) && !(p.accounts || []).length", // 账号型无账号
		"let provShowIdle = false;",                // 默认折叠
		"localStorage.getItem('wb2api.provShowIdle')",  // 偏好被读取
		"const idleList = provs.filter(provIsIdle);",   // 空壳清单
		"const shown = provShowIdle ? provs : provs.filter(p => !provIsIdle(p));", // 过滤
		`data-act="showIdle"`,                     // 展开按钮
		"localStorage.setItem('wb2api.provShowIdle'",   // 展开偏好被记住
		"provRowHtml(p, provs.indexOf(p))",        // 索引用原始下标，不错位
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少空壳折叠接线: %q", needle)
		}
	}

	// 点「展开」的分支必须真的存在，否则按钮点了没反应
	if !strings.Contains(js, "if (act === 'showIdle') {") {
		t.Error("app.js onProvClick 没有 showIdle 分支（按钮会成为死按钮）")
	}

	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	// 说明行必须是独立 tbody：与数据行分开，才不会在 colspan 上打架
	if !strings.Contains(string(htmlBytes), `id="provIdleRow"`) {
		t.Error("index.html 供应商表缺少 provIdleRow 说明行容器")
	}

	// 空壳折叠不能影响路由配置载体：默认供应商下拉与 prefer 仍读全部 providers
	for _, needle := range []string{
		"sel.innerHTML = provs.map(p => '<option value=\"' + esc(p.name || ''))",
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 默认供应商下拉应仍列全部供应商（折叠只动表格）: %q", needle)
		}
	}
}

// TestAppJSProvFormUnifiedTheme 钉住供应商弹窗的表单结构与全站一致。
//
// 用户原话：「关于你上游供应商里面的添加和修改对话框主题不统一啊」。
// 全站表单是 label.fld + <span class="lb"> + .grid2/.grid3，供应商弹窗原来还在用
// 老的 .og-fields（220px 窄列、12px 标签、4px 间距），两套结构并存。
//
// 这里钉三件事：
//  1. provFormHtml 里不得再出现 og-fields；
//  2. 必须用 label.fld + .lb，长字段占整行（.fld.wide）；
//  3. data-pf 字段名一个都不能少——onProvFormInput / commitProvEdit 按它取值，
//     改名会静默丢字段（读不出来、也写不回盘），这是最容易犯的错。
func TestAppJSProvFormUnifiedTheme(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	start := strings.Index(js, "function provFormHtml(d) {")
	end := strings.Index(js, "function openProvEdit(i) {")
	if start < 0 || end < 0 || end <= start {
		t.Fatalf("找不到 provFormHtml：start=%d end=%d", start, end)
	}
	fn := js[start:end]

	if strings.Contains(fn, "og-fields") {
		t.Error("provFormHtml 仍在用老的 .og-fields（与全站表单不统一）")
	}
	if !strings.Contains(fn, `class="fld"`) || !strings.Contains(fn, `<span class="lb">`) {
		t.Error("provFormHtml 未换成 label.fld + .lb 口径")
	}
	if !strings.Contains(fn, `class="fld wide"`) {
		t.Error("provFormHtml 长字段未占整行（.fld.wide）")
	}
	// 短字段应并排，而不是每个都独占一行
	if !strings.Contains(fn, `<div class="grid2">`) {
		t.Error("provFormHtml 的名称/类型未并排（.grid2）")
	}

	// data-pf 全清单：少一个就是一个静默丢字段的坑
	want := []string{
		"name", "type", "base_url", "api_key", "api_keys", "models", "system_prompt",
		"main_origin", "llm_base", "auth_base", "agent_host", "ug_host", "oauth_host",
		"device_header",
	}
	for _, f := range want {
		if !strings.Contains(js, `data-pf="`+f+`"`) {
			t.Errorf("app.js 丢了 data-pf=%q（弹窗该字段将读不出、写不回）", f)
		}
	}

	cssBytes, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(cssBytes)
	if !strings.Contains(css, "label.fld.wide") {
		t.Error("theme.css 缺少 .fld.wide（长字段整行）")
	}
	// 栅格里末位字段的下边距要抹掉，否则与 grid gap 叠成一条缝
	if !strings.Contains(css, ".grid2 > label.fld:last-child") {
		t.Error("theme.css 未处理 .grid2 末位字段的下边距")
	}
}
