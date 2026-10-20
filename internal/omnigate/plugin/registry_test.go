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

func TestLoginSpecClonedAndValidated(t *testing.T) {
	r := NewRegistry()
	p := samplePlugin("loginable", "Loginable")
	p.Login = &LoginSpec{
		Kind:      LoginKindOAuth,
		Transport: LoginTransportAdmin,
		Label:     "浏览器授权",
		Fields: []Field{
			{Key: "label", Label: "Label", Kind: KindText},
		},
	}
	r.MustRegister(p)

	got, err := r.Get("loginable")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the returned copy must not touch the stored descriptor.
	got.Login.Fields[0].Label = "HACKED"
	again, _ := r.Get("loginable")
	if again.Login.Fields[0].Label != "Label" {
		t.Fatalf("stored login fields mutated through Get: %q", again.Login.Fields[0].Label)
	}

	// An unknown login kind is rejected at registration.
	bad := samplePlugin("badlogin", "Bad Login")
	bad.Login = &LoginSpec{Kind: "telepathy", Transport: LoginTransportAdmin}
	if err := r.Register(bad); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("Register with bad login kind: %v", err)
	}

	// An unknown login transport is rejected at registration.
	badTransport := samplePlugin("badtransport", "Bad Transport")
	badTransport.Login = &LoginSpec{Kind: LoginKindPaste, Transport: "carrier-pigeon"}
	if err := r.Register(badTransport); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("Register with bad login transport: %v", err)
	}

	// account_mode=none must not declare a login flow.
	none := samplePlugin("nol", "No Login")
	none.AccountMode = AccountModeNone
	none.AccountFields = nil
	none.Login = &LoginSpec{Kind: LoginKindPaste, Transport: LoginTransportAccount}
	if err := r.Register(none); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("Register account_mode=none with login: %v", err)
	}
}

// 任务类型 → 插件描述的解析：调度器靠它按描述符分发，不再 switch 类型字符串。
func TestRegistryTaskByType(t *testing.T) {
	r := NewRegistry()
	p := samplePlugin("alpha", "Alpha")
	p.Actions = append(p.Actions, Action{Name: ActionCheckin, Label: "签到", Scope: ActionScopeAccount})
	p.Tasks = []Task{{Type: "alpha-checkin", Label: "签到", Action: ActionCheckin, Scope: ActionScopeAccount}}
	r.MustRegister(p)
	// A type that declares no task.
	r.MustRegister(samplePlugin("beta", "Beta"))

	desc, decl, ok := r.TaskByType("alpha-checkin")
	if !ok {
		t.Fatal("alpha-checkin 应能解析到声明它的插件")
	}
	if desc.Type != "alpha" {
		t.Fatalf("desc.Type = %q, want alpha", desc.Type)
	}
	if decl.Action != ActionCheckin || decl.Scope != ActionScopeAccount {
		t.Fatalf("decl = %+v, want checkin/account", decl)
	}
	if _, _, ok := r.TaskByType("nope"); ok {
		t.Fatal("未声明的任务类型不该解析成功")
	}
}

// 任务声明的 action 必须在同一描述符的 actions 里：否则调度器会分发到一个该类型
// 运行时不接受的操作。
func TestRegistryRejectsTaskWithUndeclaredAction(t *testing.T) {
	r := NewRegistry()
	p := samplePlugin("alpha", "Alpha")
	// samplePlugin declares only ActionRemove; a task claiming checkin is invalid.
	p.Tasks = []Task{{Type: "alpha-checkin", Label: "签到", Action: ActionCheckin}}
	if err := r.Register(p); !errors.Is(err, ErrInvalidPlugin) {
		t.Fatalf("task action 未声明应被拒绝，得到 %v", err)
	}

	// A task that declares no action is fine (it is not a provider operation).
	plain := samplePlugin("plain", "Plain")
	plain.Tasks = []Task{{Type: "plain-noop", Label: "无操作"}}
	if err := r.Register(plain); err != nil {
		t.Fatalf("未声明 action 的任务应被接受: %v", err)
	}
}

// 按 login.transport 反查类型：面板代理用它决定放行哪些 kind，新增 admin
// 传输的插件后无需再改面板。
func TestRegistryTypesByLoginTransport(t *testing.T) {
	r := NewRegistry()
	admin1 := samplePlugin("alpha", "Alpha")
	admin1.Login = &LoginSpec{Kind: LoginKindOAuth, Transport: LoginTransportAdmin}
	r.MustRegister(admin1)
	admin2 := samplePlugin("gamma", "Gamma")
	admin2.Login = &LoginSpec{Kind: LoginKindPaste, Transport: LoginTransportAdmin}
	r.MustRegister(admin2)
	acct := samplePlugin("beta", "Beta")
	acct.Login = &LoginSpec{Kind: LoginKindPaste, Transport: LoginTransportAccount}
	r.MustRegister(acct)
	// 无登录流程的类型不参与任何 transport 集合。
	r.MustRegister(samplePlugin("nol", "No Login"))

	got := r.TypesByLoginTransport(LoginTransportAdmin)
	want := []string{"alpha", "gamma"} // 词法序
	if len(got) != len(want) {
		t.Fatalf("admin 传输类型 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("admin 传输类型 = %v, want %v", got, want)
		}
	}
	if acctTypes := r.TypesByLoginTransport(LoginTransportAccount); len(acctTypes) != 1 || acctTypes[0] != "beta" {
		t.Fatalf("account 传输类型 = %v, want [beta]", acctTypes)
	}
	if newapi := r.TypesByLoginTransport(LoginTransportNewAPI); len(newapi) != 0 {
		t.Fatalf("无 newapi 传输类型时应为空，得到 %v", newapi)
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
