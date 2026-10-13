package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

func TestModelMappingStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	s := NewModelMappingStore(path)

	if got := s.Resolve("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("unmapped Resolve = %q, want identity", got)
	}
	if err := s.Set("gpt-4o", "geminiweb/gemini-pro"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("", "x"); err == nil {
		t.Fatal("empty alias must be rejected")
	}
	if err := s.Set("a", "a"); err == nil {
		t.Fatal("alias == target must be rejected")
	}
	if got := s.Resolve("gpt-4o"); got != "geminiweb/gemini-pro" {
		t.Fatalf("Resolve = %q", got)
	}
	// Persistence.
	s2 := NewModelMappingStore(path)
	if got := s2.Resolve("gpt-4o"); got != "geminiweb/gemini-pro" {
		t.Fatalf("persisted Resolve = %q", got)
	}
	if !s2.Delete("gpt-4o") {
		t.Fatal("Delete returned false")
	}
	if s2.Delete("gpt-4o") {
		t.Fatal("second Delete should be false")
	}
	if got := NewModelMappingStore(path).Resolve("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("deletion did not persist: %q", got)
	}
}

func TestRouteAppliesModelMapping(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.Set("gpt-4o", "fake/m"); err != nil {
		t.Fatal(err)
	}
	pn, m := g.route("gpt-4o")
	if pn != "fake" || m != "m" {
		t.Fatalf("route(gpt-4o) = (%q,%q), want (fake,m)", pn, m)
	}
	if got := g.ProviderOf("gpt-4o"); got != "fake" {
		t.Fatalf("ProviderOf = %q", got)
	}
	// A bare target falls back to the default provider.
	if err := g.modelMap.Set("quick", "m"); err != nil {
		t.Fatal(err)
	}
	if pn, m := g.route("quick"); pn != "fake" || m != "m" {
		t.Fatalf("route(quick) = (%q,%q)", pn, m)
	}
}

// TestRoutePreferByModelExistence 钉住 routing.prefer：裸模型名按偏好顺序选第一个
// 「模型目录里真有该模型」的供应商；都没命中回落 default_provider；带前缀的请求
// 不受偏好影响；没有偏好时保持旧行为。
func TestRoutePreferByModelExistence(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.providers["alt"] = &scriptProvider{}
	g.order = append(g.order, "alt")
	g.cfg.Routing = &config.RoutingConfig{Prefer: []string{"alt", "fake"}}
	g.modelCache["fake"] = modelEntry{models: []openai.Model{{ID: "m1"}, {ID: "vendor/only-fake"}}}
	g.modelCache["alt"] = modelEntry{models: []openai.Model{{ID: "m2"}, {ID: "m1"}}}

	// 两个目录都有 m1 → 取偏好里靠前的 alt。
	if pn, m := g.route("m1"); pn != "alt" || m != "m1" {
		t.Fatalf("route(m1) = (%q,%q), want (alt,m1)", pn, m)
	}
	// 只有 fake 有（末段匹配）→ 跳过没有的 alt，落到 fake。
	if pn, m := g.route("only-fake"); pn != "fake" || m != "only-fake" {
		t.Fatalf("route(only-fake) = (%q,%q), want (fake,only-fake)", pn, m)
	}
	// 都没有 → 回落 default_provider。
	if pn, m := g.route("nope"); pn != "fake" || m != "nope" {
		t.Fatalf("route(nope) = (%q,%q), want (fake,nope)", pn, m)
	}
	// 带前缀 → 前缀优先，不受偏好影响（fake 在偏好里靠后也照样命中）。
	if pn, m := g.route("fake/m2"); pn != "fake" || m != "m2" {
		t.Fatalf("route(fake/m2) = (%q,%q), want (fake,m2)", pn, m)
	}
	// 空模型名 → 默认模型。
	if pn, m := g.route(""); pn != "fake" || m != "m" {
		t.Fatalf("route(\"\") = (%q,%q), want (fake,m)", pn, m)
	}
	// 没有偏好 = 旧行为（一律 default_provider）。
	g.cfg.Routing = nil
	if pn, m := g.route("m1"); pn != "fake" || m != "m1" {
		t.Fatalf("no-prefer route(m1) = (%q,%q), want (fake,m1)", pn, m)
	}
	// 偏好里写了不存在的 provider：跳过，不 panic。
	g.cfg.Routing = &config.RoutingConfig{Prefer: []string{"ghost", "alt"}}
	if pn, m := g.route("m1"); pn != "alt" || m != "m1" {
		t.Fatalf("ghost-prefer route(m1) = (%q,%q), want (alt,m1)", pn, m)
	}
}

// TestModelMappingChainRoundTrip 覆盖「别名 → 目标链」（引流链）：Resolve 取首个
// 目标，Chain 返回完整链；写入时去空/去重/去自指；多目标可持久化。
func TestModelMappingChainRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	s := NewModelMappingStore(path)

	if err := s.SetTargets("ds", []string{"raccoon/sn-deepseek-v4-1-flash", "runable/deepseek/deepseek-v4.1-flash"}); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}
	if got := s.Resolve("ds"); got != "raccoon/sn-deepseek-v4-1-flash" {
		t.Fatalf("Resolve = %q", got)
	}
	chain := s.Chain("ds")
	if len(chain) != 2 || chain[1] != "runable/deepseek/deepseek-v4.1-flash" {
		t.Fatalf("Chain = %+v", chain)
	}
	// 去空 / 去重 / 去自指，保持顺序。
	if err := s.SetTargets("x", []string{"", "a", "a", "x", "b"}); err != nil {
		t.Fatalf("SetTargets: %v", err)
	}
	if got := s.Chain("x"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("cleaned chain = %+v", got)
	}
	if err := s.SetTargets("x", []string{"x"}); err == nil {
		t.Fatal("alias == target must be rejected")
	}
	if err := s.SetTargets("", []string{"a"}); err == nil {
		t.Fatal("empty alias must be rejected")
	}
	// 多目标持久化。
	if got := NewModelMappingStore(path).Chain("ds"); len(got) != 2 {
		t.Fatalf("persisted chain = %+v", got)
	}
}

