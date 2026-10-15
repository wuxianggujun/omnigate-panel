package opencode

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// metaSrv 起一个假的元数据目录（models.opencode.ai/api.json 的形状），并返回命中次数读取器。
func metaSrv(t *testing.T, providers map[string]any) (*httptest.Server, func() int) {
	t.Helper()
	if providers == nil {
		providers = map[string]any{}
	}
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		json.NewEncoder(w).Encode(providers)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// emptyMeta 是一个「抓得到、但不含任何真实模型」的元数据目录地址：给不关心元数据的
// 测试用，免得它们去打真实的 models.opencode.ai。
func emptyMeta(t *testing.T) string {
	t.Helper()
	srv, _ := metaSrv(t, map[string]any{
		"opencode": metaProvider("https://opencode.ai/inference/v1", map[string]any{
			"__none__": map[string]any{"limit": map[string]any{"context": 1}},
		}),
	})
	return srv.URL
}

// metaProvider 造一个元数据目录里的 provider 条目。
func metaProvider(api string, models map[string]any) map[string]any {
	return map[string]any{"api": api, "models": models}
}

// catalogSrv 起一个假的 /v1/models 服务。
func catalogSrv(t *testing.T, ids ...string) *httptest.Server {
	t.Helper()
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

// byID 把模型列表转成 map，方便按 id 断言。
func byID(models []openai.Model) map[string]openai.Model {
	out := make(map[string]openai.Model, len(models))
	for _, m := range models {
		out[m.ID] = m
	}
	return out
}

// 元数据目录把 /v1/models 缺的字段补齐：上下文长度、输出上限、价格、免费标记。
func TestMetadataEnrichesCatalog(t *testing.T) {
	zen := catalogSrv(t, "glm-5.3", "big-pickle", "unknown-model")
	meta, _ := metaSrv(t, map[string]any{
		"opencode": metaProvider("https://opencode.ai/inference/v1", map[string]any{
			"glm-5.3": map[string]any{
				"limit": map[string]any{"context": 1000000, "output": 131072},
				"cost":  map[string]any{"input": 1.4, "output": 4.4, "cache_read": 0.26},
			},
			"big-pickle": map[string]any{
				"limit": map[string]any{"context": 262144, "output": 65536},
				"cost":  map[string]any{"input": 0, "output": 0},
			},
		}),
	})

	p := New(Options{Name: "oc", ZenKeys: []string{"z"}, ZenBase: zen.URL, MetaURL: meta.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := byID(models)

	g, ok := got["glm-5.3"]
	if !ok {
		t.Fatalf("目录里应有 glm-5.3，得到 %v", models)
	}
	if g.ContextWindow != 1000000 || g.MaxTokens != 131072 {
		t.Errorf("上下文/输出上限 = %d/%d，want 1000000/131072", g.ContextWindow, g.MaxTokens)
	}
	if g.IsFree {
		t.Error("付费模型不该被标成免费")
	}
	if g.Pricing == nil {
		t.Fatal("付费模型应带价格")
	}
	// 单位契约：目录里是「每百万 token 美元」，ModelPricing 存「每 token 美元」
	// （面板乘回 1e6 显示成 $/1M）。差 100 万倍会显示成 $1,400,000.00/1M。
	if math.Abs(g.Pricing.Input-1.4e-6) > 1e-15 || math.Abs(g.Pricing.Output-4.4e-6) > 1e-15 {
		t.Errorf("价格应按每 token 存：in=%g out=%g", g.Pricing.Input, g.Pricing.Output)
	}
	if math.Abs(g.Pricing.InputCacheRead-0.26e-6) > 1e-15 {
		t.Errorf("缓存读价应按每 token 存：%g", g.Pricing.InputCacheRead)
	}

	bp, ok := got["big-pickle"]
	if !ok {
		t.Fatal("目录里应有 big-pickle")
	}
	if !bp.IsFree {
		t.Error("全 0 价的模型应标成免费")
	}
	if bp.Pricing != nil {
		t.Errorf("免费模型不该带价格，得到 %+v", bp.Pricing)
	}
	if bp.ContextWindow != 262144 || bp.MaxTokens != 65536 {
		t.Errorf("免费模型也该有上下文与输出上限，得到 %d/%d", bp.ContextWindow, bp.MaxTokens)
	}

	// 元数据目录里没有的模型照常返回，只是不带这些字段。
	um, ok := got["unknown-model"]
	if !ok {
		t.Fatal("元数据里没有的模型也必须留在目录里")
	}
	if um.ContextWindow != 0 || um.MaxTokens != 0 || um.Pricing != nil || um.IsFree {
		t.Errorf("元数据里没有的模型应保持 bare，得到 %+v", um)
	}
}

// 两个 tier 都给同一个模型声明了元数据时，以先轮询的 tier 为准（与模型并集同序）；
// 这条同时钉住 tier 归属：id "opencode-go" 里也含 "opencode"，判错就会把 go 的元数据
// 算到 zen 头上。
func TestMetadataFollowsTierOrder(t *testing.T) {
	zenSrv := catalogSrv(t, "shared")
	goSrv := catalogSrv(t, "shared")
	meta, _ := metaSrv(t, map[string]any{
		"opencode": metaProvider("https://opencode.ai/inference/v1", map[string]any{
			"shared": map[string]any{"limit": map[string]any{"context": 111}},
		}),
		"opencode-go": metaProvider("https://opencode.ai/inference/go/v1", map[string]any{
			"shared": map[string]any{"limit": map[string]any{"context": 222}},
		}),
	})

	opt := func(prefer string) Options {
		return Options{Name: "oc", ZenKeys: []string{"z"}, GoKeys: []string{"g"},
			ZenBase: zenSrv.URL, GoBase: goSrv.URL, MetaURL: meta.URL, Prefer: prefer}
	}
	for _, c := range []struct {
		prefer string
		want   int64
	}{{TierGo, 222}, {TierZen, 111}} {
		p := New(opt(c.prefer))
		models, err := p.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := byID(models)["shared"].ContextWindow; got != c.want {
			t.Errorf("prefer=%s：应取该 tier 的元数据 %d，得到 %d", c.prefer, c.want, got)
		}
	}
}

// 元数据目录挂了不影响模型目录本身：字段为空，但模型照常返回、不报错。
func TestMetadataFailureKeepsBareCatalog(t *testing.T) {
	zen := catalogSrv(t, "glm-5.3")
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer bad.Close()

	p := New(Options{Name: "oc", ZenKeys: []string{"z"}, ZenBase: zen.URL, MetaURL: bad.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("元数据失败不该让模型目录报错，得到 %v", err)
	}
	if len(models) != 1 || models[0].ID != "glm-5.3" {
		t.Fatalf("应保留完整目录，得到 %+v", models)
	}
	if m := models[0]; m.ContextWindow != 0 || m.MaxTokens != 0 || m.Pricing != nil || m.IsFree {
		t.Fatalf("元数据失败时应保持 bare，得到 %+v", m)
	}
}

// 元数据目录按小时级缓存：连续多次列表只抓一次（5.6MB 的目录不能每次列表都重下一份）。
func TestMetadataFetchedOncePerCacheWindow(t *testing.T) {
	zen := catalogSrv(t, "glm-5.3")
	meta, hits := metaSrv(t, map[string]any{
		"opencode": metaProvider("https://opencode.ai/inference/v1", map[string]any{
			"glm-5.3": map[string]any{"limit": map[string]any{"context": 1000000}},
		}),
	})

	p := New(Options{Name: "oc", ZenKeys: []string{"z"}, ZenBase: zen.URL, MetaURL: meta.URL})
	for i := 0; i < 3; i++ {
		if _, err := p.ListModels(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if n := hits(); n != 1 {
		t.Fatalf("元数据应只抓一次，实际 %d 次", n)
	}
}

// 上游目录全挂、退回配置兜底清单时也要补元数据；没配任何密钥（没有参与轮询的 tier）
// 时同样成立——面板上的模型列表不该因为「还没填密钥」就少掉上下文与价格。
func TestMetadataAppliesToFallbackListWithoutKeys(t *testing.T) {
	meta, _ := metaSrv(t, map[string]any{
		"opencode": metaProvider("https://opencode.ai/inference/v1", map[string]any{
			"glm-5.3": map[string]any{
				"limit": map[string]any{"context": 1000000, "output": 131072},
				"cost":  map[string]any{"input": 1.4, "output": 4.4},
			},
		}),
	})

	p := New(Options{Name: "oc", Models: []string{"glm-5.3", "gone"}, MetaURL: meta.URL})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := byID(models)
	g, ok := got["glm-5.3"]
	if !ok {
		t.Fatalf("兜底清单应有 glm-5.3，得到 %v", models)
	}
	if g.ContextWindow != 1000000 || g.MaxTokens != 131072 || g.Pricing == nil {
		t.Errorf("兜底清单也要补元数据，得到 %+v", g)
	}
	if g.OwnedBy != "oc" {
		t.Errorf("兜底模型的 owned_by 应是供应商名，得到 %q", g.OwnedBy)
	}
	if gone := got["gone"]; gone.Pricing != nil || gone.ContextWindow != 0 {
		t.Errorf("元数据里没有的兜底模型应保持 bare，得到 %+v", gone)
	}
}
