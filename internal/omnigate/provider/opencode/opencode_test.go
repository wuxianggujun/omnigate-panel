package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// sseOK 是一段最小的合法 OpenAI 流式响应。
const sseOK = "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"

// recorder 记录上游收到的请求特征，供断言使用。
type recorder struct {
	mu   sync.Mutex
	seen []reqInfo
}

type reqInfo struct {
	path    string
	auth    string
	ua      string
	client  string
	session string
}

func (r *recorder) add(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, reqInfo{
		path:    req.URL.Path,
		auth:    req.Header.Get("Authorization"),
		ua:      req.Header.Get("User-Agent"),
		client:  req.Header.Get("x-opencode-client"),
		session: req.Header.Get("x-opencode-session"),
	})
}

func (r *recorder) auths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.seen))
	for _, s := range r.seen {
		out = append(out, s.auth)
	}
	return out
}

func (r *recorder) all() []reqInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]reqInfo, len(r.seen))
	copy(out, r.seen)
	return out
}

// okServer 起一个「总是成功」的假上游，并记录收到的请求。
func okServer(t *testing.T, rec *recorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.add(r)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 同一 tier 内的密钥轮换：第一把密钥 401（凭证失效）应被冷却并自动切到第二把，
// 客户端拿到的是成功响应而不是那个 401。
func TestKeyRotationWithinTier(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"Invalid credential"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", ZenKeys: []string{"bad", "good"}, ZenBase: srv.URL})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "m"})
	if err != nil {
		t.Fatalf("应轮换到可用密钥，得到 %v", err)
	}
	st.Close()

	if got := rec.auths(); len(got) != 2 || got[0] != "Bearer bad" || got[1] != "Bearer good" {
		t.Fatalf("应恰好按顺序试两把密钥，得到 %v", got)
	}

	// 冷却生效：紧接着再发一次，应直接命中 good，不再打 bad。
	st2, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	st2.Close()
	got := rec.auths()
	if last := got[len(got)-1]; last != "Bearer good" {
		t.Fatalf("冷却期内不该再用失效密钥，最后一把是 %s", last)
	}
	if len(got) != 3 {
		t.Fatalf("冷却生效时第二次请求应只打一次上游，累计请求=%v", got)
	}
}

// 某模型只在另一个 tier 上可用时，应自动落到对的一侧：go 回 400（本套餐不含该
// 模型）后继续尝试 zen，而不是把错误直接抛给客户端。
func TestFallsBackToNextTier(t *testing.T) {
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"model is not available on this plan"}}`)
	}))
	defer goSrv.Close()
	zenSrv := okServer(t, nil)

	p := New(Options{Name: "oc", GoKeys: []string{"g"}, ZenKeys: []string{"z"},
		GoBase: goSrv.URL, ZenBase: zenSrv.URL, Prefer: TierGo})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "m"})
	if err != nil {
		t.Fatalf("go tier 不支持该模型时应回落到 zen，得到 %v", err)
	}
	st.Close()
}

// prefer 决定先走哪个 tier：默认 go（订阅套餐优先），显式 zen 时可反转。
func TestTierOrderFollowsPrefer(t *testing.T) {
	cases := []struct {
		prefer string
		want   []string
	}{
		{"", []string{TierGo, TierZen}},
		{TierGo, []string{TierGo, TierZen}},
		{TierZen, []string{TierZen, TierGo}},
	}
	for _, c := range cases {
		p := New(Options{Name: "oc", GoKeys: []string{"g"}, ZenKeys: []string{"z"}, Prefer: c.prefer})
		got := p.Tiers()
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("prefer=%q: tier 顺序 %v want %v", c.prefer, got, c.want)
		}
	}
	// 只配了一个 tier 时不应把空 tier 留在轮询里。
	p := New(Options{Name: "oc", ZenKeys: []string{"z"}})
	if got := p.Tiers(); len(got) != 1 || got[0] != TierZen {
		t.Errorf("只配 zen 时应只轮询 zen，得到 %v", got)
	}
	if (New(Options{Name: "oc"})).Ready() {
		t.Error("没有密钥时 Ready 应为 false")
	}
}

// 守卫测试：本 provider 必须如实申报身份，且绝不触碰匿名免费通道。
//
// 这条边界是刻意划的（见包注释）：匿名 tier 用 Bearer public，但它的门禁卡的是
// 「客户端身份」——逐字节比对官方工具定义、要求官方 UA 与 x-opencode-client: cli、
// 以及特定形态的 ses_* 会话 id——目的就是排除第三方网关。任何改动如果让下面的
// 断言失败，说明它在重新引入对官方客户端的伪装，请先回去读包注释。
func TestNeverImpersonatesOfficialClientAndNeverUsesAnonymousTier(t *testing.T) {
	rec := &recorder{}
	srv := okServer(t, rec)

	p := New(Options{Name: "oc", ZenKeys: []string{"z"}, GoKeys: []string{"g"},
		ZenBase: srv.URL, GoBase: srv.URL})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{
		Model:    "m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	seen := rec.all()
	if len(seen) == 0 {
		t.Fatal("没有捕获到任何上游请求")
	}
	for _, s := range seen {
		if s.ua != UserAgent {
			t.Errorf("User-Agent 必须如实申报 %q，得到 %q", UserAgent, s.ua)
		}
		if strings.Contains(strings.ToLower(s.ua), "opencode/") {
			t.Errorf("不得把 User-Agent 伪装成官方 CLI：%q", s.ua)
		}
		if s.client != "" {
			t.Errorf("不得发送 x-opencode-client（官方客户端伪装）：%q", s.client)
		}
		if strings.Contains(strings.ToLower(s.auth), "public") {
			t.Errorf("不得使用匿名 public 凭证：%q", s.auth)
		}
		if strings.Contains(s.path, "/inference") {
			t.Errorf("不得访问匿名 tier：%q", s.path)
		}
		if s.session == "" {
			t.Errorf("必须发送 x-opencode-session（上游文档要求，用于路由与提示词缓存）")
		}
	}
}

// 会话 id 必须对同一对话稳定（否则上游提示词缓存每次都失效），且能区分不同对话。
// 网关只在 prompt 型供应商上填 ChatID，OpenAI 兼容路径是空的，所以这里主要覆盖
// 派生逻辑。
func TestSessionIDStablePerConversation(t *testing.T) {
	conv := func(first string, extra ...openai.Message) provider.ChatInput {
		msgs := []openai.Message{{Role: "user", Content: openai.Content{Text: first}}}
		msgs = append(msgs, extra...)
		return provider.ChatInput{Model: "m", Messages: msgs}
	}

	a := conv("hello")
	grown := conv("hello", openai.Message{Role: "assistant", Content: openai.Content{Text: "hi"}})
	if sessionID(a) != sessionID(grown) {
		t.Error("同一对话在追加消息后应保持同一会话 id（首条消息不变）")
	}
	if sessionID(a) == sessionID(conv("hello there")) {
		t.Error("不同对话应得到不同会话 id")
	}
	if sessionID(a) == sessionID(conv("hello", openai.Message{Role: "user"})) {
		t.Log("（提示）同首条消息的多次对话共享会话 id 是预期行为：上游按会话做亲和路由")
	}
	if got := sessionID(provider.ChatInput{ChatID: "conv-1"}); got != "conv-1" {
		t.Errorf("网关给了 ChatID 时应直接使用，得到 %q", got)
	}
	if sessionID(provider.ChatInput{Model: "m"}) == "" {
		t.Error("即使没有消息也应派生出非空会话 id")
	}
}

// 模型目录取两个 tier 的并集，并按 prefer 顺序排列、按 id 去重。
func TestListModelsUnionsTiersInPreferOrder(t *testing.T) {
	mk := func(ids ...string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				data = append(data, map[string]any{"id": id, "object": "model"})
			}
			json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	goSrv := mk("shared", "go-only")
	zenSrv := mk("shared", "zen-only")

	p := New(Options{Name: "oc", GoKeys: []string{"g"}, ZenKeys: []string{"z"},
		GoBase: goSrv.URL, ZenBase: zenSrv.URL, Prefer: TierGo, MetaURL: emptyMeta(t)})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(models))
	for _, m := range models {
		got = append(got, m.ID)
	}
	want := []string{"shared", "go-only", "zen-only"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("模型并集或顺序不对：got %v want %v", got, want)
	}
}

// 一个 tier 拉不到目录不应影响另一个 tier 的模型（单个上游抖动不该让整个目录空掉）。
func TestListModelsToleratesOneTierFailure(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()
	zenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "zen-only", "object": "model"}},
		})
	}))
	defer zenSrv.Close()

	p := New(Options{Name: "oc", GoKeys: []string{"g"}, ZenKeys: []string{"z"},
		GoBase: bad.URL, ZenBase: zenSrv.URL, MetaURL: emptyMeta(t)})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("一个 tier 失败不该让整体报错，得到 %v", err)
	}
	if len(models) != 1 || models[0].ID != "zen-only" {
		t.Fatalf("应保留可用 tier 的模型，得到 %+v", models)
	}
}

// 没配任何密钥时应给出可操作的错误，而不是静默失败或去撞匿名通道。
func TestNoKeysYieldsActionableError(t *testing.T) {
	p := New(Options{Name: "opencode"})
	if _, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "m"}); err == nil {
		t.Fatal("没有密钥时应报错")
	} else if !strings.Contains(err.Error(), "密钥") {
		t.Fatalf("错误信息应提示去面板填密钥，得到 %v", err)
	}
	if _, err := p.ListModels(context.Background()); err == nil {
		t.Fatal("没有密钥且无兜底模型时 ListModels 应报错")
	}
}

// cooling 返回池里正处于冷却的密钥数。
func cooling(p *pool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := 0
	for _, until := range p.bad {
		if now.Before(until) {
			n++
		}
	}
	return n
}

// 地区受限（403 RegionError）是**模型**的问题，不是密钥的问题：绝不能因此换密钥，
// 更不能把好密钥冷却掉——否则一次请求就赔掉整池密钥，之后正常请求全部无密钥可用。
//
// 这不是假想：cat-jam-free 在 go 套餐目录里，而从本机出口请求它恒返回 403 RegionError。
// 用户勾上这个模型就会触发。所以断言两件事：① 只打一次上游就放弃；② 池里一把都没冷却。
func TestRegionErrorDoesNotPoisonKeyPool(t *testing.T) {
	rec := &recorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"RegionError","message":"This model is not available in your country"}}`)
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", GoKeys: []string{"k1", "k2"}, ZenKeys: []string{"z1", "z2"},
		GoBase: srv.URL, ZenBase: srv.URL})

	_, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "cat-jam-free"})
	if err == nil {
		t.Fatal("地区受限应报错")
	}
	if !strings.Contains(err.Error(), "地区") {
		t.Fatalf("错误信息应点明是地区问题（而不是让人去查密钥），得到 %v", err)
	}
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("地区错误只该打一次上游（换密钥/换 tier 都没用），实际 %d 次：%v", len(got), got)
	}
	if n := cooling(p.pools[0]); n != 0 {
		t.Fatalf("go 池不该冷却任何密钥，实际冷却 %d 把", n)
	}
	if len(p.pools) > 1 {
		if n := cooling(p.pools[1]); n != 0 {
			t.Fatalf("zen 池不该冷却任何密钥，实际冷却 %d 把", n)
		}
	}
}

