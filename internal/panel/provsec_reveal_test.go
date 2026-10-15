package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSProviderSecretReveal 钉住「管理员查看完整密钥」的前端接线。
//
// 用户要求：编辑上游供应商时，管理员要能看回完整的 key（掩码的前 6 后 4 不够用）。
// 服务端新开出独立端点 GET /panel/api/omni/provider-secret（admin-only、no-store、
// 审计留痕，见 provider_secret_test.go），前端这边的接线漏了都是静默的：
//
//  1. 密钥字段标签必须过 provKeyLabel——直接写死 <span class="lb"> 就没有按钮入口；
//  2. 按钮只对管理员渲染（viewer / 未登录不出现）+ 新建供应商、空字段不出现
//     （没落盘 / 没东西可取回，给了按钮就是 404 体验）；
//  3. 点击代理必须接上（按钮在 <label> 里，还要 preventDefault 防止焦点被 label 转走）；
//  4. 「查看 → 明文填充 / 隐藏 → 掩码快照还原」往返必须成立，且一次编辑会话只拉一次。
//
// 静态断言（app.js 文本）+ 运行时断言（node 里真跑一遍）双管齐下。
func TestAppJSProviderSecretReveal(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)

	static := map[string]string{
		"标签助手存在":       "function provKeyLabel(text, field, hasValue) {",
		"非管理员/新建不给按钮":  "if (!isAdmin() || !provEdit || provEdit.isNew || !hasValue) {",
		"按钮带字段名":       `data-reveal="' + field + '"`,
		"查看函数存在":       "function provRevealKeys(field) {",
		"点击代理存在":       "function onProvFormClick(ev) {",
		"点击代理已接线":      "pf.addEventListener('click', onProvFormClick);",
		"端点相对路径":       "omni/provider-secret?name=",
		"api_key 走助手":  `provKeyLabel('api_key', 'api_key', !!d.api_key)`,
		"api_keys 走助手": `provKeyLabel('api_keys', 'api_keys', Array.isArray(d.api_keys) && d.api_keys.length > 0)`,
		"zen_keys 走助手": `provKeyLabel('zen_keys', 'zen_keys', Array.isArray(d.zen_keys) && d.zen_keys.length > 0)`,
		"go_keys 走助手":  `provKeyLabel('go_keys', 'go_keys', Array.isArray(d.go_keys) && d.go_keys.length > 0)`,
		"开窗快照":         ", revealed: {}, masked: {} };",
		"隐藏按快照还原":      "d[field] = JSON.parse(JSON.stringify(provEdit.masked[field]));",
	}
	for why, needle := range static {
		if !strings.Contains(js, needle) {
			t.Errorf("%s：app.js 缺少 %q", why, needle)
		}
	}
	// 四个密钥字段恰好各用一次助手（增删字段时计数会跟着动，防止漏接 / 多接）。
	if n := strings.Count(js, "provKeyLabel('"); n != 4 {
		t.Errorf("provKeyLabel 调用应为 4 处（api_key/api_keys/zen_keys/go_keys），实际 %d 处", n)
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; provider secret reveal test skipped")
	}
	// 注意：app.js 的 `let provEdit` 声明在评测区域内，会遮蔽宿主的任何全局注入，
	// 所以要拿脚本里的真对象只能靠评测尾巴上闭包出来的 setProvEdit（与 app.js 的
	// 开窗路径同源）。
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
let fetches = 0;
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set, encodeURIComponent,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {}, ogMsg: () => {}, setTimeout: () => 0, confirm: () => true,
  authState: { role: 'admin', authenticated: true },
  api: async () => { fetches++; return { ok: true, secrets: { api_key: 'atr_REALPAYLOAD_1234567890', api_keys: ['atr_REAL_A_1', 'atr_REAL_B_2'] } }; },
};
ctx.isAdmin = () => ctx.authState.role === 'admin';
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.provKeyLabel=provKeyLabel; this.provRevealKeys=provRevealKeys;' +
  ' this.onProvFormClick=onProvFormClick; this.provFormHtml=provFormHtml;' +
  ' this.PROV_TYPES=PROV_TYPES; this.setProvEdit=function(v){ provEdit=v; };', ctx);

