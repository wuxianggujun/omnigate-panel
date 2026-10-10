package gateway

import (
	"context"
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
