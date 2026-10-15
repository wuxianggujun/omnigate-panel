package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppJSSyntax app.js 必须能通过 JS 解析器语法校验。
//
// 为什么需要：app.js 是 go:embed 进二进制的静态资源，Go 编译器不检查其内容——
// 一次对象字面量键名未加引号（Model_chat_GLM5.2 被解析成属性访问 + 数字字面量）
// 就让整个面板白屏，而所有 Go 测试依然全绿。此测试把语法校验前移到 CI。
// 无 node 环境时跳过（不阻塞无 Node 的构建机）。
func TestAppJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available; skipping JS syntax check")
	}
	path, err := filepath.Abs("app.js")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("app.js syntax error:\n%s", out)
	}
}

// TestIndexHTMLNoInlineScript index.html 不得含内联 <script> 块：
// 严格 CSP（script-src 'self'）会拦截内联脚本，页面将完全不可用。
// 外链形式 <script src="..."> 允许。
func TestIndexHTMLNoInlineScript(t *testing.T) {
	p := newTestPanel()
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest("GET", "/panel/", nil))
	body := rec.Body.String()

	rest := body
	for {
		idx := strings.Index(rest, "<script")
		if idx < 0 {
			break
		}
		rest = rest[idx:]
		end := strings.Index(rest, ">")
		if end < 0 {
			break
		}
		tag := rest[:end+1]
		if !strings.Contains(tag, "src=") {
			t.Fatalf("index.html contains inline <script> (blocked by CSP): %s", tag)
		}
		rest = rest[end:]
	}
}

// TestAppJSTopLevelSmoke app.js 顶层求值冒烟（v1.11.3/1.11.4 两连炸后补的运行时闸门）：
// node + DOM 桩执行 app.js（含按 hash 落到各视图的 go() 顶层调用），抓 TDZ/
// ReferenceError 类运行时错误——Go 侧 frontend_test 不执行 JS，语法层检查对此全盲。
// 无 node 的环境跳过（CI/精简机不受影响）；harness 与 app.js 同判（app.js 顶层
// start() 的 setInterval 会让 node 事件循环不退出，故成功路径显式 exit(0)）。
func TestAppJSTopLevelSmoke(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; JS smoke skipped")
	}
	harness := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const inert = new Proxy(function () {}, {
  get(t, k) { if (k === Symbol.toPrimitive) return () => ''; return inert; },
  set() { return true; },
  apply() { return inert; },
  construct() { return inert; },
  has() { return true; },
});
const __timers = [];
const sandbox = new Proxy({
  location: { hash: process.env.SMOKE_HASH || '#taskscenter' },
  history: { replaceState() {} },
  localStorage: { getItem: () => null, setItem() {} },
  navigator: { clipboard: { writeText: () => Promise.resolve() } },
  document: { querySelectorAll: () => [], querySelector: () => inert, getElementById: () => inert, addEventListener() {}, documentElement: inert, head: inert, body: inert, createElement: () => inert, cookie: '' },
  fetch: () => new Promise(() => {}),
  addEventListener() {}, removeEventListener() {},
  matchMedia: () => ({ matches: false, addEventListener() {} }),
  setInterval: () => 0, clearInterval: () => {},
  setTimeout: (fn) => { __timers.push(fn); return __timers.length; }, clearTimeout: () => {},
  console, JSON, Math, Date, Number, String, Boolean, Object, Array, Promise, Map, Set, RegExp, Error, TypeError, isNaN, parseInt, parseFloat, encodeURIComponent, decodeURIComponent, URL, Symbol, Proxy, Reflect,
}, { get(t, k) { return t[k]; }, has() { return true; } });
sandbox.window = sandbox; sandbox.globalThis = sandbox;
vm.createContext(sandbox);
try {
  vm.runInContext(src, sandbox, { filename: 'app.js' });
  // app.js 把首次 go(hash) 推迟到 setTimeout(0)（避开模块级 let/const 的 TDZ）。
  // 这里同步冲刷定时器队列，让 go(hash) 真正执行——否则进程在定时器触发前就 exit，
  // hash 参数形同虚设，各视图的顶层求值路径实际从未被覆盖。
  const __pending = __timers.slice();
  __timers.length = 0;
  for (const fn of __pending) fn();
  console.log('SMOKE OK');
  process.exit(0);
} catch (e) {
  console.log('SMOKE FAIL:', (e && e.stack ? e.stack : e).toString().split('\n').slice(0, 5).join('\n'));
  process.exit(1);
}
`
	hf, err := os.CreateTemp(t.TempDir(), "smoke-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hf.WriteString(harness); err != nil {
		t.Fatal(err)
	}
	hf.Close()
	for _, hash := range []string{"#taskscenter", "#accounts", "#usage", "#models", "#config", "#logs", "#runlogs", "#packages", "#providers", "#outbound"} {
		cmd := exec.Command(node, hf.Name(), "app.js")
		cmd.Dir = "." // 测试工作目录 = internal/panel
		cmd.Env = append(os.Environ(), "SMOKE_HASH="+hash)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("app.js 顶层求值 %s 崩溃: %v\n%s", hash, err, out)
		}
		if !bytes.Contains(out, []byte("SMOKE OK")) {
			t.Fatalf("app.js smoke %s 未通过:\n%s", hash, out)
		}
	}
}

// 积分扣除维度的格式必须稳定，且缺样本/缺匹配 Token 时不能伪造比例。
func TestAppJSCreditDimensionFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
	const start = src.indexOf('function trimFixed');
const end = src.indexOf('function usStat');
if (start < 0 || end < 0) throw new Error('credit helpers not found');
const ctx = { Number, String, RegExp };
vm.createContext(ctx);
vm.runInContext(
  src.slice(start, end) +
  '\nthis.fmtCredit=fmtCredit; this.fmtCreditRatio=fmtCreditRatio; this.fmtModelRate=fmtModelRate;',
  ctx
);
process.stdout.write(JSON.stringify({
  credit: ctx.fmtCredit(1.25),
  zero: ctx.fmtCredit(0),
  hundred: ctx.fmtCredit(100),
  ratio: ctx.fmtCreditRatio(12.5, 2, 400),
  noSamples: ctx.fmtCreditRatio(12.5, 0, 400),
  noTokens: ctx.fmtCreditRatio(12.5, 2, 0),
  rate: ctx.fmtModelRate('0.5'),
  noRate: ctx.fmtModelRate(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"credit":"1.25","zero":"0","hundred":"100","ratio":"12.5 / 1M","noSamples":"—","noTokens":"—","rate":"x0.5","noRate":"—"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit formatting=%s want %s", out, want)
	}
}

// 模型限流时间必须同时支持上游 reset_at、网关 until 和无重置时间三种形态。
func TestAppJSRateLimitMeta(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; rate limit formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function dur(');
const end = src.indexOf('function rateLimitRowsHtml');
if (start < 0 || end < 0) throw new Error('rate limit helpers not found');
const ctx = { Date, Number, String, Math };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.rateLimitMeta=rateLimitMeta;', ctx);
const now = new Date(2026, 8, 28, 14, 0, 0).getTime();
const reset = new Date(2026, 8, 28, 16, 0, 0).getTime();
const until = new Date(2026, 8, 28, 15, 0, 0).getTime();
const rate = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit', reset_at: new Date(reset).toISOString(), until: new Date(until).toISOString() }, now);
const unavailable = ctx.rateLimitMeta({ model: 'missing', kind: 'model_unavailable', until: new Date(until).toISOString() }, now);
const unknown = ctx.rateLimitMeta({ model: 'glm-5.3', kind: 'rate_limit' }, now);
process.stdout.write(JSON.stringify({
  rate: rate.detail,
  unavailable: unavailable.detail,
  unknown: unknown.detail,
}));`
	f, err := os.CreateTemp(t.TempDir(), "rate-limit-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("rate-limit formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"rate":"预计 2026-09-28 16:00 解封（剩余 2时00分） · 网关最快 1时00分 后重试","unavailable":"预计 1时00分 后重试","unknown":"预计解封时间未知"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("rate-limit formatting=%s want %s", out, want)
	}
}

