package config

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
)

// 注册一个新插件类型后，配置校验必须直接接受它，无需改 config 包的类型白名单。
func TestValidateAcceptsRegisteredPluginType(t *testing.T) {
	const typ = "test-custom-provider"
	if _, err := plugin.Get(typ); err == nil {
		t.Fatalf("测试类型 %q 已被注册", typ)
	}
	if err := plugin.Register(plugin.Plugin{
		Type:        typ,
		DisplayName: "Custom",
		AccountMode: plugin.AccountModeNone,
		Traits:      plugin.Traits{WireFormat: plugin.WireOpenAI},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := &Config{Providers: []ProviderConfig{{
		Name: "custom", Type: typ, BaseURL: "https://example.com/v1",
	}}}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		t.Fatalf("已注册插件类型应通过校验: %v", err)
	}
}

func TestValidateRejectsUnknownType(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{
		Name: "nope", Type: "definitely-not-a-plugin", BaseURL: "https://example.com/v1",
	}}}
	c.applyDefaults()
	if err := c.validate(); err == nil {
		t.Fatal("未知类型应被拒绝")
	}
}
