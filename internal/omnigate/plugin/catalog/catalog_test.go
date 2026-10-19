package catalog_test

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/newapi"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/opencode"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/trae"
)

// Compile-time proof that the aliases in the SPI match the existing
// implementations: the core Provider and the optional interfaces can be asserted
// on today's adapters without touching them.
var (
	_ plugin.Provider = (*raccoon.Provider)(nil)
	_ plugin.Provider = (*runable.Provider)(nil)
	_ plugin.Provider = (*opencode.Provider)(nil)
	_ plugin.Provider = (*newapi.Provider)(nil)
	_ plugin.Provider = (*openaibackend.Provider)(nil)
	_ plugin.Provider = (*geminiweb.Provider)(nil)
	_ plugin.Provider = (*trae.Provider)(nil)

	_ plugin.AccountModelLister = (*raccoon.Provider)(nil)
	_ plugin.AccountModelLister = (*newapi.Provider)(nil)
	_ plugin.AccountModelLister = (*trae.Provider)(nil)

	_ plugin.CookieRotator = (*geminiweb.Provider)(nil)

	_ plugin.TokenRefresher = (*raccoon.Provider)(nil)
	_ plugin.TokenRefresher = (*newapi.Provider)(nil)
	_ plugin.TokenRefresher = (*geminiweb.Provider)(nil)
	_ plugin.TokenRefresher = (*trae.Provider)(nil)
)

var wantTypes = []string{"raccoon", "runable", "opencode", "newapi", "openai", "geminiweb", "trae"}

func TestAllSevenTypes(t *testing.T) {
	all := catalog.All()
	if len(all) != len(wantTypes) {
		t.Fatalf("catalog.All() len = %d, want %d", len(all), len(wantTypes))
	}
	got := make(map[string]plugin.Plugin, len(all))
	for _, p := range all {
		if _, dup := got[p.Type]; dup {
			t.Fatalf("duplicate type %q in catalog", p.Type)
		}
		got[p.Type] = p
	}
	for _, typ := range wantTypes {
		if _, ok := got[typ]; !ok {
			t.Fatalf("catalog missing type %q", typ)
		}
	}
}

func TestEveryDescriptorHasFactory(t *testing.T) {
	for _, p := range catalog.All() {
		if p.Factory == nil {
			t.Errorf("type %q missing Factory", p.Type)
			continue
		}
		prov, err := p.Factory(plugin.ProviderSpec{Name: "test-" + p.Type}, plugin.Deps{})
		if err != nil {
			t.Errorf("type %q factory: %v", p.Type, err)
			continue
		}
		if prov == nil {
			t.Errorf("type %q factory returned nil provider", p.Type)
		}
	}
}

func TestRegisterInto(t *testing.T) {
	r := plugin.NewRegistry()
	catalog.RegisterInto(r)

	types := r.Types()
	sorted := append([]string(nil), types...)
	sort.Strings(sorted)
	for i := range sorted {
		if types[i] != sorted[i] {
			t.Fatalf("Registry.Types() not sorted: %v", types)
		}
	}
	for _, typ := range wantTypes {
		p, err := r.Get(typ)
		if err != nil {
			t.Fatalf("Get(%s): %v", typ, err)
		}
		if p.Type != typ {
			t.Fatalf("Get(%s).Type = %q", typ, p.Type)
		}
	}
}

func TestRegisterDefaultRegistry(t *testing.T) {
	catalog.Register() // idempotent; explicit, not init()
	for _, typ := range wantTypes {
		if _, err := plugin.Get(typ); err != nil {
			t.Fatalf("global Get(%s): %v", typ, err)
		}
	}
}

