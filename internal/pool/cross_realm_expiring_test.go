// P6 跨域快过期优先：优先级从高到低 = 成本层 > 快过期(跨域) > 域偏好 > 权重。
//
// 背景：P1 把快过期升级为硬优先层，但那一层是**域内**收窄（pickLocked / AssignUIDsForModelRealm
// 里），而域序在最外层（PickExcludingForRealms / AssignUIDsForModelRealms）。于是只要首选域
// （global）还有号，次选域（cn）的快过期号就永远进不了候选——「快过期积分优先消耗」被域偏好
// 永久旁路，快过期积分只能烂在账上。
package pool

import (
	"reflect"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

// crossRealmExpiringPool 构造「global 高余额号 + cn 高余额号 + cn 快过期号」的池。
// 三个号都无模型观测（costTier 同为 tier 1），故成本层不参与，隔离出「快过期 vs 域偏好」。
func crossRealmExpiringPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "g-rich", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn-exp", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn-rich", Domain: "www.codebuddy.cn"})
	now := time.Now()
	// cn-exp：余额很小，但 100 分明天到期（有效快过期批次）。
	p.SetCreditsDetailed("cn-exp", 120, 120, 100, now.Add(24*time.Hour), 100)
	// 两个长期高余额号：无任何快过期批次。
	p.SetCreditsDetailed("g-rich", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	p.SetCreditsDetailed("cn-rich", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	return p
}

// TestPickExcludingForRealmsCrossRealmExpiring 选号侧：有跨域快过期号时，域偏好不再挡住它——
// 无论域序是 global 优先还是 cn 优先，都落在 cn 的快过期号上；关闭开关后回到域序（global 优先）。
func TestPickExcludingForRealmsCrossRealmExpiring(t *testing.T) {
	p := crossRealmExpiringPool(t)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealms(nil, "", []string{"global", "cn"})
		if a == nil || a.UID != "cn-exp" {
			t.Fatalf("第 %d 次选号=%v，want cn-exp（快过期跨域应压过 global 域偏好）", i, a)
		}
	}
	// 域序反转同样命中快过期号：结果与域序无关。
	for i := 0; i < 30; i++ {
		a := p.PickExcludingForRealms(nil, "", []string{"cn", "global"})
		if a == nil || a.UID != "cn-exp" {
			t.Fatalf("cn-first 第 %d 次选号=%v，want cn-exp", i, a)
		}
	}

	// 关闭总开关：硬优先层消失，回到旧口径（域序 global 优先）。
	p.SetPreferExpiring(false)
	a := p.PickExcludingForRealms(nil, "", []string{"global", "cn"})
	if a == nil || realmOf(a) != "global" {
		t.Fatalf("关闭快过期优先后选号=%v，want global（回到域序）", a)
	}
}

// TestPickExcludingForRealmsCrossRealmExpiringWithModel 模型感知口径下同样跨域优先
// （reqModel 非空时走 healthyForModel；三个号都无观测 → 同层）。
func TestPickExcludingForRealmsCrossRealmExpiringWithModel(t *testing.T) {
	p := crossRealmExpiringPool(t)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	a := p.PickExcludingForRealms(nil, "glm-5.2", []string{"global", "cn"})
	if a == nil || a.UID != "cn-exp" {
		t.Fatalf("模型感知选号=%v，want cn-exp", a)
	}
}

// TestAssignUIDsForModelRealmsCrossRealmExpiring 粘性分配侧：新会话分配先跨域取快过期层，
// 整份返回（而不是按域序把 global 排在前面）；关闭开关后回到按域序拼接。
func TestAssignUIDsForModelRealmsCrossRealmExpiring(t *testing.T) {
	p := crossRealmExpiringPool(t)

	got := p.AssignUIDsForModelRealms("", []string{"global", "cn"})
	if want := []string{"cn-exp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("分配候选=%v want %v（跨域快过期层应整份返回）", got, want)
	}

	// 可用性全集不受影响（仍是全部号）：否则已绑在普通号上的会话会被判失效而重绑。
	if all := p.AvailableUIDsForModelRealm("", ""); !reflect.DeepEqual(all, []string{"cn-exp", "cn-rich", "g-rich"}) {
		t.Fatalf("可用性全集=%v want [cn-exp cn-rich g-rich]", all)
	}

	// 关闭开关：快过期层消失，按域序拼接（global 域内 + cn 域内，各自按 uid 排序）。
	p.SetPreferExpiring(false)
	got = p.AssignUIDsForModelRealms("", []string{"global", "cn"})
	if want := []string{"g-rich", "cn-exp", "cn-rich"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("关闭后分配候选=%v want %v", got, want)
	}
}

// TestPreferRealmOverSticky 粘性让路判定：粘性号在快过期层内则不让位（快过期 > 域偏好）；
// 不在层内才按域优先让位；关闭开关后一律按域优先让位。
func TestPreferRealmOverSticky(t *testing.T) {
	p := crossRealmExpiringPool(t)

	// 粘性号 cn-exp 在快过期层内 → 不让位。
	if p.PreferRealmOverSticky("cn-exp", "cn", "", []string{"global", "cn"}) {
		t.Error("粘性号在快过期层内，不应让位给 global")
	}
	// 粘性号 cn-rich 不在快过期层 → 让位给首选域 global。
	if !p.PreferRealmOverSticky("cn-rich", "cn", "", []string{"global", "cn"}) {
		t.Error("粘性号不在快过期层，应让位给 global")
	}
	// 粘性已在首选域 → 永不让位。
	if p.PreferRealmOverSticky("g-rich", "global", "", []string{"global", "cn"}) {
		t.Error("粘性已在首选域，不应让位")
	}
	// 关闭开关：快过期层消失 → cn-exp 也让位。
	p.SetPreferExpiring(false)
	if !p.PreferRealmOverSticky("cn-exp", "cn", "", []string{"global", "cn"}) {
		t.Error("关闭快过期优先后 cn-exp 应让位给 global")
	}
}

