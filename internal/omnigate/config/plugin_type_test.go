package config

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
)

// 注册一个新插件类型后，配置校验必须直接接受它，无需改 config 包的类型白名单；
// 它在描述符里声明的字段默认值也必须被 applyDefaults 采用。
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
		ConfigFields: []plugin.Field{{
			Key: "base_url", Label: "Base URL", Kind: plugin.KindURL,
			Default: "https://default.example/v1",
		}},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	c := &Config{Providers: []ProviderConfig{{Name: "custom", Type: typ}}}
	c.applyDefaults()
	if got := c.Providers[0].BaseURL; got != "https://default.example/v1" {
		t.Fatalf("插件 schema 的 base_url 默认值未生效，得到 %q", got)
	}
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