func TestDescriptorsAreValid(t *testing.T) {
	for _, p := range catalog.All() {
		t.Run(p.Type, func(t *testing.T) {
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if len(p.ConfigFields) == 0 {
				t.Fatal("no config fields")
			}
			if len(p.Capabilities) == 0 {
				t.Fatal("no capabilities")
			}
			switch p.AccountMode {
			case plugin.AccountModeNone:
				if len(p.AccountFields) != 0 {
					t.Fatalf("account_mode=none but %d account fields", len(p.AccountFields))
				}
				if p.HasCapability(plugin.CapAccounts) {
					t.Fatal("account_mode=none must not declare the accounts capability")
				}
				if !p.HasCapability(plugin.CapUpstreamKeys) {
					t.Fatal("account_mode=none should declare upstream_keys")
				}
			default:
				if len(p.AccountFields) == 0 {
					t.Fatal("account-bearing type has no account fields")
				}
				if !p.HasCapability(plugin.CapAccounts) {
					t.Fatal("account-bearing type must declare the accounts capability")
				}
				if !hasField(p.AccountFields, "label") {
					t.Fatal("account schema has no label field")
				}
			}
			for _, a := range p.Actions {
				if !a.Name.Valid() || !a.Scope.Valid() {
					t.Fatalf("invalid action: %+v", a)
				}
			}
			for _, c := range p.Capabilities {
				if !c.Valid() {
					t.Fatalf("invalid capability %q", c)
				}
			}
		})
	}
}

func TestCapabilities(t *testing.T) {
	want := map[string][]plugin.Capability{
		"raccoon":  {plugin.CapAccounts, plugin.CapBalance, plugin.CapCheckin, plugin.CapLogin, plugin.CapModels, plugin.CapRefresh},
		"runable":  {plugin.CapAccounts, plugin.CapBalance, plugin.CapCheckin, plugin.CapLogin, plugin.CapModels},
		"opencode": {plugin.CapUpstreamKeys, plugin.CapQuota, plugin.CapModels},
		"newapi":   {plugin.CapAccounts, plugin.CapBalance, plugin.CapCheckin, plugin.CapLogin, plugin.CapModels, plugin.CapRefresh},
		"openai":   {plugin.CapUpstreamKeys, plugin.CapModels},
		"geminiweb": {
			plugin.CapAccounts, plugin.CapCheckin, plugin.CapLogin, plugin.CapModels, plugin.CapRefresh,
			plugin.CapAttachments, plugin.CapCookieRotation, plugin.CapFingerprint,
		},
		"trae": {plugin.CapAccounts, plugin.CapBalance, plugin.CapCheckin, plugin.CapLogin, plugin.CapModels, plugin.CapRefresh, plugin.CapFingerprint},
	}
	for _, p := range catalog.All() {
		got := make(map[plugin.Capability]bool, len(p.Capabilities))
		for _, c := range p.Capabilities {
			got[c] = true
		}
		expected := want[p.Type]
		if len(got) != len(expected) {
			t.Fatalf("%s: capabilities = %v, want %v", p.Type, p.Capabilities, expected)
		}
		for _, c := range expected {
			if !got[c] {
				t.Fatalf("%s: missing capability %q (got %v)", p.Type, c, p.Capabilities)
			}
		}
	}
}

func TestActions(t *testing.T) {
	want := map[string][]plugin.ActionName{
		"raccoon":   {plugin.ActionCheckin, plugin.ActionRefresh, plugin.ActionLogin, plugin.ActionRemove},
		"runable":   {plugin.ActionVerify, plugin.ActionCheckin, plugin.ActionLogin, plugin.ActionRemove},
		"opencode":  {plugin.ActionQuota},
		"newapi":    {plugin.ActionCheckin, plugin.ActionRefresh, plugin.ActionLogin, plugin.ActionRemove},
		"openai":    {},
		"geminiweb": {plugin.ActionCheckin, plugin.ActionRefresh, plugin.ActionLogin, plugin.ActionRemove},
		"trae":      {plugin.ActionCheckin, plugin.ActionRefresh, plugin.ActionLogin, plugin.ActionRemove},
	}
	for _, p := range catalog.All() {
		got := make(map[plugin.ActionName]bool, len(p.Actions))
		for _, a := range p.Actions {
			got[a.Name] = true
		}
		expected := want[p.Type]
		if len(got) != len(expected) {
			t.Fatalf("%s: actions = %v, want %v", p.Type, p.Actions, expected)
		}
		for _, name := range expected {
			if !got[name] {
				t.Fatalf("%s: missing action %q (got %v)", p.Type, name, p.Actions)
			}
		}
	}
}

