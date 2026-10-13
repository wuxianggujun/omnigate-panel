package gateway

import (
	"context"
	"path/filepath"
	"testing"
)

func TestModelMappingStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-mapping.json")
	s := NewModelMappingStore(path)

	if got := s.Resolve("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("unmapped Resolve = %q, want identity", got)
	}
	if err := s.Set("gpt-4o", "geminiweb/gemini-pro"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("", "x"); err == nil {
		t.Fatal("empty alias must be rejected")
	}
	if err := s.Set("a", "a"); err == nil {
		t.Fatal("alias == target must be rejected")
	}
	if got := s.Resolve("gpt-4o"); got != "geminiweb/gemini-pro" {
		t.Fatalf("Resolve = %q", got)
	}
	// Persistence.
	s2 := NewModelMappingStore(path)
	if got := s2.Resolve("gpt-4o"); got != "geminiweb/gemini-pro" {
		t.Fatalf("persisted Resolve = %q", got)
	}
	if !s2.Delete("gpt-4o") {
		t.Fatal("Delete returned false")
	}
	if s2.Delete("gpt-4o") {
		t.Fatal("second Delete should be false")
	}
	if got := NewModelMappingStore(path).Resolve("gpt-4o"); got != "gpt-4o" {
		t.Fatalf("deletion did not persist: %q", got)
	}
}

func TestRouteAppliesModelMapping(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.Set("gpt-4o", "fake/m"); err != nil {
		t.Fatal(err)
	}
	pn, m := g.route("gpt-4o")
	if pn != "fake" || m != "m" {
		t.Fatalf("route(gpt-4o) = (%q,%q), want (fake,m)", pn, m)
	}
	if got := g.ProviderOf("gpt-4o"); got != "fake" {
		t.Fatalf("ProviderOf = %q", got)
	}
	// A bare target falls back to the default provider.
	if err := g.modelMap.Set("quick", "m"); err != nil {
		t.Fatal(err)
	}
	if pn, m := g.route("quick"); pn != "fake" || m != "m" {
		t.Fatalf("route(quick) = (%q,%q)", pn, m)
	}
}

func TestListModelsExposesAliasInScope(t *testing.T) {
	g := testGateway(&scriptProvider{})
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
	if err := g.modelMap.Set("gpt-4o", "fake/m"); err != nil {
		t.Fatal(err)
	}

	all := map[string]bool{}
	for _, m := range g.ListModelsFor(context.Background(), nil) {
		all[m.ID] = true
	}
	if !all["gpt-4o"] {
		t.Fatalf("alias not listed: %+v", all)
	}
	// Out of scope for a key restricted to another provider → not listed.
	for _, m := range g.ListModelsFor(context.Background(), []string{"other"}) {
		if m.ID == "gpt-4o" {
			t.Fatalf("alias leaked to an out-of-scope key")
		}
	}
}
