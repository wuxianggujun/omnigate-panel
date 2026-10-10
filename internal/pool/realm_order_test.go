package pool

import (
	"testing"
	"time"
)

// TestPickExcludingForRealmsPrefersOrder 全部健康时按 order 取第一个域。
func TestPickExcludingForRealmsPrefersOrder(t *testing.T) {
	p := realmPool(t)
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealms(nil, "", []string{"global", "cn"})
		if a == nil || realmOf(a) != "global" {
			t.Fatalf("want global-first, got %v", a)
		}
	}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealms(nil, "", []string{"cn", "global"})
		if a == nil || realmOf(a) != "cn" {
			t.Fatalf("want cn-first, got %v", a)
		}
	}
}

// TestPickExcludingForRealmsFallsBackWhenPreferredCooling 首选域全部软冷却时，应跨域
// 回退到次选域的健康号，而不是捞首选域的冷却号（这是「优先域优先 + 没号跨域回退」的关键）。
func TestPickExcludingForRealmsFallsBackWhenPreferredCooling(t *testing.T) {
	p := realmPool(t)
	p.Cooldown("g1", CoolSoft, time.Hour, "429")
	p.Cooldown("g2", CoolSoft, time.Hour, "429")
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealms(nil, "", []string{"global", "cn"})
		if a == nil {
			t.Fatal("nil")
		}
		if realmOf(a) != "cn" {
			t.Fatalf("preferred realm cooling → want cn healthy, got %s (%s)", a.UID, realmOf(a))
		}
	}
}

// TestPickExcludingForRealmsCooldownFallbackWhenAllCooling 两个域都没有健康号时，仍要
// 返回冷却兜底号（而非 nil），且优先域优先。
func TestPickExcludingForRealmsCooldownFallbackWhenAllCooling(t *testing.T) {
	p := realmPool(t)
	for _, uid := range []string{"cn1", "cn2", "g1", "g2"} {
		p.Cooldown(uid, CoolSoft, time.Hour, "429")
	}
	a := p.PickExcludingForRealms(nil, "", []string{"global", "cn"})
	if a == nil {
		t.Fatal("all cooling → want cooldown fallback account, got nil")
	}
	if realmOf(a) != "global" {
		t.Fatalf("cooldown fallback should prefer first realm, got %s (%s)", a.UID, realmOf(a))
	}
}

// TestPickExcludingForRealmsSingleRealm 单域退化为 PickExcludingForRealm 语义。
func TestPickExcludingForRealmsSingleRealm(t *testing.T) {
	p := realmPool(t)
	a := p.PickExcludingForRealms(nil, "", []string{"global"})
	if a == nil || realmOf(a) != "global" {
		t.Fatalf("single global realm: %v", a)
	}
	// 空列表退化为不过滤（现状）。
	if a := p.PickExcludingForRealms(nil, "", nil); a == nil {
		t.Fatal("empty realms → nil")
	}
}

// TestPreferRealmAvailable 域优先级高于会话粘性：粘性号在次选域时，只要更高优先域
// 仍有可用号就让位（true）；更高优先域无号（冷却/占满/模型级冷却）则保留粘性（false）。
func TestPreferRealmAvailable(t *testing.T) {
	p := realmPool(t)

	// 首选域 global 全健康 → 粘性 cn 让位。
	if !p.PreferRealmAvailable("cn", "", []string{"global", "cn"}) {
		t.Error("global healthy → sticky cn should yield")
	}
	// 粘性已在首选域 → 不让位（保留粘性）。
	if p.PreferRealmAvailable("global", "", []string{"global", "cn"}) {
		t.Error("sticky at first realm must be kept")
	}
	// order 反转：cn 首选时，粘性 global 让位。
	if !p.PreferRealmAvailable("global", "", []string{"cn", "global"}) {
		t.Error("cn-first order → sticky global should yield")
	}
	// 单域（显式前缀）：粘性域即唯一域 → 不让位。
	if p.PreferRealmAvailable("cn", "", []string{"cn"}) {
		t.Error("single realm → never yield")
	}

	// 首选域 global 全软冷却 → 粘性 cn 保留。
	p.Cooldown("g1", CoolSoft, time.Hour, "429")
	p.Cooldown("g2", CoolSoft, time.Hour, "429")
	if p.PreferRealmAvailable("cn", "", []string{"global", "cn"}) {
		t.Error("global cooling → sticky cn should be kept")
	}

	// 模型级冷却：更高优先域仅因该模型无号 → 不让位；换一个模型仍健康 → 让位。
	p2 := realmPool(t)
	p2.CooldownSoftForModel("g1", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "6004")
	p2.CooldownSoftForModel("g2", 10*time.Minute, time.Now().Add(30*time.Minute), "glm-5.2", "6004")
	if p2.PreferRealmAvailable("cn", "glm-5.2", []string{"global", "cn"}) {
		t.Error("global model-cooldown for glm-5.2 → sticky cn should be kept")
	}
	if !p2.PreferRealmAvailable("cn", "other-model", []string{"global", "cn"}) {
		t.Error("global healthy for other-model → sticky cn should yield")
	}

	// 在途占满：首选域唯一账号占满 → 不让位。
	p3 := realmPool(t)
	p3.SetMaxInFlight(1)
	if !p3.Acquire("g1") {
		t.Fatal("acquire g1")
	}
	if !p3.Acquire("g2") {
		t.Fatal("acquire g2")
	}
	if p3.PreferRealmAvailable("cn", "", []string{"global", "cn"}) {
		t.Error("global in-flight full → sticky cn should be kept")
	}
}

// TestWeightedAvailableUIDsForModelRealms 按 order 拼接各域加权账号（域内排序）。
func TestWeightedAvailableUIDsForModelRealms(t *testing.T) {
	p := realmPool(t)
	got := p.WeightedAvailableUIDsForModelRealms("", []string{"global", "cn"})
	want := []string{"g1", "g2", "cn1", "cn2"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WeightedAvailableUIDsForModelRealms=%v want %v", got, want)
		}
	}
	// 单域等价旧方法。
	one := p.WeightedAvailableUIDsForModelRealms("", []string{"cn"})
	ref := p.WeightedAvailableUIDsForModelRealm("", "cn")
	if len(one) != len(ref) {
		t.Fatalf("single realm mismatch: %v vs %v", one, ref)
	}
}
