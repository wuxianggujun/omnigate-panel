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
