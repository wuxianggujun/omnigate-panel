package panel

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestAppJSMergeRequestRows 钉住「OmniGate 转发层折叠」的合并逻辑。
//
// 背景：客户端 → OmniGate → WB 网关 → 30 号池，两层各记一条请求记录，于是同一次
// 调用在请求记录里出现两行。转发层（/omni/*）那行的 account 在后端被写死成
// "api-key"（gateway.go:1541）——OmniGate 把 WB 当 type=openai 供应商，眼里没有
// 账号池，转发时也不知道 WB 挑了哪个号。用户看到那行只会困惑「为什么不是账号
// 轮询」，而真实账号就在紧挨着的下一行。
//
// 合并必须严格按证据来：配对依据是 **client_ip 是不是回环**（OmniGate 与 WB 网关
// 同进程同容器，回环转发时 WB 侧记的正是这类来源），不是时间戳——同毫秒或乱序都
// 可能把不相干的两行粘到一起。这里把三条硬规则钉住：
//
//  1. 只有「转发层 + 回环上流行」才合并；account 取 WB 行、client_ip/UA 取转发层。
//  2. 找不到配对必须原样保留并打 __unmatched 标记，绝不能把别的调用的账号安过来
//     （那是编造数据，比显示 api-key 更糟）。
//  3. 一条 WB 行被多条连发转发层共享时，每条转发层各保留形状——它们有不同的客户端
//     IP / 请求 ID，合成一行就看不出「两次不同调用打到同一账号」。
//
// 2026-10-14 实测暴露过两个缺陷并被修掉（此处一并钉住，防回归）：
//   - 连发转发层时只合并第一条就推进游标，第二条被整条丢掉（数据丢失）。
//   - null/非对象行被 push 进输出，渲染时 null.time 直接抛错。
func TestAppJSMergeRequestRows(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; merge request rows test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
function grab(name, endName) {
  const at = src.indexOf('function ' + name + '(');
  if (at < 0) throw new Error('not found: ' + name);
  const endAt = src.indexOf('function ' + endName + '(');
  if (endAt < 0) throw new Error('end not found: ' + endName);
  return src.slice(at, endAt);
}
const code =
  grab('isLoopbackIP', 'isOmniForwardRow') +
  grab('isOmniForwardRow', 'mergeRequestRows') +
  grab('mergeRequestRows', 'mergeRequestPair') +
  grab('mergeRequestPair', 'renderRequestMetrics');
const ctx = { console, Object, Array, String, Number };
vm.createContext(ctx);
vm.runInContext(code + '\nthis.isLoopbackIP=isLoopbackIP;this.isOmniForwardRow=isOmniForwardRow;this.mergeRequestRows=mergeRequestRows;', ctx);

const FWD = (t, ip, rid) => ({
  time: t, request_id: rid || 'req-fwd', path: '/omni/v1/chat/completions', account: 'api-key',
  model: 'deepseek-v4-pro', status: 200, ok: true, outcome: 'success', duration_ms: 100,
  prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, credit: 0, credit_known: false,
  client_ip: ip, user_agent: 'node', provider: 'workbuddy',
});
const WB = (t, acct, realm, ip, credit) => ({
  time: t, request_id: 'req-wb', path: '/v1/chat/completions', account: acct,
  model: 'deepseek-v4-pro', realm: realm, status: 200, ok: true, outcome: 'success',
  duration_ms: 95, ttfb_ms: 90, attempts: 1, prompt_tokens: 10, completion_tokens: 5, total_tokens: 15,
  credit: credit == null ? 0.01 : credit, credit_known: true, client_ip: ip, user_agent: 'Go-http-client/1.1',
});

// 基础配对：转发层的账号是写死的 "api-key"，合并后必须换成 WB 行的真实账号，
// 而来源 IP 必须换成转发层的真实客户端（WB 行记的是回环，没有信息量）。
const merged = ctx.mergeRequestRows([
  FWD('19:34:44.951', '183.218.209.26'),
  WB('19:34:44.952', 'nick', 'cn', '127.0.0.1'),
]);
// 直连 WB（客户端绕过 OmniGate 直连网关）没有转发层，必须原样保留。
const direct = ctx.mergeRequestRows([WB('t1', 'wuxianggujun', 'global', '183.218.209.26')]);
// 找不到回路环上流行：不能硬凑，账号必须保持 api-key 而不是借来别人的号。
const unmatched = ctx.mergeRequestRows([
  FWD('t1', '183.218.209.26'),
  WB('t2', 'bob', 'cn', '10.0.0.9'),
]);
// 一条 WB 行被两条连发转发层共享：两条都要保留各自的客户端 IP。
const shared = ctx.mergeRequestRows([
  FWD('t1', '1.1.1.1', 'req-a'),
  FWD('t2', '2.2.2.2', 'req-b'),
  WB('t3', 'shared', 'cn', '127.0.0.1'),
]);
// 坏行不能让渲染崩：旧实现会把 null 原样 push 进输出。
const junk = ctx.mergeRequestRows([null, FWD('t1', '1.1.1.1'), WB('t2', 'shared', 'cn', '127.0.0.1')]);

process.stdout.write(JSON.stringify({
  mergedLen: merged.length,
  mergedAccount: merged[0] && merged[0].account,
  mergedIP: merged[0] && merged[0].client_ip,
  mergedRealm: merged[0] && merged[0].realm,
  mergedCreditKnown: merged[0] && merged[0].credit_known,
  mergedHasLayers: !!(merged[0] && merged[0].__fwd && merged[0].__peer),
  mergedRid: merged[0] && merged[0].request_id,
  directLen: direct.length,
  directAccount: direct[0] && direct[0].account,
  directHasLayers: !!(direct[0] && direct[0].__fwd),
  unmatchedLen: unmatched.length,
  unmatchedFlag: unmatched[0] && unmatched[0].__unmatched,
  unmatchedAccount: unmatched[0] && unmatched[0].account,
  secondUntouched: unmatched[1] && unmatched[1].account,
  sharedLen: shared.length,
  sharedAccounts: shared.map(r => r.account),
  sharedIPs: shared.map(r => r.client_ip),
  junkLen: junk.length,
  junkAccount: junk[0] && junk[0].account,
}));`
	f, err := os.CreateTemp(t.TempDir(), "merge-request-rows-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("merge request rows node test failed: %v\n%s", err, out)
	}
	var got struct {
		MergedLen        int      `json:"mergedLen"`
		MergedAccount    string   `json:"mergedAccount"`
		MergedIP         string   `json:"mergedIP"`
		MergedRealm      string   `json:"mergedRealm"`
		MergedCreditKnwn bool     `json:"mergedCreditKnown"`
		MergedHasLayers  bool     `json:"mergedHasLayers"`
		MergedRid        string   `json:"mergedRid"`
		DirectLen        int      `json:"directLen"`
		DirectAccount    string   `json:"directAccount"`
		DirectHasLayers  bool     `json:"directHasLayers"`
		UnmatchedLen     int      `json:"unmatchedLen"`
		UnmatchedFlag    bool     `json:"unmatchedFlag"`
		UnmatchedAccount string   `json:"unmatchedAccount"`
		SecondUntouched  string   `json:"secondUntouched"`
		SharedLen        int      `json:"sharedLen"`
		SharedAccounts   []string `json:"sharedAccounts"`
		SharedIPs        []string `json:"sharedIPs"`
		JunkLen          int      `json:"junkLen"`
		JunkAccount      string   `json:"junkAccount"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("merge request rows 输出不可解析: %v\n%s", err, out)
	}
	if got.MergedLen != 1 || got.MergedAccount != "nick" || got.MergedIP != "183.218.209.26" ||
		got.MergedRealm != "cn" || !got.MergedCreditKnwn || !got.MergedHasLayers || got.MergedRid != "req-fwd" {
		t.Errorf("合并行字段不对: %+v", got)
	}
	if got.DirectLen != 1 || got.DirectAccount != "wuxianggujun" || got.DirectHasLayers {
		t.Errorf("直连 WB 行应原样保留: %+v", got)
	}
	if got.UnmatchedLen != 2 || !got.UnmatchedFlag || got.UnmatchedAccount != "api-key" || got.SecondUntouched != "bob" {
		t.Errorf("未配对必须原样保留且不借账号: %+v", got)
	}
	if got.SharedLen != 2 || strings.Join(got.SharedAccounts, ",") != "shared,shared" ||
		strings.Join(got.SharedIPs, ",") != "1.1.1.1,2.2.2.2" {
		t.Errorf("连发转发层应各自保留形状: %+v", got)
	}
	if got.JunkLen != 1 || got.JunkAccount != "shared" {
		t.Errorf("坏行应被跳过而不是让渲染崩: %+v", got)
	}
}