// TestModelMappingJSONBackCompat 钉住落盘/接口形状：单目标是裸字符串（与旧格式
// 字节级兼容），多目标才是数组；两种写法都要能读回。
func TestModelMappingJSONBackCompat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	if err := os.WriteFile(path, []byte(`{"one":"fake/m","two":["fake/a","fake/b"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewModelMappingStore(path)
	if got := s.Resolve("one"); got != "fake/m" {
		t.Fatalf("string-form Resolve = %q", got)
	}
	if got := s.Chain("two"); len(got) != 2 || got[0] != "fake/a" {
		t.Fatalf("array-form Chain = %+v", got)
	}
	raw, err := json.Marshal(s.All()["one"])
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"fake/m"` {
		t.Fatalf("single target marshals as %s, want a bare string", raw)
	}
}

// TestModelChainFallbacks 覆盖引流链在路由层的展开：主目标之后的目标被解析成
// (provider, model) 回退候选；单目标 / 未映射没有回退。
func TestModelChainFallbacks(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.SetTargets("ds", []string{"fake/primary", "fake/backup"}); err != nil {
		t.Fatal(err)
	}
	if pn, m := g.route("ds"); pn != "fake" || m != "primary" {
		t.Fatalf("route(ds) = (%q,%q), want (fake,primary)", pn, m)
	}
	fb := g.modelChainFallbacks("ds")
	if len(fb) != 1 || fb[0].Provider != "fake" || fb[0].Model != "backup" {
		t.Fatalf("chain fallbacks = %+v", fb)
	}
	if err := g.modelMap.Set("one", "fake/only"); err != nil {
		t.Fatal(err)
	}
	if fb := g.modelChainFallbacks("one"); fb != nil {
		t.Fatalf("single-target should have no chain fallback: %+v", fb)
	}
	if fb := g.modelChainFallbacks("nope"); fb != nil {
		t.Fatalf("unmapped should have no chain fallback: %+v", fb)
	}
}

// namedProvider is a scriptProvider with a settable name, so a test can build a
// gateway with two distinct providers (primary + chain fallback).
type namedProvider struct {
	scriptProvider
	name string
}

func (p *namedProvider) Name() string { return p.name }

// TestModelChainFallbackTriesNextTarget 端到端验证引流链：主目标供应商失败（如额度
// 耗尽 / 无可用账号）时，HandleChat 自动改试链上下一个供应商，并把请求记到真正服务
// 它的供应商名下。
func TestModelChainFallbackTriesNextTarget(t *testing.T) {
	bad := &namedProvider{name: "prim", scriptProvider: scriptProvider{events: []provider.Event{
		{Type: provider.EventError, Text: "no credits"},
	}}}
	good := &namedProvider{name: "alt", scriptProvider: scriptProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "ok"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}}
	g := &Gateway{
		cfg:        &config.Config{DefaultProvider: "prim", DefaultModel: "m"},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{"prim": bad, "alt": good},
		order:      []string{"prim", "alt"},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
		convs:      map[string]*conv{},
		pending:    map[string]raccoonPending{},
	}
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.SetTargets("x", []string{"prim/bad", "alt/good"}); err != nil {
		t.Fatal(err)
	}
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), &openai.ChatRequest{
		Model:    "x",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)
	if !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("expected fallback text, got: %s", w.Body.String())
	}
	if meta.Provider != "alt" {
		t.Fatalf("served by %q, want alt", meta.Provider)
	}
}

// TestModelChainFallbackTargetIsNotAliasResolved 钉住：链上回退目标即使与某个别名同名，
// 也必须按字面 "provider/model" 解析，不能被别名表二次解析（否则回退会静默失效）。
func TestModelChainFallbackTargetIsNotAliasResolved(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.SetTargets("A", []string{"fake/primary", "fake/backup"}); err != nil {
		t.Fatal(err)
	}
	// 人为构造一个与回退目标同名的别名：若被二次解析，会变成 fake/elsewhere。
	if err := g.modelMap.Set("fake/backup", "fake/elsewhere"); err != nil {
		t.Fatal(err)
	}
	fb := g.modelChainFallbacks("A")
	if len(fb) != 1 || fb[0].Provider != "fake" || fb[0].Model != "backup" {
		t.Fatalf("chain fallback = %+v, want fake/backup (literal, not alias-resolved)", fb)
	}
}

func TestListModelsExposesAliasInScope(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.Set("gpt-4o", "fake/m"); err != nil {
		t.Fatal(err)
	}

	all := map[string]bool{}
	for _, m := range g.ListModelsFor(context.Background(), nil) {
		all[m.ID] = true
	}
	if !all["gpt-4o"] {
		t.Fatalf("alias not listed: %+v", all)
	}
	// Out of scope for a key restricted to another provider → not listed.
	for _, m := range g.ListModelsFor(context.Background(), []string{"other"}) {
		if m.ID == "gpt-4o" {
			t.Fatalf("alias leaked to an out-of-scope key")
		}
	}
}