func TestLoginSpecs(t *testing.T) {
	byType := pluginMap(t)
	wantKind := map[string]plugin.LoginKind{
		"raccoon":   plugin.LoginKindOAuth,
		"runable":   plugin.LoginKindPaste,
		"newapi":    plugin.LoginKindSnippet,
		"geminiweb": plugin.LoginKindPaste,
		"trae":      plugin.LoginKindOAuth,
	}
	wantTransport := map[string]plugin.LoginTransport{
		"raccoon":   plugin.LoginTransportAdmin,
		"runable":   plugin.LoginTransportAccount,
		"newapi":    plugin.LoginTransportNewAPI,
		"geminiweb": plugin.LoginTransportAdmin,
		"trae":      plugin.LoginTransportAdmin,
	}
	for typ, kind := range wantKind {
		spec := byType[typ].Login
		if spec == nil {
			t.Fatalf("%s: missing login spec", typ)
		}
		if spec.Kind != kind {
			t.Fatalf("%s: login kind = %q, want %q", typ, spec.Kind, kind)
		}
		if spec.Transport != wantTransport[typ] {
			t.Fatalf("%s: login transport = %q, want %q", typ, spec.Transport, wantTransport[typ])
		}
		if !byType[typ].HasCapability(plugin.CapLogin) {
			t.Fatalf("%s: declares login spec but not the login capability", typ)
		}
	}
	// Types without an interactive flow must not declare login or the capability.
	for _, typ := range []string{"opencode", "openai"} {
		if byType[typ].Login != nil {
			t.Fatalf("%s: unexpected login spec", typ)
		}
		if byType[typ].HasCapability(plugin.CapLogin) {
			t.Fatalf("%s: unexpected login capability", typ)
		}
	}
}

func TestTraitDifferences(t *testing.T) {
	byType := pluginMap(t)

	if got := byType["runable"].Traits.MaxPromptChars; got != 8000 {
		t.Fatalf("runable MaxPromptChars = %d, want 8000", got)
	}
	if !byType["runable"].Traits.ToolEmulation {
		t.Fatal("runable should emulate tools")
	}
	if !byType["geminiweb"].Traits.Attachments || !byType["geminiweb"].Traits.RotatesCookie {
		t.Fatal("geminiweb should declare attachments and cookie rotation")
	}
	if byType["openai"].Traits.ToolEmulation {
		t.Fatal("openai has native tool calls; ToolEmulation should be false")
	}
	for _, typ := range []string{"raccoon", "newapi", "trae"} {
		if !byType[typ].Traits.AccountScopedModels {
			t.Fatalf("%s model catalog should be account-scoped", typ)
		}
	}
	if byType["geminiweb"].Traits.AccountScopedModels {
		t.Fatal("geminiweb exposes a built-in catalog; AccountScopedModels should be false")
	}
	for _, typ := range wantTypes {
		if !byType[typ].Traits.SystemPrompt {
			t.Fatalf("%s should support system_prompt injection", typ)
		}
	}
}

