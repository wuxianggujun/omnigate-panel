package gateway

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// catalogProvider 是一个只提供模型目录的假上游（openai 型：无账号，不需要鉴权目录）。
type catalogProvider struct {
	name   string
	models []openai.Model
}

func (p *catalogProvider) Name() string { return p.name }
func (p *catalogProvider) Type() string { return "openai" }
func (p *catalogProvider) ListModels(context.Context) ([]openai.Model, error) {
	return p.models, nil
}
func (p *catalogProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, errors.New("not used")
}

func catalogGateway(t *testing.T, ps ...*catalogProvider) *Gateway {
	t.Helper()
	g := &Gateway{
		cfg:            &config.Config{DefaultProvider: ps[0].name, DefaultModel: "m"},
		log:            logx.New(10),
		providers:      map[string]provider.Provider{},
		accounts:       map[string][]*provider.Account{},
		modelCache:     map[string]modelEntry{},
		modelCachePath: filepath.Join(t.TempDir(), "model-cache.json"),
		convs:          map[string]*conv{},
		pending:        map[string]raccoonPending{},
	}
	for _, p := range ps {
		g.providers[p.name] = p
		g.order = append(g.order, p.name)
	}
	return g
}

// TestListModelsForScopesToAllowedProviders 入站密钥收窄后，模型目录只该剩它有权用的
// 供应商：客户端连「有哪些模型」都不该看到，否则等于把别人的能力清单摊开。
func TestListModelsForScopesToAllowedProviders(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: []openai.Model{{ID: "glm-5.3"}}},
		&catalogProvider{name: "runable", models: []openai.Model{{ID: "zai/glm-5.3-flash"}}},
	)
	ctx := context.Background()

	all := g.ListModels(ctx)
	if len(all) != 2 {
		t.Fatalf("全量目录=%v want 2 条", all)
	}

	only := g.ListModelsFor(ctx, []string{"raccoon"})
	if len(only) != 1 || only[0].ID != "raccoon/glm-5.3" {
		t.Fatalf("收窄目录=%v want [raccoon/glm-5.3]", only)
	}
	if only[0].OwnedBy != "raccoon" {
		t.Errorf("OwnedBy=%q want raccoon", only[0].OwnedBy)
	}

	// 空 allowed = 全量（未收窄的密钥）。
	if got := g.ListModelsFor(ctx, nil); !reflect.DeepEqual(modelIDs(got), modelIDs(all)) {
		t.Errorf("空 allowed 应等于全量: %v vs %v", modelIDs(got), modelIDs(all))
	}
	// 授权了一个不存在的供应商：目录为空，但绝不该回落成全量。
	if got := g.ListModelsFor(ctx, []string{"nope"}); len(got) != 0 {
		t.Errorf("未知供应商授权应得到空目录，得到 %v", modelIDs(got))
	}
}

// TestProviderOfMatchesRoute 授权校验用的 ProviderOf 必须与实际路由 route 同源：
// 两者不一致会出现「放行了却打不到」或「拦住了却能打」。
func TestProviderOfMatchesRoute(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: nil},
		&catalogProvider{name: "runable", models: nil},
	)
	for _, tc := range []struct{ model, want string }{
		{"raccoon/glm-5.3", "raccoon"}, // 已知前缀 → 该供应商
		{"runable/zai/x", "runable"},
		{"glm-5.3", "raccoon"},    // 裸名 → 默认供应商
		{"", "raccoon"},           // 空 → 默认供应商
		{"typo/gpt-6", "raccoon"}, // 前缀不是已知供应商 → 回落默认
	} {
		got := g.ProviderOf(tc.model)
		if got != tc.want {
			t.Errorf("ProviderOf(%q)=%q want %q", tc.model, got, tc.want)
		}
		if routed, _ := g.route(tc.model); routed != got {
			t.Errorf("ProviderOf(%q)=%q 与 route=%q 不一致", tc.model, got, routed)
		}
	}
}

