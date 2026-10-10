package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// flakyLister 是「需要鉴权的目录」provider 桩（实现 AccountModelLister）：正常时
// 返回带积分价的目录，fail 时返回错误 + 无价的降级目录（模拟账号 token 失效）。
type flakyLister struct {
	fail bool
}

func (p *flakyLister) Name() string { return "raccoon" }
func (p *flakyLister) Type() string { return "raccoon" }

func (p *flakyLister) ListModels(context.Context) ([]openai.Model, error) {
	return []openai.Model{{ID: "builtin", Object: "model"}}, nil
}

func (p *flakyLister) ListModelsWithAccount(context.Context, *provider.Account) ([]openai.Model, error) {
	if p.fail {
		return []openai.Model{{ID: "builtin", Object: "model"}}, errors.New("upstream 401")
	}
	credits := 0.75
	return []openai.Model{{ID: "sn-glm-5-3", Object: "model", Credits: &credits, CreditUnit: "multiplier"}}, nil
}

func (p *flakyLister) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, errors.New("not used")
}

// 缓存过期后刷新失败：必须沿用最近一次「带积分价」的成功结果，而不是被无价的降级
// 目录覆盖（否则面板上的积分价会周期性消失）。
func TestModelsKeepsLastGoodOnRefreshFailure(t *testing.T) {
	prov := &flakyLister{}
	g := testGateway(prov)
	g.accounts["raccoon"] = []*provider.Account{{Label: "a", Cookie: "tok"}}

	first, err := g.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first) != 1 || first[0].Credits == nil || *first[0].Credits != 0.75 {
		t.Fatalf("first = %+v, want priced model", first)
	}

	// 让缓存过期，并让上游开始报错。
	g.modelMu.Lock()
	g.modelCache["raccoon"] = modelEntry{models: first, at: time.Now().Add(-modelCacheTTL - time.Minute)}
	g.modelMu.Unlock()
	prov.fail = true

	got, err := g.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("refresh failure should fall back to cache, got err: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sn-glm-5-3" || got[0].Credits == nil {
		t.Fatalf("got = %+v, want cached priced model", got)
	}
}

// 完全没有缓存时刷新失败：退回上游给的降级目录（仍可路由），但绝不写进缓存。
func TestModelsFallsBackToDegradedWithoutCache(t *testing.T) {
	prov := &flakyLister{fail: true}
	g := testGateway(prov)
	g.accounts["raccoon"] = []*provider.Account{{Label: "a", Cookie: "tok"}}

	got, err := g.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("err = %v, want degraded fallback", err)
	}
	if len(got) != 1 || got[0].ID != "builtin" {
		t.Fatalf("got = %+v, want builtin fallback", got)
	}
	g.modelMu.Lock()
	_, cached := g.modelCache["raccoon"]
	g.modelMu.Unlock()
	if cached {
		t.Fatal("degraded result must not be cached")
	}
}
