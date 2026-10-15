package panel

import "testing"

// TestMaskProviderKeyPool 供应商级密钥池（provider.api_keys）必须逐个脱敏：它和顶层的
// 网关 api_keys 同名，但顶层那批是网关自己的密钥（刻意不脱敏，见 TestMaskRestoreRoundTrip）。
func TestMaskProviderKeyPool(t *testing.T) {
	cfg := map[string]any{
		"api_keys": []any{"gw-key-must-stay"},
		"providers": []any{
			map[string]any{
				"name":     "o",
				"type":     "openai",
				"api_key":  "sk-single-1234567890",
				"api_keys": []any{"sk-pool-a-1234567890", "", "sk-pool-b-1234567890"},
			},
		},
	}

	out := maskSecrets(cfg).(map[string]any)

	// 顶层网关密钥不动。
	if gw := out["api_keys"].([]any); gw[0] != "gw-key-must-stay" {
		t.Fatalf("顶层 api_keys 不该脱敏，得到 %v", gw[0])
	}

	pool := out["providers"].([]any)[0].(map[string]any)["api_keys"].([]any)
	if len(pool) != 3 {
		t.Fatalf("长度应保持，得到 %v", pool)
	}
	if s, _ := pool[0].(string); s == "sk-pool-a-1234567890" || !isMasked(s) {
		t.Fatalf("密钥池[0] 应脱敏，得到 %v", pool[0])
	}
	if pool[1] != "" {
		t.Fatalf("空元素应原样，得到 %v", pool[1])
	}
	if s, _ := pool[2].(string); s == "sk-pool-b-1234567890" || !isMasked(s) {
		t.Fatalf("密钥池[2] 应脱敏，得到 %v", pool[2])
	}

	// 保存路径：未改动的掩码必须按位还原成真 key（前端整份回传也不会把密钥写成掩码串）。
	back := restoreSecrets(out, cfg)
	bpool := back.(map[string]any)["providers"].([]any)[0].(map[string]any)["api_keys"].([]any)
	if bpool[0] != "sk-pool-a-1234567890" || bpool[2] != "sk-pool-b-1234567890" {
		t.Fatalf("密钥池还原不一致：%v", bpool)
	}
}

// TestMaskOpencodeTierPools opencode 供应商的 zen_keys / go_keys 也必须逐个脱敏。
//
// 这两个池名不在 secretKey 的单数白名单里（它只认 api_key），元素又是裸串而不是对象，
// 所以非常容易漏——漏掉就等于把 OpenCode 的付费密钥明文回显给只读访问者。
func TestMaskOpencodeTierPools(t *testing.T) {
	cfg := map[string]any{
		"providers": []any{
			map[string]any{
				"name":     "opencode",
				"type":     "opencode",
				"zen_keys": []any{"zen-key-aaa-1234567890", ""},
				"go_keys":  []any{"go-key-bbb-1234567890", "go-key-ccc-1234567890"},
			},
		},
	}

	out := maskSecrets(cfg).(map[string]any)
	prov := out["providers"].([]any)[0].(map[string]any)

	for _, f := range []string{"zen_keys", "go_keys"} {
		arr := prov[f].([]any)
		for i, e := range arr {
			s, _ := e.(string)
			if s == "" {
				continue
			}
			if !isMasked(s) {
				t.Fatalf("%s[%d] 未脱敏：%v", f, i, e)
			}
			if s == "zen-key-aaa-1234567890" || s == "go-key-bbb-1234567890" || s == "go-key-ccc-1234567890" {
				t.Fatalf("%s[%d] 仍是明文：%v", f, i, e)
			}
		}
	}

	// 保存路径：两个池都要按位还原（否则面板保存一次就把密钥写成掩码串）。
	back := restoreSecrets(out, cfg).(map[string]any)["providers"].([]any)[0].(map[string]any)
	if z := back["zen_keys"].([]any); z[0] != "zen-key-aaa-1234567890" {
		t.Fatalf("zen_keys 还原失败：%v", z)
	}
	if g := back["go_keys"].([]any); g[0] != "go-key-bbb-1234567890" || g[1] != "go-key-ccc-1234567890" {
		t.Fatalf("go_keys 还原失败：%v", g)
	}
}
