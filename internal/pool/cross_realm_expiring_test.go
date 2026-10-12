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