// TestRouteScopedPicksAllowedAliasTarget：别名链首在权限外、链上另一个目标在
// 权限内时，收窄密钥必须真的落到权限内的目标，而不是被入口一刀切拒掉。
// 目录里的别名归属与 alias_of 也必须按权限裁剪。
func TestRouteScopedPicksAllowedAliasTarget(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: []openai.Model{{ID: "m"}}},
		&catalogProvider{name: "runable", models: []openai.Model{{ID: "m1"}}},
	)
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.SetModelMappingTargets("deepseek", []string{"raccoon/m", "runable/m1"}); err != nil {
		t.Fatalf("配链: %v", err)
	}

	if got := g.ProviderFor("deepseek", []string{"runable"}); got != "runable" {
		t.Errorf("ProviderFor(别名, 只授 runable)=%q want runable", got)
	}
	if p, m, ok := g.routeScoped("deepseek", []string{"runable"}); !ok || p != "runable" || m != "m1" {
		t.Errorf("routeScoped(别名, 只授 runable)=(%q,%q,%v) want (runable,m1,true)", p, m, ok)
	}
	if got := g.ProviderFor("deepseek", []string{"nope"}); got != "" {
		t.Errorf("链上没有任何权限内目标时应返回空，得到 %q", got)
	}
	if got := g.ProviderFor("raccoon/m", []string{"runable"}); got != "" {
		t.Errorf("显式指定权限外供应商应返回空，得到 %q", got)
	}

	models := g.ListModelsFor(context.Background(), []string{"runable"})
	var alias *openai.Model
	for i := range models {
		if models[i].ID == "deepseek" {
			alias = &models[i]
		}
	}
	if alias == nil {
		t.Fatalf("权限内别名不该从目录消失: %v", modelIDs(models))
	}
	if alias.OwnedBy != "runable" {
		t.Errorf("别名 OwnedBy=%q want runable", alias.OwnedBy)
	}
	if !reflect.DeepEqual(alias.AliasOf, []string{"runable/m1"}) {
		t.Errorf("别名 AliasOf=%v want [runable/m1]", alias.AliasOf)
	}
}

// TestRouteScopedPrefersAllowedProvider：裸名按 routing.prefer 选供应商时，
// 必须跳过权限外的偏好项，落到权限内且目录里真有该模型的供应商。
func TestRouteScopedPrefersAllowedProvider(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: []openai.Model{{ID: "shared"}}},
		&catalogProvider{name: "runable", models: []openai.Model{{ID: "shared"}}},
	)
	g.cfg.Routing = &config.RoutingConfig{Prefer: []string{"raccoon", "runable"}}
	g.modelCache = map[string]modelEntry{
		"raccoon": {models: []openai.Model{{ID: "shared"}}},
		"runable": {models: []openai.Model{{ID: "shared"}}},
	}
	if p, m, ok := g.routeScoped("shared", []string{"runable"}); !ok || p != "runable" || m != "shared" {
		t.Errorf("routeScoped(shared, 只授 runable)=(%q,%q,%v) want (runable,shared,true)", p, m, ok)
	}
	if p, _, _ := g.routeScoped("shared", []string{"raccoon"}); p != "raccoon" {
		t.Errorf("只授 raccoon 时应选 raccoon，得到 %q", p)
	}
}

// TestScopedChainFallbackSkipsSelectedPrimary：链首被权限跳过、实际主目标是链上
// 第二个目标时，回退列表不能把主目标再列一遍（否则失败后会重复打同一个上游）。
func TestScopedChainFallbackSkipsSelectedPrimary(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "raccoon", models: []openai.Model{{ID: "m"}}},
		&catalogProvider{name: "runable", models: []openai.Model{{ID: "m1"}}},
	)
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.SetModelMappingTargets("drain", []string{"raccoon/m", "runable/m1"}); err != nil {
		t.Fatalf("配链: %v", err)
	}
	fb := g.modelChainFallbacksScoped("drain", "runable", "m1", []string{"runable"})
	if len(fb) != 0 {
		t.Errorf("主目标之后没有权限内候选，兜底应为空，得到 %v", fb)
	}
}

