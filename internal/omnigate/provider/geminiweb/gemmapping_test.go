package geminiweb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGemMappingStoreSetResolveDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gem-mapping.json")
	s := NewGemMappingStore(path)

	if _, ok := s.Resolve("code-reviewer"); ok {
		t.Fatal("empty store should not resolve anything")
	}
	if !s.Set("code-reviewer", GemMapping{GemID: "g1", BaseModel: "gemini-pro", AccountID: "账号 1"}) {
		t.Fatal("Set returned false")
	}
	got, ok := s.Resolve("code-reviewer")
	if !ok {
		t.Fatal("Resolve failed after Set")
	}
	if got.GemID != "g1" || got.BaseModel != "gemini-pro" || got.AccountID != "账号 1" {
		t.Fatalf("unexpected mapping: %+v", got)
	}
	if got.Model != "code-reviewer" {
		t.Fatalf("Model should be filled: %q", got.Model)
	}

	// BaseModel defaults when empty.
	s.Set("writer", GemMapping{GemID: "g2"})
	if g, _ := s.Resolve("writer"); g.BaseModel != defaultGemBaseModel {
		t.Fatalf("BaseModel default = %q, want %q", g.BaseModel, defaultGemBaseModel)
	}
	// A mapping with no gem_id is rejected.
	if s.Set("bad", GemMapping{}) {
		t.Fatal("Set with empty gem_id should fail")
	}

	// Persistence: a fresh store reads what the first wrote.
	s2 := NewGemMappingStore(path)
	if _, ok := s2.Resolve("code-reviewer"); !ok {
		t.Fatal("mapping did not persist")
	}

	if !s2.Delete("code-reviewer") {
		t.Fatal("Delete returned false")
	}
	if s2.Delete("code-reviewer") {
		t.Fatal("second Delete should return false")
	}
	if _, ok := NewGemMappingStore(path).Resolve("code-reviewer"); ok {
		t.Fatal("deletion did not persist")
	}
}

func TestGemMappingStoreDeleteByGem(t *testing.T) {
	s := NewGemMappingStore("")
	s.Set("a", GemMapping{GemID: "g1", AccountID: "账号 1"})
	s.Set("b", GemMapping{GemID: "g1", AccountID: "账号 1"})
	s.Set("c", GemMapping{GemID: "g1", AccountID: "账号 2"}) // different account
	s.Set("d", GemMapping{GemID: "g2", AccountID: "账号 1"})

	removed := s.DeleteByGem("g1", "账号 1")
	if len(removed) != 2 || removed[0] != "a" || removed[1] != "b" {
		t.Fatalf("DeleteByGem removed %v, want [a b]", removed)
	}
	if _, ok := s.Resolve("a"); ok {
		t.Fatal("a should be gone")
	}
	if _, ok := s.Resolve("c"); !ok {
		t.Fatal("c (different account) should survive")
	}
	if _, ok := s.Resolve("d"); !ok {
		t.Fatal("d (different gem) should survive")
	}
	if got := s.DeleteByGem("g1", "账号 1"); len(got) != 0 {
		t.Fatalf("second DeleteByGem should be empty, got %v", got)
	}
}

func TestProviderCatalogExposesGemModels(t *testing.T) {
	p := NewProvider("gw", nil)
	base := len(p.catalog())

	if _, ok := p.ResolveGem("code-reviewer"); ok {
		t.Fatal("no store attached → ResolveGem must be false")
	}

	p.SetGemMappingStore(NewGemMappingStore(""))
	if p.SetGemMapping("code-reviewer", GemMapping{GemID: "g1", BaseModel: "gemini-pro"}) != true {
		t.Fatal("SetGemMapping failed")
	}
	cat := p.catalog()
	if len(cat) != base+1 {
		t.Fatalf("catalog size = %d, want %d", len(cat), base+1)
	}
	found := false
	for _, m := range cat {
		if m.ID == "code-reviewer" {
			found = true
		}
	}
	if !found {
		t.Fatal("gem alias not exposed in catalog")
	}
	if gm, ok := p.ResolveGem("code-reviewer"); !ok || gm.GemID != "g1" {
		t.Fatalf("ResolveGem = %+v, %v", gm, ok)
	}
}

func TestGemMappingStoreNoPathIsMemoryOnly(t *testing.T) {
	s := NewGemMappingStore("")
	s.Set("x", GemMapping{GemID: "g"})
	if _, ok := s.Resolve("x"); !ok {
		t.Fatal("memory-only store should resolve")
	}
	// Ensure no stray file was written.
	if _, err := os.Stat("gem-mapping.json"); err == nil {
		t.Fatal("memory-only store must not write a file")
	}
}
