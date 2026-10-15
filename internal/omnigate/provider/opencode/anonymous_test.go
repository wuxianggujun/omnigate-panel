package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// 匿名会话的形态断言：ses_ + 12 位 hex + 14 位 base62；时间部分的低 12 位必须落在
// counter 1-63 的窗口（实测 0xFC0-0xFFF 之外即 403，即使 ses_ 形态正确）。
var (
	anonSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
	anonTimeRe    = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)
	hex40Re       = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// anonServer 起一个假匿名上游：记录请求，回一段流式正文。
func anonServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 匿名通道的请求形态必须与实测通过门禁的完全一致：字面量 public 凭证、官方 CLI
// UA、x-opencode-client: cli、四个同值的 ses_ 会话头、40 位 hex 项目头、禁带
// x-opencode-request；体里 stream:true、tools 含齐官方五件套。
//
// 上游门禁一变，这条测试就是第一现场（改它之前先重跑一遍探测，再改 anonymous.go）。
func TestAnonymousLaneMirrorsOfficialClient(t *testing.T) {
	rec := &recorder{}
	anon := anonServer(t, rec)

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: anon.URL, MetaURL: emptyMeta(t)})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{
		Model:    "longcat-2.5-preview-free",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	})
	if err != nil {
		t.Fatalf("匿名通道应能直接服务免费模型（零密钥），得到 %v", err)
	}
	st.Close()

	seen := rec.all()
	if len(seen) != 1 {
		t.Fatalf("应恰好打一次匿名上游，得到 %d 次", len(seen))
	}
	s := seen[0]
	if s.auth != "Bearer public" {
		t.Errorf("匿名凭证必须是字面量 Bearer public，得到 %q", s.auth)
	}
	if s.ua != anonUserAgent {
		t.Errorf("UA 必须镜像官方 CLI 形态 %q，得到 %q", anonUserAgent, s.ua)
	}
	if s.client != "cli" {
		t.Errorf("x-opencode-client 必须是 cli，得到 %q", s.client)
	}
	if s.session == "" || s.session != s.sessionID || s.session != s.affinity || s.session != s.xSessionID {
		t.Errorf("四个会话头必须同值：session=%q session-id=%q affinity=%q X-Session-Id=%q",
			s.session, s.sessionID, s.affinity, s.xSessionID)
	}
	if !anonSessionRe.MatchString(s.session) || !anonTimeRe.MatchString(s.session[4:16]) {
		t.Errorf("会话 id 形态不符官方（含降序时间窗口）：%q", s.session)
	}
	if !hex40Re.MatchString(s.project) {
		t.Errorf("x-opencode-project 必须是裸 40 位 hex，得到 %q", s.project)
	}
	if s.requestHdr != "" {
		t.Errorf("禁带 x-opencode-request（门禁点名拒绝），得到 %q", s.requestHdr)
	}
	if s.path != "/v1/chat/completions" {
		t.Errorf("匿名端点路径应为 /v1/chat/completions，得到 %q", s.path)
	}

	var body struct {
		Stream bool `json:"stream"`
		Tools  []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(s.body, &body); err != nil {
		t.Fatalf("请求体解析失败: %v（body=%s）", err, s.body)
	}
	if !body.Stream {
		t.Error("匿名请求体必须 stream:true（门禁要求）")
	}
	names := map[string]bool{}
	for _, tl := range body.Tools {
		names[tl.Function.Name] = true
	}
	for _, want := range []string{"shell", "edit", "glob", "grep", "read"} {
		if !names[want] {
			t.Errorf("匿名请求体缺少官方工具 %q（缺一即 403）", want)
		}
	}
}

// 非免费模型不走匿名通道：别让付费请求先去撞免费门禁白费一趟。
func TestAnonymousNotUsedForPaidModel(t *testing.T) {
	anonRec := &recorder{}
	anon := anonServer(t, anonRec)
	keyRec := &recorder{}
	keySrv := okServer(t, keyRec)

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: anon.URL,
		GoKeys: []string{"g"}, GoBase: keySrv.URL, ZenBase: keySrv.URL, MetaURL: emptyMeta(t)})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "claude-sonnet-5"})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if n := len(anonRec.all()); n != 0 {
		t.Fatalf("付费模型不该打匿名通道，实际 %d 次", n)
	}
	if n := len(keyRec.all()); n != 1 {
		t.Fatalf("付费模型应走密钥池，实际 %d 次", n)
	}
}

