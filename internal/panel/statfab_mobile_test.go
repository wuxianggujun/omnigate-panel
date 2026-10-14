// statfab_mobile_test.go 锁两件事：
//  1) 账号池概览：7 项统计条归位在 #view-accounts 顶部，id 一个都不能少
//     （setStat/markStat 全按 id 找，少一个就静默不刷新）；右下角那套悬浮卡
//     整套已删除（HTML / CSS / JS 都不许留残留，否则就是死代码）。
//  2) 手机版适配：三档断点必须存在，且都只覆盖 max-width（桌面端一行声明不动）。
//     抽屉相关元素（汉堡按钮 / 遮罩）与 JS 开关函数要一一对应。
package panel

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 账号池概览：统计条放回 #view-accounts 顶部，且悬浮卡整套不留残余。
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
	// 悬浮卡片整套必须删干净（收回账号池页），一个 id 都不能留。
	for _, id := range []string{"statFab", "statFabTab", "statFabBody", "fabPulse", "fabTotal", "fabAvail", "fabCooling"} {
		if strings.Contains(H, `id="`+id+`"`) {
			t.Errorf("index.html 仍残留悬浮卡 #%s：已收回账号池页，请删干净", id)
		}
	}
	// 关键：.stats 必须回到 #view-accounts 里。
	acc := H[strings.Index(H, `id="view-accounts"`):]
	acc = acc[:strings.Index(acc, `id="view-taskscenter"`)]
	if !strings.Contains(acc, `<div class="stats">`) {
		t.Error("统计条不在 #view-accounts 内：应放回账号池页顶部")
	}
	// .stats 容器只此一个（不引入第二套统计样式）。
	if n := strings.Count(H, `<div class="stats">`); n != 1 {
		t.Errorf(".stats 容器 %d 个，want 1", n)
	}
	// 7 个统计项必须都在那一个 .stats 里（不能散到别处）。
	i := strings.Index(H, `<div class="stats">`)
	block := H[i:]
	if j := strings.Index(block, `</div>`); j > 0 {
		// 逐层找到 .stats 的收尾：最后一个统计项的 </div></div> 之后。
		block = H[i : i+len(block)]
	}
	for _, id := range []string{"sTotal", "sHealthy", "sCooling", "sDisabled", "sPaused", "sCredits", "sSticky"} {
		if !strings.Contains(H[i:], `id="`+id+`"`) {
			t.Errorf("统计项 #%s 不在页内", id)
		}
	}
}

// 统计条放回页内后的 CSS：不得留任何悬浮卡规则，#toasts 回到右下角贴底。
func TestStatFabCSS(t *testing.T) {
	css, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	C := string(css)
	// 悬浮卡整套必须删干净（收回账号池页），一条规则都不能留。
	for _, must := range []string{".statfab", "statfab-body", "statfab-tab"} {
		if strings.Contains(C, must) {
			t.Errorf("theme.css 仍残留悬浮卡规则 %q：已收回账号池页，请删干净", must)
		}
	}
	// --fab-lift 机制只服务悬浮卡，一并删除。
	if strings.Contains(C, "--fab-lift") {
		t.Error("theme.css 仍残留 --fab-lift：服务它的悬浮卡已删")
	}
	// #toasts 回到右下角贴底（悬浮卡没了，不必再抬）。
	if !strings.Contains(C, "#toasts { position: fixed; right: 20px; bottom: 20px;") {
		t.Error("#toasts 未回到右下角贴底")
	}
	// #btnNav 桌面端隐藏、900px 内显示。
	if !strings.Contains(C, "#btnNav { display: none; }") {
		t.Error("#btnNav 未在桌面端隐藏")
	}
	if !strings.Contains(C, "#btnNav { display: inline-flex !important; }") {
		t.Error("#btnNav 未在 ≤900px 显示")
	}
}

