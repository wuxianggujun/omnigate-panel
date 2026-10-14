// statfab_mobile_test.go 锁两件事：
//  1) 账号池概览卡：从页内顶部统计条挪到右下角悬浮卡片后，7 项统计的 id 一个
//     都不能少（setStat/markStat 全按 id 找，少一个就静默不刷新），且不能再
//     留在 #view-accounts 里（否则等于没挪）。
//  2) 手机版适配：三档断点必须存在，且都只覆盖 max-width（桌面端一行声明不动）。
//     抽屉相关元素（汉堡按钮 / 遮罩）与 JS 开关函数要一一对应。
package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 任务 A：账号池概览右下角化。
func TestAccountStatsMovedToFloatingCard(t *testing.T) {
	h, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	H := string(h)

	// 7 项统计的 id 必须原样保留（loadOverview 里 setStat/markStat 按 id 找）。
	for _, id := range []string{
		"sTotal", "sHealthy", "sCooling", "sDisabled",
		"sPaused", "sCredits", "sSticky",
	} {
		if n := strings.Count(H, `id="`+id+`"`); n != 1 {
			t.Errorf("id=%s 出现 %d 次，want 1（挪位置不能丢统计项）", id, n)
		}
	}
	// 悬浮卡片三件套。
	for _, id := range []string{"statFab", "statFabTab", "statFabBody"} {
		if !strings.Contains(H, `id="`+id+`"`) {
			t.Errorf("index.html 缺少 #%s", id)
		}
	}
	// 关键：.stats 必须离开 #view-accounts。还在里面 = 改造没生效。
	acc := H[strings.Index(H, `id="view-accounts"`):]
	acc = acc[:strings.Index(acc, `id="view-taskscenter"`)]
	if strings.Contains(acc, `<div class="stats">`) {
		t.Error("统计条仍在 #view-accounts 内：应挪到右下角悬浮卡片")
	}
	// 卡片里仍是一个 .stats 容器（复用原网格，避免引入第二套统计样式）。
	if strings.Count(H, `<div class="stats">`) != 1 {
		t.Errorf(".stats 容器 %d 个，want 1", strings.Count(H, `<div class="stats">`))
	}
	// 角标数字：收起态也要看得见关键三个数。
	for _, id := range []string{"fabTotal", "fabAvail", "fabCooling", "fabPulse"} {
		if !strings.Contains(H, `id="`+id+`"`) {
			t.Errorf("悬浮卡角标缺少 #%s", id)
		}
	}
}

