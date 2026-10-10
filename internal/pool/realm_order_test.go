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
