// 快过期硬优先层（P1）+ 保底豁免（P3）+ 层内分级加权（P5）。
//
// 背景：旧实现只把快过期号的选号权重乘 expiringVirtualSlots，实测只领先几个百分点
// （53% : 47%），且仅在会话分配时生效——「30 分明天过期」会被高余额长期号压住而
// 白白蒸发。P1 把它升级为硬语义：最优成本层里只要有有效快过期号，候选就收窄到它们
// 之内（在途占满自然溢出，不堵死）。
package pool

import (
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

// expiringPool 构造「一个快过期小余额号 + 一个长期高余额号」的池。
// 两个号都无模型观测（costTier 同为 tier 1），故不涉及成本分层。
func expiringPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "expiring"})
	p.Add(&auth.Auth{UID: "rich"})
	now := time.Now()
	// expiring：余额很小，但 100 分明天到期（在 expiring_soon 窗口内的有效批次）。
	p.SetCreditsDetailed("expiring", 120, 120, 100, now.Add(24*time.Hour), 100)
	// rich：百万余额，无任何快过期批次。
	p.SetCreditsDetailed("rich", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	return p
}

// TestExpiringHardTierBeatsHighCredits P1：只要存在有效快过期号，选号就完全落在它
// 身上——高余额长期号不再是「大多数」，而是零。关闭开关后回到旧口径（高余额占多数）。
func TestExpiringHardTierBeatsHighCredits(t *testing.T) {
	p := expiringPool(t)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	for i := 0; i < 30; i++ {
		a := p.Pick()
		if a == nil || a.UID != "expiring" {
			t.Fatalf("第 %d 次选号=%v，want expiring（快过期硬优先层应独占候选）", i, a)
		}
	}

	// 关闭总开关：硬优先层消失，高余额号按权重吃掉绝大多数流量。
	p.SetPreferExpiring(false)
	counts := map[string]int{}
	for i := 0; i < 200; i++ {
		a := p.Pick()
		if a == nil {
			t.Fatal("关闭快过期优先后选号返回 nil")
		}
		counts[a.UID]++
	}
	if counts["rich"] <= counts["expiring"] {
		t.Fatalf("关闭快过期优先后 counts=%v，want rich 占多数", counts)
	}
}

// TestExpiringTierOverflowsWhenInFlightFull P1 的防堵死性质：快过期号在途占满时
// 候选收窄后为空 → 自然溢出到普通层，而不是无号可用。
func TestExpiringTierOverflowsWhenInFlightFull(t *testing.T) {
	p := expiringPool(t)
	p.SetMaxInFlight(1)
	if !p.Acquire("expiring") {
		t.Fatal("acquire expiring 失败")
	}
	defer p.Release("expiring")

	a := p.Pick()
	if a == nil || a.UID != "rich" {
		t.Fatalf("在途占满的快过期号应溢出到普通层，got %v", a)
	}
}

// TestExpiringTierSkipsTinyResidue P5 门槛：快过期量低于 expiringMinCredits 时不进
// 优先层（为残渣把所有新会话改道到一个号并不划算）。
func TestExpiringTierSkipsTinyResidue(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "tiny"})
	p.Add(&auth.Auth{UID: "rich"})
	now := time.Now()
	p.SetCreditsDetailed("tiny", 100, 100, expiringMinCredits-1, now.Add(time.Hour), expiringMinCredits-1)
	p.SetCreditsDetailed("rich", 100, 100, 0, time.Time{}, 0)

	p.mu.RLock()
	tiny := expiringPreferred(p.byUID["tiny"], now)
	p.mu.RUnlock()
	if tiny {
		t.Fatalf("快过期量 %d < 门槛 %d，不应进入优先层", expiringMinCredits-1, expiringMinCredits)
	}

	got := p.AssignUIDsForModelRealm("m", "")
	want := []string{"rich", "tiny"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("分配候选=%v want %v（无优先层时应回落全集）", got, want)
	}
}

