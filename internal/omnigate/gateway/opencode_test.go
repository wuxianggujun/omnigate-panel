package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/opencode"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// opencodeFake 是 opencode 形态的假上游：记录收到的 ChatInput，回一段固定正文。
type opencodeFake struct {
	inputs []provider.ChatInput
}

func (p *opencodeFake) Name() string                                       { return "oc" }
func (p *opencodeFake) Type() string                                       { return "opencode" }
func (p *opencodeFake) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }

func (p *opencodeFake) StreamChat(_ context.Context, _ *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	p.inputs = append(p.inputs, in)
	return &scriptStream{events: []provider.Event{
		{Type: provider.EventText, Text: "ok"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}, nil
}

// statelessGateway 搭一个「账号池为空」但带 st/cool/sess 的网关——密钥型上游的形态。
func statelessGateway(t *testing.T, p provider.Provider) *Gateway {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(p)
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	// 刻意不往 g.accounts 里塞任何东西：密钥型上游的账号池本来就是空的。
	return g
}

// opencode 是密钥型上游（凭证在供应商的 zen_keys / go_keys 池里），账号池天然为空。
// 它必须走 stateless 分支，而不是被「has no accounts configured」挡下——后者是
// 一种最难发现的故障：配置界面一切正常，每个请求都失败。
func TestOpencodeRunsWithoutAccounts(t *testing.T) {
	prov := &opencodeFake{}
	g := statelessGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "oc", Type: "opencode", GoKeys: []string{"sk-go"}},
	}

	req := &openai.ChatRequest{
		Model:    "oc/claude-sonnet-5",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	// opencode 是 chat 型上游：messages 必须原样转发，不能被压成单条 prompt。
	got := prov.inputs[0]
	if len(got.Messages) != 1 || got.Messages[0].Content.Text != "hi" {
		t.Fatalf("messages 应原样转发: %+v", got.Messages)
	}
	if got.Prompt != "" {
		t.Fatalf("chat 型上游不该被压成单条 prompt，得到 %q", got.Prompt)
	}
}

// 账号型上游仍然是「没账号就报错」——不能让 opencode 的放宽顺手把这条口径也放开了。
func TestAccountProvidersStillRequireAccounts(t *testing.T) {
	prov := &runableFake{}
	g := statelessGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{{Name: "fake", Type: "runable"}}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq("hi"), w)
	if w.Code == http.StatusOK {
		t.Fatalf("没有账号的账号型上游不该成功，body=%s", w.Body.String())
	}
	if len(prov.inputs) != 0 {
		t.Fatalf("没有账号时不该调用上游，实际 %d 次", len(prov.inputs))
	}
}

// opencode 是 OpenAI 兼容上游，reasoning_effort 要转发（runable 之类的自研协议没有
// 对应概念，不在其中）。
func TestSupportsEffortIncludesOpencode(t *testing.T) {
	for _, typ := range []string{"openai", "raccoon", "opencode"} {
		if !supportsEffort(typ) {
			t.Errorf("%s 应转发 reasoning_effort", typ)
		}
	}
	for _, typ := range []string{"runable", "geminiweb", "newapi"} {
		if supportsEffort(typ) {
			t.Errorf("%s 不该被标记为支持 reasoning_effort", typ)
		}
	}
}

// 工厂必须真能把 type=opencode 的配置装配成 provider，并把它注册进路由表。
// 开关分支写错（比如类型名拼错）时，配置能存、面板显示正常，但请求会 404/未知供应商。
//
// 顺带钉住一个刻意的设计：opencode **不实现** SetAPIKeys。gateway 的通用密钥池装配
// 对实现了该接口的后端调用它；若 opencode 也实现（或以后有人「顺手补上」），
// 配置里出现 api_keys 时就会用那份通用池覆盖掉 zen/go 两个池——症状是配好的密钥
// 突然全部失效。所以这里故意在配置里塞一份 api_keys，断言两个 tier 池仍然完好。
func TestFactoryBuildsOpencodeProvider(t *testing.T) {
	build := func(t *testing.T, p config.ProviderConfig) *opencode.Provider {
		t.Helper()
		cfg := &config.Config{Providers: []config.ProviderConfig{p}}
		if err := cfg.Normalize(); err != nil {
			t.Fatalf("配置应合法: %v", err)
		}
		st, err := state.Load(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		g, err := New(cfg, st, logx.New(10))
		if err != nil {
			t.Fatalf("工厂应能构造 opencode 供应商: %v", err)
		}
		prov := g.providers[p.Name]
		if prov == nil {
			t.Fatalf("provider %q 未注册（providers=%v）", p.Name, g.ProviderNames())
		}
		if prov.Type() != "opencode" {
			t.Fatalf("type = %q, want opencode", prov.Type())
		}
		oc, ok := prov.(*opencode.Provider)
		if !ok {
			t.Fatalf("应为 *opencode.Provider，得到 %T", prov)
		}
		return oc
	}

	// tier_prefer 决定顺序；通用 api_keys 不得干扰。
	oc := build(t, config.ProviderConfig{
		Name: "oc", Type: "opencode",
		ZenKeys:    []string{"sk-zen-pool"},
		GoKeys:     []string{"sk-go-pool"},
		TierPrefer: "zen",
		APIKeys:    []string{"generic-pool-must-not-clear-tiers"},
	})
	if !oc.Ready() {
		t.Fatal("配了两个池的密钥应 Ready")
	}
	if got := strings.Join(oc.Tiers(), ","); got != "zen,go" {
		t.Fatalf("tiers 应遵循 tier_prefer=zen，得到 %q", got)
	}

	// 留空 = 默认 go 优先（订阅套餐先跑，用不完再落到按量付费）。
	oc = build(t, config.ProviderConfig{
		Name: "oc2", Type: "opencode",
		ZenKeys: []string{"sk-zen-pool"},
		GoKeys:  []string{"sk-go-pool"},
	})
	if got := strings.Join(oc.Tiers(), ","); got != "go,zen" {
		t.Fatalf("留空应按 go 优先，得到 %q", got)
	}

	// 只配一个 tier：另一个 tier 不该出现在候选里（拿另一边的密钥打过去只会 401）。
	oc = build(t, config.ProviderConfig{
		Name: "oc3", Type: "opencode", GoKeys: []string{"sk-go-pool"},
	})
	if got := strings.Join(oc.Tiers(), ","); got != "go" {
		t.Fatalf("只有 go 密钥时 tiers 应只含 go，得到 %q", got)
	}
	if !oc.Ready() {
		t.Fatal("只要有任一 tier 的密钥就应 Ready")
	}

	// 一个池都没配 = 不可用（面板按空壳隐藏、签到守卫会挂起它）。
	oc = build(t, config.ProviderConfig{Name: "oc4", Type: "opencode"})
	if oc.Ready() {
		t.Fatal("没有任何密钥的 opencode 不该 Ready")
	}
	if got := oc.Tiers(); len(got) != 0 {
		t.Fatalf("没有密钥时不该有可用 tier，得到 %v", got)
	}
}
