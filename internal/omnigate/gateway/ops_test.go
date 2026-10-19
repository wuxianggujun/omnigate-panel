package gateway

import (
	"context"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// typedProvider is a Provider whose Type() is settable, so ProviderType resolves
// the same way it does for a real provider.
type typedProvider struct {
	name string
	typ  string
}

func (p *typedProvider) Name() string                                       { return p.name }
func (p *typedProvider) Type() string                                       { return p.typ }
func (p *typedProvider) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }
func (p *typedProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return nil, nil
}

// opsGateway builds a minimal gateway whose provider map is keyed by provider
// name (not provider.Name()), which is what Ops/ProviderType read.
func opsGateway(t *testing.T, name, typ string, accts ...*provider.Account) *Gateway {
	t.Helper()
	g := testGateway(&scriptProvider{})
	g.providers = map[string]provider.Provider{name: &typedProvider{name: name, typ: typ}}
	g.order = []string{name}
	g.accounts = map[string][]*provider.Account{name: accts}
	g.cfg = &config.Config{
		DefaultProvider: name,
		Providers:       []config.ProviderConfig{{Name: name, Type: typ}},
	}
	return g
}

// 声明了 checkin 的类型才被 Supports 接受；类型字符串不再出现在分发逻辑里。
func TestOpsSupportsMatchesDescriptor(t *testing.T) {
	catalog.Register()
	cases := map[string]bool{
		"raccoon":   true,
		"runable":   true,
		"trae":      true,
		"newapi":    true,
		"geminiweb": true,
		"openai":    false,
		"opencode":  false,
	}
	for typ, want := range cases {
		g := opsGateway(t, "p-"+typ, typ)
		if got := g.Ops("p-" + typ).Supports(plugin.ActionCheckin); got != want {
			t.Errorf("%s: Supports(checkin) = %v, want %v", typ, got, want)
		}
	}
}

// 未注册类型回退到 genericOps：不支持任何维护动作，账号列表走内存视图。
func TestOpsUnknownTypeFallsBackToGeneric(t *testing.T) {
	g := opsGateway(t, "mystery", "unregistered-type", &provider.Account{Label: "a", Cookie: "c"})
	ops := g.Ops("mystery")
	if ops.Supports(plugin.ActionCheckin) {
		t.Fatal("generic ops must not support checkin")
	}
	rows, err := ops.ListAccounts(context.Background(), "mystery", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0]["label"] != "a" || rows[0]["has_token"] != true {
		t.Fatalf("generic rows = %#v", rows)
	}
	if _, err := ops.HandleAction(context.Background(), "mystery", plugin.ActionRequest{Action: plugin.ActionCheckin}); err == nil {
		t.Fatal("generic ops HandleAction should error")
	}
}
