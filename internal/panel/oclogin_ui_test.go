package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSOpencodeLoginUI 钉住 opencode 的「登录配置 / 免费模型·无需账号」页签（添加账号流）。
//
// 背景：用户三次指出 opencode 必须以「登录配置」的形态进入「账号池 → 添加账号 → 来源」，
// 而不是一句「该供应商类型不使用『账号』」把入口写死。落地方案：
//
//   - 来源 opencode → 两个页签：「登录配置」（zen / go 密钥池，掩码回显，不改 = 保持原值）
//     与「免费模型·无需账号」（匿名免费层开关，默认开启）；
//   - 保存复用 saveOmniConfig 的统一口径（校验 → 落盘 → 热重载）；密钥掩码还原由服务端
//     兜底（secret.go 的 providerSecretFields 覆盖 zen_keys / go_keys）；
//   - 防呆：页签没预填成功（配置没加载出来）时拒绝保存——空表单绝不能被当成
//     「清空密钥池」静默写上去；
//   - openai 类的兜底说明不再把 opencode 扯进来（来源下拉的说明同理）。
//
// 静态断言（app.js / index.html 文本）+ 运行时断言（node 里真跑一遍）双管齐下，
// 范式同 keyhealth_ui_test.go。
func TestAppJSOpencodeLoginUI(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)

	static := map[string]string{
		"来源映射存在":       "if (t === 'opencode') return [['oclogin', '登录配置'], ['ocanon', '免费模型·无需账号']];",
		"面板映射存在":       "oclogin: 'addTabOcLogin', ocanon: 'addTabOcAnon',",
		"供应商查找函数":      "function ocSourceProvider() {",
		"预填世代标记":       "function ocFillTag() {",
		"预填函数存在":       "function fillOcTabs() {",
		"统一保存函数":       "async function saveOcProviderPatch(msgId, patch, okText) {",
		"密钥保存函数":       "async function saveOcLoginKeys() {",
		"开关保存函数":       "async function saveOcAnon() {",
		"切页预填已接线":      "if (tab === 'oclogin' || tab === 'ocanon') fillOcTabs();",
		"消息复位已接线":      "ogMsg('ocMsg', ''); ogMsg('ocAnonMsg', '');",
		"打开弹窗预取配置":     "try { await loadOmniConfig(); } catch (e) { /* OmniGate 未启用时忽略 */ }",
		"按钮已接线":        "on('btnOcSaveKeys', saveOcLoginKeys);",
		"开关按钮已接线":      "on('btnOcSaveAnon', saveOcAnon);",
		"防呆守卫存在":       "if (ocFilledFor !== ocFillTag()) { ogMsg(msgId, '登录配置尚未加载完成：请关闭弹窗后重新打开再保存', 'err'); return; }",
		"保存成功返回 true":   "return true; // 调用方（opencode 登录配置等）据此决定要不要改写提示语",
		"配置未加载返回 false": "if (!omniCfg) { ogMsg(mid, '配置尚未加载', 'err'); return false; }",
	}
	for why, needle := range static {
		if !strings.Contains(js, needle) {
			t.Errorf("%s：app.js 缺少 %q", why, needle)
		}
	}

	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, needle := range []string{
		`id="addTabOcLogin"`, `id="addTabOcAnon"`,
		`id="ocZenKeys"`, `id="ocGoKeys"`, `id="ocAnonOn"`, `id="ocCurrent"`,
		`id="btnOcSaveKeys"`, `id="btnOcSaveAnon"`, `id="ocMsg"`, `id="ocAnonMsg"`,
		"opencode=登录配置（密钥即登录凭证）",
		"openai 类供应商不使用「账号」",
		"启用「免费模型·无需账号」（推荐，默认开启）",
	} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html 缺少 %q", needle)
		}
	}
	for _, stale := range []string{
		"该供应商类型（openai / newapi / opencode）不使用「账号」",
		"opencode <b>零密钥即可用免费模型</b>（匿名通道默认开启）",
		"免登录通道", // 2026-10 改名「免费模型·无需账号」：旧页签名不得回归（用户看不懂「免登录」的概念）
	} {
		if strings.Contains(html, stale) {
			t.Errorf("index.html 仍有旧文案（用户三次质问的落点）: %q", stale)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; opencode login UI test skipped")
	}
	// 评测区域：function addSourceType 到 async function openAdd——覆盖来源类型/页签映射
	// 与五个新增函数。omniCfg / addSource / lastOmni / omniCfgPath 在真实 app.js 里声明在
	// 区域外，尾巴里先行初始化成全局；$ / ogMsg / saveOmniConfig / loadOmniConfig 全部
	// 注入桩，S 是桩们的共享状态（消息、成功保存序列、元素袋）。
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function addSourceType(name) {');
const end = src.indexOf('async function openAdd() {');
if (start < 0 || end < 0 || end < start) throw new Error('opencode region not found');
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set, RegExp, Date,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nomniCfg = null; addSource = "__wb"; lastOmni = []; omniCfgPath = "";' +
  '\nthis.S = { msgs: {}, saved: [], saveOk: true, elems: {} };' +
  '\nthis.ADD_PANELS = ADD_PANELS;' +
  '\nthis.$ = id => this.S.elems[id] || null;' +
  "\nthis.ogMsg = (id, text, cls) => { this.S.msgs[id] = String(text || '') + '|' + (cls || ''); };" +
  '\nthis.saveOmniConfig = async msgId => { if (this.S.saveOk) this.S.saved.push(msgId || ""); return this.S.saveOk; };' +
  '\nthis.loadOmniConfig = async () => {};' +
  '\nthis.setOmniCfg = v => { omniCfg = v; };' +
  '\nthis.getOmniCfg = () => omniCfg;' +
  '\nthis.setAddSource = v => { addSource = v; };' +
  '\nthis.setLastOmni = v => { lastOmni = v; };',
  ctx);

(async () => {
  ctx.setLastOmni([
    { name: 'opencode', type: 'opencode', accounts: [] },
    { name: 'atria', type: 'openai', accounts: [] },
    { name: 'rc', type: 'raccoon', accounts: [] },
  ]);
  const tabsOc = ctx.addTabsForSource('opencode');
  const tabsOpenai = ctx.addTabsForSource('atria');
  const tabsRaccoon = ctx.addTabsForSource('rc');
  const tabsUnknown = ctx.addTabsForSource('ghost');
  const panels = ctx.ADD_PANELS.oclogin + ',' + ctx.ADD_PANELS.ocanon;

  // —— 正常链路：预填 → 保存 zen 密钥池 → 保存免费模型开关 ——
  ctx.setAddSource('opencode');
  ctx.setOmniCfg({ providers: [
    { name: 'opencode', type: 'opencode', zen_keys: ['sk-zen…abcd'], go_keys: [], anonymous: false },
    { name: 'atria', type: 'openai', api_keys: ['sk-a'] },
  ] });
  ctx.S.elems.ocZenKeys = { value: '' };
  ctx.S.elems.ocGoKeys = { value: '' };
  ctx.S.elems.ocAnonOn = { checked: true };
  ctx.S.elems.ocCurrent = { textContent: '' };
  ctx.fillOcTabs();
  const filledZen = ctx.S.elems.ocZenKeys.value;
  const filledGo = ctx.S.elems.ocGoKeys.value;
  const filledAnon = ctx.S.elems.ocAnonOn.checked;
  const curText = ctx.S.elems.ocCurrent.textContent;

  ctx.S.elems.ocZenKeys.value = 'sk-zen…abcd\nsk-new-123';
  ctx.S.elems.ocGoKeys.value = '';
  await ctx.saveOcLoginKeys();
  const provAfterKeys = JSON.parse(JSON.stringify(ctx.getOmniCfg().providers[0]));
  const keysMsg = ctx.S.msgs.ocMsg;
  const atriaKeys = ctx.getOmniCfg().providers[1].api_keys;

  ctx.S.elems.ocAnonOn.checked = true;
  await ctx.saveOcAnon();
  const provAfterAnon = JSON.parse(JSON.stringify(ctx.getOmniCfg().providers[0]));
  const anonMsg = ctx.S.msgs.ocAnonMsg;

  // —— 拒绝路径 1：来源不是 opencode，一个字段都不许动 ——
  ctx.setAddSource('rc');
  ctx.S.msgs.ocAnonMsg = '';
  await ctx.saveOcAnon();
  const rejectMsg = ctx.S.msgs.ocAnonMsg;
  const provCount = ctx.getOmniCfg().providers.length;

  // —— 拒绝路径 2：未预填（配置加载失败又恢复）时空表单不得当「清空池」提交 ——
  ctx.setAddSource('opencode');
  ctx.setOmniCfg(null);
  ctx.fillOcTabs(); // 预填失败：ocFilledFor 清空
  ctx.setOmniCfg({ providers: [
    { name: 'opencode', type: 'opencode', zen_keys: ['sk-keep-me'], go_keys: [], anonymous: true },
  ] });
  ctx.S.elems.ocZenKeys.value = '';
  ctx.S.msgs.ocMsg = '';
  await ctx.saveOcLoginKeys();
  const unfilledMsg = ctx.S.msgs.ocMsg;
  const keepKeys = ctx.getOmniCfg().providers[0].zen_keys;

  // —— 保存失败：不改写成功提示 ——
  ctx.fillOcTabs();
  ctx.S.saveOk = false;
  ctx.S.msgs.ocAnonMsg = '';
  ctx.S.elems.ocAnonOn.checked = false;
  await ctx.saveOcAnon();
  const failMsg = ctx.S.msgs.ocAnonMsg;

  process.stdout.write(JSON.stringify({
    tabsOc, tabsOpenai, tabsRaccoon, tabsUnknown, panels,
    filledZen, filledGo, filledAnon, curText,
    keysMsg, zen: provAfterKeys.zen_keys, go: provAfterKeys.go_keys, atriaKeys,
    anonMsg, anon: provAfterAnon.anonymous,
    rejectMsg, provCount,
    unfilledMsg, keepKeys,
    saved: ctx.S.saved.join(','),
    failMsg,
  }));
})().catch(e => { console.error('runtime:', (e && e.stack) || e); process.exit(1); });`
	f, err := os.CreateTemp(t.TempDir(), "oclogin-ui-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("opencode login UI node test failed: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	for why, want := range map[string]string{
		"opencode 得到两个页签":  `"tabsOc":[["oclogin","登录配置"],["ocanon","免费模型·无需账号"]]`,
		"openai 仍走说明页":    `"tabsOpenai":[["ognone","说明"]]`,
		"raccoon 页签不受影响":  `"tabsRaccoon":[["ogauth","浏览器登录"],["ogtoken","粘贴 Token"]]`,
		"未知来源兜底不变":      `"tabsUnknown":[["ognone","说明"]]`,
		"面板映射正确":        `"panels":"addTabOcLogin,addTabOcAnon"`,
		"掩码密钥原样预填":      `"filledZen":"sk-zen…abcd"`,
		"go 池空预填":       `"filledGo":""`,
		"匿名关（显式 false）":  `"filledAnon":false`,
		"状态行含 zen 计数":    `zen 1 把密钥`,
		"密钥保存提示改写":      `"keysMsg":"登录配置已保存并热生效|ok"`,
		"zen 追加新密钥":      `"zen":["sk-zen…abcd","sk-new-123"]`,
		"go 清空提交":        `"go":[]`,
		"其它供应商未被波及":     `"atriaKeys":["sk-a"]`,
		"开关保存提示改写":      `"anonMsg":"「免费模型·无需账号」已开启并热生效|ok"`,
		"开关写回 true":      `"anon":true`,
		"非 opencode 来源拒绝": `"rejectMsg":"当前来源不是 opencode 供应商|err"`,
		"供应商列表未被增删":     `"provCount":2`,
		"未预填拒绝并给指引":     `"unfilledMsg":"登录配置尚未加载完成：请关闭弹窗后重新打开再保存|err"`,
		"已有密钥未被清空":      `"keepKeys":["sk-keep-me"]`,
		"成功保存恰好两次":      `"saved":"ocMsg,ocAnonMsg"`,
		"失败不写成功提示":      `"failMsg":""`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s：node 输出缺少 %q\n完整输出: %s", why, want, got)
		}
	}
}
