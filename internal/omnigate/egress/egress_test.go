package egress

import (
	"net/http"
	"strings"
	"testing"
)

// TestZeroProfileIsHonest 锁住默认行为：没有配置时如实申报，绝不悄悄伪装。
func TestZeroProfileIsHonest(t *testing.T) {
	r := Profile{}.Resolve()
	if !strings.HasPrefix(r.UserAgent, "omnigate/") {
		t.Fatalf("零值 Profile 应如实申报，得到 UA=%q", r.UserAgent)
	}
	if r.TLSProfile != "" {
		t.Fatalf("零值 Profile 不该启用 TLS 伪装，得到 %q", r.TLSProfile)
	}
}

// TestPresetDefaults 覆盖内置预设的关键字段。这些值本身就是伪装的可信度：
// 少一个 x-app / originator，上游就能看出不是官方客户端。
func TestPresetDefaults(t *testing.T) {
	cc := Profile{Preset: PresetClaudeCode}.Resolve()
	if !strings.HasPrefix(cc.UserAgent, "claude-cli/") {
		t.Errorf("claude-code UA=%q", cc.UserAgent)
	}
	if cc.Header("x-app") != "cli" {
		t.Errorf("claude-code 缺 x-app=cli，得到 %q", cc.Header("x-app"))
	}
	if cc.Header("anthropic-version") == "" {
		t.Error("claude-code 缺 anthropic-version")
	}
	if cc.TLSProfile == "" {
		t.Error("claude-code 应自带 TLS 指纹")
	}

	cx := Profile{Preset: PresetCodex}.Resolve()
	if !strings.HasPrefix(cx.UserAgent, "codex_cli_rs/") {
		t.Errorf("codex UA=%q", cx.UserAgent)
	}
	if cx.Header("originator") != "codex_cli_rs" {
		t.Errorf("codex 缺 originator，得到 %q", cx.Header("originator"))
	}

	br := Profile{Preset: PresetBrowser}.Resolve()
	if br.Header("Sec-Fetch-Mode") != "cors" {
		t.Errorf("browser 缺 Sec-Fetch-Mode，得到 %q", br.Header("Sec-Fetch-Mode"))
	}
}

// TestProviderDefaultPreset：newapi 的历史默认是浏览器，空 Profile 必须仍然
// 解析成浏览器而不是 omnigate——否则老部署的出站指纹会静默变化。
func TestProviderDefaultPreset(t *testing.T) {
	r := Profile{}.ResolveWith(PresetBrowser)
	if r.Preset != PresetBrowser {
		t.Fatalf("Preset=%q", r.Preset)
	}
	if r.UserAgent != ChromeUA {
		t.Fatalf("UA=%q", r.UserAgent)
	}
}

// TestExplicitOverridesWin 覆盖显式字段优先级：UA / 头 / TLS 都能单独改。
func TestExplicitOverridesWin(t *testing.T) {
	r := Profile{
		Preset:     PresetClaudeCode,
		UserAgent:  "claude-cli/9.9.9 (external, cli)",
		Headers:    map[string]string{"anthropic-beta": "custom-beta"},
		TLSProfile: "none",
	}.Resolve()
	if r.UserAgent != "claude-cli/9.9.9 (external, cli)" {
		t.Errorf("UA 覆盖失败：%q", r.UserAgent)
	}
	if r.Header("anthropic-beta") != "custom-beta" {
		t.Errorf("头覆盖失败：%q", r.Header("anthropic-beta"))
	}
	// 其它预设头必须保留：只替换同名头，不是替换整个头集。
	if r.Header("x-app") != "cli" {
		t.Errorf("未覆盖的头被丢了：x-app=%q", r.Header("x-app"))
	}
	if r.TLSProfile != "" {
		t.Errorf("TLSProfile=none 应显式关闭，得到 %q", r.TLSProfile)
	}
}

// TestHonestPresetName：面板把「如实申报」写成 "omnigate"（enum 不接受空值），
// 它必须与留空完全等价。
func TestHonestPresetName(t *testing.T) {
	if got := (Profile{Preset: PresetHonest}).Resolve(); !strings.HasPrefix(got.UserAgent, "omnigate/") {
		t.Fatalf("Preset=%q 应等价于默认，UA=%q", PresetHonest, got.UserAgent)
	}
	// 显式选「如实申报」还应能压过 provider 默认（newapi 默认浏览器）。
	if got := (Profile{Preset: PresetHonest}).ResolveWith(PresetBrowser); !strings.HasPrefix(got.UserAgent, "omnigate/") {
		t.Fatalf("显式如实申报应压过类型默认，UA=%q", got.UserAgent)
	}
}

// TestApplyWritesUAAndHeaders 验证 Apply 真的把身份写进请求，且不碰 net/http
// 自有的 Host / Content-Length。
func TestApplyWritesUAAndHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/messages", nil)
	Profile{Preset: PresetClaudeCode, Headers: map[string]string{
		"Host":           "evil.example",
		"Content-Length": "999",
	}}.Resolve().Apply(req)

	if !strings.HasPrefix(req.Header.Get("User-Agent"), "claude-cli/") {
		t.Errorf("UA 未写入：%q", req.Header.Get("User-Agent"))
	}
	if req.Header.Get("x-app") != "cli" {
		t.Errorf("x-app 未写入：%q", req.Header.Get("x-app"))
	}
	if req.Header.Get("Host") != "" || req.Header.Get("Content-Length") != "" {
		t.Error("Host / Content-Length 不该被 Apply 写入")
	}
}

// TestUnknownPresetFallsBack：手工改坏的配置不该产出一个半成品身份。
func TestUnknownPresetFallsBack(t *testing.T) {
	r := Profile{Preset: "not-a-preset"}.Resolve()
	if !strings.HasPrefix(r.UserAgent, "omnigate/") {
		t.Fatalf("未知预设应退回默认，UA=%q", r.UserAgent)
	}
}