// 请求记录行必须紧凑、可读，并带上调用来源（IP / UA）；来源缺失时以 — 兜底。
func TestAppJSRequestLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if ([escStart, escEnd, fmtStart, fmtEnd, reqStart, reqEnd].some(v => v < 0)) throw new Error('request log helpers not found');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(
  src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) +
  '\nthis.requestLogText=requestLogText;',
  ctx
);
const time = new Date(2026, 8, 28, 14, 5, 6).toISOString();
const good = { time, status: 200, outcome: 'success', model: 'glm-5.3', account: '账号(uid8)', duration_ms: 1250, total_tokens: 2300, credit_known: true, credit: 0.12, request_id: 'req-1', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0' };
const noSource = { ...good, request_id: 'req-3', client_ip: '', user_agent: '' };
const cached = { ...good, request_id: 'req-2', cache_hit_tokens: 2257, cache_miss_tokens: 43 };
const withRealm = { ...good, request_id: 'req-4', realm: 'global' };
const withEffort = { ...good, request_id: 'req-5', reasoning_effort: 'high', reasoning_tokens: 1234 };
const withProvider = { ...good, request_id: 'req-6', provider: 'raccoon' };
const withReasoning = { ...good, request_id: 'req-7', reasoning: true };
// credit=0（免费/零倍率模型）与「键被 omitempty 省略」都必须显示 0 credit，不能显示
// 成未知 —— 后者曾经是真实缺陷：Number(undefined)=NaN → 判成未知。
const zeroCredit = { ...good, request_id: 'req-8', credit: 0 };
const missingCredit = { ...good, request_id: 'req-9' };
delete missingCredit.credit;
process.stdout.write(JSON.stringify({
  good: ctx.requestLogText(good),
  noSource: ctx.requestLogText(noSource),
  cached: ctx.requestLogText(cached),
  withRealm: ctx.requestLogText(withRealm),
  withEffort: ctx.requestLogText(withEffort),
  withProvider: ctx.requestLogText(withProvider),
  withReasoning: ctx.requestLogText(withReasoning),
  zeroCredit: ctx.requestLogText(zeroCredit),
  missingCredit: ctx.requestLogText(missingCredit),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-log-format-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request log formatting node test failed: %v\n%s", err, out)
	}
	text := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-1"
	noSource := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | — | — | 1.25s | 2.3k tok | 0.12 credit | req-3"
	cached := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | 命中 98.1% | req-2"
	withRealm := "14:05:06 | 200 成功 | glm-5.3 | 国际版 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-4"
	withEffort := "14:05:06 | 200 成功 | glm-5.3 | 思考 high·1.2ktok | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-5"
	withProvider := "14:05:06 | 200 成功 | glm-5.3 | raccoon | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-6"
	withReasoning := "14:05:06 | 200 成功 | glm-5.3 | 有思考 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0.12 credit | req-7"
	zeroCredit := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0 credit | req-8"
	missingCredit := "14:05:06 | 200 成功 | glm-5.3 | 账号(uid8) | 203.0.113.7 | python-requests/2.31.0 | 1.25s | 2.3k tok | 0 credit | req-9"
	want := `{"good":` + strconv.Quote(text) + `,"noSource":` + strconv.Quote(noSource) + `,"cached":` + strconv.Quote(cached) + `,"withRealm":` + strconv.Quote(withRealm) + `,"withEffort":` + strconv.Quote(withEffort) + `,"withProvider":` + strconv.Quote(withProvider) + `,"withReasoning":` + strconv.Quote(withReasoning) + `,"zeroCredit":` + strconv.Quote(zeroCredit) + `,"missingCredit":` + strconv.Quote(missingCredit) + `}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request log formatting=%s want %s", out, want)
	}
}

// 请求记录「积分」列：credit_known=true 且 credit=0、或键被 omitempty 省略（免费 /
// 零倍率模型）必须显示 0，而不是未知。这曾经是真实缺陷：Number(undefined)=NaN 被
// 判成未知显示成 —，于是「请求记录里根本看不到积分」。
func TestAppJSRequestCreditCell(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit cell test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const cellStart = src.indexOf('function reqCreditCell');
const cellEnd = src.indexOf('function reqRealmCell');
if ([fmtStart, fmtEnd, cellStart, cellEnd].some(v => v < 0)) throw new Error('credit cell helpers not found');
const ctx = { Number, String, Math, RegExp };
vm.createContext(ctx);
vm.runInContext(
  'var omPriceMap = null;\n' +
  src.slice(fmtStart, fmtEnd) + src.slice(cellStart, cellEnd) +
  '\nthis.reqCreditCell=reqCreditCell;',
  ctx
);
const zero = { credit_known: true, credit: 0, model: 'deepseek-v4.1-flash' };
const omitted = { credit_known: true, model: 'deepseek-v4.1-flash' };
const known = { credit_known: true, credit: 1.25, model: 'glm-5.3' };
const unknown = { credit_known: false, model: 'glm-5.3' };
process.stdout.write(JSON.stringify({
  zero: ctx.reqCreditCell(zero),
  omitted: ctx.reqCreditCell(omitted),
  known: ctx.reqCreditCell(known),
  unknown: ctx.reqCreditCell(unknown),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-cell-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit cell node test failed: %v\n%s", err, out)
	}
	zeroCell := `<span class=\"muted\" title=\"上游回报本次消耗 0 积分（该模型免费 / 零倍率）\">0</span>`
	want := `{"zero":"` + zeroCell + `","omitted":"` + zeroCell + `","known":"1.25","unknown":"<span class=\"muted\">—</span>"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit cell=%s want %s", out, want)
	}
}

// 请求记录筛选：IP / UA / 模型 / 账号 / 供应商 / 请求 ID 的包含匹配（空格分词 AND）
// + 结果与供应商的精确匹配。
func TestAppJSRequestMatch(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; request filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function reqMatch');
const end = src.indexOf('function reqOutcomeTag');
if (start < 0 || end < 0) throw new Error('reqMatch not found');
const ctx = {};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.reqMatch=reqMatch;', ctx);
const base = { outcome: 'success', client_ip: '203.0.113.7', user_agent: 'python-requests/2.31.0', model: 'cn:glm-5.3', realm: 'cn', account: '示例(uid8)', provider: 'raccoon', request_id: 'req-1' };
const other = { outcome: 'http_error', client_ip: '198.51.100.4', user_agent: 'Mozilla/5.0 Chrome/120', model: 'global:hy3', realm: 'global', account: '甲(uid9)', provider: 'runable', request_id: 'req-2' };
const rows = [base, other];
const pick = f => rows.filter(e => ctx.reqMatch(e, f)).map(e => e.request_id);
process.stdout.write(JSON.stringify({
  all: pick({ q: '', outcome: '' }),
  byIP: pick({ q: '203.0.113', outcome: '' }),
  byUA: pick({ q: 'chrome/120', outcome: '' }),
  byModel: pick({ q: 'glm', outcome: '' }),
  multiKw: pick({ q: 'glm success', outcome: '' }),
  multiMiss: pick({ q: 'glm chrome', outcome: '' }),
  byOutcome: pick({ q: '', outcome: 'http_error' }),
  combined: pick({ q: '198.51', outcome: 'http_error' }),
  byRealmCn: pick({ q: '国内', outcome: '' }),
  byRealmGlobal: pick({ q: '国际', outcome: '' }),
  byProvider: pick({ q: '', outcome: '', provider: 'raccoon' }),
  providerMiss: pick({ q: '', outcome: '', provider: 'deepseek' }),
  byProviderKw: pick({ q: 'runable', outcome: '' }),
}));`
	f, err := os.CreateTemp(t.TempDir(), "request-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("request filter node test failed: %v\n%s", err, out)
	}
	// q 对 outcome 不参与匹配（outcome 有独立下拉），multiKw 里的 success 命中不了任何字段。
	const want = `{"all":["req-1","req-2"],"byIP":["req-1"],"byUA":["req-2"],"byModel":["req-1"],"multiKw":[],"multiMiss":[],"byOutcome":["req-2"],"combined":["req-2"],"byRealmCn":["req-1"],"byRealmGlobal":["req-2"],"byProvider":["req-1"],"providerMiss":[],"byProviderKw":["req-2"]}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("request filter=%s want %s", out, want)
	}
}

// 模型按条件查询：域 / 能力 / 档位 / 价格 / 关键词，以及倍率、上下文、输出排序。
func TestAppJSModelFilter(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; model filter test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function mdRateValue');
const end = src.indexOf('function mdRowHtml');
if (start < 0 || end < 0) throw new Error('model filter helpers not found');
const ctx = { Number, String, Array, Object, isFinite, parseFloat };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.mdMatch=mdMatch; this.mdSortList=mdSortList; this.mdRateValue=mdRateValue;', ctx);
const models = [
  { id: 'glm-5.2', realm: 'cn', name: 'GLM-5.2', vendor: 'Zhipu', tags: ['视觉'], supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: true, supported_efforts: ['high', 'xhigh'], default_effort: 'high', is_default: false, credits: '0.79', promo_factor: 0.5, promo_credits: '0.40', promo_label: '夜间折扣', context_length: 1000000, max_output_tokens: 131000 },
  { id: 'hy3', realm: 'cn', name: 'Hy3', supports_tool_call: true, supports_images: true, supports_reasoning: true, can_disable_thinking: false, supported_efforts: ['low', 'high'], default_effort: 'high', is_default: false, credits: '0', promo_factor: 0, promo_credits: '0', promo_label: '限时免费', context_length: 192000, max_output_tokens: 64000 },
  { id: 'hy3', realm: 'global', name: 'Hy3 Global', supports_tool_call: false, supports_images: false, supports_reasoning: false, supported_efforts: [], is_default: false, credits: '0.11', context_length: 1000000, max_output_tokens: 393000 },
  { id: 'auto', realm: 'cn', name: 'Auto', supports_tool_call: true, supports_images: true, supports_reasoning: true, is_default: true, credits: null, context_length: 256000, max_output_tokens: 32000 },
];
const ids = list => list.map(m => m.id);
const filter = f => ids(ctx.mdSortList(models.filter(m => ctx.mdMatch(m, f)), f));
process.stdout.write(JSON.stringify({
  all: ids(models),
  realm: filter({ realm: 'cn' }),
  tool: filter({ cap: 'tool' }),
  vision: filter({ cap: 'vision' }),
  reasoning: filter({ cap: 'reasoning' }),
  isDefault: filter({ cap: 'default' }),
  effortOff: filter({ effort: 'off' }),
  effortLow: filter({ effort: 'low' }),
  free: filter({ promo: 'free' }),
  promo: filter({ promo: 'promo' }),
  discount: filter({ promo: 'discount' }),
  q: filter({ q: 'glm zhipu' }),
  qMiss: filter({ q: 'glm nosuch' }),
  sortRate: filter({ sort: 'rate' }),
  sortContext: filter({ sort: 'context' }),
  sortOutput: filter({ sort: 'output' }),
  sortName: filter({ sort: 'name' }),
  rateFree: ctx.mdRateValue(models[1]),
  rateMissing: ctx.mdRateValue(models[3]),
}));`
	f, err := os.CreateTemp(t.TempDir(), "model-filter-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("model filter node test failed: %v\n%s", err, out)
	}
	const want = `{"all":["glm-5.2","hy3","hy3","auto"],` +
		`"realm":["glm-5.2","hy3","auto"],` +
		`"tool":["glm-5.2","hy3","auto"],` +
		`"vision":["glm-5.2","hy3","auto"],` +
		`"reasoning":["glm-5.2","hy3","auto"],` +
		`"isDefault":["auto"],` +
		`"effortOff":["glm-5.2"],` +
		`"effortLow":["hy3"],` +
		`"free":["hy3"],` +
		`"promo":["glm-5.2","hy3"],` +
		`"discount":["glm-5.2"],` +
		`"q":["glm-5.2"],` +
		`"qMiss":[],` +
		`"sortRate":["hy3","hy3","glm-5.2","auto"],` +
		`"sortContext":["glm-5.2","hy3","auto","hy3"],` +
		`"sortOutput":["hy3","glm-5.2","hy3","auto"],` +
		`"sortName":["auto","glm-5.2","hy3","hy3"],` +
		`"rateFree":0,"rateMissing":null}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("model filter=%s\nwant %s", out, want)
	}
}

// 域优先级「逐模型优先域 / 优先供应商」表：按裸名去重渲染、逐模型下拉（默认/国内版/
// 国际版）、通配规则命中提示、搜索过滤、以及「优先供应商」列（= model_mapping 的行内
// 呈现）。无需用户手写模型名。
//
// 2026-10：加了「优先供应商」列，renderRealmPicker 因此读 omniCfg.providers 与
// omniMapping 两个模块级快照。沙箱必须注入它们，否则一 render 就 ReferenceError ——
// 这也是这条测试存在的意义：前端新依赖一漏注入，这里立刻红。
func TestAppJSRealmPicker(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; realm picker test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let rrOrder');
const end = src.indexOf('async function loadRealmRouting');
if (start < 0 || end < 0) throw new Error('realm picker helpers not found');
const els = { rrBody: { innerHTML: '' }, rrCount: { textContent: '' } };
const ctx = { Number, String, Array, Object, Set, console, $: id => els[id] || null, esc: s => String(s),
  omniCfg: { default_provider: 'workbuddy', providers: [{ name: 'workbuddy' }, { name: 'atria' }] },
  omniMapping: {}, omniMappingOff: {} };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.setModels=a=>{mdAll=a;};this.setState=o=>{if(o.exact)rrExact=o.exact;if(o.wild)rrWild=o.wild;if(o.query!=null)rrQuery=o.query;};this.setMap=m=>{omniMapping=m;};this.render=renderRealmPicker;this.effective=rrEffective;', ctx);
ctx.setModels([
  { id: 'glm-5.2', realm: 'cn' },
  { id: 'glm-5.2', realm: 'global' },
  { id: 'deepseek-v4.1-flash', realm: 'global' },
  { id: 'hy3', realm: 'cn' },
]);
ctx.setState({ exact: { 'hy3': 'global' }, wild: { 'deepseek-*': 'global' }, query: '' });
ctx.render();
const html = els.rrBody.innerHTML;
const count = els.rrCount.textContent;
const deepHint = html.includes('deepseek-v4.1-flash') && html.includes('（通配→国际版）');
const hy3Sel = (html.match(/value="global" selected/g) || []).length;
// 「优先供应商」列：未钉时列出所有供应商 + 一个「跟随全局路由」空选项；
// 被 mapping 钉住的模型要显示 selected 与「已钉」角标。
const provSelCount = (html.match(/class="rr-pin"/g) || []).length;
const hasFollowGlobal = html.includes('<option value="">跟随全局路由</option>');
const noPinBadgeOnDeep = html.includes('deepseek-v4.1-flash') && !html.split('deepseek-v4.1-flash')[1].split('</tr>')[0].includes('已钉');
ctx.setMap({ 'hy3': 'atria/Atria-Dawn-Preview' });
ctx.render();
const hy3Pinned = els.rrBody.innerHTML.includes('已钉 → atria');
const hy3PinSelected = /value="atria" selected/.test(els.rrBody.innerHTML);
ctx.setMap({});
ctx.setState({ query: 'deep' });
ctx.render();
const qHtml = els.rrBody.innerHTML;
process.stdout.write(JSON.stringify({
  count,
  deepHint,
  hy3Sel,
  eff: ctx.effective('deepseek-v4.1-flash'),
  qDeep: qHtml.includes('deepseek-v4.1-flash'),
  qGlm: qHtml.includes('glm-5.2'),
  provSelCount, hasFollowGlobal, noPinBadgeOnDeep, hy3Pinned, hy3PinSelected,
}));`
	f, err := os.CreateTemp(t.TempDir(), "realm-picker-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("realm picker node test failed: %v\n%s", err, out)
	}
	const want = `{"count":"共 3 个模型","deepHint":true,"hy3Sel":1,"eff":"global","qDeep":true,"qGlm":false,` +
		`"provSelCount":3,"hasFollowGlobal":true,"noPinBadgeOnDeep":true,"hy3Pinned":true,"hy3PinSelected":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("realm picker=%s\nwant %s", out, want)
	}
}

// 用量时序图的柱体类名不得叫 bar：账号池的积分条是 .bar{height:3px}，而 SVG2 里
// height 是 rect 的 CSS 几何属性——同名类会把每根柱子压成 3px 高，图看起来"没数据"。
// 这个坑只能在浏览器里看出来，所以在这里钉住类名。
func TestAppJSUsageChartBarClass(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; usage chart test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const end = src.indexOf('function fmtTokTip');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
if ([start, end, escStart, escEnd, fmtStart, fmtEnd].some(v => v < 0)) throw new Error('usage chart helpers not found');
const host = { innerHTML: '', textContent: '' };
const ctx = {
  Date, Number, String, Math, RegExp, isNaN, Set, Array, Object, Infinity,
  document: { getElementById: () => host },
  $: () => host,
};
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(start, end) +
  '\nthis.renderUsageChart=renderUsageChart;', ctx);
const series = [
  { t: '2026-09-30T09', scope: 'hour', prompt_tokens: 35, completion_tokens: 16, total_tokens: 51, requests: 1 },
  { t: '2026-09-30T11', scope: 'hour', prompt_tokens: 978324, completion_tokens: 20621, total_tokens: 998945, requests: 39 },
  { t: '2026-09-30T13', scope: 'hour', prompt_tokens: 27400952, completion_tokens: 104913, total_tokens: 27505865, requests: 200 },
];
ctx.renderUsageChart(series);
const svg = host.innerHTML;
process.stdout.write(JSON.stringify({
  hasUsbar: svg.includes('class="usbar"'),
  hasBareBar: /class="bar"/.test(svg),
  hasGradient: svg.includes('usGradP') && svg.includes('usGradC'),
  barCount: (svg.match(/class="usbar"/g) || []).length,
  hasPeak: svg.includes('峰值'),
  hasAvg: svg.includes('均值'),
  emptyState: (function () { ctx.renderUsageChart([]); return host.innerHTML.includes('us-empty'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "usage-chart-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("usage chart node test failed: %v\n%s", err, out)
	}
	const want = `{"hasUsbar":true,"hasBareBar":false,"hasGradient":true,"barCount":6,"hasPeak":true,"hasAvg":true,"emptyState":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("usage chart=%s\nwant %s", out, want)
	}
}

// 时间范围控件：预设 → 查询参数的映射。要点：
//   - 「今天」必须发浏览器本地时区的 00:00（服务端时区未必一致），且不带 to；
//   - 滚动预设 rolling=true 发 hours（服务端整点对齐），rolling=false 折算成 from；
//   - 「全部历史」两者都不发；「自定义」发用户挑的 from/to。
func TestAppJSTimeRangeQuery(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; time range test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const TRANGE_PRESETS');
const end = src.indexOf('function rateLimitMeta');
if (start < 0 || end < 0 || end < start) throw new Error('trange helpers not found');
const host = { innerHTML: '' };
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams,
  document: { getElementById: () => host },
  $: () => host,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.trangeState=trangeState; this.trangeQuery=trangeQuery; this.trangeLabel=trangeLabel; this.trangeMidnight=trangeMidnight;', ctx);
const q = (preset, rolling) => {
  ctx.trangeState('t').preset = preset;
  return ctx.trangeQuery('t', rolling).toString();
};
const secOf = d => String(Math.floor(d.getTime() / 1000));
const approx = (qs, wantSec) => {
  const m = /(?:^|&)from=(\d+)/.exec(qs);
  return m && Math.abs(Number(m[1]) - wantSec) < 120;
};
const now = Date.now();
const todayQ = q('today', true);
process.stdout.write(JSON.stringify({
  todayIsMidnight: todayQ === 'from=' + secOf(ctx.trangeMidnight()),
  todayNoTo: !/to=/.test(todayQ),
  rolling24: q('24', true),
  rolling72: q('72', true),
  rolling0: q('0', true),
  log0: q('0', false),
  log24From: approx(q('24', false), Math.floor((now - 24 * 3600e3) / 1000)),
  log24HasHours: /hours=/.test(q('24', false)),
  log7dFrom: approx(q('168', false), Math.floor((now - 168 * 3600e3) / 1000)),
  custom: (function () {
    const st = ctx.trangeState('t');
    st.preset = 'custom';
    st.from = new Date(2026, 8, 30, 9, 0, 0);
    st.to = new Date(2026, 8, 30, 18, 30, 0);
    return ctx.trangeQuery('t', true).toString();
  })(),
  labelCustom: ctx.trangeLabel('t'),
  labelToday: (function () { ctx.trangeState('t').preset = 'today'; return ctx.trangeLabel('t'); })(),
}));`
	f, err := os.CreateTemp(t.TempDir(), "trange-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("time range node test failed: %v\n%s", err, out)
	}
	local := func(h, m int) string {
		return strconv.FormatInt(time.Date(2026, 9, 30, h, m, 0, 0, time.Local).Unix(), 10)
	}
	want := `{"todayIsMidnight":true,"todayNoTo":true,` +
		// rolling0 必须是 "hours=0"（显式全部历史）。此前期望值是空串——那恰好把
		// issue #121 的错误行为固化成了断言：空 query 会被后端的 72 小时缺省接管，
		// 于是「全部历史」显示成「近 3 天」。
		// log0 仍为空：请求记录端点没有缺省窗口，不传 from/to 就是全部历史。
		`"rolling24":"hours=24","rolling72":"hours=72","rolling0":"hours=0","log0":"",` +
		`"log24From":true,"log24HasHours":false,"log7dFrom":true,` +
		`"custom":"from=` + local(9, 0) + `&to=` + local(18, 30) + `",` +
		`"labelCustom":"9-30 09:00 → 9-30 18:30","labelToday":"今天"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("time range=%s\nwant %s", out, want)
	}
}

// 配置表单与 CFG_MAP 必须一一对应，且面板声称"可在线改"的热生效键必须真的
// 出现在表单里。
//
// 为什么需要：`logging.request_client_info` 曾经在表单里存在过，后来在某次改动中
// 被连带删掉，而 Go 侧的配置键、livecfg 热生效通路、README 的描述都还在——面板
// 少了一个开关而 Go 测试全绿，只有人肉点开配置页才会发现。这里把"表单字段 ↔
// CFG_MAP"与"关键热改键必须在表单里"两条都钉住。
func TestConfigFormMatchesCFGMap(t *testing.T) {
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)

	// CFG_MAP 块（下面两条检查共用）。
	mapBlock := js[strings.Index(js, "const CFG_MAP = {"):]
	mapBlock = mapBlock[:strings.Index(mapBlock, "\n};")]
	// 不能按行首匹配：CFG_MAP 里多个键写在同一行（`a: [...], b: [...]`），只有行首
	// 那个带换行缩进。按「前面是行首或分隔符」判定才不漏。
	inMap := func(name string) bool {
		return regexp.MustCompile(`(?:^|[\s,{])` + regexp.QuoteMeta(name) + `:\s*\[`).MatchString(mapBlock)
	}

	// 1) 表单里的每个 name 都要有 CFG_MAP 条目（否则收集/回填都拿不到它）。
	form := html[strings.Index(html, `<form id="cfgForm">`):]
	form = form[:strings.Index(form, "</form>")]
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`name="([a-z_0-9]+)"`).FindAllStringSubmatch(form, -1) {
		names[m[1]] = true
	}
	if len(names) == 0 {
		t.Fatal("未从配置表单解析出任何 name 字段")
	}
	for n := range names {
		if !inMap(n) {
			t.Errorf("表单字段 %q 在 CFG_MAP 里没有条目（保存时会被静默丢弃）", n)
		}
	}

	// 2) CFG_MAP 里的每个键都要在表单里有控件（否则回填/保存是空转）。
	for _, m := range regexp.MustCompile(`(?:^|[\s,{])([a-z_0-9]+):\s*\[`).FindAllStringSubmatch(mapBlock, -1) {
		if !names[m[1]] {
			t.Errorf("CFG_MAP 键 %q 在配置表单里没有对应控件", m[1])
		}
	}

	// 3) 明确断言这一个键：后端有配置项、README 说面板可改，UI 不能少。
	if !strings.Contains(js, "request_client_info: ['logging', 'request_client_info']") {
		t.Error("CFG_MAP 缺 request_client_info 条目")
	}
	if !names["request_client_info"] {
		t.Error("配置表单缺「记录调用来源」开关（logging.request_client_info）")
	}
}