// TestAssignPrefersExpiringTier P1 粘性侧：新会话分配优先落在快过期层；批次被烧空
// （expiring 归零）后立刻回落全集。可用性校验走的是另一个方法，不受此收窄影响。
func TestAssignPrefersExpiringTier(t *testing.T) {
	p := expiringPool(t)

	got := p.AssignUIDsForModelRealm("m", "")
	if want := []string{"expiring"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("分配候选=%v want %v", got, want)
	}
	// 可用性全集必须仍是两个号：否则已绑在 rich 上的会话会被判失效而重绑
	// （那等于让粘性给快过期让路，是刻意未启用的另一档策略）。
	if all := p.AvailableUIDsForModelRealm("m", ""); !reflect.DeepEqual(all, []string{"expiring", "rich"}) {
		t.Fatalf("可用性全集=%v want [expiring rich]", all)
	}

	// 批次烧空：expiring 快过期归零 → 优先层消失。
	p.SetCreditsDetailed("expiring", 20, 120, 0, time.Time{}, 0)
	got = p.AssignUIDsForModelRealm("m", "")
	if want := []string{"expiring", "rich"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("批次烧空后分配候选=%v want %v", got, want)
	}
}

// TestExpiringPreferredIgnoresExpiredBatch 过期时刻已过的批次不算有效快过期
// （expiringNow 的既有语义，P1 层判据必须继承它）。
func TestExpiringPreferredIgnoresExpiredBatch(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	now := time.Now()
	p.SetCreditsDetailed("u1", 100, 100, 100, now.Add(time.Hour), 100)

	p.mu.RLock()
	if !expiringPreferred(p.byUID["u1"], now) {
		t.Fatal("未过期批次应进入优先层")
	}
	if expiringPreferred(p.byUID["u1"], now.Add(2*time.Hour)) {
		t.Fatal("已过期批次不应进入优先层")
	}
	p.mu.RUnlock()
}

// TestCreditFloorExemptsExpiring P3：快过期号不受积分保底拦截——保底拦下来等于
// 白送（余额 96 < floor 100、明天到期的号，96 分注定蒸发）。
func TestCreditFloorExemptsExpiring(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetCreditFloor(100)
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCredits("poor", 30, 0)
	p.NoteModelCost("poor", "hy4-preview", 2.9, 1000) // tier 2：实测收费

	// 无快过期批次：照旧被 floor 拦住。
	if a := p.PickExcludingForRealm(nil, "hy4-preview", ""); a != nil {
		t.Fatalf("触底且无快过期批次，want nil，got %v", a.UID)
	}

	// 刷入有效快过期批次（余额 96 仍低于 floor）：放行。
	p.SetCreditsDetailed("poor", 96, 100, 96, time.Now().Add(24*time.Hour), 96)
	a := p.PickExcludingForRealm(nil, "hy4-preview", "")
	if a == nil || a.UID != "poor" {
		t.Fatalf("快过期号应豁免保底，got %v", a)
	}
}

// TestExpiringWeightScalesWithAmount P5：层内加权按快过期量分级——100 分的号拿到
// 满额 expiringVirtualSlots，10 分的号只有 1+2×0.1。两个号余额/闲置一致，故权重比
// 就是量因子之比。
func TestExpiringWeightScalesWithAmount(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "big"})
	p.Add(&auth.Auth{UID: "small"})
	now := time.Now()
	at := now.Add(24 * time.Hour)
	p.SetCreditsDetailed("big", 100, 100, 100, at, 100)
	p.SetCreditsDetailed("small", 100, 100, 10, at, 10)

	p.mu.RLock()
	wBig := p.routingWeightOf(p.byUID["big"], 100, 100, now)
	wSmall := p.routingWeightOf(p.byUID["small"], 100, 100, now)
	p.mu.RUnlock()

	wantBig := 1 + float64(expiringVirtualSlots-1) // 量因子满额
	wantSmall := 1 + float64(expiringVirtualSlots-1)*10.0/100.0
	if ratio, want := wBig/wSmall, wantBig/wantSmall; math.Abs(ratio-want) > 1e-9 {
		t.Fatalf("量因子比=%v want %v（wBig=%v wSmall=%v）", ratio, want, wBig, wSmall)
	}
}
