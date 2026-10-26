package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	// 过期缓存走后台刷新（stale-while-revalidate）：等它落定，确认失败后旧目录仍在。
	g.modelRefreshWG.Wait()
	cached, ok := g.cachedModels("raccoon")
	if !ok || len(cached) != 1 || cached[0].ID != "sn-glm-5-3" {
		t.Fatalf("后台刷新失败后缓存被改坏: %+v ok=%v", cached, ok)
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

// 空目录不是「这家供应商没有模型」，而是「这次没读到」。它既不能覆盖上一次的
// 完好目录，也不能把自己从落库快照里抹掉：线上曾因一次拉取返回空列表，让 aotera
// 的 7 个模型在重启后永远消失（日志写「已载入 5 个」，库里只剩 4 个）。
func TestModelsEmptyRefreshKeepsLastGood(t *testing.T) {
	prov := &emptyThenGoodLister{}
	g := testGateway(prov)
	g.accounts["raccoon"] = []*provider.Account{{Label: "a", Cookie: "tok"}}

	first, err := g.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("first = %+v, want 2 models", first)
	}
	if _, ok := g.cachedModels("raccoon"); !ok {
		t.Fatal("首次成功后应写入缓存")
	}

	// 让缓存过期，并让上游返回「无错但空」。
	g.modelMu.Lock()
	g.modelCache["raccoon"] = modelEntry{models: first, at: time.Now().Add(-modelCacheTTL - time.Minute)}
	g.modelMu.Unlock()
	prov.empty = true

	got, err := g.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("空目录应回落到缓存而不是报错: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got = %+v, want cached 2 models", got)
	}
	cached, ok := g.cachedModels("raccoon")
	if !ok || len(cached) != 2 {
		t.Fatalf("空目录不该覆盖缓存，实际 %+v ok=%v", cached, ok)
	}
	// 过期缓存的刷新是后台的：等它落定再检查缓存与落库快照。
	g.modelRefreshWG.Wait()
	cached, ok = g.cachedModels("raccoon")
	if !ok || len(cached) != 2 {
		t.Fatalf("后台刷新返回空目录后缓存被改坏: %+v ok=%v", cached, ok)
	}
	// 落库快照必须仍然包含这家供应商。
	path := filepath.Join(t.TempDir(), "model-cache.json")
	g.modelCachePath = path
	g.saveModelCache()
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatalf("读回落盘快照失败: %v", rerr)
	}
	var saved modelCacheFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatalf("解析落盘快照失败: %v", err)
	}
	if len(saved["raccoon"]) != 2 {
		t.Fatalf("落盘快照丢了 raccoon: %+v", saved)
	}
}

// emptyThenGoodLister 模拟「第一次正常、之后无错但返回空目录」的上游。
type emptyThenGoodLister struct {
	empty bool
}

func (p *emptyThenGoodLister) Name() string { return "raccoon" }
func (p *emptyThenGoodLister) Type() string { return "raccoon" }

func (p *emptyThenGoodLister) ListModels(context.Context) ([]openai.Model, error) {
	if p.empty {
		return nil, nil
	}
	return []openai.Model{{ID: "m1", Object: "model"}, {ID: "m2", Object: "model"}}, nil
}

func (p *emptyThenGoodLister) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, errors.New("not used")
}

// 模型目录跨重启持久化：一次成功后落盘；重建网关（上游已不可用）时仍能立刻给出
// 带积分价的目录，而不是退回「无价」。
func TestModelCachePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-cache.json")

	// 第一次：拉取成功 → 落盘。
	g1 := testGateway(&flakyLister{})
	g1.accounts["raccoon"] = []*provider.Account{{Label: "a", Cookie: "tok"}}
	g1.modelCachePath = path
	if _, err := g1.models(context.Background(), "raccoon"); err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cache file not written: %v", err)
	}

	// 重启：新网关 + 上游不可用（fail=true 且没有账号）。
	g2 := testGateway(&flakyLister{fail: true})
	g2.modelCachePath = path
	g2.loadModelCache()
	got, err := g2.models(context.Background(), "raccoon")
	if err != nil {
		t.Fatalf("after restart: %v", err)
	}
	if len(got) != 1 || got[0].ID != "sn-glm-5-3" || got[0].Credits == nil || *got[0].Credits != 0.75 {
		t.Fatalf("got = %+v, want persisted priced model", got)
	}
}
