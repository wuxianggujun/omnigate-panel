package config

import (
	"strings"
	"testing"
)

// opencode 供应商的两个 tier 密钥池：清理规则与通用 api_keys 一致（去空白、去重、
// 全空归一为 nil），tier_prefer 只接受 go/zen，大小写不敏感。
func TestNormalizeOpencodeTiers(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{
		Name:       "opencode",
		Type:       "opencode",
		ZenKeys:    []string{"  zen-a  ", "", "zen-a", "zen-b"},
		GoKeys:     []string{"go-a"},
		TierPrefer: "  ZEN ",
		GoBaseURL:  "https://opencode.ai/zen/go/",
	}}}
	if err := c.Normalize(); err != nil {
		t.Fatalf("合法配置不该被拒绝: %v", err)
	}
	p := c.Providers[0]

	if got := strings.Join(p.ZenKeys, ","); got != "zen-a,zen-b" {
		t.Fatalf("zen_keys 应去空白去重，得到 %q", got)
	}
	if got := strings.Join(p.GoKeys, ","); got != "go-a" {
		t.Fatalf("go_keys 应去空白去重，得到 %q", got)
	}
	if p.TierPrefer != "zen" {
		t.Fatalf("tier_prefer 应归一小写并去空白，得到 %q", p.TierPrefer)
	}
	if p.GoBaseURL != "https://opencode.ai/zen/go" {
		t.Fatalf("go_base_url 应去掉尾斜杠，得到 %q", p.GoBaseURL)
	}

	// 全空池必须归一为 nil（而不是空切片）：omitempty 与「有没有配密钥」的判断
	// 都依赖它，留一个 len=0 的切片会让面板把空壳当成配过。
	b := &Config{Providers: []ProviderConfig{{
		Name: "oc", Type: "opencode",
		ZenKeys: []string{"", "   "},
		GoKeys:  []string{},
	}}}
	if err := b.Normalize(); err != nil {
		t.Fatalf("空池不该被拒绝: %v", err)
	}
	if b.Providers[0].ZenKeys != nil {
		t.Fatalf("全空 zen_keys 应归一为 nil，得到 %#v", b.Providers[0].ZenKeys)
	}
	if b.Providers[0].GoKeys != nil {
		t.Fatalf("空 go_keys 应归一为 nil，得到 %#v", b.Providers[0].GoKeys)
	}
	if b.Providers[0].TierPrefer != "" {
		t.Fatalf("留空的 tier_prefer 应保持空（等于默认 go），得到 %q", b.Providers[0].TierPrefer)
	}
}

// tier_prefer 写错只会静默退回默认顺序，在面板上很难察觉，所以加载/保存时就拒绝。
func TestValidateOpencodeTierPrefer(t *testing.T) {
	base := func(prefer string) *Config {
		return &Config{Providers: []ProviderConfig{{
			Name: "oc", Type: "opencode", GoKeys: []string{"go-a"}, TierPrefer: prefer,
		}}}
	}
	for _, ok := range []string{"", "go", "zen"} {
		if err := base(ok).Normalize(); err != nil {
			t.Errorf("tier_prefer=%q 应被接受，得到 %v", ok, err)
		}
	}
	err := base("go-plus").Normalize()
	if err == nil {
		t.Fatal("非法 tier_prefer 应被拒绝")
	}
	if !strings.Contains(err.Error(), "tier_prefer") || !strings.Contains(err.Error(), "go-plus") {
		t.Fatalf("报错应点名字段与取值，得到 %v", err)
	}
}

// 同一把密钥同时出现在两个池里一定有一个填错了（两个 tier 的密钥互不通用，
// 错填只会拿到 Invalid credential，而且两边的错误串一模一样，事后极难排查）。
// 同时这条报错绝不能把密钥原样写进错误里——错误会进日志与面板提示。
func TestValidateOpencodeCrossPoolDuplicate(t *testing.T) {
	const shared = "sk-duplicated-key-1234567890"
	c := &Config{Providers: []ProviderConfig{{
		Name:    "oc",
		Type:    "opencode",
		ZenKeys: []string{"sk-zen-only", shared},
		GoKeys:  []string{shared, "sk-go-only"},
	}}}
	err := c.Normalize()
	if err == nil {
		t.Fatal("同一把密钥出现在两个池里应被拒绝")
	}
	msg := err.Error()
	if !strings.Contains(msg, "oc") {
		t.Fatalf("报错应点名供应商，得到 %v", msg)
	}
	if strings.Contains(msg, shared) {
		t.Fatalf("报错不得回显完整密钥，得到 %v", msg)
	}
	if !strings.Contains(msg, shared[:6]) {
		t.Fatalf("报错应给出可辨认的前缀（便于定位是哪一把），得到 %v", msg)
	}

	// 不同密钥的两个池是正常的：不该误报。
	ok := &Config{Providers: []ProviderConfig{{
		Name: "oc", Type: "opencode",
		ZenKeys: []string{"sk-zen-only"}, GoKeys: []string{"sk-go-only"},
	}}}
	if err := ok.Normalize(); err != nil {
		t.Fatalf("不同密钥的两个池不该被拒绝: %v", err)
	}
	// api_keys 与 zen_keys 恰好相同不算「两个 tier 混填」——api_keys 对 opencode
	// 不生效，但也没理由因此拒绝。
	other := &Config{Providers: []ProviderConfig{{
		Name: "oc", Type: "opencode",
		APIKeys: []string{shared}, ZenKeys: []string{shared},
	}}}
	if err := other.Normalize(); err != nil {
		t.Fatalf("api_keys 与 zen_keys 相同不该被拒绝: %v", err)
	}
}

// opencode 是合法类型（漏在 validate 的枚举里会让整份配置存不下去）。
func TestOpencodeTypeIsAccepted(t *testing.T) {
	c := &Config{Providers: []ProviderConfig{{Name: "oc", Type: "opencode"}}}
	if err := c.Normalize(); err != nil {
		t.Fatalf("opencode 应是合法类型: %v", err)
	}
}
