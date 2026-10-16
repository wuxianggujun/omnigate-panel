package panel

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSKeyHealthSurfacing 钉住「上游密钥健康」的前端接线（自动禁用可见性）。
//
// 用户要求：额度耗尽的密钥要能自动禁用，被禁用的状况必须看得见（哪把、为什么、
// 什么时候会被自动清理）。服务端数据走 GET /panel/api/omni/key-health
// （keyhealth_endpoint_test.go 钉权限与形态），前端这边的接线漏了都是静默的：
//
//  1. loadOmniConfig 必须随配置一起拉 key-health，且失败静默（omniKeyHealth=null）；
//  2. 供应商行要有「N 把禁用」角标（provHealthBadgeHtml 接进 provRowHtml）；
//  3. 编辑弹窗的 api_keys 下方要有明细块（掩码 + 原因 + 连败 + 自动重试/清理）；
//  4. 没有异常数据的供应商一个标记都不该出现。
//
// 静态断言（app.js 文本）+ 运行时断言（node 里真跑一遍）双管齐下。
func TestAppJSKeyHealthSurfacing(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)

	static := map[string]string{
		"全局状态存在":   "let omniKeyHealth = null;",
		"拉取函数存在":   "async function fetchOmniKeyHealth() {",
		"端点相对路径":   "omniKeyHealth = await api('omni/key-health');",
		"加载时已接线":   "await fetchOmniKeyHealth();",
		"角标函数存在":   "function provHealthBadgeHtml(p) {",
		"行内已接线":    "+ provHealthBadgeHtml(p) + '</div>'",
		"弹窗明细已接线":  "out.push(provHealthLinesHtml(d.name || ''));",
		"明细含自动清理":  "预计 ' + healthTimeStr(k.cleanup_at) + ' 自动清理",
		"明细含自动重试":  "最早 ' + healthTimeStr(k.until) + ' 自动重试",
		"提示文案已更新":  "额度耗尽的自动禁用，长期复败",
		"失败静默降级":   "omniKeyHealth = null;",
	}
	for why, needle := range static {
		if !strings.Contains(js, needle) {
			t.Errorf("%s：app.js 缺少 %q", why, needle)
		}
	}

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; key health UI test skipped")
	}
	// 评测区域：const PROV_TYPES 到 openProvEdit——覆盖 provEdit/provShowIdle 声明、
	// 密钥计数、health 助手、行渲染与弹窗表单。`let omniKeyHealth` 声明在区域内，
	// 注入只能走评测尾巴的 setKeyHealth（与 app.js 自己的赋值同源）。
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PROV_TYPES');
const end = src.indexOf('function openProvEdit(i) {');
if (start < 0 || end < 0 || end < start) throw new Error('provider region not found');
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set, RegExp, Date,
  esc: s => String(s == null ? '' : s),
  isAdmin: () => true,
  omniModelsText: p => (Array.isArray(p.models) ? p.models.join(', ') : (p.models || '')),
  localStorage: { getItem: () => null, setItem: () => {} },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.setKeyHealth=function(v){ omniKeyHealth=v; };' +
  ' this.setProvEdit=function(v){ provEdit=v; };' +
  ' this.provHealthBadgeHtml=provHealthBadgeHtml; this.provHealthLinesHtml=provHealthLinesHtml;' +
  ' this.provRowHtml=provRowHtml; this.provFormHtml=provFormHtml; this.healthTimeStr=healthTimeStr;',
  ctx);

ctx.setProvEdit({ i: 2, isNew: false, draft: {}, revealed: {}, masked: {}, secret: null });
ctx.setKeyHealth({ providers: [ { name: 'atria', keys: [
  { key: 'atr_OoG9tEee…sDCA', disabled: true, reason: 'quota', strikes: 3,
    until: 1760000000, first_seen: 1759000000, cleanup_at: 1760500000,
    last_error: 'upstream HTTP 429: insufficient_quota' },
  { key: 'atr_PKQzVd_Q…5OyU' },
] } ], cleanup_strikes: 3, cleanup_days: 7 });

const badge = ctx.provHealthBadgeHtml({ name: 'atria' });
const noBadge = ctx.provHealthBadgeHtml({ name: 'other' });
const row = ctx.provRowHtml({ name: 'atria', type: 'openai', api_keys: ['a', 'b'], accounts: [], models: [] }, 0);
const form = ctx.provFormHtml({ name: 'atria', type: 'openai', api_keys: ['atr_PKQzVd_Q…5OyU'], accounts: [] });
const formOther = ctx.provFormHtml({ name: 'other', type: 'openai', api_keys: ['x'], accounts: [] });
const ts = ctx.healthTimeStr(1760500000);
const tsZero = ctx.healthTimeStr(0);
// 无数据（拉取失败）时不得崩、不得出标记。
ctx.setKeyHealth(null);
const nullBadge = ctx.provHealthBadgeHtml({ name: 'atria' });
const nullForm = ctx.provFormHtml({ name: 'atria', type: 'openai', api_keys: ['x'], accounts: [] });

process.stdout.write(JSON.stringify({
  badgeHasCount: badge.indexOf('1 把禁用') >= 0,
  badgeHasTitle: badge.indexOf('自动从配置清理') >= 0,
  noBadge: noBadge,
  rowHasBadge: row.indexOf('1 把禁用') >= 0,
  formHasHealth: form.indexOf('密钥健康') >= 0,
  formHasMasked: form.indexOf('atr_OoG9tEee…sDCA') >= 0,
  formHasDisabled: form.indexOf('已自动禁用（额度耗尽）') >= 0,
  formHasStrikes: form.indexOf('连续 3 次') >= 0,
  formHasRetry: form.indexOf('自动重试') >= 0,
  formHasCleanup: form.indexOf('自动清理') >= 0,
  formOtherClean: formOther.indexOf('密钥健康') < 0,
  tsShape: /^\d{4}-\d{2}-\d{2}$/.test(ts),
  tsZero: tsZero,
  nullBadge: nullBadge,
  nullFormClean: nullForm.indexOf('密钥健康') < 0,
}));`
	f, err := os.CreateTemp(t.TempDir(), "keyhealth-ui-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("key health UI node test failed: %v\n%s", err, out)
	}
	const want = `{"badgeHasCount":true,"badgeHasTitle":true,"noBadge":"","rowHasBadge":true,` +
		`"formHasHealth":true,"formHasMasked":true,"formHasDisabled":true,"formHasStrikes":true,` +
		`"formHasRetry":true,"formHasCleanup":true,"formOtherClean":true,` +
		`"tsShape":true,"tsZero":"","nullBadge":"","nullFormClean":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("key health UI=%s\nwant %s", out, want)
	}
}