// TestAppJSRequestSplitToggleLock 钉住请求记录「分开看转发层」开关的接线。
//
// 默认必须合并（账号列显示真实账号、来源列显示真实客户端 IP），和用户以前直连 WB
// 时看到的样子一致；开关只是排障用的逃生门。配对判据、跳过规则、折叠条数说明都
// 要留在代码里——任何一个丢了，面板就会退回「一行 api-key 看不懂」的旧观感。
func TestAppJSRequestSplitToggleLock(t *testing.T) {
	jsBytes, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(jsBytes)
	for _, needle := range []string{
		"LS_REQ_SPLIT",                                     // 开关偏好落 localStorage，不用每次手动勾
		"const rows = reqSplitView ? list : mergeRequestRows(list);", // 渲染走合并数据
		"if ($('reqSplit')) {",                             // 顶层绑定
		"localStorage.setItem(LS_REQ_SPLIT,",               // 偏好持久化
		"const foldNote = folded > 0",                      // 折叠条数要说出来，否则像渲染漏行
	} {
		if !strings.Contains(js, needle) {
			t.Errorf("app.js 缺少转发层开关接线: %q", needle)
		}
	}

	htmlBytes, err := os.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(htmlBytes)
	if !strings.Contains(html, `id="reqSplit"`) {
		t.Error("index.html 请求记录工具栏缺少「分开看转发层」复选框")
	}
	if !strings.Contains(html, "分开看转发层") {
		t.Error("index.html 「分开看转发层」复选框缺少可见文案")
	}

	cssBytes, err := os.ReadFile("theme.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cssBytes), ".fbar label.fchk") {
		t.Error("theme.css 缺少筛选栏复选框横排样式（不写会把文字顶到下一行）")
	}
}