// 免费模型的门禁（FreeTierError）同理：那是「只对官方客户端开放」，换密钥不会有任何
// 变化，也不能把密钥冷却掉。
func TestFreeTierErrorDoesNotPoisonKeyPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`)
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", GoKeys: []string{"k1", "k2"}, GoBase: srv.URL})
	_, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "cat-jam-free"})
	if err == nil {
		t.Fatal("免费门禁应报错")
	}
	if !strings.Contains(err.Error(), "官方客户端") {
		t.Fatalf("错误信息应说明这是客户端身份门禁，得到 %v", err)
	}
	if n := cooling(p.pools[0]); n != 0 {
		t.Fatalf("不该冷却任何密钥，实际冷却 %d 把", n)
	}
}

// 反过来：真正是密钥问题时必须照旧轮换并冷却（不能因为上面两条把轮换也关掉）。
func TestInvalidCredentialStillCoolsAndRotates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad" {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"server_error","message":"Upstream request failed: Invalid credential"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, sseOK)
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", GoKeys: []string{"bad", "good"}, GoBase: srv.URL})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "m"})
	if err != nil {
		t.Fatalf("应轮换到可用密钥，得到 %v", err)
	}
	st.Close()
	if n := cooling(p.pools[0]); n != 1 {
		t.Fatalf("失效密钥应被冷却（恰好 1 把），实际 %d 把", n)
	}
}

// 上游信封有两种形态，两种都要认得出来：判据是按 type 串而不是嵌套位置。
func TestClassifyFailureHandlesBothEnvelopes(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   upstreamErrKind
	}{
		{"无效凭证（server_error 信封）", 401,
			`{"error":{"type":"server_error","message":"Upstream request failed: Invalid credential"}}`, errKey},
		{"缺密钥（AuthError 信封）", 401,
			`{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`, errKey},
		{"地区受限（type 在第一层）", 403,
			`{"type":"error","error":{"type":"RegionError","message":"not available in your country"}}`, errModel},
		{"地区受限（type 在第二层）", 403,
			`{"error":{"type":"RegionError","message":"not available in your country"}}`, errModel},
		{"免费门禁", 403,
			`{"type":"error","error":{"type":"FreeTierError","message":"only within OpenCode"}}`, errModel},
		{"缺会话头", 400,
			`{"type":"error","error":{"type":"MissingSessionID","message":"missing x-opencode-session"}}`, errModel},
		{"模型不可用（非本 tier）", 400,
			`{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`, errTier},
		{"限流", 429, `{"error":{"message":"rate limited"}}`, errKey},
		{"Cloudflare 之类无 JSON 的 403", 403, `<html>forbidden</html>`, errKey},
		{"服务端错误", 500, `{"error":{"message":"boom"}}`, errTier},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyFailure(c.status, []byte(c.body)); got != c.want {
				t.Fatalf("classifyFailure(%d) = %v, want %v", c.status, got, c.want)
			}
		})
	}
}

// 「模型不可用」= 本 tier 没有它，应回落到下一个 tier（自动落到对的一侧），
// 而不是当成终态失败——很多模型只在一个 tier 上。
func TestModelUnavailableFallsBackToNextTier(t *testing.T) {
	rec := &recorder{}
	goSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`)
	}))
	defer goSrv.Close()
	zenSrv := okServer(t, rec)

	p := New(Options{Name: "oc", GoKeys: []string{"g"}, ZenKeys: []string{"z"},
		GoBase: goSrv.URL, ZenBase: zenSrv.URL, Prefer: TierGo})
	st, err := p.StreamChat(context.Background(), nil, provider.ChatInput{Model: "zen-only-model"})
	if err != nil {
		t.Fatalf("go 没有该模型时应回落到 zen，得到 %v", err)
	}
	st.Close()
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("应恰好打两次（go 一次 + zen 一次），得到 %d 次", len(got))
	}
}