// 同到期时间按面额降序；其余未用完包与零/负余额包分别聚合。
func TestAppJSDetailGroups(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; detail groups test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_DEFAULT_DETAIL_LIMIT');
const end = src.indexOf('function renderPackages');
if (start < 0 || end < 0) throw new Error('detail group functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.pkDetailGroups = pkDetailGroups; this.pkDetailLimit = pkDetailLimit;', ctx);
const input = [
  { id: 'small-late', size: 100, remain: 1, expires_at: 400 },
  { id: 'zero-early-b', size: 200, remain: 0, expires_at: 200 },
  { id: 'small-early', size: 100, remain: 2, expires_at: 200 },
  { id: 'large-unknown', size: 300, remain: 3, end_time: '' },
  { id: 'zero-early-a', size: 200, remain: -1, expires_at: 200 },
  { id: 'small-unknown', size: 100, remain: 1, end_time: '' },
  { id: 'large-early', size: 300, remain: 4, expires_at: 200 },
  { id: 'zero-late', size: 300, remain: 0, expires_at: 300 },
];
const before = input.map(p => p.id).join(',');
const out = ctx.pkDetailGroups(input, 2);
process.stdout.write(JSON.stringify({
  visible: out.visible.map(p => p.id),
  rest: out.rest.map(p => p.id),
  used: out.used.map(p => p.id),
  restSize: out.restSize,
  restRemain: out.restRemain,
  usedSize: out.usedSize,
  defaultLimit: ctx.pkDetailLimit({}),
  configuredLimit: ctx.pkDetailLimit({ panel: { package_detail_limit: 7 } }),
  unchanged: input.map(p => p.id).join(',') === before,
}));`
	f, err := os.CreateTemp(t.TempDir(), "detail-groups-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("detail groups node test failed: %v\n%s", err, out)
	}
	const want = `{"visible":["large-early","small-early"],"rest":["small-late","large-unknown","small-unknown"],"used":["zero-early-b","zero-early-a","zero-late"],"restSize":500,"restRemain":5,"usedSize":700,"defaultLimit":5,"configuredLimit":7,"unchanged":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("detail groups=%s want %s", out, want)
	}
}

