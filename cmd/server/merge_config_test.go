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
func TestMergeConfigMapsNestedStillMerges(t *testing.T) {
	cur := map[string]any{"pool": map[string]any{"max_in_flight": float64(4), "keep": "yes"}}
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