// 密钥无效时也必须能拿到模型目录：目录是公开的，带坏密钥会被 401 拒，摘掉鉴权重试即可。
//
// 为什么值得专门钉：不重试时用户看到的是「模型下拉是空的」，第一反应是查网络、
// 查供应商有没有配错，而不是查那把密钥——把「密钥错」伪装成「目录拉不到」是很难查的。
func TestListModelsRetriesWithoutKeyOnBadCredential(t *testing.T) {
	var withAuth, withoutAuth int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			withAuth++
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"server_error","message":"Upstream request failed: Invalid credential"}}`)
			return
		}
		withoutAuth++
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "public-a"}, {"id": "public-b"}},
		})
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", ZenKeys: []string{"bad"}, ZenBase: srv.URL, MetaURL: emptyMeta(t)})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("密钥无效不该连目录都拿不到，得到 %v", err)
	}
	if len(models) != 2 || models[0].ID != "public-a" {
		t.Fatalf("应拿到公开目录，得到 %+v", models)
	}
	if withAuth != 1 || withoutAuth != 1 {
		t.Fatalf("应「带密钥一次 → 摘掉密钥重试一次」，实际 带=%d 不带=%d", withAuth, withoutAuth)
	}
}

// 密钥有效时不该多打一次：带密钥拿到的目录可能比公开目录更全（付费可见范围）。
func TestListModelsUsesKeyWhenValid(t *testing.T) {
	n := 0
	sawAuth := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if r.Header.Get("Authorization") == "Bearer good" {
			sawAuth = true
		}
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   []map[string]any{{"id": "paid-only"}},
		})
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", ZenKeys: []string{"good"}, ZenBase: srv.URL, MetaURL: emptyMeta(t)})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "paid-only" {
		t.Fatalf("应拿到带密钥的目录，得到 %+v", models)
	}
	if n != 1 || !sawAuth {
		t.Fatalf("密钥有效时应只打一次且带鉴权，实际 %d 次 带鉴权=%v", n, sawAuth)
	}
}

// 密钥无效会同时打两次（带/不带），但目录仍然拿到——顺带确认不会把错误体的原始
// 大响应整段塞进报错（上游出错时可能回一页 HTML）。
func TestListTierErrorIsClipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, strings.Repeat("x", 5000))
	}))
	defer srv.Close()

	p := New(Options{Name: "oc", ZenKeys: []string{"k"}, ZenBase: srv.URL})
	_, err := p.ListModels(context.Background())
	if err == nil {
		t.Fatal("500 应报错")
	}
	if len(err.Error()) > 600 {
		t.Fatalf("报错应被裁剪，实际 %d 字符", len(err.Error()))
	}
}
