package panel

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMaskSecret 掩码形态：前 6 字符 + 长度；空串与已掩码值幂等。
func TestMaskSecret(t *testing.T) {
	if got := maskSecret(""); got != "" {
		t.Fatalf("空串应原样返回，得到 %q", got)
	}
	// 短凭证只露前一半：否则「5 字符的密码」会被整段显示，等于没脱敏。
	if got := maskSecret("short"); got != "sh…(len=5)" {
		t.Fatalf("短凭证应只露前一半，得到 %q", got)
	}
	got := maskSecret("sk-6d0688c114022101529441a57ef50bd3")
	if !strings.HasPrefix(got, "sk-6d0") || !strings.HasSuffix(got, "(len=35)") {
		t.Fatalf("掩码形态不对：%q", got)
	}
	if !isMasked(got) {
		t.Fatalf("isMasked 未识别 %q", got)
	}
	if maskSecret(got) != got {
		t.Fatalf("掩码应幂等：%q -> %q", got, maskSecret(got))
	}
}

// TestMaskRestoreRoundTrip 掩码后不得残留明文，还原后必须与原配置逐字节一致。
func TestMaskRestoreRoundTrip(t *testing.T) {
	cfg := map[string]any{
		"api_keys": []any{"sk-gateway-key-must-stay"},
		"providers": []any{
			map[string]any{
				"name":    "raccoon-main",
				"type":    "raccoon",
				"api_key": "upstream-key-1234567890",
				"accounts": []any{
					map[string]any{
						"label":         "浣熊账号1",
						"email":         "a@b.c",
						"cookie":        "abc1234567890",
						"refresh_token": "rt-abcdefgh",
						"password":      "p@ssw0rd",
					},
				},
			},
			map[string]any{
				"name": "runable-main",
				"accounts": []any{
					map[string]any{"label": "runable-1", "cookie": "__Secure-better-auth.session_token=xyz789"},
				},
			},
		},
		// 签到任务里的模板占位不是凭证：必须原样保留（脱敏会让运维看不懂）。
		"checkin": map[string]any{
			"tasks": []any{
				map[string]any{
					"name":    "t1",
					"headers": map[string]any{"Authorization": "Bearer {{access_token}}"},
					"body":    `{"cookie":"{{cookie}}"}`,
				},
			},
		},
		// 代理 URL 的 userinfo 单独脱敏，host/port 保留。
		"outbound": map[string]any{
			"proxies": []any{
				map[string]any{"name": "p1", "url": "http://user:pass@127.0.0.1:7890"},
				map[string]any{"name": "p2", "url": "http://127.0.0.1:7891"},
			},
		},
	}

	masked := maskSecrets(cfg)
	raw, err := json.Marshal(masked)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"upstream-key-1234567890", "abc1234567890", "rt-abcdefgh", "p@ssw0rd",
		"__Secure-better-auth.session_token=xyz789", "user:pass",
	} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("掩码后仍含明文 %q：%s", secret, raw)
		}
	}
	for _, keep := range []string{
		"sk-gateway-key-must-stay", // 网关自己的 api_keys 不脱敏
		"Bearer {{access_token}}",  // 模板占位原样
		"{{cookie}}",               // body 里的模板占位原样
		"http://127.0.0.1:7891",    // 无 userinfo 的 URL 原样
		maskURLUserinfo("http://user:pass@127.0.0.1:7890"), // 有 userinfo：只掩 userinfo
	} {
		if !strings.Contains(string(raw), keep) {
			t.Fatalf("掩码结果应保留 %q：%s", keep, raw)
		}
	}

	restored := restoreSecrets(masked, cfg)
	rb, _ := json.Marshal(restored)
	ob, _ := json.Marshal(cfg)
	if string(rb) != string(ob) {
		t.Fatalf("还原不一致：\n got %s\nwant %s", rb, ob)
	}
}

// TestRestoreSecretsKeepsUserEdits 用户改了掩码外的部分（host / 新增字段）时不得被旧值覆盖。
func TestRestoreSecretsKeepsUserEdits(t *testing.T) {
	old := map[string]any{
		"proxies": []any{
			map[string]any{"name": "p1", "url": "http://user:pass@old.example:7890"},
		},
	}
	// 前端回传：userinfo 仍是掩码，但 host 被改成 new.example；并新增一个无掩码的代理。
	incoming := map[string]any{
		"proxies": []any{
			map[string]any{"name": "p1", "url": "http://" + maskSecret("user:pass") + "@new.example:7890"},
			map[string]any{"name": "p2", "url": "http://plain:secret@h2:1080"},
		},
	}
	out := restoreSecrets(incoming, old)
	proxies := out.(map[string]any)["proxies"].([]any)
	if got := proxies[0].(map[string]any)["url"]; got != "http://user:pass@new.example:7890" {
		t.Fatalf("userinfo 应还原、host 改动应保留，得到 %v", got)
	}
	// 新代理没有旧值可还原：掩码/明文原样保留（明文是用户刚输入的，不该被抹掉）。
	if got := proxies[1].(map[string]any)["url"]; got != "http://plain:secret@h2:1080" {
		t.Fatalf("新增代理应原样保留，得到 %v", got)
	}
}

// TestRestoreSecretsIdentityMatching 删掉数组首元素后，剩余元素不得错位套用旧密钥。
func TestRestoreSecretsIdentityMatching(t *testing.T) {
	old := map[string]any{
		"providers": []any{
			map[string]any{"name": "a", "api_key": "KEY-A"},
			map[string]any{"name": "b", "api_key": "KEY-B"},
		},
	}
	incoming := map[string]any{
		"providers": []any{
			map[string]any{"name": "b", "api_key": "KEY…(len=5)"},
		},
	}
	out := restoreSecrets(incoming, old)
	provs := out.(map[string]any)["providers"].([]any)
	if got := provs[0].(map[string]any)["api_key"]; got != "KEY-B" {
		t.Fatalf("应按 name 匹配到 b 的密钥，得到 %v", got)
	}
}

// TestRestoreSecretsNoOldValue 没有旧值可还原时保留掩码（交由上层校验报错），绝不猜测。
func TestRestoreSecretsNoOldValue(t *testing.T) {
	incoming := map[string]any{"cookie": "abc…(len=3)"}
	out := restoreSecrets(incoming, map[string]any{})
	if got := out.(map[string]any)["cookie"]; got != "abc…(len=3)" {
		t.Fatalf("无旧值应保留掩码，得到 %v", got)
	}
}

// TestMaskSecretsSkipTop 顶层跳过键（网关 api_key）不被脱敏，嵌套同名键仍脱敏。
func TestMaskSecretsSkipTop(t *testing.T) {
	cfg := map[string]any{
		"api_key": "gateway-key-1234567890",
		"upstream": map[string]any{
			"api_key": "upstream-key-0987654321",
		},
	}
	out := maskSecrets(cfg, "api_key").(map[string]any)
	if out["api_key"] != "gateway-key-1234567890" {
		t.Fatalf("顶层 api_key 不应脱敏，得到 %v", out["api_key"])
	}
	if got := out["upstream"].(map[string]any)["api_key"]; got == "upstream-key-0987654321" {
		t.Fatalf("嵌套 api_key 应脱敏，得到 %v", got)
	}
}