func TestProviderSpecificFields(t *testing.T) {
	byType := pluginMap(t)

	assertConfigFields(t, byType["opencode"], "base_url", "go_base_url", "zen_keys", "go_keys", "tier_prefer", "anonymous", "models")
	assertConfigFields(t, byType["raccoon"], "main_origin", "llm_base", "auth_base", "system_prompt")
	assertConfigFields(t, byType["trae"], "agent_host", "ug_host", "oauth_host", "system_prompt")
	assertConfigFields(t, byType["runable"], "base_url", "device_header", "max_prompt_chars", "system_prompt")
	assertConfigFields(t, byType["newapi"], "base_url", "models", "system_prompt")
	assertConfigFields(t, byType["openai"], "base_url", "api_key", "api_keys", "models")
	assertConfigFields(t, byType["geminiweb"], "models", "system_prompt")

	assertAccountFields(t, byType["runable"], "label", "cookie", "email", "password")
	assertAccountFields(t, byType["raccoon"], "label", "cookie", "refresh_token")
	assertAccountFields(t, byType["newapi"], "label", "cookie", "refresh_token", "api_key", "expires_at", "session_cookie")
	assertAccountFields(t, byType["geminiweb"], "label", "cookie")
	assertAccountFields(t, byType["trae"], "label", "cookie", "refresh_token", "expires_at", "uid", "machine_id", "device_id", "api_host")

	// newapi has two built-in scheduler tasks; a refresh-only task is distinct
	// from the daily check-in.
	tasks := make(map[string]bool)
	for _, task := range byType["newapi"].Tasks {
		tasks[task.Type] = true
	}
	if !tasks["newapi"] || !tasks["newapi-refresh"] {
		t.Fatalf("newapi tasks = %v", byType["newapi"].Tasks)
	}
}

func TestPluginJSONContract(t *testing.T) {
	byType := pluginMap(t)
	raw, err := json.Marshal(byType["openai"])
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"type", "display_name", "account_mode", "traits", "config_schema", "account_schema", "actions", "tasks", "capabilities"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("plugin JSON missing %q: %s", key, raw)
		}
	}
	if got["account_mode"] != "none" {
		t.Fatalf("account_mode = %v", got["account_mode"])
	}
	fields, ok := got["config_schema"].([]any)
	if !ok || len(fields) == 0 {
		t.Fatalf("config_schema = %#v", got["config_schema"])
	}
	foundSecret := false
	for _, item := range fields {
		f, _ := item.(map[string]any)
		if _, ok := f["key"]; !ok {
			t.Fatalf("config field missing key: %#v", f)
		}
		if _, ok := f["kind"]; !ok {
			t.Fatalf("config field missing kind: %#v", f)
		}
		if _, ok := f["required"]; !ok {
			t.Fatalf("config field missing required: %#v", f)
		}
		if f["key"] == "api_key" && f["kind"] == "secret" {
			foundSecret = true
		}
	}
	if !foundSecret {
		t.Fatalf("openai config_schema has no secret api_key: %s", raw)
	}
	if accounts, ok := got["account_schema"].([]any); !ok || len(accounts) != 0 {
		t.Fatalf("openai account_schema = %#v, want []", got["account_schema"])
	}
}

func pluginMap(t *testing.T) map[string]plugin.Plugin {
	t.Helper()
	out := make(map[string]plugin.Plugin)
	for _, p := range catalog.All() {
		out[p.Type] = p
	}
	return out
}

func assertConfigFields(t *testing.T, p plugin.Plugin, keys ...string) {
	t.Helper()
	got := make(map[string]bool, len(p.ConfigFields))
	for _, f := range p.ConfigFields {
		got[f.Key] = true
	}
	for _, key := range keys {
		if !got[key] {
			t.Fatalf("%s config_schema missing %q (got %v)", p.Type, key, p.ConfigFields)
		}
	}
}

func assertAccountFields(t *testing.T, p plugin.Plugin, keys ...string) {
	t.Helper()
	got := make(map[string]bool, len(p.AccountFields))
	for _, f := range p.AccountFields {
		got[f.Key] = true
	}
	for _, key := range keys {
		if !got[key] {
			t.Fatalf("%s account_schema missing %q (got %v)", p.Type, key, p.AccountFields)
		}
	}
}

func hasField(fields []plugin.Field, key string) bool {
	for _, f := range fields {
		if f.Key == key {
			return true
		}
	}
	return false
}