// paidExpiringPool 复刻线上案例：cn 的快过期号（cn-exp）对 pay-model 实测**收费**（tier 2），
// 国际版号（g-free）实测**免费**（tier 0）。口径来自 2026-10-12 线上台账：
// `ㅤ.`(cn) 在 deepseek-v4.1-flash 上 cost 0.00087/1k，14 个 global 号 cost=0。
func paidExpiringPool(t *testing.T) *Pool {
	t.Helper()
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "g-free", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn-exp", Domain: "www.codebuddy.cn"})
	now := time.Now()
	p.SetCreditsDetailed("g-free", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	p.SetCreditsDetailed("cn-exp", 120, 120, 100, now.Add(24*time.Hour), 100)
	p.NoteModelCost("g-free", "pay-model", 0, 1000)   // tier 0：实测免费
	p.NoteModelCost("cn-exp", "pay-model", 0.9, 1000) // tier 2：实测收费
	return p
}

// freeTierExpiringPool 同上，但 cn-exp 对 free-model 也实测免费（同属最优成本层 tier 0）。
// 用于确认成本门槛不会误伤「快过期号本来就免费」的正常场景。
func freeTierExpiringPool(t *testing.T) *Pool {
	t.Helper()
	p := paidExpiringPool(t)
	p.NoteModelCost("cn-exp", "free-model", 0, 1000) // tier 0：与 g-free 同层
	p.NoteModelCost("g-free", "free-model", 0, 1000)
	return p
}

// TestAssignExpiringRespectsCostTier 粘性分配侧必须过成本层（成本层 > 快过期）：
// 快过期号在收费层、国际版号在免费层时，新会话不能被跨域快过期层改道到收费号上。
func TestAssignExpiringRespectsCostTier(t *testing.T) {
	p := paidExpiringPool(t)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 分配候选：快过期层被成本门拦掉 → 退到按域序拼接。
	if got, want := p.AssignUIDsForModelRealms("pay-model", []string{"global", "cn"}), []string{"g-free", "cn-exp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("分配候选=%v want %v（快过期号在收费层，不应被硬优先）", got, want)
	}
	// 单域入口同样不收窄。
	if got, want := p.AssignUIDsForModelRealm("pay-model", "global"), []string{"g-free"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("单域分配=%v want %v", got, want)
	}
	// 选号：跨域快过期趟被成本门拦掉 → 落到免费的国际版号。
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealms(nil, "pay-model", []string{"global", "cn"})
		if a == nil || a.UID != "g-free" {
			t.Fatalf("第 %d 次选号=%v want g-free（成本层高于快过期）", i, a)
		}
	}
	// 粘性让路：粘在收费快过期号上 → 让位（否则粘性绕过成本分层，一直付费）。
	if !p.PreferRealmOverSticky("cn-exp", "cn", "pay-model", []string{"global", "cn"}) {
		t.Error("粘性号在收费层、域内有免费号，应让位")
	}
}

// TestExpiringKeptWhenSameCostTier 快过期号与普通号同处最优成本层时，快过期优先照旧生效
// （成本门只在「快过期号更贵」时拦截，不改变 P6 的跨域快过期语义）。
func TestExpiringKeptWhenSameCostTier(t *testing.T) {
	p := freeTierExpiringPool(t)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	if got, want := p.AssignUIDsForModelRealms("free-model", []string{"global", "cn"}), []string{"cn-exp"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("分配候选=%v want %v（同成本层内快过期跨域优先）", got, want)
	}
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForRealms(nil, "free-model", []string{"global", "cn"})
		if a == nil || a.UID != "cn-exp" {
			t.Fatalf("第 %d 次选号=%v want cn-exp（同成本层内快过期优先）", i, a)
		}
	}
	if p.PreferRealmOverSticky("cn-exp", "cn", "free-model", []string{"global", "cn"}) {
		t.Error("快过期号已在最优成本层内，不应让位")
	}
}

// TestPreferRealmOverStickyYieldsWhenCostlier 粘性号即使已在首选域，只要它不是最优成本层
// （域内有更便宜的可用号）也要让位——「已在首选域」不再等于「永不让位」。
func TestPreferRealmOverStickyYieldsWhenCostlier(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.SetCostExploreInterval(0)
	p.Add(&auth.Auth{UID: "g-free", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g-paid", Domain: "www.workbuddy.ai"})
	p.SetCreditsDetailed("g-free", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	p.SetCreditsDetailed("g-paid", 1_000_000, 1_000_000, 0, time.Time{}, 0)
	p.NoteModelCost("g-free", "pay-model", 0, 1000)
	p.NoteModelCost("g-paid", "pay-model", 0.9, 1000)

	// 粘在收费号上（域内还有免费号）→ 让位。
	if !p.PreferRealmOverSticky("g-paid", "global", "pay-model", []string{"global", "cn"}) {
		t.Error("粘性号非最优成本层（域内有免费号），应让位")
	}
	// 粘在免费号上（本身已是最优层，无快过期批次）→ 按域优先，仍在首选域故不让位。
	if p.PreferRealmOverSticky("g-free", "global", "pay-model", []string{"global", "cn"}) {
		t.Error("粘性号已在最优成本层且首选域，不应让位")
	}
}
