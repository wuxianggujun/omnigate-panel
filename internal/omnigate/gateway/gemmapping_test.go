package gateway

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// A Gem turn is account-scoped: openStream must serve the pinned account without
// disturbing the provider's global round-robin index.
func TestOpenStreamPinsPreferredAccount(t *testing.T) {
	g := testGateway(&scriptProvider{})
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}, {Label: "b", Cookie: "c"}}
	st.SetActive("fake", 0)

	meta := &ReqMeta{}
	ctx := WithReqMeta(context.Background(), meta)
	s, err := g.openStream(ctx, "fake", g.providers["fake"], provider.ChatInput{PreferredAccount: "b"})
	if err != nil {
		t.Fatalf("openStream: %v", err)
	}
	_ = s.Close()
	if meta.Account != "b" {
		t.Fatalf("served by %q, want b", meta.Account)
	}
	if got := st.ActiveIndex("fake"); got != 0 {
		t.Fatalf("pinned turn moved active index to %d, want 0", got)
	}
}

func TestOpenStreamUnknownPreferredAccount(t *testing.T) {
	g := testGateway(&scriptProvider{})
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}}

	if _, err := g.openStream(context.Background(), "fake", g.providers["fake"],
		provider.ChatInput{PreferredAccount: "zzz"}); err == nil {
		t.Fatal("expected error for an unknown pinned account")
	}
}

// The provider-scoped Gem mapping admin surface round-trips through the gateway.
func TestGemMappingAdminRoundTrip(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.cfg.Providers = []config.ProviderConfig{{Name: "gw", Type: "geminiweb"}}
	gp := geminiweb.NewProvider("gw", nil)
	gp.SetGemMappingStore(geminiweb.NewGemMappingStore(filepath.Join(t.TempDir(), "gem-mapping.json")))
	g.providers["gw"] = gp
	g.order = []string{"gw"}

	if err := g.GeminiWebSetGemMapping("gw", "code-reviewer", "g1", "", "账号 1"); err != nil {
		t.Fatalf("set: %v", err)
	}
	ms, err := g.GeminiWebListGemMappings("gw")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ms) != 1 || ms[0].Model != "code-reviewer" || ms[0].GemID != "g1" || ms[0].BaseModel != "gemini-pro" {
		t.Fatalf("unexpected mappings: %+v", ms)
	}
	if ok, err := g.GeminiWebDeleteGemMapping("gw", "code-reviewer"); err != nil || !ok {
		t.Fatalf("delete: ok=%v err=%v", ok, err)
	}
	if ms, _ := g.GeminiWebListGemMappings("gw"); len(ms) != 0 {
		t.Fatalf("mapping survived delete: %+v", ms)
	}
}

// ListModels must surface a mapped Gem alias (issue #2) so a client can select it.
func TestListModelsIncludesGemAlias(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.cfg.Providers = []config.ProviderConfig{{Name: "gw", Type: "geminiweb"}}
	gp := geminiweb.NewProvider("gw", []string{"gemini-pro"})
	gp.SetGemMappingStore(geminiweb.NewGemMappingStore(""))
	if !gp.SetGemMapping("code-reviewer", geminiweb.GemMapping{GemID: "g1"}) {
		t.Fatal("SetGemMapping failed")
	}
	g.providers["gw"] = gp
	g.order = []string{"gw"}

	models := g.ListModels(context.Background())
	found := false
	for _, m := range models {
		if m.ID == "gw/code-reviewer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gem alias not listed: %+v", models)
	}
}