// 任务 A 的 CSS：悬浮卡必须 fixed 定位，且复用 .stats 网格（不另立一套）。
func TestStatFabCSS(t *testing.T) {
	css, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	C := string(css)
	if !strings.Contains(C, ".statfab {") {
		t.Error("theme.css 缺少 .statfab 规则")
	}
	// fixed 定位：这是"悬浮"的实质。
	i := strings.Index(C, ".statfab {")
	seg := C[i:]
	if j := strings.Index(seg, "\n  }"); j > 0 {
		seg = seg[:j]
	}
	if !strings.Contains(seg, "position: fixed") {
		t.Errorf(".statfab 未 fixed 定位：\n%s", seg)
	}
	if !strings.Contains(C, ".statfab-body .stats {") {
		t.Error("theme.css 缺少 .statfab-body .stats（应复用原统计网格）")
	}
	// #btnNav 桌面端隐藏、900px 内显示。
	if !strings.Contains(C, "#btnNav { display: none; }") {
		t.Error("#btnNav 未在桌面端隐藏")
	}
	if !strings.Contains(C, "#btnNav { display: inline-flex !important; }") {
		t.Error("#btnNav 未在 ≤900px 显示")
	}

	// ── 四个集成问题（都是"加了个固定元素，却忘了它会和谁打架"）──────────
	// ① 展开面板往上长：.statfab 是 flex column，默认 DOM 顺序会让面板掉到
	//    tab 下面（贴屏幕底边看不见）。column-reverse 让 tab 贴底、面板向上。
	if !strings.Contains(C, "flex-direction: column-reverse") {
		t.Error(".statfab 未用 column-reverse：展开面板会掉到 tab 下方、贴屏幕底边")
	}
	// ② 展开面板限高：7 项在手机上排 2 列 4 行近 300px，不封顶会占掉半屏。
	if !strings.Contains(C, "max-height: calc(100dvh - 132px)") {
		t.Error(".statfab-body 未限高：手机上展开会占掉半屏")
	}
	if !strings.Contains(C, "overscroll-behavior: contain") {
		t.Error(".statfab-body 未做 overscroll 收边：滚到底会把页面也带着滚")
	}
	// ③ toast 让位：#toasts 原来贴 bottom:20px，会和卡片叠在一起挡住它。
	if !regexp.MustCompile(`#toasts \{[^}]*bottom: 64px`).MatchString(C) {
		t.Error("#toasts 仍贴 bottom:20px：会与右下角悬浮卡重叠")
	}
	// ④ 有 sticky .savebar 的页面要抬高卡片，否则压住「保存」主按钮。
	//    关键是让位量必须走 CSS 变量 --fab-lift：写死偏移必错（B 方案按"停在自然
	//    位置时的高度"给 92px，复测仍被压住 —— 内容不满一屏时 sticky 保存条停在
	//    屏幕中间而不是底边）。变量由 app.js 的 syncFabLift() 实测写入。
	if !strings.Contains(C, `body[data-view="config"] .statfab,`) ||
		!strings.Contains(C, `body[data-view="providers"] .statfab {`) ||
		!strings.Contains(C, `bottom: calc(16px + var(--fab-lift))`) {
		t.Error("配置/供应商页未按 --fab-lift 变量让位：悬浮卡会盖住保存按钮")
	}
	if !strings.Contains(C, `:root { --fab-lift: 0px; }`) {
		t.Error("theme.css 缺少 --fab-lift 初始值")
	}
	// 写死的让位量不能回来：那种 .statfab.open { bottom: 92px } 一律拒绝。
	// 注意不能无脑匹配"任何 .statfab 带 bottom: Npx"—— ≤400px 断点里的
	// .statfab { bottom: 8px } 是窄机收边距，合法。只查让位规则那段。
	if regexp.MustCompile(`\.statfab[^{}]*(?:\.open[^{}]*)?\{[^}]*bottom: \d+px`).MatchString(
		regexp.MustCompile(`(?s)配置页 / 供应商页底部.*?:root`).FindString(C)) {
		t.Error("让位量又被写死了：必须走 var(--fab-lift)，否则某状态下会压住保存按钮")
	}
}

