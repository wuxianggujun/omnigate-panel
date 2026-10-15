package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSOpencodeProviderWiring 钉住 opencode 供应商在面板上的接线。
//
// 容易漏、且漏了都**静默**失效的点：
//  1. 类型下拉里必须有 opencode——没有它用户根本加不出这类供应商（代码里再多支持
//     也没用，界面上无从选择）。
//  2. 空壳判定必须认 zen_keys / go_keys：只认 api_key/api_keys 的话，配好密钥的
//     opencode 会被当成空壳直接隐藏；反过来，把 api_key 也算成有效凭证的话，
//     只填了 api_key 的 opencode 会显示「正常」，实际每次请求都报无密钥。
//     两个方向都要钉——这是同一个函数的两侧。
//  3. 表单里必须出现这两个池的输入框（否则界面上无从填写）。
//  4. 匿名免费通道的三态判定（字段缺省 = 开 / 显式 false = 关）必须与服务端
//     config.ProviderConfig.AnonymousEnabled 一致：面板说空壳、服务端其实能用
//     （或反过来）都是静默不一致，用户根本看不出问题出在哪。
//
// 静态断言（app.js 是纯文本）+ 运行时断言（node 里真跑一遍）双管齐下：静态部分保证
// 接线在，运行时部分保证判定逻辑真的对。
func TestAppJSOpencodeProviderWiring(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)

	static := map[string]string{
		"类型下拉含 opencode": `'opencode']`,
		"类型判定认 opencode": `type === 'opencode'`,
		"密钥计数函数存在":       "function provKeyCount(p) {",
		"空壳判定改用密钥计数":     "provKeyCount(p) > 0",
		"匿名三态判定函数存在":     "function provAnonEnabled(p) {",
		"空壳判定认匿名通道":      "provAnonEnabled(p)) return false",
		"徽标认匿名通道":        `provAnonEnabled(p)) return '<span class="tag ok">免费通道</span>'`,
		"表单含匿名开关":        `data-pf="anonymous"`,
	}
	for why, needle := range static {
		if !strings.Contains(js, needle) {
			t.Errorf("%s：app.js 缺少 %q", why, needle)
		}
	}
	// 徽标与空壳判定必须同源：各自算一遍 hasKey 就会打架（一个说正常、一个把它藏了）。
	if n := strings.Count(js, "const hasKey = provKeyCount(p) > 0;"); n != 2 {
		t.Errorf("provBadgeHtml 与 provIsIdle 应各自调用 provKeyCount（同源口径），实际出现 %d 次", n)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; opencode provider wiring test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function omniModelsText');
const end = src.indexOf('/* ── 网关密钥（按供应商收窄');
if (start < 0 || end < 0 || end < start) throw new Error('provider table region not found');
const els = {};
const mk = id => (els[id] = els[id] || {
  innerHTML: '', textContent: '', value: '', checked: false, hidden: false,
  classList: { _s: new Set(), add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); }, contains(c) { return this._s.has(c); } },
  querySelector: () => null,
});
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {}, ogMsg: () => {}, setTimeout: () => 0, confirm: () => true,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.provKeyCount=provKeyCount; this.provIsIdle=provIsIdle;' +
  ' this.provBadgeHtml=provBadgeHtml; this.provFormHtml=provFormHtml;' +
  ' this.provAnonEnabled=provAnonEnabled; this.PROV_TYPES=PROV_TYPES;', ctx);