// TestAppJSRequestLogTextMergedLayers 钉住合并行 tooltip 能看到两层原文。
//
// 合并只影响「怎么显示」，被合并掉的那两行原文必须仍可核对：账号从哪来、积分从哪
// 来、转发层的 request_id 是什么。看不到原文的话，用户就无法验证面板没有编造数据。
// 未配对的转发层则必须明说「未找到对应的上游记录」——不说清的话，那一行的
// account=api-key 会让人以为账号突然没了。
func TestAppJSRequestLogTextMergedLayers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; merged layer tooltip test skipped")
	}
	script := `const fs = require('fs');
const vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8');
const reqStart = src.indexOf('function requestLogText');
const reqEnd = src.indexOf('function fmtBytes');
if (reqStart < 0 || reqEnd < 0) throw new Error('request log helpers not found');
const escStart = src.indexOf('function esc(');
const escEnd = src.indexOf('function ago(');
const fmtStart = src.indexOf('function fmtTok(');
const fmtEnd = src.indexOf('function usStat(');
const ctx = { Date, Number, String, Math, RegExp, isNaN };
vm.createContext(ctx);
vm.runInContext(src.slice(escStart, escEnd) + src.slice(fmtStart, fmtEnd) + src.slice(reqStart, reqEnd) + '\nthis.requestLogText=requestLogText;', ctx);
const time = new Date(2026, 9, 14, 19, 34, 44).toISOString();
const fwd = { time, status: 200, outcome: 'success', model: 'deepseek-v4-pro', account: 'api-key', duration_ms: 100, prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, client_ip: '183.218.209.26', user_agent: 'node', request_id: 'req-fwd' };
const peer = { time, status: 200, outcome: 'success', model: 'deepseek-v4-pro', account: 'nick', realm: 'cn', duration_ms: 95, prompt_tokens: 10, completion_tokens: 5, total_tokens: 15, credit: 0.01, credit_known: true, client_ip: '127.0.0.1', user_agent: 'Go-http-client/1.1', request_id: 'req-wb' };
const merged = Object.assign({}, fwd, { account: peer.account, realm: peer.realm, credit: peer.credit, credit_known: true, __fwd: fwd, __peer: peer });
const missingPeer = Object.assign({}, fwd, { __unmatched: true });
process.stdout.write(JSON.stringify({
  merged: ctx.requestLogText(merged),
  unmatched: ctx.requestLogText(missingPeer),
}));`
	f, err := os.CreateTemp(t.TempDir(), "merged-layers-tooltip-*.cjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(script); err != nil {
		t.Fatal(err)
	}
	f.Close()
	out, err := exec.Command(node, f.Name(), "app.js").CombinedOutput()
	if err != nil {
		t.Fatalf("merged layer tooltip node test failed: %v\n%s", err, out)
	}
	var got struct {
		Merged    string `json:"merged"`
		Unmatched string `json:"unmatched"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("merged layer tooltip 输出不可解析: %v\n%s", err, out)
	}
	for _, needle := range []string{
		"转发层(OmniGate)", // 两层原文都能看到：账号/积分的出处可核对
		"上游(WB网关)",     //
		"req-fwd",          // 转发层请求 ID
		"req-wb",           // 上游请求 ID
		"nick",             // 合并后的真实账号
		"183.218.209.26",   // 转发层的真实客户端 IP
	} {
		if !strings.Contains(got.Merged, needle) {
			t.Errorf("合并行 tooltip 缺少 %q: %s", needle, got.Merged)
		}
	}
	if !strings.Contains(got.Unmatched, "未找到对应的上游记录") {
		t.Errorf("未配对的转发层必须说明「未找到对应的上游记录」: %s", got.Unmatched)
	}
}