const MASK = 'atr_PK…5OyU…(len=36)';
const MASK2 = 'atr_5_…JvEj…(len=36)';
const atria = { name: 'atria', type: 'openai', base_url: 'https://api.atria-asi.ai/v1',
  api_key: MASK, api_keys: [MASK, MASK2], accounts: [] };

// 场景①：管理员编辑已落盘的供应商 → 每个非空密钥字段一个按钮。
ctx.setProvEdit({ i: 2, isNew: false, draft: {}, revealed: {}, masked: {} });
const fAdmin = ctx.provFormHtml(atria);
const adminKeyBtn = fAdmin.indexOf('data-reveal="api_key"') >= 0;
const adminPoolBtn = fAdmin.indexOf('data-reveal="api_keys"') >= 0;
const adminBtnLabel = fAdmin.indexOf('查看完整') >= 0;
const fOc = ctx.provFormHtml({ name: 'oc', type: 'opencode', zen_keys: ['sk-z…1111…(len=20)'], go_keys: ['sk-g…2222…(len=20)'], accounts: [] });
const adminZenBtn = fOc.indexOf('data-reveal="zen_keys"') >= 0;
const adminGoBtn = fOc.indexOf('data-reveal="go_keys"') >= 0;

// 场景②：viewer 一个按钮都没有（服务端那条端点本来也 403）。
ctx.authState.role = 'viewer';
const viewerBtn = ctx.provFormHtml(atria).indexOf('data-reveal=') >= 0;

// 场景③：新建供应商（没落盘）与空字段都不给按钮。
ctx.authState.role = 'admin';
ctx.setProvEdit({ i: -1, isNew: true, draft: {}, revealed: {}, masked: {} });
const newProvBtn = ctx.provFormHtml(atria).indexOf('data-reveal=') >= 0;
ctx.setProvEdit({ i: 0, isNew: false, draft: {}, revealed: {}, masked: {} });
const emptyBtn = ctx.provFormHtml({ name: 'o', type: 'openai', api_key: '', api_keys: [], accounts: [] }).indexOf('data-reveal=') >= 0;

// 场景④：查看 → 明文填充；池子复用缓存；隐藏 → 掩码快照还原。
const d = JSON.parse(JSON.stringify(atria));
const box = { i: 2, isNew: false, draft: d, revealed: {}, masked: {} };
for (const f of ['api_key', 'api_keys', 'zen_keys', 'go_keys']) {
  if (d[f] !== undefined) box.masked[f] = JSON.parse(JSON.stringify(d[f]));
}
ctx.setProvEdit(box);
(async () => {
  await ctx.provRevealKeys('api_key');
  const revealedKey = d.api_key;
  const revealedBtnText = ctx.$('provForm').innerHTML.indexOf('隐藏') >= 0;
  await ctx.provRevealKeys('api_keys');
  const revealedPool = d.api_keys.join(',');
  const fetchOnce = fetches;
  await ctx.provRevealKeys('api_key');
  const hiddenKey = d.api_key;
  await ctx.provRevealKeys('api_keys');
  const hiddenPool = d.api_keys.join(',');
  process.stdout.write(JSON.stringify({
    adminKeyBtn, adminPoolBtn, adminBtnLabel, adminZenBtn, adminGoBtn,
    viewerBtn, newProvBtn, emptyBtn,
    revealedKey, revealedBtnText, revealedPool, fetchOnce,
    hiddenKey, hiddenPool,
  }));
})();`
	f, err := os.CreateTemp(t.TempDir(), "provsec-reveal-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("provider secret reveal node test failed: %v\n%s", err, out)
	}
	const want = `{"adminKeyBtn":true,"adminPoolBtn":true,"adminBtnLabel":true,"adminZenBtn":true,"adminGoBtn":true,` +
		`"viewerBtn":false,"newProvBtn":false,"emptyBtn":false,` +
		`"revealedKey":"atr_REALPAYLOAD_1234567890","revealedBtnText":true,` +
		`"revealedPool":"atr_REAL_A_1,atr_REAL_B_2","fetchOnce":1,` +
		`"hiddenKey":"atr_PK…5OyU…(len=36)",` +
		`"hiddenPool":"atr_PK…5OyU…(len=36),atr_5_…JvEj…(len=36)"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("provider secret reveal=%s\nwant %s", out, want)
	}
}