const form = ctx.provFormHtml({ name: 'oc', type: 'opencode', zen_keys: ['sk-zen-aaaaaaaaaaaa'], accounts: [] });
const formOff = ctx.provFormHtml({ name: 'oc2', type: 'opencode', anonymous: false, accounts: [] });
process.stdout.write(JSON.stringify({
  typeInList: ctx.PROV_TYPES.indexOf('opencode') >= 0,
  // 有任一池的密钥就不算空壳
  idleZen: ctx.provIsIdle({ type: 'opencode', zen_keys: ['sk-zen-aaaa'] }),
  idleGo: ctx.provIsIdle({ type: 'opencode', go_keys: ['sk-go-aaaa'] }),
  // 零密钥但匿名通道默认开 = 不空壳（免费模型零密钥就能用）
  idleAnonDefault: ctx.provIsIdle({ type: 'opencode' }),
  // 显式关闭匿名又没密钥 = 空壳（该被隐藏）
  idleAnonOff: ctx.provIsIdle({ type: 'opencode', anonymous: false }),
  // 掩码串也算「有密钥」：面板读回来的是掩码，不能因此把正常供应商判成空壳
  idleMasked: ctx.provIsIdle({ type: 'opencode', go_keys: ['sk-abc…1234…(len=51)'] }),
  // opencode 不认通用 api_key：关掉匿名后只填它仍判为空壳
  idleAPIKeyAnonOff: ctx.provIsIdle({ type: 'opencode', anonymous: false, api_key: 'sk-opencode-aaaa' }),
  // 普通密钥型不受影响（回归）
  idleOpenAI: ctx.provIsIdle({ type: 'openai', api_keys: ['sk-x'] }),
  keyCountOpencode: ctx.provKeyCount({ type: 'opencode', zen_keys: ['a', ''], go_keys: ['b'] }),
  keyCountOpenai: ctx.provKeyCount({ type: 'openai', api_key: 'k', api_keys: ['x'], zen_keys: ['ignored'] }),
  badgeOk: ctx.provBadgeHtml({ type: 'opencode', zen_keys: ['sk-zen-aaaa'] }).indexOf('正常') >= 0,
  badgeAnonDefault: ctx.provBadgeHtml({ type: 'opencode' }).indexOf('免费通道') >= 0,
  badgeNoKeyAnonOff: ctx.provBadgeHtml({ type: 'opencode', anonymous: false }).indexOf('缺密钥') >= 0,
  badgeAPIKeyOnlyAnonOff: ctx.provBadgeHtml({ type: 'opencode', anonymous: false, api_key: 'sk-a' }).indexOf('缺密钥') >= 0,
  // 三态判定与服务端一致：缺省开、显式 false 关、其它类型没有这回事
  anonEnabledDefault: ctx.provAnonEnabled({ type: 'opencode' }),
  anonEnabledOff: ctx.provAnonEnabled({ type: 'opencode', anonymous: false }),
  anonEnabledOpenai: ctx.provAnonEnabled({ type: 'openai' }),
  // 表单：两个池 + tier_prefer + go_base_url + 匿名开关都要在；通用 api_key 框不该出现
  //（给了就会有人把 zen key 填进去，保存成功却永远 401）
  formHasZen: form.indexOf('data-pf="zen_keys"') >= 0,
  formHasGo: form.indexOf('data-pf="go_keys"') >= 0,
  formHasPrefer: form.indexOf('data-pf="tier_prefer"') >= 0,
  formHasGoBase: form.indexOf('data-pf="go_base_url"') >= 0,
  formHasAnon: form.indexOf('data-pf="anonymous"') >= 0,
  formAnonCheckedByDefault: form.indexOf('data-pf="anonymous" checked') >= 0,
  formAnonUncheckedWhenOff: formOff.indexOf('data-pf="anonymous" checked') < 0 && formOff.indexOf('data-pf="anonymous"') >= 0,
  formNoGenericKey: form.indexOf('data-pf="api_key"') < 0 && form.indexOf('data-pf="api_keys"') < 0,
  // 说明文案要提到免费模型（用户找的就是这个）
  formMentionsFreeModels: form.indexOf('longcat-2.5-preview-free') >= 0,
}));`
	f, err := os.CreateTemp(t.TempDir(), "opencode-ui-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("opencode provider node test failed: %v\n%s", err, out)
	}
	const want = `{"typeInList":true,"idleZen":false,"idleGo":false,"idleAnonDefault":false,"idleAnonOff":true,` +
		`"idleMasked":false,"idleAPIKeyAnonOff":true,"idleOpenAI":false,` +
		`"keyCountOpencode":2,"keyCountOpenai":2,` +
		`"badgeOk":true,"badgeAnonDefault":true,"badgeNoKeyAnonOff":true,"badgeAPIKeyOnlyAnonOff":true,` +
		`"anonEnabledDefault":true,"anonEnabledOff":false,"anonEnabledOpenai":false,` +
		`"formHasZen":true,"formHasGo":true,"formHasPrefer":true,"formHasGoBase":true,` +
		`"formHasAnon":true,"formAnonCheckedByDefault":true,"formAnonUncheckedWhenOff":true,` +
		`"formNoGenericKey":true,"formMentionsFreeModels":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("opencode provider wiring=%s\nwant %s", out, want)
	}
}