// 匿名被门禁拒（403 FreeTierError）时回落到密钥池：免费模型在有密钥的部署里
// 不该因为匿名门禁抖动而失败。
func TestAnonymousFallsBackToKeys(t *testing.T) {
	anon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	}))
	defer anon.Close()
	keyRec := &recorder{}
	keySrv := okServer(t, keyRec)

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: anon.URL,
		GoKeys: []string{"g"}, GoBase: keySrv.URL, MetaURL: emptyMeta(t)})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "step-5-preview-free"})
	if err != nil {
		t.Fatalf("匿名被拒后应用密钥池兜住，得到 %v", err)
	}
	st.Close()
	if n := len(keyRec.all()); n != 1 {
		t.Fatalf("应回落到密钥池一次，实际 %d 次", n)
	}
}

// 零密钥 + 匿名被拒：报错必须把路指清楚（门禁/密钥），而不是丢一个裸 403。
func TestAnonymousNoKeysErrorIsActionable(t *testing.T) {
	anon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"only within OpenCode"}}`)
	}))
	defer anon.Close()

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: anon.URL, MetaURL: emptyMeta(t)})
	_, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "longcat-2.5-preview-free"})
	if err == nil {
		t.Fatal("两条通道都不可用时应报错")
	}
	if !strings.Contains(err.Error(), "门禁") && !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("报错要点明匿名门禁或密钥（让人知道下一步做什么），得到 %v", err)
	}
}

// 零密钥 + 模型不在免费范围（匿名开启）：零密钥没有别的通道可回落——代价上就是
// 「值得先问一次匿名通道」，不再本地预判拦截（big-pickle 这类名字不含 free 的免费
// 模型因此冷启动也能用）；上游拒绝后的报错必须可操作（指路密钥）。
func TestAnonymousNoKeysPaidModelErrorIsActionable(t *testing.T) {
	rec := &recorder{}
	anon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"only within OpenCode"}}`)
	}))
	defer anon.Close()

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: anon.URL, MetaURL: emptyMeta(t)})
	_, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "claude-sonnet-5"})
	if err == nil {
		t.Fatal("付费模型 + 零密钥应报错")
	}
	if n := len(rec.all()); n != 1 {
		t.Fatalf("零密钥时应先尝试一次匿名通道，实际 %d 次", n)
	}
	if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("报错应提示配置密钥，得到 %v", err)
	}
}

