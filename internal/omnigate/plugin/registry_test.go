package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func samplePlugin(typ, display string) Plugin {
	return Plugin{
		Type:        typ,
		DisplayName: display,
		AccountMode: AccountModePool,
		Traits:      Traits{WireFormat: WireOpenAI},
		ConfigFields: []Field{
			{Key: "base_url", Label: "Base URL", Kind: KindURL},
		},
		AccountFields: []Field{
			{Key: "label", Label: "Label", Kind: KindText, Required: true},
		},
		Capabilities: []Capability{CapAccounts, CapModels},
		Actions: []Action{
			{Name: ActionRemove, Label: "Remove", Scope: ActionScopeAccount},
		},
		Tasks: []Task{},
	}
}

func mustPanic(t *testing.T, fn func()) any {
	t.Helper()
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		fn()
	}()
	if recovered == nil {
		t.Fatal("expected panic, got none")
	}
	return recovered
}

func TestRegistryRegisterGetAndOrder(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(samplePlugin("zeta", "Same"))
	r.MustRegister(samplePlugin("alpha", "Alpha"))
	r.MustRegister(samplePlugin("beta", "Same"))

	p, err := r.Get("alpha")
	if err != nil {
		t.Fatalf("Get(alpha): %v", err)
	}
	if p.DisplayName != "Alpha" {
		t.Fatalf("Get(alpha).DisplayName = %q", p.DisplayName)
	}

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("All() len = %d, want 3", len(all))
	}
	wantOrder := []string{"alpha", "beta", "zeta"}
	for i, want := range wantOrder {
		if all[i].Type != want {
			t.Fatalf("All()[%d].Type = %q, want %q (order: %v)", i, all[i].Type, want, typesOf(all))
		}
	}

	types := r.Types()
	wantTypes := []string{"alpha", "beta", "zeta"}
	for i := range wantTypes {
		if types[i] != wantTypes[i] {
			t.Fatalf("Types() = %v, want %v", types, wantTypes)
		}
	}
}

func TestRegistryDuplicate(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(samplePlugin("dup", "Dup")); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	err := r.Register(samplePlugin("dup", "Dup 2"))
	if !errors.Is(err, ErrDuplicateType) {
		t.Fatalf("duplicate Register error = %v, want ErrDuplicateType", err)
	}
	if !strings.Contains(err.Error(), `"dup"`) {
		t.Fatalf("duplicate error should name the type: %v", err)
	}

	got := mustPanic(t, func() { r.MustRegister(samplePlugin("dup", "Dup 3")) })
	if !errors.Is(got.(error), ErrDuplicateType) {
		t.Fatalf("MustRegister panic = %v, want ErrDuplicateType", got)
	}
}

func TestRegistryUnknownType(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(samplePlugin("alpha", "Alpha"))

	_, err := r.Get("nope")
	if !errors.Is(err, ErrUnknownType) {
		t.Fatalf("Get(nope) error = %v, want ErrUnknownType", err)
	}
	if !strings.Contains(err.Error(), "nope") || !strings.Contains(err.Error(), "alpha") {
		t.Fatalf("unknown-type error should name the missing and registered types: %v", err)
	}
}

func TestRegistryRejectsInvalidPlugins(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Plugin)
	}{
		{"empty type", func(p *Plugin) { p.Type = "" }},
		{"bad type", func(p *Plugin) { p.Type = "Bad Type" }},
		{"empty display", func(p *Plugin) { p.DisplayName = "" }},
		{"bad account mode", func(p *Plugin) { p.AccountMode = "weird" }},
		{"bad wire format", func(p *Plugin) { p.Traits.WireFormat = "soap" }},
		{"negative prompt limit", func(p *Plugin) { p.Traits.MaxPromptChars = -1 }},
		{"bad field kind", func(p *Plugin) { p.ConfigFields[0].Kind = "bogus" }},
		{"duplicate field", func(p *Plugin) {
			p.ConfigFields = append(p.ConfigFields, p.ConfigFields[0])
		}},
		{"dangling show_if", func(p *Plugin) {
			p.ConfigFields = append(p.ConfigFields, Field{
				Key: "x", Label: "X", Kind: KindText,
				ShowIf: &Condition{Key: "missing"},
			})
		}},
		{"none with account fields", func(p *Plugin) { p.AccountMode = AccountModeNone }},
		{"pool without account fields", func(p *Plugin) { p.AccountFields = nil }},
		{"unknown capability", func(p *Plugin) { p.Capabilities = []Capability{"telepathy"} }},
		{"duplicate capability", func(p *Plugin) { p.Capabilities = append(p.Capabilities, CapModels) }},
		{"unknown action", func(p *Plugin) {
			p.Actions = []Action{{Name: "explode", Label: "Boom", Scope: ActionScopeAccount}}
		}},
		{"duplicate action", func(p *Plugin) {
			p.Actions = append(p.Actions, Action{Name: ActionRemove, Label: "Remove again", Scope: ActionScopeAccount})
		}},
		{"empty task type", func(p *Plugin) { p.Tasks = []Task{{Label: "x"}} }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := samplePlugin("valid", "Valid")
			tc.mutate(&p)
			r := NewRegistry()
			err := r.Register(p)
			if !errors.Is(err, ErrInvalidPlugin) {
				t.Fatalf("Register error = %v, want ErrInvalidPlugin", err)
			}
		})
	}
}

func TestRegistryReturnsCopies(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(samplePlugin("alpha", "Alpha"))

	first, err := r.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	first.ConfigFields[0].Label = "mutated"
	first.Capabilities[0] = CapQuota

	second, err := r.Get("alpha")
	if err != nil {
		t.Fatal(err)
	}
	if second.ConfigFields[0].Label != "Base URL" {
		t.Fatalf("stored field was mutated through Get: %q", second.ConfigFields[0].Label)
	}
	if second.Capabilities[0] != CapAccounts {
		t.Fatalf("stored capability was mutated through Get: %v", second.Capabilities)
	}
}

func TestRegistryNormalizesNilSlices(t *testing.T) {
	r := NewRegistry()
	p := samplePlugin("plain", "Plain")
	p.AccountMode = AccountModeNone
	p.AccountFields = nil
	p.Actions = nil
	p.Tasks = nil
	r.MustRegister(p)

	got, err := r.Get("plain")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"account_schema":[]`, `"actions":[]`, `"tasks":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("normalized JSON missing %s: %s", want, raw)
		}
	}
}

func TestPackageLevelRegistry(t *testing.T) {
	if _, err := Get("pkgtest"); err == nil {
		return // already registered by an earlier -count run
	}
	if err := Register(samplePlugin("pkgtest", "Package Test")); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := Get("pkgtest"); err != nil {
		t.Fatalf("Get after Register: %v", err)
	}
	found := false
	for _, typ := range Types() {
		if typ == "pkgtest" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("Types() does not include pkgtest")
	}
}

func typesOf(ps []Plugin) []string {
	out := make([]string, len(ps))
	for i := range ps {
		out[i] = ps[i].Type
	}
	return out
}
