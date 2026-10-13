package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// 供应商级密钥池 / 系统提示词的解析与规范化：去空白、去重；空池等于没配。
func TestProviderKeyPoolNormalize(t *testing.T) {
	raw := `{
	  "providers": [
	    {"name": "a", "type": "openai", "api_key": "sk-single"},
	    {"name": "b", "type": "openai",
	     "api_keys": [" sk-x ", "", "sk-x", "sk-y"], "system_prompt": "  你是助手  "}
	  ]
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Normalize(); err != nil {
		t.Fatal(err)
	}

	if got := c.Provider("a").ProviderKeys(); len(got) != 1 || got[0] != "sk-single" {
		t.Fatalf("没配池子应回落单个 api_key: %v", got)
	}
	b := c.Provider("b")
	got := b.ProviderKeys()
	if len(got) != 2 || got[0] != "sk-x" || got[1] != "sk-y" {
		t.Fatalf("密钥池应去空白去重: %v", got)
	}
	if b.SystemPrompt != "你是助手" {
		t.Fatalf("system_prompt 应去首尾空白: %q", b.SystemPrompt)
	}
}

// ProviderKeys 的优先级：池子 > 单个 api_key > nil。
func TestProviderKeysPrecedence(t *testing.T) {
	var d ProviderConfig
	if err := json.Unmarshal([]byte(`{"name":"d","api_key":"sk-one","api_keys":["sk-pool"]}`), &d); err != nil {
		t.Fatal(err)
	}
	if k := d.ProviderKeys(); len(k) != 1 || k[0] != "sk-pool" {
		t.Fatalf("池子应优先于单个 api_key: %v", k)
	}
	if k := (&ProviderConfig{}).ProviderKeys(); k != nil {
		t.Fatalf("都没配应返回 nil: %v", k)
	}
}

// 新字段必须加法式：老 omnigate.json 反序列化后不受影响，序列化时 omitempty 不冒出空值。
func TestProviderNewFieldsAreAdditive(t *testing.T) {
	var p ProviderConfig
	if err := json.Unmarshal([]byte(`{"name":"o","type":"openai","base_url":"https://x/v1"}`), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.APIKeys) != 0 || p.SystemPrompt != "" {
		t.Fatalf("老配置不该带出新字段: %+v", p)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"api_keys"`, `"system_prompt"`} {
		if strings.Contains(string(raw), k) {
			t.Fatalf("空值不该序列化出 %s：%s", k, raw)
		}
	}
}
