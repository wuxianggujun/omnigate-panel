package config

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/trae"
)

// 内置类型的连接默认值来自插件描述符，且不依赖 catalog.Register：配置加载可能
// 发生在注册表初始化之前（测试、嵌入式调用），结果必须一致。
func TestApplyDefaultsFromCatalog(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{
		{Name: "rb", Type: "runable"},
		{Name: "oa", Type: "openai"},
		{Name: "rc", Type: "raccoon"},
		{Name: "tr", Type: "trae"},
		{Name: "na", Type: "newapi"},
		{Name: "gw", Type: "geminiweb"},
	}}
	c.applyDefaults()

	if got := c.Providers[0].BaseURL; got != runable.DefaultBaseURL {
		t.Errorf("runable base_url = %q, want %q", got, runable.DefaultBaseURL)
	}
	if got := c.Providers[0].DeviceHeader; got != runable.DefaultDeviceHeader {
		t.Errorf("runable device_header = %q, want %q", got, runable.DefaultDeviceHeader)
	}
	if got := c.Providers[1].BaseURL; got != "https://api.openai.com/v1" {
		t.Errorf("openai base_url = %q", got)
	}
	if got := c.Providers[2].MainOrigin; got != raccoon.DefaultMainOrigin {
		t.Errorf("raccoon main_origin = %q, want %q", got, raccoon.DefaultMainOrigin)
	}
	if got := c.Providers[3].AgentHost; got != trae.DefaultAgentHost {
		t.Errorf("trae agent_host = %q, want %q", got, trae.DefaultAgentHost)
	}
	// 显式值永远优先于 schema 默认值。
	if got := c.Providers[4].BaseURL; got != "" {
		t.Errorf("newapi 没有通用默认值，不应被填成 %q", got)
	}
	if got := c.Providers[5].BaseURL; got != "" {
		t.Errorf("geminiweb 没有 base_url，不应被填成 %q", got)
	}
}

func TestApplyFieldDefaultsKeepsExplicitValues(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{
		Name: "rb", Type: "runable",
		BaseURL:      "https://proxy.example/v1",
		DeviceHeader: "custom-header",
	}}}
	c.applyDefaults()
	if c.Providers[0].BaseURL != "https://proxy.example/v1" {
		t.Errorf("显式 base_url 被覆盖: %q", c.Providers[0].BaseURL)
	}
	if c.Providers[0].DeviceHeader != "custom-header" {
		t.Errorf("显式 device_header 被覆盖: %q", c.Providers[0].DeviceHeader)
	}
}