// 工具合并语义（对应 2026-10-15 的 P2/P3/P5/P6 四项实测）：五件套齐即可，
// 额外工具可留；同名项用官方定义替换（客户端可能来自别的 opencode 版本，
// 逐字节差异不赌）。
func TestMergeAnonTools(t *testing.T) {
	client := []openai.Tool{
		{Type: "function", Function: openai.ToolDef{Name: "my_custom_tool"}},
		{Type: "function", Function: openai.ToolDef{Name: "shell", Description: "老版本客户端的 shell"}},
	}
	merged := mergeAnonTools(client)
	b, _ := json.Marshal(merged)
	var arr []struct {
		Function struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"function"`
	}
	if err := json.Unmarshal(b, &arr); err != nil {
		t.Fatalf("合并结果不是合法工具数组: %v", err)
	}
	names := map[string]int{}
	for _, tl := range arr {
		names[tl.Function.Name]++
		if tl.Function.Name == "shell" && tl.Function.Description == "老版本客户端的 shell" {
			t.Error("同名工具应被官方定义替换，而不是保留客户端那份")
		}
	}
	if names["my_custom_tool"] != 1 {
		t.Errorf("客户端自定义工具必须保留，得到 %v", names)
	}
	for _, want := range []string{"shell", "edit", "glob", "grep", "read"} {
		if names[want] != 1 {
			t.Errorf("官方五件套里 %q 应恰好一份，得到 %v", want, names)
		}
	}
	if len(arr) != 6 {
		t.Errorf("合并结果应为 1 自定义 + 5 官方 = 6，得到 %d", len(arr))
	}
	if got := len(mergeAnonTools(nil)); got != 5 {
		t.Errorf("无客户端工具时应为 5 件套本身，得到 %d", got)
	}
}

// 内嵌工具定义表必须能解析、且恰好是门禁要求的五件套（名字逐个校验：改名即 403）。
func TestCanonicalAnonToolsEmbed(t *testing.T) {
	canon, names := canonicalAnonTools()
	if len(canon) != 5 {
		t.Fatalf("官方工具表应为 5 件，得到 %d", len(canon))
	}
	for _, want := range []string{"shell", "edit", "glob", "grep", "read"} {
		if !names[want] {
			t.Errorf("官方工具表缺 %q", want)
		}
	}
	if len(names) != 5 {
		t.Errorf("官方工具表名字集合应恰好 5 个，得到 %v", names)
	}
}

// 会话/项目铸造：稳定（提示词缓存亲和）、形态正确（门禁逐字校验）、对话间可区分。
func TestAnonSessionMint(t *testing.T) {
	a1 := anonSession("conv-1")
	if a1 != anonSession("conv-1") {
		t.Error("同一对话信号必须铸出恒定会话 id")
	}
	if a1 == anonSession("conv-2") {
		t.Error("不同对话应得到不同会话 id")
	}
	if !anonSessionRe.MatchString(a1) {
		t.Errorf("会话形态不符：%q", a1)
	}
	if !anonTimeRe.MatchString(a1[4:16]) {
		t.Errorf("时间部分不在门禁接受的 counter 窗口：%q", a1[4:16])
	}
	p1 := anonProject("omnigate:oc")
	if !hex40Re.MatchString(p1) || p1 != anonProject("omnigate:oc") {
		t.Errorf("项目头应为稳定的 40 位 hex，得到 %q", p1)
	}
}

// 免费判定/路由资格：
//   - 零密钥供应商没有可回落的通道 → 任何模型都先试匿名（免费模型含 big-pickle
//     这类名字不带 free 的直接可用；非免费的由上游给出确切结论）；
//   - 配了密钥的供应商 → 只有判定为免费的才匿名优先（名字含 free / 元数据全 0 价 /
//     匿名目录在列）；
//   - 匿名关闭时一律不命中。
func TestIsAnonEligible(t *testing.T) {
	zero := New(Options{Name: "oc", Anonymous: true, MetaURL: emptyMeta(t)})
	if !zero.isAnonEligible("longcat-2.5-preview-free") {
		t.Error("零密钥时免费模型应命中匿名通道")
	}
	if !zero.isAnonEligible("claude-sonnet-5") {
		t.Error("零密钥时没有别的通道可回落，任何模型都应先试匿名")
	}
	p := New(Options{Name: "oc", Anonymous: true, GoKeys: []string{"g"}, MetaURL: emptyMeta(t)})
	if !p.isAnonEligible("longcat-2.5-preview-free") {
		t.Error("名字带 free 应命中匿名通道")
	}
	if p.isAnonEligible("claude-sonnet-5") {
		t.Error("有密钥时付费模型不该命中匿名通道")
	}
	// 手动放一份「新鲜」的免费目录缓存 → 名字不含 free 的免费模型也应命中。
	p.anonCat.set = map[string]bool{"big-pickle": true}
	p.anonCat.ids = []string{"big-pickle"}
	p.anonCat.next = time.Now().Add(time.Hour)
	if !p.isAnonEligible("big-pickle") {
		t.Error("匿名免费目录在列的模型应命中")
	}
	off := New(Options{Name: "oc2", MetaURL: emptyMeta(t)})
	if off.isAnonEligible("longcat-2.5-preview-free") {
		t.Error("匿名通道关闭时不该命中")
	}
}

// 纯匿名供应商（零密钥）的模型目录：两个 tier 的公开目录 + 匿名免费目录的并集；
// 免费目录里的模型打上 IsFree；免费目录请求带 Bearer public。
func TestListModelsAnonymousOnly(t *testing.T) {
	zen := catalogSrv(t, "shared", "zen-only")
	goSrv := catalogSrv(t, "shared", "go-only")

	var freeAuth, freeUA, freeClient string
	free := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		freeAuth = r.Header.Get("Authorization")
		freeUA = r.Header.Get("User-Agent")
		freeClient = r.Header.Get("x-opencode-client")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "big-pickle"}, {"id": "shared"}},
		})
	}))
	t.Cleanup(free.Close)

	p := New(Options{Name: "oc", Anonymous: true, AnonBase: free.URL,
		ZenBase: zen.URL, GoBase: goSrv.URL, MetaURL: emptyMeta(t)})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("纯匿名供应商必须能列模型，得到 %v", err)
	}
	got := byID(models)
	for _, want := range []string{"shared", "zen-only", "go-only", "big-pickle"} {
		if _, ok := got[want]; !ok {
			t.Errorf("目录缺 %q（得到 %+v）", want, models)
		}
	}
	if !got["big-pickle"].IsFree {
		t.Error("匿名免费目录里的模型应标记 IsFree")
	}
	if freeAuth != "Bearer public" || freeUA != anonUserAgent || freeClient != "cli" {
		t.Errorf("免费目录请求也要带镜像头：auth=%q ua=%q client=%q", freeAuth, freeUA, freeClient)
	}
}

// Ready 的口径：匿名开 = 零密钥也 Ready（面板不按空壳隐藏）；匿名关 = 只看密钥。
func TestReadyCountsAnonymousLane(t *testing.T) {
	if p := New(Options{Name: "oc", Anonymous: true}); !p.Ready() {
		t.Error("匿名通道开启时零密钥也应 Ready")
	}
	if p := New(Options{Name: "oc"}); p.Ready() {
		t.Error("匿名关闭且无密钥时不该 Ready")
	}
	if p := New(Options{Name: "oc", Anonymous: true, GoKeys: []string{"g"}}); !p.Ready() {
		t.Error("有密钥时当然 Ready")
	}
}
