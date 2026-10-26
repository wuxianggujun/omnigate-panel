package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// TestProbeProviderHitsUpstream 覆盖「添加供应商」的探测路径：用一份未保存的
// openai 类型配置真实打一次上游 /models，并带上填写的 api_key。
func TestProbeProviderHitsUpstream(t *testing.T) {
	catalog.Register()
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-chat","object":"model"},{"id":"deepseek-reasoner","object":"model"}]}`))
	}))
	defer srv.Close()

	g := &Gateway{cfg: &config.Config{DataDir: t.TempDir()}, log: logx.New(10)}
	models, err := g.ProbeProvider(context.Background(), config.ProviderConfig{
		Name: "probe-openai", Type: "openai", BaseURL: srv.URL, APIKey: "sk-test",
	})
	if err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if gotPath != "/models" {
		t.Errorf("上游路径=%q want /models", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization=%q want Bearer sk-test", gotAuth)
	}
	if len(models) != 2 || models[0].ID != "deepseek-chat" {
		t.Fatalf("探测目录=%v want 2 个 deepseek 模型", models)
	}
	// 探测不得写入网关的模型缓存。
	if _, ok := g.cachedModels("probe-openai"); ok {
		t.Errorf("探测不应写入模型缓存")
	}
}

// TestProbeProviderUsesAPIKeyPool 只填密钥池（api_keys，不填单个 api_key）时，
// 探测也必须带上池里的密钥——否则会退化成匿名请求，探测结果与保存后不一致。
func TestProbeProviderUsesAPIKeyPool(t *testing.T) {
	catalog.Register()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"pool-model"}]}`))
	}))
	defer srv.Close()

	g := &Gateway{cfg: &config.Config{DataDir: t.TempDir()}, log: logx.New(10)}
	models, err := g.ProbeProvider(context.Background(), config.ProviderConfig{
		Name: "probe-pool", Type: "openai", BaseURL: srv.URL,
		APIKeys: []string{"sk-pool-1", "sk-pool-2"},
	})
	if err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Bearer sk-pool-") {
		t.Errorf("密钥池未被注入，Authorization=%q", gotAuth)
	}
	if len(models) != 1 || models[0].ID != "pool-model" {
		t.Fatalf("探测目录=%v want [pool-model]", models)
	}
}

// TestProbeProviderConfiguredModelsShortCircuit 显式配置了 models 时探测直接返回
// 该清单、不访问上游（与 openaibackend.ListModels 的白名单语义一致）。
func TestProbeProviderConfiguredModelsShortCircuit(t *testing.T) {
	catalog.Register()
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"should-not-appear"}]}`))
	}))
	defer srv.Close()

	g := &Gateway{cfg: &config.Config{DataDir: t.TempDir()}, log: logx.New(10)}
	models, err := g.ProbeProvider(context.Background(), config.ProviderConfig{
		Name: "probe-openai", Type: "openai", BaseURL: srv.URL,
		Models: []string{"m-a", "m-b"},
	})
	if err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if hit != 0 {
		t.Errorf("配置了 models 时不应访问上游，实际访问 %d 次", hit)
	}
	if len(models) != 2 || models[0].ID != "m-a" || models[1].ID != "m-b" {
		t.Fatalf("探测目录=%v want [m-a m-b]", models)
	}
}

