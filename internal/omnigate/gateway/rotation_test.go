package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// coolProvider returns an error for one specific account label and a scripted
// stream for the rest, recording the order accounts were tried.
type coolProvider struct {
	failLabel string
	err       error
	events    []provider.Event
	calls     []string
}

func (p *coolProvider) Name() string                                       { return "fake" }
func (p *coolProvider) Type() string                                       { return "openai" }
func (p *coolProvider) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }
func (p *coolProvider) StreamChat(_ context.Context, acc *provider.Account, _ provider.ChatInput) (provider.Stream, error) {
	p.calls = append(p.calls, acc.Label)
	if acc.Label == p.failLabel {
		return nil, p.err
	}
	return &scriptStream{events: p.events}, nil
}

// 限流：某账号回 429 时换下一个账号并把该账号冷却；后续请求直接绕开冷却账号。
func TestOpenStreamSkipsCoolingAccount(t *testing.T) {
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prov := &coolProvider{
		failLabel: "a",
		err:       &raccoon.UpstreamError{Status: 429, Message: "too many requests"},
		events:    []provider.Event{{Type: provider.EventText, Text: "ok"}, {Type: provider.EventFinish, Finish: "stop"}},
	}
	g := testGateway(prov)
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}, {Label: "b", Cookie: "c"}}

	in := provider.ChatInput{Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}}}
	meta := &ReqMeta{}
	ctx := WithReqMeta(context.Background(), meta)

	s, err := g.openStream(ctx, "fake", prov, in)
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	_ = s.Close()
	if meta.Account != "b" {
		t.Fatalf("served by %q, want b (a is rate limited)", meta.Account)
	}
	if !g.cool.Cooling("fake", "a", time.Now()) {
		t.Fatal("account a should be cooling after a 429")
	}

	// 强制从 a 起轮转：冷却中的 a 必须被跳过，直接走 b。
	g.st.SetActive("fake", 0)
	prov.calls = nil
	s2, err := g.openStream(ctx, "fake", prov, in)
	if err != nil {
		t.Fatalf("openStream 2: %v", err)
	}
	_ = s2.Close()
	for _, c := range prov.calls {
		if c == "a" {
			t.Fatalf("cooling account a must be skipped, calls=%v", prov.calls)
		}
	}
	if len(prov.calls) == 0 || prov.calls[0] != "b" {
		t.Fatalf("calls = %v, want first b", prov.calls)
	}
}

// 账号级错误（401 / 5xx）必须继续轮换。以前只有 429 / 积分耗尽才换号，其它错误
// 直接返回，ActiveIndex 会永远停在同一个坏账号上：每次请求都先撞它一次，后面的
// 账号永远轮不到。
func TestOpenStreamRotatesOnAccountSpecificFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"auth", &raccoon.UpstreamError{Status: 401, Message: "token expired"}},
		{"server", errors.New("upstream HTTP 500: overloaded")},
		{"network", errors.New("dial tcp: connection refused")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := state.Load(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prov := &coolProvider{
				failLabel: "a",
				err:       tc.err,
				events:    []provider.Event{{Type: provider.EventText, Text: "ok"}, {Type: provider.EventFinish, Finish: "stop"}},
			}
			g := testGateway(prov)
			g.st = st
			g.cool = newCooldownLedger()
			g.sess = newSessionHealth()
			g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}, {Label: "b", Cookie: "c"}}

			meta := &ReqMeta{}
			ctx := WithReqMeta(context.Background(), meta)
			in := provider.ChatInput{Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}}}
			s, err := g.openStream(ctx, "fake", prov, in)
			if err != nil {
				t.Fatalf("openStream: %v", err)
			}
			_ = s.Close()
			if meta.Account != "b" {
				t.Fatalf("served by %q, want b", meta.Account)
			}
			if len(prov.calls) != 2 || prov.calls[0] != "a" || prov.calls[1] != "b" {
				t.Fatalf("calls = %v, want [a b]", prov.calls)
			}
			if got := st.ActiveIndex("fake"); got != 1 {
				t.Fatalf("成功账号应成为新的 ActiveIndex，得到 %d", got)
			}
		})
	}
}

// 请求本身不合法（400 / prompt 超限）时换号只会重演同一句拒绝，必须直接返回。
func TestOpenStreamDoesNotRotateOnRequestError(t *testing.T) {
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prov := &coolProvider{
		failLabel: "a",
		err:       errors.New("upstream HTTP 400: invalid request"),
		events:    []provider.Event{{Type: provider.EventText, Text: "ok"}},
	}
	g := testGateway(prov)
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}, {Label: "b", Cookie: "c"}}

	meta := &ReqMeta{}
	ctx := WithReqMeta(context.Background(), meta)
	in := provider.ChatInput{Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}}}
	if _, err := g.openStream(ctx, "fake", prov, in); err == nil {
		t.Fatal("400 应原样上报")
	}
	if len(prov.calls) != 1 || prov.calls[0] != "a" {
		t.Fatalf("请求错误不该换号，calls = %v", prov.calls)
	}
	if got := st.ActiveIndex("fake"); got != 0 {
		t.Fatalf("失败账号不该被记为 active，得到 %d", got)
	}
}
