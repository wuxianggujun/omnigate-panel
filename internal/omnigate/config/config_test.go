package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// api_keys 必须同时接受裸串（历史写法）与对象（收窄写法），且序列化时未命名未收窄的
// 密钥写回裸串——否则升级一次就把既有配置整体改写成对象形态，手改体验变差。
func TestAPIKeysAcceptsStringAndObject(t *testing.T) {
	raw := `{
		"providers":[{"name":"raccoon","type":"raccoon"},{"name":"runable","type":"runable"}],
		"api_keys":["sk-full",{"key":"sk-rc","name":"浣熊专用","providers":["raccoon"]}]
	}`
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.APIKeys) != 2 {
		t.Fatalf("api_keys 条目数=%d want 2", len(c.APIKeys))
	}
	if k, ok := c.APIKeys.Find("sk-full"); !ok || k.Scoped() || k.Name != "" {
		t.Errorf("裸串应归一为「未命名 + 全量」，得到 %+v", k)
	}
	if k, ok := c.APIKeys.Find("sk-rc"); !ok || !k.Scoped() || !k.Allows("raccoon") || k.Allows("runable") {
		t.Errorf("收窄密钥授权判定错误: %+v", k)
	}
	if _, ok := c.APIKeys.Find(""); ok {
		t.Error("空 key 不该匹配任何条目（否则等于静默全量放行）")
	}

	// 全量密钥写回裸串，收窄密钥写成对象：两种形态各自保持最省事的写法。
	out, err := json.Marshal(&c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"sk-full"`) {
		t.Errorf("全量密钥应写回裸串: %s", out)
	}
	if !strings.Contains(string(out), `"providers":["raccoon"]`) {
		t.Errorf("收窄密钥应写成对象: %s", out)
	}

	var c2 Config
	if err := json.Unmarshal(out, &c2); err != nil {
		t.Fatal(err)
	}
	if len(c2.APIKeys) != 2 || !c2.APIKeys[1].Scoped() || c2.APIKeys[1].Name != "浣熊专用" {
		t.Fatalf("往返后不一致: %+v", c2.APIKeys)
	}
}

// PanelKey 是面板自己那把密钥，恒全量且**绝不能落盘**——写进文件后轮换 api_key 会让
// 旧钥匙残留在 omnigate.json 里继续可用。
func TestPanelKeyNeverSerialized(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "raccoon", Type: "raccoon"}},
		PanelKey:  "sk-panel-secret",
	}
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "sk-panel-secret") || strings.Contains(string(out), "panel_key") {
		t.Fatalf("PanelKey 泄漏进配置: %s", out)
	}
	var c2 Config
	if err := json.Unmarshal(out, &c2); err != nil {
		t.Fatal(err)
	}
	if c2.PanelKey != "" {
		t.Errorf("PanelKey 不该能从文件读回，得到 %q", c2.PanelKey)
	}
}

// 收窄到不存在的 provider 必须 fail fast：写错名字等于这把钥匙什么都调不了，与其让
// 客户端拿到 403 摸不着头脑，不如在加载/保存时就拒绝。
func TestAPIKeysRejectUnknownProvider(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "raccoon", Type: "raccoon"}},
		APIKeys:   APIKeyList{{Key: "sk-x", Providers: []string{"typo"}}},
	}
	if err := c.Normalize(); err == nil {
		t.Fatal("want error for unknown provider in api_keys")
	}
}

// 空 key 条目必须被拒绝：空串与「未携带密钥」撞车，等于静默开一个全量放行的洞。
func TestAPIKeysRejectEmptyKey(t *testing.T) {
	c := &Config{
		Providers: []ProviderConfig{{Name: "raccoon", Type: "raccoon"}},
		APIKeys:   APIKeyList{{Key: ""}},
	}
	if err := c.Normalize(); err == nil {
		t.Fatal("want error for empty api key")
	}
}
