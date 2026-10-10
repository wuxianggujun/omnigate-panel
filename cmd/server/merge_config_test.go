package main

import "testing"

// outbound 段是「整段替换」语义：删掉代理/路由后保存必须真正清掉旧键，
// 否则会残留「路由引用已删代理」→ 校验 400（出站代理卡片保存失败的根因）。
func TestMergeConfigMapsOutboundAtomic(t *testing.T) {
	cur := map[string]any{
		"listen": ":7863",
		"outbound": map[string]any{
			"proxies": []any{map[string]any{"name": "old"}},
			"routes":  map[string]any{"workbuddy": "old"},
		},
	}
	incoming := map[string]any{
		"outbound": map[string]any{
			"proxies": []any{},
			"routes":  map[string]any{},
		},
	}
	got := mergeConfigMaps(cur, incoming)
	ob, ok := got["outbound"].(map[string]any)
	if !ok {
		t.Fatalf("outbound type=%T", got["outbound"])
	}
	if p, _ := ob["proxies"].([]any); len(p) != 0 {
		t.Errorf("proxies=%v want empty（整段替换）", p)
	}
	if r, _ := ob["routes"].(map[string]any); len(r) != 0 {
		t.Errorf("routes=%v want empty（不残留旧路由）", r)
	}
	if got["listen"] != ":7863" {
		t.Errorf("无关键被覆盖：listen=%v", got["listen"])
	}
}

// 其它嵌套对象仍保持「深合并」：只覆盖提交的键，未提交的键保留。
func TestMergeConfigMapsNestedStillMerges(t *testing.T) {	cur := map[string]any{"pool": map[string]any{"max_in_flight": float64(4), "keep": "yes"}}
	incoming := map[string]any{"pool": map[string]any{"max_in_flight": float64(8)}}
	got := mergeConfigMaps(cur, incoming)
	pool, ok := got["pool"].(map[string]any)
	if !ok {
		t.Fatalf("pool type=%T", got["pool"])
	}
	if pool["max_in_flight"] != float64(8) {
		t.Errorf("max_in_flight=%v want 8", pool["max_in_flight"])
	}
	if pool["keep"] != "yes" {
		t.Errorf("未提交的键应保留，keep=%v", pool["keep"])
	}
}

// realm_routing 段同样是「整段替换」：删掉 prefer 规则后保存必须真正清掉旧键
// （深合并会让被删的规则残留 → 用户以为删了却还在生效）。
func TestMergeConfigMapsRealmRoutingAtomic(t *testing.T) {
	cur := map[string]any{
		"realm_routing": map[string]any{
			"order":  []any{"cn", "global"},
			"prefer": map[string]any{"deepseek-*": "global"},
		},
	}
	incoming := map[string]any{
		"realm_routing": map[string]any{
			"order":  []any{"global", "cn"},
			"prefer": map[string]any{},
		},
	}
	got := mergeConfigMaps(cur, incoming)
	rr, ok := got["realm_routing"].(map[string]any)
	if !ok {
		t.Fatalf("realm_routing type=%T", got["realm_routing"])
	}
	if p, _ := rr["prefer"].(map[string]any); len(p) != 0 {
		t.Errorf("prefer=%v want empty（整段替换，不残留旧规则）", p)
	}
	if o, _ := rr["order"].([]any); len(o) != 2 || o[0] != "global" {
		t.Errorf("order=%v want [global cn]", rr["order"])
	}
}

// normalizeRealmRouting：order 去重 + 非法 fail fast + 空回落缺省；prefer 域校验。
func TestNormalizeRealmRouting(t *testing.T) {
	c := Default()
	c.RealmRouting.Order = []string{"global", "global"}
	if err := c.normalizeRealmRouting(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if len(c.RealmRouting.Order) != 1 || c.RealmRouting.Order[0] != "global" {
		t.Fatalf("order=%v want [global]", c.RealmRouting.Order)
	}

	c = Default()
	c.RealmRouting.Order = nil
	if err := c.normalizeRealmRouting(); err != nil {
		t.Fatalf("normalize empty: %v", err)
	}
	if len(c.RealmRouting.Order) != 2 || c.RealmRouting.Order[0] != "cn" || c.RealmRouting.Order[1] != "global" {
		t.Fatalf("empty order should fall back to [cn global], got %v", c.RealmRouting.Order)
	}

	// 非法域 fail fast（静默忽略会让用户误以为优先级生效了）。
	c = Default()
	c.RealmRouting.Order = []string{"bogus"}
	if err := c.normalizeRealmRouting(); err == nil {
		t.Fatal("illegal order realm should fail fast")
	}

	c = Default()
	c.RealmRouting.Prefer = map[string]string{"deepseek-*": "mars"}
	if err := c.normalizeRealmRouting(); err == nil {
		t.Fatal("prefer with illegal realm should fail fast")
	}

	c = Default()
	c.RealmRouting.Prefer = map[string]string{"  ": "cn"}
	if err := c.normalizeRealmRouting(); err == nil {
		t.Fatal("prefer with empty model name should fail fast")
	}
}
