package gateway

import (
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// creditGateway 只装选号需要的最小依赖（配置 + 余额快照缓存）。
func creditGateway(cr *config.CreditRouting) *Gateway {
	return &Gateway{cfg: &config.Config{CreditRouting: cr}, credits: newCreditCache()}
}

func creditAccounts(labels ...string) []*provider.Account {
	out := make([]*provider.Account, 0, len(labels))
	for _, l := range labels {
		out = append(out, &provider.Account{Label: l})
	}
	return out
}

func sameOrder(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// 每日积分的到期时刻是「下一个本地零点」，落在窗口内；每月积分只有在月末才进窗口；
// 奖励 / 充值积分无已知到期，永远不参与快过期偏好。
func TestExpiringOfWalletKinds(t *testing.T) {
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.Local)
	snap := creditSnapshot{Wallets: []creditWallet{
		{Kind: creditDaily, Amount: 100},
		{Kind: creditMonthly, Amount: 1000}, // 11-01 到期，> 168h → 不算
		{Kind: creditNone, Amount: 6000},
	}}
	total, earliest := expiringOf(snap, now, 168*time.Hour)
	if total != 100 {
		t.Errorf("expiring=%d want 100（只有每日积分在窗口内）", total)
	}
	if want := time.Date(2026, 10, 12, 0, 0, 0, 0, time.Local); !earliest.Equal(want) {
		t.Errorf("earliest=%v want %v", earliest, want)
	}
	// 月末：每月积分也进窗口，但每日仍先到期。
	monthEnd := time.Date(2026, 10, 30, 15, 0, 0, 0, time.Local)
	total, earliest = expiringOf(snap, monthEnd, 168*time.Hour)
	if total != 1100 {
		t.Errorf("月末 expiring=%d want 1100", total)
	}
	if want := time.Date(2026, 10, 31, 0, 0, 0, 0, time.Local); !earliest.Equal(want) {
		t.Errorf("月末 earliest=%v want %v", earliest, want)
	}
}

func TestWalletKindOf(t *testing.T) {
	cases := map[string]creditKind{
		"daily_points":    creditDaily,
		"daily_credits":   creditDaily,
		"monthly_points":  creditMonthly,
		"monthly_credits": creditMonthly,
		"reward_points":   creditNone,
		"topup_points":    creditNone,
		"":                creditNone,
	}
	for in, want := range cases {
		if got := walletKindOf(in); got != want {
			t.Errorf("walletKindOf(%q)=%v want %v", in, got, want)
		}
	}
}

// 有快过期积分的账号被提到最前，其余保持原轮询顺序。
func TestPrioritizeExpiringOrdersHotFirst(t *testing.T) {
	g := creditGateway(nil)
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.Local)
	as := creditAccounts("a", "b", "c")
	g.noteCredits("raccoon", "b", 500, []creditWallet{{Kind: creditDaily, Amount: 500}})
	g.noteCredits("raccoon", "c", 6000, []creditWallet{{Kind: creditNone, Amount: 6000}})
	g.noteCredits("raccoon", "a", 0, []creditWallet{{Kind: creditDaily, Amount: 0}})
	if got := g.prioritizeExpiring("raccoon", as, []int{0, 1, 2}, now); !sameOrder(got, []int{1, 0, 2}) {
		t.Fatalf("order=%v want [1 0 2]", got)
	}
}

// 同为每日到期时金额大的先上：先烧快过期里最多的，避免为几分散号。
func TestPrioritizeExpiringTieBreakByAmount(t *testing.T) {
	g := creditGateway(nil)
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.Local)
	as := creditAccounts("a", "b")
	g.noteCredits("raccoon", "a", 10, []creditWallet{{Kind: creditDaily, Amount: 10}})
	g.noteCredits("raccoon", "b", 900, []creditWallet{{Kind: creditDaily, Amount: 900}})
	if got := g.prioritizeExpiring("raccoon", as, []int{0, 1}, now); !sameOrder(got, []int{1, 0}) {
		t.Fatalf("order=%v want [1 0]", got)
	}
}

// 低于门槛的快过期积分不参与（与 pool.expiringMinCredits 同口径）。
func TestPrioritizeExpiringBelowMinIgnored(t *testing.T) {
	g := creditGateway(nil)
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.Local)
	as := creditAccounts("a", "b")
	g.noteCredits("raccoon", "a", 3, []creditWallet{{Kind: creditDaily, Amount: 3}})
	if got := g.prioritizeExpiring("raccoon", as, []int{0, 1}, now); !sameOrder(got, []int{0, 1}) {
		t.Fatalf("order=%v want 原顺序 [0 1]", got)
	}
}

// 关闭开关 / 没有快照时必须保持旧的轮询顺序（零回归）。
func TestPrioritizeExpiringDisabledOrNoSnapshot(t *testing.T) {
	now := time.Date(2026, 10, 11, 15, 0, 0, 0, time.Local)
	as := creditAccounts("a", "b")
	off := false
	g := creditGateway(&config.CreditRouting{Enabled: &off})
	g.noteCredits("raccoon", "b", 500, []creditWallet{{Kind: creditDaily, Amount: 500}})
	if got := g.prioritizeExpiring("raccoon", as, []int{0, 1}, now); !sameOrder(got, []int{0, 1}) {
		t.Fatalf("关闭时 order=%v want [0 1]", got)
	}
	if got := creditGateway(nil).prioritizeExpiring("raccoon", as, []int{0, 1}, now); !sameOrder(got, []int{0, 1}) {
		t.Fatalf("无快照时 order=%v want [0 1]", got)
	}
}

// CreditExpiring 无快照时 ok=false；有快照时返回窗口内的快过期量与最早到期时刻。
func TestCreditExpiringSnapshot(t *testing.T) {
	g := creditGateway(nil)
	if _, _, ok := g.CreditExpiring("raccoon", "a"); ok {
		t.Error("无快照应返回 ok=false")
	}
	g.noteCredits("raccoon", "a", 6472, []creditWallet{
		{Kind: creditDaily, Amount: 472},
		{Kind: creditNone, Amount: 6000},
	})
	exp, earliest, ok := g.CreditExpiring("raccoon", "a")
	if !ok || exp != 472 {
		t.Fatalf("expiring=%d ok=%v want 472/true", exp, ok)
	}
	if earliest.IsZero() {
		t.Error("应返回最早到期时刻")
	}
}