func modelIDs(ms []openai.Model) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}

// TestCompleteTextRespectsScope：深度研究等内部多步流程用 CompleteText，必须
// 和 HandleChat 一样按 ctx 上的 KeyProviders 选目标；否则收窄密钥可以借研究
// 链路绕开 /v1beta 的入口校验。
func TestCompleteTextRespectsScope(t *testing.T) {
	g := testGateway(textProvider("fake", "from fake"))
	g.modelCachePath = filepath.Join(t.TempDir(), "model-cache.json")
	g.providers["alt"] = textProvider("alt", "from alt")
	g.order = []string{"fake", "alt"}
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "fake", Type: "openai"},
		{Name: "alt", Type: "openai", Models: []string{"m1"}},
	}
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.SetModelMappingTargets("deepseek", []string{"fake/m", "alt/m1"}); err != nil {
		t.Fatalf("配链: %v", err)
	}

	// 链首 fake 在范围外、链上 alt 在范围内：研究步骤应直接落到 alt。
	out, err := g.CompleteText(
		WithReqMeta(context.Background(), &ReqMeta{KeyProviders: []string{"alt"}}),
		"deepseek", "hi")
	if err != nil {
		t.Fatalf("CompleteText: %v", err)
	}
	if out != "from alt" {
		t.Errorf("CompleteText=%q want from alt（不能打范围外的链首）", out)
	}

	// 链上没有任何范围内的目标：明确报错，而不是回落到全量路由。
	if _, err := g.CompleteText(
		WithReqMeta(context.Background(), &ReqMeta{KeyProviders: []string{"nope"}}),
		"deepseek", "hi"); err == nil {
		t.Error("范围内没有目标时应报错")
	}
}

// TestRouteScopedRewritesForeignDefaultModel 空 model 请求在默认供应商被收窄掉时，
// 落点会换成允许的供应商；此时不能把 default_model 自带的原供应商前缀原样带过去，
// 否则上游会收到一个自己目录里不存在的模型名（线上就是这样：默认
// fengshao1227/deepseek-v4.1-flash，而 newapi-* 密钥只授 cline-pass/yijia/atria）。
func TestRouteScopedRewritesForeignDefaultModel(t *testing.T) {
	g := catalogGateway(t,
		&catalogProvider{name: "fengshao1227", models: []openai.Model{{ID: "deepseek-v4.1-flash"}}},
		&catalogProvider{name: "cline-pass", models: []openai.Model{{ID: "deepseek-v4.1-flash"}}},
	)
	g.cfg.DefaultModel = "fengshao1227/deepseek-v4.1-flash"

	// 默认供应商在范围内：非收窄路径保持既有行为，原样返回 default_model
	// （resolveModel 会在该供应商目录里按末尾段匹配）。
	if p, m, ok := g.routeScoped("", nil); !ok || p != "fengshao1227" || m != "fengshao1227/deepseek-v4.1-flash" {
		t.Errorf("全量空 model=(%q,%q,%v) want (fengshao1227,fengshao1227/deepseek-v4.1-flash,true)", p, m, ok)
	}
	// 默认供应商在范围外：落点是 cline-pass，模型必须剥掉别家前缀。
	if p, m, ok := g.routeScoped("", []string{"cline-pass"}); !ok || p != "cline-pass" || m != "deepseek-v4.1-flash" {
		t.Errorf("收窄空 model=(%q,%q,%v) want (cline-pass,deepseek-v4.1-flash,true)", p, m, ok)
	}
	// 默认模型本来就是裸名：不要动它。
	g.cfg.DefaultModel = "deepseek-v4.1-flash"
	if p, m, ok := g.routeScoped("", []string{"cline-pass"}); !ok || p != "cline-pass" || m != "deepseek-v4.1-flash" {
		t.Errorf("裸默认模型收窄=(%q,%q,%v) want (cline-pass,deepseek-v4.1-flash,true)", p, m, ok)
	}
}
