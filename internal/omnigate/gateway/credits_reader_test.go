package gateway

import (
	"context"
	"errors"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// fakeBalanceProvider 实现 Provider + BalanceReader，并顺带实现 TokenRefresher，
// 用来验证余额刷新走的是能力接口而不是类型字符串。
type fakeBalanceProvider struct {
	name string
	bal  *provider.Balance
	err  error

	calls int

	expired    bool
	refreshTo  string
	refreshErr error
	refreshed  bool
}

func (p *fakeBalanceProvider) Name() string { return p.name }
func (p *fakeBalanceProvider) Type() string { return "fake" }
func (p *fakeBalanceProvider) ListModels(context.Context) ([]openai.Model, error) {
	return nil, nil
}
func (p *fakeBalanceProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, nil
}
func (p *fakeBalanceProvider) QueryBalance(context.Context, *provider.Account) (*provider.Balance, error) {
	p.calls++
	return p.bal, p.err
}
func (p *fakeBalanceProvider) TokenExpired(*provider.Account) bool { return p.expired }
func (p *fakeBalanceProvider) RefreshAccount(_ context.Context, acc *provider.Account) error {
	p.refreshed = true
	if p.refreshErr != nil {
		return p.refreshErr
	}
	if p.refreshTo != "" {
		acc.Cookie = p.refreshTo
	}
	return nil
}
func (p *fakeBalanceProvider) Unauthorized(error) bool { return false }

// creditReaderGateway 装好 refreshCredits 需要的最小依赖（含 state，供 token 落盘）。
func creditReaderGateway(t *testing.T, prov provider.Provider, accts ...*provider.Account) *Gateway {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{
		cfg:       &config.Config{},
		log:       logx.New(10),
		st:        st,
		providers: map[string]provider.Provider{prov.Name(): prov},
		accounts:  map[string][]*provider.Account{prov.Name(): accts},
		credits:   newCreditCache(),
	}
}

// 实现了 BalanceReader 的类型自动进入余额刷新：钱包按重置节奏映射成选号类别，
// 快过期优先选号立刻可用。类型字符串完全不参与判断。
func TestRefreshCreditsUsesBalanceReader(t *testing.T) {
	prov := &fakeBalanceProvider{name: "p", bal: &provider.Balance{
		Available: 7100,
		Unit:      "积分",
		Wallets: []provider.BalanceWallet{
			{Type: "daily", Amount: 100, Resets: provider.ResetDaily},
			{Type: "monthly", Amount: 1000, Resets: provider.ResetMonthly},
			{Type: "reward", Amount: 6000, Resets: provider.ResetNever},
		},
	}}
	g := creditReaderGateway(t, prov, &provider.Account{Label: "a", Cookie: "tok"})

	g.refreshCredits(context.Background(), "p")

	if prov.calls != 1 {
		t.Fatalf("QueryBalance calls = %d, want 1", prov.calls)
	}
	snap, ok := g.credits.get(creditKey("p", "a"))
	if !ok || snap.Available != 7100 {
		t.Fatalf("snapshot = %+v ok=%v, want available 7100", snap, ok)
	}
	if len(snap.Wallets) != 3 {
		t.Fatalf("wallets = %+v", snap.Wallets)
	}
	// 只有每日钱包在快过期窗口内（每月 1 号才到期）。
	exp, earliest, ok := g.CreditExpiring("p", "a")
	if !ok || exp != 100 || earliest.IsZero() {
		t.Fatalf("CreditExpiring = %d/%v ok=%v, want 100/非零/true", exp, earliest, ok)
	}
}

// 没有实现 BalanceReader 的类型静默跳过：选号退化为旧轮询行为，且不写任何快照。
func TestRefreshCreditsSkipsNonReader(t *testing.T) {
	prov := &typedProvider{name: "p", typ: "openai"}
	g := creditReaderGateway(t, prov, &provider.Account{Label: "a", Cookie: "k"})

	g.refreshCredits(context.Background(), "p")

	if _, ok := g.credits.get(creditKey("p", "a")); ok {
		t.Fatal("非 BalanceReader 不该写余额快照")
	}
}

// token 型凭据在查余额前先续期：过期 → 调 RefreshAccount → 用新 token 查询。
func TestRefreshCreditsRefreshesExpiredToken(t *testing.T) {
	prov := &fakeBalanceProvider{
		name:      "p",
		bal:       &provider.Balance{Available: 5, Wallets: []provider.BalanceWallet{{Amount: 5, Resets: provider.ResetDaily}}},
		expired:   true,
		refreshTo: "fresh",
	}
	acc := &provider.Account{Label: "a", Cookie: "stale", RefreshToken: "r"}
	g := creditReaderGateway(t, prov, acc)

	g.refreshCredits(context.Background(), "p")

	if !prov.refreshed {
		t.Fatal("过期 token 应触发续期")
	}
	if acc.Cookie != "fresh" {
		t.Fatalf("cookie = %q, want fresh", acc.Cookie)
	}
	if prov.calls != 1 {
		t.Fatalf("QueryBalance calls = %d, want 1", prov.calls)
	}
	if got := g.st.Cookie("p", "a", ""); got != "fresh" {
		t.Fatalf("续期后的 token 未落盘: %q", got)
	}
}

// 续期失败只记账告警，随后仍按旧凭据查一次余额——与旧实现一致（错误会以快照形式
// 节流重试，而不是每个请求都打上游）。
func TestRefreshCreditsRefreshFailureStillQueries(t *testing.T) {
	prov := &fakeBalanceProvider{
		name:       "p",
		err:        errors.New("401"),
		expired:    true,
		refreshErr: errors.New("refresh_token 过期"),
	}
	g := creditReaderGateway(t, prov, &provider.Account{Label: "a", Cookie: "stale"})

	g.refreshCredits(context.Background(), "p")

	if !prov.refreshed || prov.calls != 1 {
		t.Fatalf("refreshed=%v calls=%d, want true/1", prov.refreshed, prov.calls)
	}
	snap, ok := g.credits.get(creditKey("p", "a"))
	if !ok || snap.Err != "401" {
		t.Fatalf("失败应写入带 Err 的快照以节流重试, got %+v ok=%v", snap, ok)
	}
}

// 空凭据的账号直接跳过：既不续期也不打上游。
func TestRefreshCreditsSkipsAccountWithoutCredential(t *testing.T) {
	prov := &fakeBalanceProvider{name: "p", bal: &provider.Balance{}}
	g := creditReaderGateway(t, prov, &provider.Account{Label: "a", RefreshToken: "r"})

	g.refreshCredits(context.Background(), "p")

	if prov.calls != 0 || prov.refreshed {
		t.Fatalf("无凭据账号不该触发任何上游调用, calls=%d refreshed=%v", prov.calls, prov.refreshed)
	}
}

func TestCreditKindOfResetCadence(t *testing.T) {
	cases := map[provider.ResetCadence]creditKind{
		provider.ResetNever:   creditNone,
		provider.ResetDaily:   creditDaily,
		provider.ResetMonthly: creditMonthly,
	}
	for in, want := range cases {
		if got := creditKindOf(in); got != want {
			t.Errorf("creditKindOf(%q)=%v want %v", in, got, want)
		}
	}
}