// 精确剩余天数聚合、账号内按总余额钳制、无到期批次不进入图表。
func TestAppJSExpirySummary(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry summary test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const PK_ACCOUNT_COLORS');
const end = src.indexOf('function renderExpiryDistribution');
if (start < 0 || end < 0) throw new Error('expiry summary functions not found');
const ctx = { Date, Math, Number, String, Map, Array, Object, isFinite };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.summarizeCreditDays = summarizeCreditDays; this.pkAccountColorMap = pkAccountColorMap;', ctx);
const day = 86400000, now = 100000;
const out = ctx.summarizeCreditDays([
  { uid: 'a', remain: 100, packages: [
    { name: 'soon-a', remain: 30, expires_at: now + day },
    { name: 'later', remain: 70, expires_at: now + 7 * day },
  ] },
  { uid: 'b', remain: 55, packages: [
    { name: 'soon-b', remain: 20, expires_at: now + day },
    { name: 'unknown', remain: 5, end_time: '' },
  ] },
  { uid: 'err', error: 'offline' },
], now);
process.stdout.write(JSON.stringify({
  rows: out.rows.map(row => ({ days: row.days, credits: row.credits })),
  accountCount: out.accountCount,
  unavailable: out.unavailable,
  colorA: ctx.pkAccountColorMap([{ uid: 'b' }, { uid: 'a' }]).get('a'),
  colorB: ctx.pkAccountColorMap([{ uid: 'a' }, { uid: 'b' }]).get('b'),
}));`
	f, err := os.CreateTemp(t.TempDir(), "expiry-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry summary node test failed: %v\n%s", err, out)
	}
	const want = `{"rows":[{"days":1,"credits":50},{"days":7,"credits":70}],"accountCount":3,"unavailable":1,"colorA":"#4f8cff","colorB":"#25b08b"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry summary=%s want %s", out, want)
	}
}