// 统计条放回页内后的 JS：悬浮卡三件套与让位机制必须全部删除。
func TestStatFabJS(t *testing.T) {
	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	J := string(js)
	// 悬浮卡相关函数与调用全部删除（收回账号池页）。
	for _, fn := range []string{
		"function initStatFab(", "function setStatFabOpen(", "function syncFab(",
		"function syncFabLift(", "initStatFab();", "STATFAB_KEY", "wb2api.statfab",
		"statFabOpen", "classList.toggle('open', statFabOpen)",
	} {
		if strings.Contains(J, fn) {
			t.Errorf("app.js 仍残留悬浮卡代码 %q：已收回账号池页，请删干净", fn)
		}
	}
	// --fab-lift 的 JS 侧写入也必须删除。
	if strings.Contains(J, "--fab-lift") {
		t.Error("app.js 仍写入 --fab-lift：服务它的悬浮卡已删")
	}
	// resize 挂钩本身要留着（抽屉要靠它自动收起），只是不能再带 syncFabLift。
	if !strings.Contains(J, "window.innerWidth > 900 && navDrawerOpen()") {
		t.Error("resize 挂钩丢了")
	}
	// loadOverview 必须仍在（统计条的数据全从这里走），且不能再调 syncFab。
	f := strings.Index(J, "async function loadOverview")
	if f < 0 {
		t.Fatal("app.js 缺少 loadOverview")
	}
	e := strings.Index(J[f+10:], "\nasync function ")
	end := len(J)
	if e >= 0 {
		end = f + 10 + e
	}
	if strings.Contains(J[f:end], "syncFab(") {
		t.Error("loadOverview 仍调用 syncFab：悬浮卡已删")
	}
	if !strings.Contains(J[f:end], "setStat('sTotal'") {
		t.Error("loadOverview 未写统计条：账号池页统计条会是 -")
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

// 左下角登录账号卡片（#meCard）。
// 原来侧边栏底部只有一行 .sig，登录账号名只出现在顶栏 #whoami —— 手机端顶栏一折行
// 就被挤出首屏，于是把账号信息放到侧边栏底部那块常驻空位上。
// 锁的几处：结构 id 齐全、访客态整卡隐藏、与顶栏同源（authState）、
// 退出复用 doLogout（不复制一份登出逻辑）、改密码按 data-admin 门禁
// （目标表单 paccBox 本身带 data-admin，按钮不门禁就会点进空分组）。
func TestLeftBottomMeCard(t *testing.T) {
	h, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	H := string(h)
	// ① 结构：六个 id 一个不能少（renderMeCard 按 id 填内容）。
	for _, id := range []string{"meCard", "meName", "meRole", "meAva", "btnMePw", "btnMeOut"} {
		if !strings.Contains(H, "id=\""+id+"\"") {
			t.Errorf("登录账号卡片缺少 #%s", id)
		}
	}
	// ② 位置：必须在 .sig 之前、</nav> 之内 —— 放到别处就离开"左下角"了。
	meIdx, sigIdx, navEnd := strings.Index(H, "id=\"meCard\""), strings.Index(H, "class=\"sig\""), strings.Index(H, "</nav>")
	if meIdx < 0 || sigIdx < 0 || meIdx > sigIdx {
		t.Error("#meCard 未放在 .sig 之前：顺序反了，视觉上不会贴在侧边栏底部")
	}
	if meIdx < 0 || navEnd < 0 || meIdx > navEnd {
		t.Error("#meCard 跑到了 </nav> 外面")
	}
	// ③ 改密码按钮必须带 data-admin：目标 paccBox 带 data-admin，
	//    非 admin 时整组 display:none，按钮不门禁会点进一个空白分组。
	re := regexp.MustCompile(`(?s)<button[^>]*id="btnMePw"[^>]*>`)
	m := re.FindString(H)
	if m == "" {
		t.Fatal("找不到 btnMePw 按钮")
	}
	if !strings.Contains(m, "data-admin") {
		t.Error("btnMePw 缺 data-admin：viewer/user 点进去会落到被隐藏的空分组")
	}

	js, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	J := string(js)
	// ④ 渲染函数 + 从 applyRole 调用（只在函数里定义等于没接线）。
	if !strings.Contains(J, "function renderMeCard(") {
		t.Fatal("app.js 缺少 renderMeCard")
	}
	if f := strings.Index(J, "function applyRole()"); f < 0 {
		t.Fatal("app.js 缺少 applyRole")
	} else if !strings.Contains(J[f:f+1600], "renderMeCard(") {
		t.Error("applyRole 未调用 renderMeCard：卡片永远不会被填上账号")
	}
	// ⑤ 访客态整卡隐藏 —— 没账号可展示时露出一张空卡比不显示更糟。
	if !strings.Contains(J, "card.hidden = !(authed && name)") {
		t.Error("renderMeCard 未按登录态隐藏卡片")
	}
	// ⑥ 登出必须复用 doLogout：顶栏和卡片各写一份，将来只会改一处。
	if !strings.Contains(J, "async function doLogout()") {
		t.Error("登出逻辑未抽成 doLogout：顶栏与卡片会各有一份")
	}
	if !strings.Contains(J, "$('btnMeOut').onclick = doLogout") ||
		!strings.Contains(J, "$('btnLogout').onclick = doLogout") {
		t.Error("顶栏与卡片的退出按钮未统一走 doLogout")
	}
	// ⑦ 改密码跳到配置页 panel 组（那里有改自己密码的表单）。
	if !strings.Contains(J, "showCfgSec('panel')") {
		t.Error("改密码未跳到配置页「面板账号」组")
	}

	css, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	C := string(css)
	// ⑧ 账号名过长要省略：.nav 宽 208px 固定，不省略会把侧边栏撑开。
	if !strings.Contains(C, ".nav .me-name") || !strings.Contains(C, "text-overflow: ellipsis") {
		t.Error(".nav .me-name 未做省略：长账号名会把侧边栏撑宽")
	}
	if !strings.Contains(C, ".nav .me[hidden]") {
		t.Error(".nav .me 未处理 [hidden]：hidden 属性对 display:flex 的块不生效")
	}
}