// 任务 A 的 JS：initStatFab / setStatFabOpen / syncFab 三件套 + loadOverview 内
// 必须真的调用 syncFab（否则卡片永远显示 "-"）。
func TestStatFabJS(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	J := string(js)
	for _, fn := range []string{"function initStatFab()", "function setStatFabOpen(", "function syncFab("} {
		if !strings.Contains(J, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	if !strings.Contains(J, "wb2api.statfab") {
		t.Error("app.js 未持久化展开状态（wb2api.statfab）")
	}
	// .open 是让 CSS/调试时能看出展开态的类，保留。
	if !strings.Contains(J, "classList.toggle('open', statFabOpen)") {
		t.Error("setStatFabOpen 未切换 .open")
	}
	// syncFabLift：让位量由 JS 实测写入 --fab-lift（写死必错）。
	// 四个挂载点缺一不可：切页、展开/收起、resize、scroll。
	if !strings.Contains(J, "function syncFabLift()") {
		t.Fatal("app.js 缺少 syncFabLift：让位量没人算")
	}
	if !strings.Contains(J, `--fab-lift: `) {
		t.Error("syncFabLift 未写入 CSS 变量 --fab-lift")
	}
	if !strings.Contains(J, "addEventListener('scroll', () => syncFabLift()") {
		t.Error("未挂 scroll：滚动时 sticky 保存条吸底/回原位，让位量会失效")
	}
	if !strings.Contains(J, "window.innerWidth > 900 && navDrawerOpen()") {
		t.Error("resize 挂钩丢了")
	}
	// loadOverview 内必须调用 syncFab —— 只在函数外定义等于没接线。
	f := strings.Index(J, "async function loadOverview")
	if f < 0 {
		t.Fatal("app.js 缺少 loadOverview")
	}
	// Go 的 strings.Index 没有 fromIndex 参数，要往后找得先切片。
	e := strings.Index(J[f+10:], "\nasync function ")
	end := len(J)
	if e >= 0 {
		end = f + 10 + e
	}
	if !strings.Contains(J[f:end], "syncFab(") {
		t.Error("loadOverview 未调用 syncFab：悬浮卡数字永远是 -")
	}
	// bootstrapAuth 里初始化。
	b := strings.Index(J, "async function bootstrapAuth")
	if b < 0 || !strings.Contains(J[b:], "initStatFab();") {
		t.Error("bootstrapAuth 未调用 initStatFab")
	}
}

// 手机版适配：三档断点齐备，且只覆盖 max-width（不碰桌面端）。
func TestMobileBreakpoints(t *testing.T) {
	css, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	C := string(css)
	for _, bp := range []string{
		"@media (max-width: 900px)",
		"@media (max-width: 560px)",
		"@media (max-width: 400px)",
	} {
		if !strings.Contains(C, bp) {
			t.Errorf("theme.css 缺少断点 %s", bp)
		}
	}
	// 抽屉：侧导航 fixed + transform 滑出，遮罩独立元素。
	for _, must := range []string{
		"position: fixed; left: 0; top: 0; bottom: 0; width: 248px",
		"transform: translateX(-102%)",
		"body.nav-open .nav { transform: none; }",
		".navscrim {",
	} {
		if !strings.Contains(C, must) {
			t.Errorf("theme.css 缺少抽屉样式 %q", must)
		}
	}
	// 固定宽度弹窗在手机上必须让位（内联 width 只能靠 !important 压过）。
	if !strings.Contains(C, ".dlg, .veil .dlg { width: auto !important; max-width: 100%; }") {
		t.Error("固定宽度弹窗未在手机上改为自适应")
	}
	// 桌面端 .shell 两列布局必须原样保留（这是"电脑没问题"的前提）。
	if !strings.Contains(C, ".shell { display: grid; grid-template-columns: 208px 1fr;") {
		t.Error("桌面端 .shell 两列布局被改动")
	}
}

// 抽屉三件套：HTML 元素与 JS 函数对应，Esc / 遮罩 / 选中项三条关闭路径都在。
func TestNavDrawerWiring(t *testing.T) {
	H, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	J, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	h, j := string(H), string(J)
	if !strings.Contains(h, `id="btnNav"`) {
		t.Error("index.html 缺少汉堡按钮 #btnNav")
	}
	if !strings.Contains(h, `id="navScrim"`) {
		t.Error("index.html 缺少抽屉遮罩 #navScrim")
	}
	for _, fn := range []string{"function setNavDrawer(", "function navDrawerOpen()", "function initNavDrawer("} {
		if !strings.Contains(j, fn) {
			t.Errorf("app.js 缺少 %s", fn)
		}
	}
	// 三条关闭路径：遮罩点击、Esc、选中任一项。
	// 注意 strings.Index 只有两个参数（没有 JS 那种 fromIndex），要截函数体
	// 得先切片再 Index。
	d := j
	if k := strings.Index(j, "function initNavDrawer("); k >= 0 {
		d = j[k:]
		if e := strings.Index(d, "\n  }"); e > 0 {
			d = d[:e]
		}
	} else {
		t.Error("app.js 缺少 initNavDrawer 定义")
	}
	if !strings.Contains(d, "sc.addEventListener('click'") {
		t.Error("抽屉未绑遮罩点击关闭")
	}
	if !strings.Contains(d, "e.key === 'Escape'") {
		t.Error("抽屉未绑 Esc 关闭")
	}
	if !strings.Contains(d, "querySelectorAll('.nav a')") {
		t.Error("抽屉未在选中导航项后自动关闭")
	}
	// 拉宽到桌面尺寸要自动收起，否则遮罩残留盖住内容。
	if !strings.Contains(d, "window.innerWidth > 900") {
		t.Error("抽屉未在拉宽到桌面尺寸时自动收起")
	}
}

// 手机版 CSS 不能被 PowerShell 之类的工具写坏（中文注释全变 ? 的那种）。
func TestMobileTailorNoMojibake(t *testing.T) {
	for _, f := range []string{"theme.css", "index.html", "app.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "\uFFFD") {
			t.Errorf("%s 含 U+FFFD 替换字符（UTF-8 被写坏）", f)
		}
	}
	// 段首注释要在（说明这块为什么存在，而不是一堆魔法数字）。
	css, _ := os.ReadFile("theme.css")
	if !regexp.MustCompile(`手机版适配`).Match(css) {
		t.Error("theme.css 缺少手机版适配段说明注释")
	}
}

// theme.css 本身必须是结构健全的 CSS：花括号配平、注释闭合、没有游离的注释尾行。
// 这三条是 s/task_a_css_fix.cjs 留下 `──── */` 游离尾行那类事故的唯一防线 ——
// JS 拼注释时首行与尾行动态拼接，很容易把尾巴单独留在下一行。CSS 注释不闭合的
// 后果是后面全部规则被吞掉，而 go:embed 照样编译通过，只在浏览器里整页没样式。
func TestThemeCSSStructuralSanity(t *testing.T) {
	b, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	C := string(b)
	// 统一换行：两侧都要查（文件是 CRLF，但脚本改写时可能混进 LF）。
	flat := strings.ReplaceAll(C, "\r\n", "\n")

	// 花括号必须一样多。
	if o, c := strings.Count(flat, "{"), strings.Count(flat, "}"); o != c {
		t.Errorf("theme.css 花括号不配平：%d 个 { vs %d 个 }", o, c)
	}
	// 注释必须闭合（逐字符状态机；不能按行数 /* 与 */，也不能全文交替找 ——
	// 注释里出现的 /panel/api/* 会被误判成闭合）。
	inC := false
	opens, closes := 0, 0
	for i := 0; i < len(flat); i++ {
		if inC {
			if i+1 < len(flat) && flat[i] == '*' && flat[i+1] == '/' {
				inC, closes = false, closes+1
				i++
			}
			continue
		}
		if flat[i] == '/' && i+1 < len(flat) && flat[i+1] == '*' {
			inC, opens = true, opens+1
			i++
		}
	}
	if inC {
		t.Error("theme.css 有注释没闭合：其后的全部规则都会被浏览器吞掉")
	}
	if opens != closes {
		t.Errorf("theme.css 注释 %d 开 / %d 闭", opens, closes)
	}
	// 游离的注释尾行（某行单独是 ──── */ 或 */，前后都没有对应的注释体）。
	// 正常 CSS 有两种合法的「单 */ 行」：
	//   ① 多行注释块的收尾（上一行是注释正文，下一行是规则）；
	//   ② 视觉分隔线的右半（行首有 ──，与左半成对）。
	// 真正的事故形态是：注释块的收尾行被脚本多复制了一份，表现为紧跟在
	// 一条 */ 后面又出现一条只含 ──── */ 的行 —— 判定「连续两条闭注释行」即可。
	lines := strings.Split(flat, "\n")
	for n := 1; n < len(lines); n++ {
		cur, prev := lines[n], lines[n-1]
		isClose := func(l string) bool {
			s := strings.TrimSpace(l)
			return s == "*/" || regexp.MustCompile(`^[\s──━═]*\*/$`).MatchString(s)
		}
		if isClose(cur) && isClose(prev) {
			// 相邻两条都是「闭注释行」= 上面那条的开注释块必定已经被提前闭合过，
			// 多出来的这条就是游离尾行。
			t.Errorf("theme.css 第 %d/%d 行连续两条注释尾行：%q / %q（多出的那条是游离的，会让后面的规则全被吞掉）", n, n+1, prev, cur)
		}
	}
	// index.html 的 HTML 标签也要配平（抽离统计条时最容易扯坏 div）。
	hb, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	H := strings.ReplaceAll(string(hb), "\r\n", "\n")
	for _, tag := range []string{"div", "section", "table", "thead", "tbody", "form", "header", "footer", "select", "nav"} {
		o := regexp.MustCompile(`<` + tag + `[\s>]`).FindAllStringIndex(H, -1)
		c := strings.Count(H, "</"+tag+">")
		if len(o) != c {
			t.Errorf("index.html <%s> %d 开 / %d 闭", tag, len(o), c)
		}
	}
}