// TestAppJSCollectConfigClearable 钉住 collectConfig 的空串语义。
//
// 覆盖型字段（user_agent / prompt_file）空串必须照发：漏发会让面板显示"已保存"
// 而 config.json 里的值没变（issue #102 附带发现 2）。
//
// 同时钉住反面：其余文本字段空串仍然不下发。这条同样重要——若哪天为了修上面那个
// 问题改成"所有空串都发"，表单里任何一个没填的框都会变成"请清空"，静默抹掉配置。
func TestAppJSCollectConfigClearable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; collectConfig test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('const CFG_MAP');
const end = src.indexOf('/* Go 时长字段即时校验');
if (start < 0 || end < 0 || end < start) throw new Error('collectConfig region not found');
const mk = v => ({ type: 'text', value: v });
const cfgForm = { elements: {
  listen: mk(''),
  api_key: mk('secret'),
  user_agent: mk(''),
  prompt_file: mk(''),
  checkin_hours: mk(''),
}};
const ctx = {
  Date, Number, String, Math, Map, Array, Object, isNaN, URLSearchParams, Set,
  document: { getElementById: id => (id === 'cfgForm' ? cfgForm : null) },
  $: id => (id === 'cfgForm' ? cfgForm : null),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectConfig = collectConfig;', ctx);
const out = ctx.collectConfig();
const has = (o, k) => Object.prototype.hasOwnProperty.call(o || {}, k);
process.stdout.write(JSON.stringify([
  has(out.upstream, 'user_agent'), (out.upstream || {}).user_agent,
  has(out.prompt, 'file'), (out.prompt || {}).file,
  has(out, 'listen'),
  has(out.schedule, 'checkin_hours'),
  out.api_key
]));`
	f, err := os.CreateTemp(t.TempDir(), "cfgc-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("collectConfig node test failed: %v\n%s", err, out)
	}
	// [user_agent 已发, 其值, prompt.file 已发, 其值, listen 未发, checkin_hours 未发, api_key]
	const want = `[true,"",true,"",false,false,"secret"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("collectConfig=%s want %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSExpirySortedByExpiry 到期提醒按「最近到期」升序，且不分域（issue #125）。
//
// 后端 /panel/api/packages 是按**余额降序**返回的，恰好把 CN 账号都排在前面、
// global 排在末尾，看上去像"按域分组"，实际只是余额顺序。本用例刻意按这个形态构造
// 输入（余额高的到期最晚、global 余额最低），若前端不重排就会保持该顺序而失败。
//
// 日期按「今天 +N 天」生成而不是写死：写死的话过了那天用例就会自己失效。
func TestAppJSExpirySortedByExpiry(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; expiry sort test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function expBatches');
const end = src.indexOf('async function loadExpiry');
if (start < 0 || end < 0 || end < start) throw new Error('expiry region not found');
const sink = { innerHTML: '', textContent: '', hidden: true };
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: () => sink,
  Date, Math, Number, String, Map, Array, Object, isNaN,
  lastPackages: null, lastPackagesAt: 0,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderExpiry = renderExpiry;', ctx);
const iso = n => {
  const d = new Date(); d.setHours(0, 0, 0, 0); d.setDate(d.getDate() + n);
  const p = x => String(x).padStart(2, '0');
  return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate());
};
const pack = (days, amount) => ({ end_time: iso(days) + ' 00:00:00', remain: amount });
// 模拟后端顺序：余额降序 → CN 高余额在前且到期最晚，global 最少且最快到期。
const d = { accounts: [
  { uid: 'a', nickname: 'cn-late',  realm: 'cn',     packages: [pack(16, 900)] },
  { uid: 'b', nickname: 'cn-mid',   realm: 'cn',     packages: [pack(12, 700)] },
  { uid: 'c', nickname: 'gl-soon',  realm: 'global', packages: [pack(2, 500)] },
  { uid: 'd', nickname: 'no-expiry', realm: 'cn',    packages: [pack(-3, 100)] },
  { uid: 'e', nickname: 'broken',   realm: 'cn',     error: 'offline' },
] };
ctx.renderExpiry(d);
const names = [...sink.innerHTML.matchAll(/<span class="exp-nm">([^<]*)<\/span>/g)].map(m => m[1]);
process.stdout.write(JSON.stringify(names));`
	f, err := os.CreateTemp(t.TempDir(), "expsort-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("expiry sort node test failed: %v\n%s", err, out)
	}
	// 最快到期的排最前（跨域）；无 7 天内到期的与查询失败的排最后。
	const want = `["gl-soon","cn-mid","cn-late","no-expiry","broken"]`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("expiry order=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestAppJSChartTooltipInsideBar 图表 tooltip 必须挂在每根柱子内部（issue #128）。
//
// <title> 在 SVG 里描述的是**父元素**。此前它被平铺在 <svg> 根下（<rect> 是自闭合的，
// 无法包含子节点），于是整张图共用一个 tooltip —— 浏览器取第一个 —— 悬停任何柱子都
// 显示同一份数据。这类问题不报错、不影响渲染，只靠肉眼看很容易漏。
//
// 断言结构：<g> 数量 == 数据点数，且每个 <g> 紧跟一个 <title>；同时确认没有
// 游离在 <g> 之外的 <title>。
func TestAppJSChartTooltipInsideBar(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; chart tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function parsePointTime');
const chartStart = src.indexOf('function renderUsageChart');
if (start < 0 || chartStart < 0) throw new Error('chart functions not found');
let end = src.indexOf('\nfunction ', chartStart + 10);
if (end < 0) end = src.length;
const sinks = {};
const mk = id => (sinks[id] = { innerHTML: '', textContent: '' });
const ctx = {
  esc: s => String(s == null ? '' : s),
  fmtTok: v => String(v == null ? 0 : v),
  $: id => (sinks[id] || mk(id)),
  Date, Math, Number, String, Map, Array, Object, isNaN, Infinity, isFinite, Set,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.renderUsageChart = renderUsageChart;', ctx);
const series = [];
for (let h = 0; h < 5; h++) {
  const d = new Date(); d.setHours(d.getHours() - (4 - h), 0, 0, 0);
  const iso = d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' +
              String(d.getDate()).padStart(2, '0') + 'T' + String(d.getHours()).padStart(2, '0');
  series.push({ t: iso, scope: 'hour', prompt_tokens: (h + 1) * 100,
                completion_tokens: (h + 1) * 10, total_tokens: (h + 1) * 110, requests: h + 1 });
}
ctx.renderUsageChart(series);
const svg = sinks['usChart'] ? sinks['usChart'].innerHTML : '';
const groups = svg.match(/<g><title>/g) || [];
const titles = svg.match(/<title>[^<]*<\/title>/g) || [];
// 游离的 <title>：前面不是 <g>（即仍平铺在根下）
const loose = (svg.match(/(?:<rect[^>]*\/>|<\/g>)<title>/g) || []).length;
process.stdout.write(JSON.stringify({
  points: series.length, groups: groups.length, titles: titles.length, loose: loose,
}));`
	f, err := os.CreateTemp(t.TempDir(), "charttip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("chart tooltip node test failed: %v\n%s", err, out)
	}
	const want = `{"points":5,"groups":5,"titles":5,"loose":0}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("chart tooltip structure=%s\nwant %s（groups 应等于数据点数，loose 应为 0）",
			strings.TrimSpace(string(out)), want)
	}
}

// 运行日志（GET /panel/api/logs 环形缓冲）：频道筛选、关键词筛选、最新在上、空态兜底。
//
// 为什么需要：运行日志页是新增视图，签到 / 保活记录靠它展示。这里把渲染顺序
// （最新在上）、频道筛选、关键词筛选、计数文案与空态文案全部钉住，避免以后改
// 排序或筛选条件时静默回归。
func TestAppJSRunLogFormatting(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; run log formatting test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('let rlEntries = [];');
const end = src.indexOf('async function loadRunLogs');
if (start < 0 || end < 0 || end < start) throw new Error('run log helpers not found');
const els = { rlWrap: { innerHTML: '' }, rlCount: { textContent: '' }, rlCh: { value: '' }, rlQ: { value: '' } };
const ctx = {
  Date, String, Number, Array, isNaN,
  $: id => els[id] || null,
  esc: s => String(s == null ? '' : s),
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.setEntries=a=>{rlEntries=a;};this.render=renderRunLogs;this.rlClock=rlClock;this.rlChLabel=rlChLabel;', ctx);
const iso = (h, m, s) => new Date(2026, 9, 10, h, m, s).toISOString();
ctx.setEntries([
  { ts: iso(9, 0, 0), ch: 'chat', text: 'chat req model=glm' },
  { ts: iso(9, 1, 0), ch: 'task', text: 'checkin uid=abc ok' },
  { ts: iso(9, 2, 0), ch: 'task', text: 'keepalive uid=abc ok' },
  { ts: iso(9, 3, 0), ch: 'sys', text: 'server started' },
]);
els.rlCh.value = ''; els.rlQ.value = '';
ctx.render();
const all = els.rlWrap.innerHTML;
const allCount = els.rlCount.textContent;
const newestFirst = all.indexOf('server started') < all.indexOf('chat req model=glm');
els.rlCh.value = 'task'; ctx.render();
const task = els.rlWrap.innerHTML;
const taskCount = els.rlCount.textContent;
els.rlCh.value = ''; els.rlQ.value = 'uid=abc'; ctx.render();
const kw = els.rlWrap.innerHTML;
els.rlQ.value = 'nope'; ctx.render();
const none = els.rlWrap.innerHTML;
ctx.setEntries([]); els.rlQ.value = ''; ctx.render();
const empty = els.rlWrap.innerHTML;
const emptyCount = els.rlCount.textContent;
process.stdout.write(JSON.stringify({
  allHasChat: all.includes('chat req model=glm'),
  allHasTask: all.includes('checkin uid=abc ok') && all.includes('keepalive uid=abc ok'),
  allHasSys: all.includes('server started'),
  allCount, newestFirst,
  taskHasCheckin: task.includes('checkin uid=abc ok'),
  taskHasKeepalive: task.includes('keepalive uid=abc ok'),
  taskHasChat: task.includes('chat req model=glm'),
  taskHasSys: task.includes('server started'),
  taskCount,
  kwHasCheckin: kw.includes('checkin uid=abc ok'),
  kwHasSys: kw.includes('server started'),
  noneEmpty: none.includes('没有符合筛选条件的运行日志'),
  emptyText: empty.includes('暂无运行日志'),
  emptyCount,
  clock: ctx.rlClock(iso(9, 2, 3)),
  clockBad: ctx.rlClock(''),
  labels: ctx.rlChLabel('task') + '/' + ctx.rlChLabel('chat') + '/' + ctx.rlChLabel('sys') + '/' + ctx.rlChLabel(''),
}));`
	f, err := os.CreateTemp(t.TempDir(), "runlog-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("run log formatting node test failed: %v\n%s", err, out)
	}
	const want = `{"allHasChat":true,"allHasTask":true,"allHasSys":true,"allCount":"4 / 4 行","newestFirst":true,` +
		`"taskHasCheckin":true,"taskHasKeepalive":true,"taskHasChat":false,"taskHasSys":false,"taskCount":"2 / 4 行",` +
		`"kwHasCheckin":true,"kwHasSys":false,"noneEmpty":true,"emptyText":true,"emptyCount":"—",` +
		`"clock":"09:02:03","clockBad":"--:--:--","labels":"任务/对话/系统/系统"}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("run log formatting=%s\nwant %s", strings.TrimSpace(string(out)), want)
	}
}

// TestUsagePageSurfacesInterruptedRequests 回归：客户端中断的请求（上游已消耗 token
// 但末帧 usage 永远到不了）必须在用量页被显式标注。
//
// 为什么需要：面板用量只按「采到的 usage」累加，而 NewAPI 等前端拿不到 usage 时会按
// 请求体本地估算补记，两边天然对不上账。用户曾据此怀疑面板漏记——这个标注一旦被
// 顺手删掉，差额又会变成查无头绪的悬案，所以在这里钉死。
func TestUsagePageSurfacesInterruptedRequests(t *testing.T) {
	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	for _, needle := range []string{`id="usGapWrap"`, `id="usGap"`} {
		if !strings.Contains(html, needle) {
			t.Errorf("index.html 用量页缺少「未计入 token」提示容器: %q", needle)
		}
	}

	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, needle := range []string{
		"interrupted_requests", // totals / by_* 的中断计数
		"$('usGapWrap')",       // 用量页提示的接线
		`<th class="num">中断</th>`,
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少中断口径接线: %q", needle)
		}
	}

	cssBytes, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), ".usgap") {
		t.Error("theme.css 缺少 .usgap 提示样式")
	}
}

// TestAppJSGatewayKeyCard 钉住「网关密钥」卡的归一与范围判定。
//
// api_keys 允许裸串（全量，历史写法）与对象（可收窄到指定供应商）两种形态，编辑器
// 必须把两者归一成同一形状，否则裸串条目在卡里会渲染成空。角标必须如实反映授权范围
// ——「全量 / 收窄」是运维判断「这把钥匙能调什么」的唯一线索，标错会直接误判权限。
//
// 2026-10：卡片新增「这把 key 能调哪些模型」预览（keyScopePreviewHtml）。预览读
// omniMapping / omniMappingOff 两个模块级快照（model_mapping 落单独文件、不在
// omniCfg 里），沙箱必须注入；同时新增断言把预览钉住——它就是为了治「改了路由忘
// 改 scope，客户端永久 403 却看不出原因」这个坑，不能悄悄退化。
func TestAppJSGatewayKeyCard(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; gateway key card test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 网关密钥（按供应商收窄');
const end = src.indexOf('async function saveOmniConfig');
if (start < 0 || end < 0 || end < start) throw new Error('gateway key region not found');
const ctx = { JSON, Math, Array, Object, String, Number, Set, isNaN, esc: s => String(s == null ? '' : s) };
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.normOmniKeys = normOmniKeys; this.keyCard = keyCard;', ctx);
ctx.omniCfg = {
  providers: [
    { name: 'raccoon', models: ['glm-5.2'] },
    { name: 'runable', models: [] },
  ],
  api_keys: ['sk-full', { key: 'sk-rc', name: '浣熊专用', providers: ['raccoon'] }],
};
ctx.omniMapping = { 'glm-5.2': 'raccoon/glm-5.2' };
ctx.omniMappingOff = {};
const keys = ctx.normOmniKeys();
const cards = keys.map((k, i) => ctx.keyCard(k, i));
// 预览口径：目录模型 + mapping 别名（别名只在「目标供应商在 scope 内」时曝光）。
// 全量卡（scope 空 = 全量）：目录 1 个（raccoon/glm-5.2；runable 目录空）
//         + 别名 glm-5.2（与目录项 ID 不同 → 不去重）→ 共 2 个、其中 1 个别名。
// 收窄卡（scope=[raccoon]）：目录仍是那 1 个 + 同一别名 → 同样 2 个、1 个别名。
const fullModelCount = /全量 · 共 <b>2<\/b> 个模型/.test(cards[0]);
const fullHasAlias = cards[0].includes('含 1 个别名');
const scopedHasAlias = cards[1].includes('含 1 个别名');
process.stdout.write(JSON.stringify({
  norm: keys,
  fullTag: cards[0].includes('<span class="tag ok">全量</span>'),
  scopedTag: cards[1].includes('<span class="tag warn">收窄</span>'),
  keepsFullKey: cards[0].includes('sk-full'),
  keepsName: cards[1].includes('浣熊专用'),
  raccoonChecked: cards[1].includes('value="raccoon" checked'),
  runableChecked: cards[1].includes('value="runable" checked'),
  fullModelCount, fullHasAlias, scopedHasAlias,
}));`
	f, err := os.CreateTemp(t.TempDir(), "gwkey-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("gateway key card node test failed: %v\n%s", err, out)
	}
	const want = `{"norm":[{"key":"sk-full","name":"","providers":[]},{"key":"sk-rc","name":"浣熊专用","providers":["raccoon"]}],` +
		`"fullTag":true,"scopedTag":true,"keepsFullKey":true,"keepsName":true,"raccoonChecked":true,"runableChecked":false,` +
		`"fullModelCount":true,"fullHasAlias":true,"scopedHasAlias":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("gateway key card=%s\nwant %s", out, want)
	}
}

// TestAppJSCreditRoutingForm 钉住「积分快过期优先」表单 ↔ credit_routing 的映射。
//
// 这个映射有两个容易搞错的点：① 留空必须**删除字段**（回落后端默认值），否则会把
// 面板打开时的空串写成 0/""，配置里凭空多出一堆无意义字段；② enabled 必须恒写，
// 否则用户勾掉「启用」后保存，字段消失 → 后端按「缺省启用」处理，等于关不掉。
func TestAppJSCreditRoutingForm(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; credit routing form test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('function collectCreditRouting');
const end = src.indexOf('async function saveOmniConfig');
if (start < 0 || end < 0 || end < start) throw new Error('credit routing region not found');
const fields = {};
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN,
  $: id => fields[id],
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) + '\nthis.collectCreditRouting = collectCreditRouting;', ctx);
function run(init, vals) {
  ctx.omniCfg = init;
  for (const k of Object.keys(fields)) delete fields[k];
  Object.assign(fields, vals);
  ctx.collectCreditRouting();
  return ctx.omniCfg.credit_routing;
}
process.stdout.write(JSON.stringify({
  filled: run({}, {
    crEnabled: { checked: true },
    crWindow: { value: ' 24h ' },
    crMin: { value: '3' },
    crRefresh: { value: '5m' },
  }),
  blanked: run({ credit_routing: { expiring_soon: '1h', min_expiring: 9, refresh_interval: '1m' } }, {
    crEnabled: { checked: false },
    crWindow: { value: '' },
    crMin: { value: '' },
    crRefresh: { value: '' },
  }),
  // 卡片没渲染出来（元素全缺）时绝不能把 enabled 翻成 false
  missing: run({ credit_routing: { enabled: true, expiring_soon: '1h' } }, {}),
}));`
	f, err := os.CreateTemp(t.TempDir(), "credit-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("credit routing form node test failed: %v\n%s", err, out)
	}
	const want = `{"filled":{"enabled":true,"expiring_soon":"24h","min_expiring":3,"refresh_interval":"5m"},` +
		`"blanked":{"enabled":false},"missing":{"enabled":true,"expiring_soon":"1h"}}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("credit routing form=%s\nwant %s", out, want)
	}
}

// TestAppJSProviderTable 钉住供应商「紧凑列表 + 编辑弹窗」。
//
// 曾经的形态是每行一张 og-card、所有字段常驻展开，七八个供应商就把页面拉成一条
// 长龙。现在改成列表（名称/类型/状态 + 模型数·账号数·endpoint）点行开弹窗。要钉住的：
//   ① 列表行数 = 供应商数，默认 provider 下拉同步；
//   ② 弹窗改字段先落 draft、点「确定」才回写 omniCfg（否则边打字边重渲染会顶掉光标）；
//   ③ 名称重复必须拦下且弹窗不关（否则保存时两个同名供应商会被后端拒掉）；
//   ④ 健康角标按类型分类：openai/newapi 缺 key vs 需账号的类型缺账号。
func TestAppJSProviderTable(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; provider table test skipped")
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
  classList: {
    _s: new Set(),
    add(c) { this._s.add(c); },
    remove(c) { this._s.delete(c); },
    contains(c) { return this._s.has(c); },
    // 双参数 toggle 必须实现：DOM 的 classList.toggle(cls, force) 第二个参数是「强制」，
    // 传 true 必加、传 false 必删。供应商页计数角标的可点态走的就是这条路；少了它，
    // provCnt 一渲染就抛 TypeError，整个供应商列表都出不来。
    toggle(c, force) {
      const want = force === undefined ? !this._s.has(c) : !!force;
      if (want) this._s.add(c); else this._s.delete(c);
      return want;
    },
  },
  querySelector: () => null,
});
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {},
  ogMsg: () => {},
  setTimeout: () => 0,
  confirm: () => true,
  // provFormHtml 里的密钥字段标签会问 isAdmin（管理员才有「查看完整」按钮）；
  // 这个垫子跑 viewer 口径，旧版表单输出保持不变。管理员口径由
  // provsec_reveal_test.go 的运行时断言单独钉。
  isAdmin: () => false,
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.renderProvEditor=renderProvEditor; this.openProvEdit=openProvEdit;' +
  ' this.onProvFormInput=onProvFormInput; this.commitProvEdit=commitProvEdit;' +
  ' this.addProvider=addProvider; this.provBadgeHtml=provBadgeHtml; this.onProvClick=onProvClick;' +
  ' this.provIsIdle=provIsIdle; this.provsHaveIdle=provsHaveIdle; this.togProvIdle=togProvIdle;', ctx);
ctx.omniCfg = { default_provider: 'raccoon', providers: [
  { name: 'atria', type: 'openai', base_url: 'https://api.atria-asi.ai/v1', api_key: 'atr_PKxxx', models: ['Atria-Dawn-Preview'], accounts: [] },
  { name: 'raccoon', type: 'raccoon', models: ['a', 'b', 'c'], accounts: [{ label: 'x' }, { label: 'y' }] },
  { name: 'runable', type: 'runable', models: ['p'], accounts: [] },
] };
ctx.renderProvEditor();
const dp = els.cfgDefaultProvider.innerHTML;
// 计数角标的文本与可点态必须在 togProvIdle() 之前取：JSON.stringify 在脚本末尾，
// 那时已切到展开态，原样读会把两个值都写成展开后的样子。
const cnt = els.provCnt.textContent;
const cntLinky = els.provCnt.classList.contains('linky');
// 2026-10：空壳供应商默认根本不渲染。runable（type=runable + accounts:[]）是空壳，
// 表格里不该出现它，也没有说明行——只有计数角标在有空壳时变成可点的「2 / 3」。
// 默认供应商下拉仍要列全部 3 家（隐藏只动表格，不动配置载体——否则配了但暂时没
// 账号的供应商会从下拉里消失，用户以为配置丢了）。
const body = els.provBody.innerHTML;
// 展开：走 togProvIdle —— on('provCnt') 的绑定在 vm 切片之外（init 里），测试里
// 调不到那个匿名处理器，但 togProvIdle 是它唯一的动作，等价且更直白。
// 注意不能写 ctx.provShowIdle = true：那是脚本里 let 出来的变量，ctx 赋值只是加
// 同名属性，渲染读到的还是原来的 false。
ctx.togProvIdle();
const bodyShown = els.provBody.innerHTML;
const cntShown = els.provCnt.textContent;
ctx.openProvEdit(0);
const form = els.provForm.innerHTML;
const veilOn = els.provVeil.classList.contains('on');
ctx.onProvFormInput({ target: { dataset: { pf: 'name' }, value: 'atria2' } });
ctx.commitProvEdit();
const renamed = ctx.omniCfg.providers[0].name;
const veilOff = !els.provVeil.classList.contains('on');
ctx.openProvEdit(1);
ctx.onProvFormInput({ target: { dataset: { pf: 'name' }, value: 'atria2' } });
ctx.commitProvEdit();
const dupKept = ctx.omniCfg.providers[1].name;
const dupVeilStillOn = els.provVeil.classList.contains('on');
ctx.addProvider();
const newIsNew = els.provDlgTitle.textContent === '添加供应商';
process.stdout.write(JSON.stringify({
  rowCount: (body.match(/data-act="editProv"/g) || []).length,
  // 隐藏态：空壳不在表格里，计数角标显示「可见/总数」且可点
  idleHidden: !body.includes('runable') && body.includes('atria') && body.includes('raccoon'),
  cntIdle: cnt === '2 / 3',
  cntLinky: cntLinky,
  // 展开态：空壳回来，角标回到「3 个」、不可点
  rowCountShown: (bodyShown.match(/data-act="editProv"/g) || []).length,
  shownHasRunable: bodyShown.includes('runable'),
  cntShown: cntShown === '3 个',
  cntNotLinky: !els.provCnt.classList.contains('linky'),
  // 默认供应商下拉必须仍是 3 家（隐藏只动表格的证明）
  hasNames: body.includes('atria') && body.includes('raccoon'),
  hasTypeTag: body.includes('<span class="tag mute">openai</span>'),
  byline: body.includes('1 模型') && body.includes('2 账号'),
  badgeOk: ctx.provBadgeHtml({ type: 'openai', api_key: 'k' }).includes('正常'),
  badgeNoKey: ctx.provBadgeHtml({ type: 'openai' }).includes('缺 api_key'),
  badgeNoAcct: ctx.provBadgeHtml({ type: 'raccoon', accounts: [] }).includes('无账号'),
  dpOptions: (dp.match(/<option/g) || []).length,
  cnt,
  formHasKey: form.includes('atr_PKxxx'),
  formHasModels: form.includes('Atria-Dawn-Preview'),
  formHasSys: form.includes('system_prompt'),
  formHasPool: form.includes('api_keys'),
  veilOn, renamed, veilOff, dupKept, dupVeilStillOn, newIsNew,
}));`
	f, err := os.CreateTemp(t.TempDir(), "provtable-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("provider table node test failed: %v\n%s", err, out)
	}
	const want = `{"rowCount":2,"idleHidden":true,"cntIdle":true,"cntLinky":true,` +
		`"rowCountShown":3,"shownHasRunable":true,"cntShown":true,"cntNotLinky":true,"hasNames":true,"hasTypeTag":true,"byline":true,` +
		`"badgeOk":true,"badgeNoKey":true,"badgeNoAcct":true,"dpOptions":3,"cnt":"2 / 3",` +
		`"formHasKey":true,"formHasModels":true,"formHasSys":true,"formHasPool":true,` +
		`"veilOn":true,"renamed":"atria2","veilOff":true,"dupKept":"raccoon","dupVeilStillOn":true,"newIsNew":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("provider table=%s\nwant %s", out, want)
	}
}

// TestAppJSOmniConfigSaveSurvivesSplitViews 钉住「供应商页卡片按归属拆到别的页」之后，
// 保存配置不再崩、也不再顺手清空别人的字段。
//
// 为什么需要：网关密钥搬到了「配置」页、模型名映射搬到了「模型与档位」页，而那些字段
// 全都由同一个 saveOmniConfig() 汇总提交。曾经这里有三类真实故障：
//   ① 从别的页点「保存并生效」时 $('cfgDefaultProvider') 返回 null，读 .value 直接抛错；
//   ② omniCfg 只在供应商页加载，从配置页触发保存时是 null —— 旧代码直接 return，
//      用户点了保存什么也不发生；
//   ③ collectXxx 若不判空，会把「元素不在当前视图」误判成「用户要清空」，
//      于是保存一次就把快过期策略 / 第三方兜底 / 按 IP 限流 / routing.prefer 全删掉。
func TestAppJSOmniConfigSaveSurvivesSplitViews(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; omni config save test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
// 取 normalizeOmniForSave → saveOmniConfig 结束这一段。起点必须早于 normalizeOmniForSave
// 的定义，否则 saveOmniConfig 在闭包里取不到它。
const start = src.indexOf('function normalizeOmniForSave');
const end = src.indexOf('/* ── OmniGate 账号授权');
if (start < 0 || end < 0 || end < start) throw new Error('omni save region not found');

// $() 必须能表达「元素不存在」= null（卡片被拆到别的页就是这个状态），
// 所以用一个只返回已登记 id 的注册表；未登记的 id 一律 null。
const els = {};
const registered = new Set();
const mk = (id) => {
  if (!registered.has(id)) return null;
  return (els[id] = els[id] || {
    innerHTML: '', textContent: '', value: '', checked: false, hidden: false,
    classList: { _s: new Set(), add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); }, contains(c) { return this._s.has(c); } },
    querySelector: () => null,
  });
};
let sentBody = null;
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {}, ogMsg: () => {}, setTimeout: () => 0, confirm: () => true,
  loadOmniConfig: async () => { ctx.omniCfg = ctx.omniCfg || { providers: [] }; },
  loadOverview: async () => {}, loadOutbound: () => {},
  view: 'config',
  api: async (path, opts) => { sentBody = opts && opts.body ? JSON.parse(opts.body) : null; return {}; },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.saveOmniConfig=saveOmniConfig; this.collectCreditRouting=collectCreditRouting;' +
  ' this.collectReliability=collectReliability; this.collectRouting=collectRouting;', ctx);

(async () => {
  const r = {};
  // 场景 1：omniCfg 为 null（= 从「配置」页的网关密钥卡触发保存、还没进过供应商页）。
  //   旧代码直接 return "配置尚未加载"，用户点了保存什么也不发生；新代码先补齐再存。
  ctx.omniCfg = null;
  await ctx.saveOmniConfig('keyMsg');
  r.omniCfgReloaded = !!ctx.omniCfg && Array.isArray(ctx.omniCfg.providers);

  // 场景 2：omniCfg 有值但全部控件缺失（卡片已按归属拆到别的页）。
  //   collectXxx 必须「元素缺失 = 保持原值」，否则保存一次就把快过期策略 / 兜底 / 限流 / 路由全清空。
  ctx.omniCfg = {
    default_provider: 'raccoon', default_model: 'm1', mode: 'native', reasoning: true,
    routing: { prefer: ['atria', 'runable'] },
    credit_routing: { enabled: true, expiring_soon: '168h', min_expiring: 5, refresh_interval: '10m' },
    fallback: { enabled: true, models: ['raccoon/x'] },
    rate_limit: { enabled: true, trust_proxy: true, window: 60, max: 10 },
    providers: [{ name: 'raccoon', type: 'raccoon', models: ['a', 'b'], accounts: [{ label: 'x' }] }],
  };
  sentBody = null;
  await ctx.saveOmniConfig('keyMsg');
  const b = sentBody || {};
  r.creditRoutingKept = JSON.stringify(b.credit_routing) === '{"enabled":true,"expiring_soon":"168h","min_expiring":5,"refresh_interval":"10m"}';
  r.fallbackKept = !!b.fallback && b.fallback.enabled === true && b.fallback.models[0] === 'raccoon/x';
  r.rateLimitKept = !!b.rate_limit && b.rate_limit.enabled === true &&
    b.rate_limit.trust_proxy === true && b.rate_limit.max === 10;
  r.routingKept = JSON.stringify(b.routing) === '{"prefer":["atria","runable"]}';
  r.globalsKept = b.default_provider === 'raccoon' && b.default_model === 'm1' &&
    b.mode === 'native' && b.reasoning === true;
  r.providersKept = b.providers.length === 1 && b.providers[0].models.join(',') === 'a,b';

  // 场景 3：控件存在时仍要正常收集（回归保护：不能因为「可能为 null」就一律不读）。
  // 注意 cfgRoutingPrefer 已改成有序卡片列表（rrList / rrAddSel），不再从 DOM 读值：
  // 收藏在 rrWrite 里直接改 omniCfg.routing.prefer，collectRouting 只做归一。
  ctx.omniCfg = { default_provider: 'raccoon', providers: [] };
  ['cfgDefaultProvider', 'cfgDefaultModel', 'cfgMode', 'cfgReasoning',
   'crEnabled', 'crWindow', 'crMin', 'crRefresh'].forEach(registered.add, registered);
  Object.assign(els, {
    cfgDefaultProvider: { innerHTML: '', value: 'atria', classList: { add() {}, remove() {}, contains: () => false } },
    cfgDefaultModel: { value: '  md  ' },
    cfgMode: { value: 'text' },
    cfgReasoning: { checked: true },
    crEnabled: { checked: false }, crWindow: { value: '' }, crMin: { value: '' }, crRefresh: { value: '' },
  });
  // 列表里「+ 加入优先级」/「↑ ↓」最终写进 omniCfg.routing.prefer；这里直接模拟
  // 用户排好的结果，再交给 collectRouting 归一（与真实交互路径一致：rrWrite →
  // 收起 → saveOmniConfig → collectRouting）。
  ctx.omniCfg.routing = { prefer: ['  atria  ', 'runable', '', 'atria'] };
  sentBody = null;
  await ctx.saveOmniConfig();
  const b3 = sentBody || {};
  r.formValuesCollected = b3.default_provider === 'atria' && b3.default_model === 'md' &&
    b3.mode === 'text' && b3.reasoning === true && JSON.stringify(b3.routing) === '{"prefer":["atria","runable"]}';
  // 控件存在且留空 = 真的想回落默认：enabled 恒写，其余字段删除。
  r.blankedDropsDefaults = !!b3.credit_routing && b3.credit_routing.enabled === false &&
    b3.credit_routing.expiring_soon === undefined && b3.credit_routing.min_expiring === undefined &&
    b3.credit_routing.refresh_interval === undefined;

  process.stdout.write(JSON.stringify(r));
})().catch(e => { console.log('FAIL', e.stack); process.exit(1); });`
	// 注意后缀必须是 .cjs：仓库根的 package.json 声明了 "type": "module"，
	// 用 .js 会让 Node 把脚本当 ESM 加载，require 直接 ReferenceError（本机 Windows
	// 上因为 t.TempDir() 不在仓库内所以碰不到，只在服务器 CI 上炸）。
	f, err := os.CreateTemp(t.TempDir(), "omnisave-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("omni config save node test failed: %v\n%s", err, out)
	}
	const want = `{"omniCfgReloaded":true,"creditRoutingKept":true,"fallbackKept":true,` +
		`"rateLimitKept":true,"routingKept":true,"globalsKept":true,"providersKept":true,` +
		`"formValuesCollected":true,"blankedDropsDefaults":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("omni config save=%s\nwant %s", out, want)
	}
}

// TestAppJSConfigSectionTabs 钉住配置页的二级分组 Tab。
//
// 为什么需要：配置页原本是 7 张卡纵向堆成的 ~261 行 / 48 字段长表单，改一个藏在
// 最底部的值得从头滚到尾。现在按归属分 6 组（基本/密钥/任务/账号池/高级/面板账号），
// 一次只看一组。要钉住的是三件容易在后续改动里被弄坏的事：
//   ① 6 个分组与 6 个 tab 一一对应，默认停在「基本」、其余隐藏；
//   ② 切 tab 只改 hidden，不碰表单内容 —— 7 张卡仍在同一个 <form id="cfgForm"> 里，
//      loadConfig/saveConfig 口径不变，隐藏分组里的字段照样提交；
//   ③ 「面板账号」组按角色收敛：viewer 连 tab 都看不到（paccBox 本就带 data-admin，
//      不然点进去是一整片空白）。
func TestAppJSConfigSectionTabs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; config section tab test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 配置页二级分组 Tab');
const end = src.indexOf('/* 首次进入延到本轮脚本求值之后再 go()');
if (start < 0 || end < 0 || end < start) throw new Error('cfg tab region not found');

// 最小 DOM：只实现这段代码真正碰到的 API。dataset.scrollIntoView 要能被调用，
// 否则切换分组会在真实浏览器里抛错。
const els = {};
const mk = id => (els[id] = els[id] || {
  dataset: {}, innerHTML: '', textContent: '', value: '', checked: false, hidden: false,
  classList: { _s: new Set(), add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); }, contains(c) { return this._s.has(c); }, toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); } },
  querySelectorAll: () => [],
  addEventListener: () => {}, scrollIntoView: () => {},
});
let role = 'admin';
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {}, ogMsg: () => {}, setTimeout: () => 0,
  isAdmin: () => role === 'admin',
  document: {
    querySelectorAll: sel => {
      if (sel !== '.cfgsec-sec') return [];
      return ['basic', 'keys', 'tasks', 'pool', 'advanced', 'panel'].map(s => {
        const e = mk('sec-' + s);
        e.dataset.sec = s;
        return e;
      });
    },
    body: { dataset: {} },
  },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.showCfgSec=showCfgSec; this.refreshCfgSecTabs=refreshCfgSecTabs;' +
  ' this.cfgSecAllowed=cfgSecAllowed;' +
  // cfgSec 是 let，直接 this.cfgSec=cfgSec 拷的是快照，后面 showCfgSec 改了
  // 闭包里的值这边看不出来 —— 用 getter 才是实时读到当前分组。
  ' Object.defineProperty(this, "cfgSec", { get: () => cfgSec });', ctx);

const r = {};
// ① 6 个分组名与 tab 名严格一致（HTML 侧由另一处断言兜住，这里锁前端常量）。
r.sections = ['basic', 'keys', 'tasks', 'pool', 'advanced', 'panel'].join(',');

// ② 切 tab 只改 hidden、不动表单内容：每个分组的 hidden 与传入的 sec 一致，
//    且被隐藏的分组没有被清空 innerHTML（否则切走再切回，未保存的字段就丢了）。
['basic', 'keys', 'pool'].forEach((s, i) => els['sec-' + s].innerHTML = 'keep' + i);
ctx.showCfgSec('keys');
r.keysShown = els['sec-keys'].hidden === false;
r.othersHidden = ['basic', 'tasks', 'pool', 'advanced', 'panel'].every(s => els['sec-' + s].hidden === true);
r.hiddenKeepsContent = els['sec-basic'].innerHTML === 'keep0' && els['sec-pool'].innerHTML === 'keep2';
r.currentSec = ctx.cfgSec;

// ③ 非法分组名回落「基本」，不能把整页打成全隐藏。
ctx.showCfgSec('nope');
r.badSecFallsBack = ctx.cfgSec === 'basic' && els['sec-basic'].hidden === false;

// ④ 角色收敛：admin 看得到「面板账号」，viewer 看不到；viewer 停留在这组会被踢回基本。
r.adminSeesPanel = ctx.cfgSecAllowed('panel') === true;
role = 'viewer';
ctx.refreshCfgSecTabs();
r.viewerHidesPanel = ctx.cfgSecAllowed('panel') === false;
r.viewerBouncedFromPanel = ctx.cfgSec === 'basic';
process.stdout.write(JSON.stringify(r));`
	f, err := os.CreateTemp(t.TempDir(), "cfgsec-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("config section tab node test failed: %v\n%s", err, out)
	}
	const want = `{"sections":"basic,keys,tasks,pool,advanced,panel","keysShown":true,` +
		`"othersHidden":true,"hiddenKeepsContent":true,"currentSec":"keys",` +
		`"badSecFallsBack":true,"adminSeesPanel":true,"viewerHidesPanel":true,"viewerBouncedFromPanel":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("config section tabs=%s\nwant %s", out, want)
	}
}

// TestAppJSProviderSectionTabs 钉住供应商页的二级分组 Tab。
// 背景：供应商页原本 4 张卡纵向堆成长页，且一次混用三套表单写法（grid/fld、
// og-sub/og-fields、裸按钮排），同一页格子宽度与标签上下位全不一样——就是用户
// 说的"布局乱、格式不统一"。现在按归属分 4 组、字段统一收编到 grid/fld。
// 本测试锁三件事：分组名与 tab 一一对应、切 tab 只改不改内容（否则切走再切回
// 会丢掉没保存的字段）、越权分组连 tab 一起收掉（非 admin 看不到「指纹」）。
func TestAppJSProviderSectionTabs(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; provider section tab test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const start = src.indexOf('/* ── 视图内二级分组 Tab');
const end = src.indexOf('/* ── 配置页二级分组 Tab');
if (start < 0 || end < 0 || end < start) throw new Error('prov tab region not found');

const els = {};
const mk = id => (els[id] = els[id] || {
  dataset: {}, innerHTML: '', textContent: '', value: '', checked: false, hidden: false,
  classList: { _s: new Set(), add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); }, contains(c) { return this._s.has(c); }, toggle(c, on) { on ? this._s.add(c) : this._s.delete(c); } },
  querySelectorAll: () => [],
  addEventListener: () => {}, scrollIntoView: () => {},
});
let role = 'admin';
const ctx = {
  JSON, Math, Array, Object, String, Number, isNaN, console, Set,
  esc: s => String(s == null ? '' : s),
  $: id => mk(id),
  toast: () => {}, ogMsg: () => {}, setTimeout: () => 0,
  isAdmin: () => role === 'admin',
  document: {
    querySelectorAll: sel => {
      if (sel !== '.cfgsec-sec') return [];
      return ['prov', 'route', 'credit', 'fp'].map(s => {
        const e = mk('psec-' + s);
        e.dataset.sec = s;
        return e;
      });
    },
    body: { dataset: {} },
  },
};
vm.createContext(ctx);
vm.runInContext(src.slice(start, end) +
  '\nthis.showProvSec=showProvSec; this.refreshProvSecTabs=refreshProvSecTabs;' +
  ' this.provSecAllowed=provSecAllowed;' +
  ' Object.defineProperty(this, "provSec", { get: () => provSec });', ctx);

const r = {};
// ① 4 个分组名与 tab 名严格一致（HTML 侧由另一处断言兜住，这里锁前端常量）。
r.sections = ['prov', 'route', 'credit', 'fp'].join(',');

// ② 切 tab 只改 hidden、不动表单内容：隐藏分组里的字段必须保持原样，
//    否则切走再切回，没保存的供应商配置就丢了（保存读的是同一批元素）。
['prov', 'route', 'credit'].forEach((s, i) => els['psec-' + s].innerHTML = 'keep' + i);
ctx.showProvSec('credit');
r.creditShown = els['psec-credit'].hidden === false;
r.othersHidden = ['prov', 'route', 'fp'].every(s => els['psec-' + s].hidden === true);
r.hiddenKeepsContent = els['psec-prov'].innerHTML === 'keep0' && els['psec-route'].innerHTML === 'keep1';
r.currentSec = ctx.provSec;

// ③ 非法分组名回落「供应商」，不能把整页打成全隐藏。
ctx.showProvSec('nope');
r.badSecFallsBack = ctx.provSec === 'prov' && els['psec-prov'].hidden === false;

// ④ 角色收敛：admin 看得到「指纹」，viewer 看不到，且停留在这组会被踢回。
r.adminSeesFp = ctx.provSecAllowed('fp') === true;
ctx.showProvSec('fp');          // 还在 admin 态：这一下必须打得开
r.adminCanOpenFp = els['psec-fp'].hidden === false;
role = 'viewer';
ctx.refreshProvSecTabs();
r.viewerHidesFp = ctx.provSecAllowed('fp') === false;
r.viewerBouncedFromFp = ctx.provSec === 'prov';
process.stdout.write(JSON.stringify(r));`
	f, err := os.CreateTemp(t.TempDir(), "provsec-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("provider section tab node test failed: %v\n%s", err, out)
	}
	const want = `{"sections":"prov,route,credit,fp","creditShown":true,"othersHidden":true,` +
		`"hiddenKeepsContent":true,"currentSec":"credit","badSecFallsBack":true,` +
		`"adminSeesFp":true,"adminCanOpenFp":true,"viewerHidesFp":true,"viewerBouncedFromFp":true}`
	if strings.TrimSpace(string(out)) != want {
		t.Fatalf("provider section tabs=%s\nwant %s", out, want)
	}
}
