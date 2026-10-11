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
		{"glm-5.3", "raccoon"},  // 裸名 → 默认供应商
		{"", "raccoon"},         // 空 → 默认供应商
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

func modelIDs(ms []openai.Model) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