// TestRefreshModelsBypassesTTL 覆盖「刷新上游目录」：即便缓存未过期也重新拉取，
// 并把新目录写回缓存。
func TestRefreshModelsBypassesTTL(t *testing.T) {
	catalog.Register()
	var hit int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit++
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"fresh"}]}`))
	}))
	defer srv.Close()

	g := &Gateway{
		cfg:        &config.Config{DataDir: t.TempDir()},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
	}
	// 预热一份「刚成功」的旧目录：TTL 内，models() 会直接返回它。
	g.modelCache["up"] = modelEntry{models: []openai.Model{{ID: "stale"}}, at: time.Now()}
	prov, err := newProvider(&config.ProviderConfig{Name: "up", Type: "openai", BaseURL: srv.URL}, plugin.Deps{})
	if err != nil {
		t.Fatalf("newProvider: %v", err)
	}
	g.providers["up"] = prov

	if cached, ok := g.cachedModels("up"); !ok || cached[0].ID != "stale" {
		t.Fatalf("预热缓存异常: %v %v", cached, ok)
	}
	got, err := g.RefreshModels(context.Background(), "up")
	if err != nil {
		t.Fatalf("RefreshModels: %v", err)
	}
	if hit != 1 {
		t.Errorf("刷新应绕过 TTL 打一次上游，实际 %d 次", hit)
	}
	if len(got) != 1 || got[0].ID != "fresh" {
		t.Fatalf("刷新结果=%v want [fresh]", got)
	}
	if cached, _ := g.cachedModels("up"); cached[0].ID != "fresh" {
		t.Errorf("刷新后缓存=%v want fresh", cached)
	}
}

// failModelsProvider always fails its catalog fetch, so the refresh path exercises
// the "keep last good" fallback. It mimics raccoon/trae (error + degraded list).
type failModelsProvider struct{ name string }

func (p *failModelsProvider) Name() string { return p.name }
func (p *failModelsProvider) Type() string { return "openai" }
func (p *failModelsProvider) ListModels(context.Context) ([]openai.Model, error) {
	return nil, errors.New("upstream 500")
}
func (p *failModelsProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, errors.New("not used")
}

// TestRefreshModelsFallsBackToCache 刷新失败时返回最近一次成功目录 + 错误，避免
// 面板因一次网络抖动就清空已有目录。
func TestRefreshModelsFallsBackToCache(t *testing.T) {
	g := &Gateway{
		cfg:        &config.Config{DataDir: t.TempDir()},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
	}
	g.modelCache["up"] = modelEntry{models: []openai.Model{{ID: "kept"}}, at: time.Now()}
	g.providers["up"] = &failModelsProvider{name: "up"}

	got, err := g.RefreshModels(context.Background(), "up")
	if err == nil {
		t.Fatalf("上游失败时应返回错误")
	}
	if len(got) != 1 || got[0].ID != "kept" {
		t.Fatalf("刷新失败应返回缓存目录，得到 %v", got)
	}
}

// TestRefreshModelsUnknownProvider 未知供应商直接报错，不做任何猜测。
func TestRefreshModelsUnknownProvider(t *testing.T) {
	g := &Gateway{
		cfg:        &config.Config{DataDir: t.TempDir()},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
	}
	_, err := g.RefreshModels(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("未知供应商应报 unknown provider，得到 %v", err)
	}
}

// TestSpecOfThreadsIdentityAndEndpoints 锁住「配置 → ProviderSpec」这一唯一映射
// 点：出站身份、协议方言与 New API 用户接口表面都必须原样传下去，否则面板填了
// 却运行时没生效——这类问题最难查，因为它看起来配置完全正常。
func TestSpecOfThreadsIdentityAndEndpoints(t *testing.T) {
	p := &config.ProviderConfig{
		Name:             "anyrouter",
		Type:             "newapi",
		BaseURL:          "https://anyrouter.top",
		ClientPreset:     "claude-code",
		ClientUserAgent:  "claude-cli/9.9.9 (external, cli)",
		ClientHeaders:    map[string]string{"anthropic-beta": "x"},
		ClientTLSProfile: "chrome120",
		Protocol:         "anthropic",
		CheckinPath:      "/api/user/sign_in",
		SelfPath:         "/api/user/self",
		RefreshPath:      "/api/user/auth/refresh",
		UserHeader:       "New-Api-User",
		AuthMode:         "cookie",
		SigninPath:       "/login",
		ModelsPath:       "/v1/models",
		ChatPath:         "/v1/chat/completions",
	}
	spec := specOf(p)
	if spec.Protocol != "anthropic" {
		t.Errorf("Protocol=%q", spec.Protocol)
	}
	if spec.Client.Preset != "claude-code" || spec.Client.UserAgent != "claude-cli/9.9.9 (external, cli)" {
		t.Errorf("Client=%+v", spec.Client)
	}
	if spec.Client.Headers["Anthropic-Beta"] != "x" || spec.Client.TLSProfile != "chrome120" {
		t.Errorf("Client 覆盖项丢失: %+v", spec.Client)
	}
	if !spec.Endpoints.UsesCookieAuth() {
		t.Errorf("Endpoints.AuthMode=%q", spec.Endpoints.AuthMode)
	}
	if spec.Endpoints.CheckinPath != "/api/user/sign_in" || spec.Endpoints.UserHeader != "New-Api-User" {
		t.Errorf("Endpoints=%+v", spec.Endpoints)
	}
	if spec.Endpoints.SigninPath != "/login" {
		t.Errorf("SigninPath=%q", spec.Endpoints.SigninPath)
	}
}

// TestProbeProviderCarriesIdentity 探测必须与保存后运行时走同一条构造路径：
// 自定义出站身份在探测阶段也要生效，否则会出现「探测通过、保存后被拒」。
func TestProbeProviderCarriesIdentity(t *testing.T) {
	catalog.Register()
	var gotUA, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotPath = r.URL.Path
		// 返回一个模型而不是空目录：空目录现在是「这次没读到」的错误信号
		// （见 openaibackend.ListModels），会把本用例的身份断言掩盖掉。
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	g := &Gateway{cfg: &config.Config{DataDir: t.TempDir()}, log: logx.New(10)}
	if _, err := g.ProbeProvider(context.Background(), config.ProviderConfig{
		Name: "probe-identity", Type: "openai", BaseURL: srv.URL,
		ClientPreset: "claude-code",
	}); err != nil {
		t.Fatalf("ProbeProvider: %v", err)
	}
	if !strings.HasPrefix(gotUA, "claude-cli/") {
		t.Errorf("探测的出站 UA=%q，身份没有生效", gotUA)
	}
	if gotPath != "/models" {
		t.Errorf("path=%q", gotPath)
	}
}
