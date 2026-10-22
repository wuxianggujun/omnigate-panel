package config

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/egress"
)

// 出站身份与协议方言的规范化：未知值一律降级，绝不因为一行手写配置让供应商
// 变成不可路由；同时保证老配置（不带这些字段）行为完全不变。
func TestNormalizeClientAndProtocol(t *testing.T) {
	raw := `{
	  "providers": [
	    {"name": "legacy", "type": "openai", "base_url": "https://x/v1"},
	    {"name": "claude", "type": "newapi",
	     "client_preset": "  Claude-Code ",
	     "client_user_agent": "  claude-cli/9.9.9 (external, cli) ",
	     "client_headers": {"  anthropic-beta ": " x ", "": "y"},
	     "client_tls_profile": " Chrome120 ",
	     "protocol": "Anthropic",
	     "auth_mode": " COOKIE "},
	    {"name": "broken", "type": "openai",
	     "client_preset": "not-a-preset", "protocol": "garbage"}
	  ]
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}

	legacy := c.Provider("legacy")
	if legacy.ClientPreset != "" || legacy.Protocol != "openai" {
		t.Fatalf("老配置不该被赋预设/方言: %+v", legacy)
	}
	if prof := legacy.ClientProfile(); prof.Preset != "" || prof.UserAgent != "" {
		t.Fatalf("零值 ClientProfile 应保持空: %+v", prof)
	}

	claude := c.Provider("claude")
	if claude.ClientPreset != "claude-code" {
		t.Errorf("client_preset=%q", claude.ClientPreset)
	}
	if claude.Protocol != "anthropic" {
		t.Errorf("protocol=%q", claude.Protocol)
	}
	if claude.AuthMode != "cookie" {
		t.Errorf("auth_mode=%q", claude.AuthMode)
	}
	if claude.ClientUserAgent != "claude-cli/9.9.9 (external, cli)" {
		t.Errorf("client_user_agent=%q", claude.ClientUserAgent)
	}
	if claude.ClientTLSProfile != "chrome120" {
		t.Errorf("client_tls_profile=%q", claude.ClientTLSProfile)
	}
	if got := claude.ClientHeaders["Anthropic-Beta"]; got != "x" {
		t.Errorf("请求头应规范化并去空白: %#v", claude.ClientHeaders)
	}
	prof := claude.ClientProfile()
	if prof.Preset != egress.PresetClaudeCode || prof.UserAgent == "" {
		t.Errorf("ClientProfile 应带上预设与覆盖的 UA: %+v", prof)
	}

	// 未知预设 → Resolve 退回如实申报；未知方言 → openai。构建仍成功。
	broken := c.Provider("broken")
	if broken.Protocol != "openai" {
		t.Errorf("未知方言应降到 openai，得到 %q", broken.Protocol)
	}
	if got := broken.ClientProfile().Resolve().UserAgent; !strings.HasPrefix(got, "omnigate/") {
		t.Errorf("未知预设应退回如实申报，得到 %q", got)
	}
}

// New API 的用户接口表面：只去空白、不改大小写路径，空值保持空（默认值由适配器
// 在 Endpoints.Normalized 里补），这样「留空 = 走历史默认」不依赖配置文件内容。
func TestNormalizeUserAPIEndpoints(t *testing.T) {
	raw := `{"name":"ar","type":"newapi","base_url":"https://anyrouter.top",
	  "checkin_path": " /api/user/sign_in ", "user_header": " New-Api-User ",
	  "auth_mode": "Cookie", "signin_path": "/login"}`
	var p ProviderConfig
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	var c Config
	c.Providers = []ProviderConfig{p}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}
	got := c.Provider("ar")
	if got.CheckinPath != "/api/user/sign_in" || got.UserHeader != "New-Api-User" || got.SigninPath != "/login" {
		t.Fatalf("路径/头未规范化: %+v", got)
	}
	if got.AuthMode != "cookie" {
		t.Errorf("auth_mode=%q", got.AuthMode)
	}
	if got.SelfPath != "" || got.RefreshPath != "" {
		t.Errorf("未填的默认项应保持空，交给适配器补默认: %+v", got)
	}
}
