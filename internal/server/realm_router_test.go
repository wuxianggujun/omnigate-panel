package server

import (
	"reflect"
	"testing"
)

// noProvides 目录探测“无数据”（known=false）：不参与域剔除。
func noProvides(realm, bare string) (bool, bool) { return false, false }

func TestRealmRouterExplicitPrefix(t *testing.T) {
	rr := NewRealmRouter([]string{"cn", "global"}, map[string]string{"glm-*": "cn"})
	// 显式前缀是硬指定：忽略 order/prefer/provides。
	if bare, realms := rr.Ordered("global:gpt-5.4", noProvides); bare != "gpt-5.4" || !reflect.DeepEqual(realms, []string{"global"}) {
		t.Fatalf("global: bare=%q realms=%v", bare, realms)
	}
	if bare, realms := rr.Ordered("cn:glm-5.2", noProvides); bare != "glm-5.2" || !reflect.DeepEqual(realms, []string{"cn"}) {
		t.Fatalf("cn: bare=%q realms=%v", bare, realms)
	}
	// 非枚举前缀视为裸名（如 "foo:bar" 不是域前缀）。
	if bare, realms := rr.Ordered("foo:bar", nil); bare != "foo:bar" || !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("foo:bar bare=%q realms=%v", bare, realms)
	}
}

func TestRealmRouterOrderAndPrefer(t *testing.T) {
	// 裸名默认按 order。
	rr := NewRealmRouter([]string{"cn", "global"}, nil)
	if _, realms := rr.Ordered("foo", nil); !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("bare default: %v", realms)
	}
	// prefer 命中 → 提到最前。
	rr = NewRealmRouter([]string{"cn", "global"}, map[string]string{"deepseek-*": "global"})
	if _, realms := rr.Ordered("deepseek-v4.1-flash", nil); !reflect.DeepEqual(realms, []string{"global", "cn"}) {
		t.Fatalf("prefer global: %v", realms)
	}
	if _, realms := rr.Ordered("glm-5.2", nil); !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("no rule: %v", realms)
	}
	// 更具体的规则优先：gpt-5.6-* 覆盖 gpt-*。
	rr = NewRealmRouter([]string{"cn", "global"}, map[string]string{"gpt-*": "global", "gpt-5.6-*": "cn"})
	if _, realms := rr.Ordered("gpt-5.6-sol", nil); !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("specific rule: %v", realms)
	}
	if _, realms := rr.Ordered("gpt-5.4", nil); !reflect.DeepEqual(realms, []string{"global", "cn"}) {
		t.Fatalf("generic rule: %v", realms)
	}
}

func TestRealmRouterProvidesFilter(t *testing.T) {
	rr := NewRealmRouter([]string{"cn", "global"}, nil)
	onlyGlobal := func(realm, bare string) (bool, bool) { return true, realm == "global" }
	// 目录已知：只有 global 提供 → 剔除 cn，避免把请求浪费在错域（MaxRotate 只有 3）。
	if _, realms := rr.Ordered("gpt-5.4", onlyGlobal); !reflect.DeepEqual(realms, []string{"global"}) {
		t.Fatalf("global-only: %v", realms)
	}
	// 目录冷（known=false）→ 不剔除。
	if _, realms := rr.Ordered("gpt-5.4", noProvides); !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("cold catalog: %v", realms)
	}
	// 目录已知但两域都不提供（新上架）→ 保留完整顺序，交给选号/上游判定。
	none := func(realm, bare string) (bool, bool) { return true, false }
	if _, realms := rr.Ordered("brand-new", none); !reflect.DeepEqual(realms, []string{"cn", "global"}) {
		t.Fatalf("none provides: %v", realms)
	}
	// prefer global 但只有 cn 提供 → 剔除 global，落到 cn。
	rr2 := NewRealmRouter([]string{"cn", "global"}, map[string]string{"x-*": "global"})
	onlyCN := func(realm, bare string) (bool, bool) { return true, realm == "cn" }
	if _, realms := rr2.Ordered("x-model", onlyCN); !reflect.DeepEqual(realms, []string{"cn"}) {
		t.Fatalf("prefer global but only cn: %v", realms)
	}
}

// order 定义了允许的域集合：prefer 不得把未启用的域重新引入（global 逃生门关闭时
// order 只剩 ["cn"]，prefer→global 必须被忽略）。
func TestRealmRouterPreferCannotReenableDisabledRealm(t *testing.T) {
	rr := NewRealmRouter([]string{"cn"}, map[string]string{"deepseek-*": "global"})
	if _, realms := rr.Ordered("deepseek-v4.1-flash", nil); !reflect.DeepEqual(realms, []string{"cn"}) {
		t.Fatalf("prefer must not re-enable disabled realm: %v", realms)
	}
}

func TestRealmRouterNormalizeAndReconfigure(t *testing.T) {
	// 非法项忽略、去重、空回落缺省。
	rr := NewRealmRouter([]string{"bogus", "cn", "cn"}, nil)
	if !reflect.DeepEqual(rr.Order(), []string{"cn"}) {
		t.Fatalf("normalize: %v", rr.Order())
	}
	rr = NewRealmRouter(nil, nil)
	if !reflect.DeepEqual(rr.Order(), []string{"cn", "global"}) {
		t.Fatalf("default order: %v", rr.Order())
	}
	// Reconfigure 热替换。
	rr.Reconfigure([]string{"global", "cn"}, nil)
	if _, realms := rr.Ordered("foo", nil); !reflect.DeepEqual(realms, []string{"global", "cn"}) {
		t.Fatalf("reconfigure: %v", realms)
	}
}

func TestResolveModelBackCompat(t *testing.T) {
	// 老入口语义不变：裸名 → cn；显式前缀 → 该域。
	if realm, bare := resolveModel("glm-5.2"); realm != "cn" || bare != "glm-5.2" {
		t.Fatalf("bare: %q %q", realm, bare)
	}
	if realm, bare := resolveModel("global:gpt-5.4"); realm != "global" || bare != "gpt-5.4" {
		t.Fatalf("global: %q %q", realm, bare)
	}
}
