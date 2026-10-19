package plugin

import (
	"encoding/json"
	"testing"
)

func TestFieldKindStableValues(t *testing.T) {
	want := map[FieldKind]string{
		KindText:        "text",
		KindSecret:      "secret",
		KindTextarea:    "textarea",
		KindList:        "list",
		KindNumber:      "number",
		KindDuration:    "duration",
		KindBool:        "bool",
		KindEnum:        "enum",
		KindMultiSelect: "multiselect",
		KindURL:         "url",
		KindJSON:        "json",
		KindNote:        "note",
		KindHidden:      "hidden",
	}
	for kind, value := range want {
		if string(kind) != value {
			t.Fatalf("kind %q serializes as %q, want %q", kind, string(kind), value)
		}
		if !kind.Valid() {
			t.Fatalf("kind %q is not valid", kind)
		}
	}
	if FieldKind("nope").Valid() {
		t.Fatal("unknown kind reported valid")
	}
}

func TestFieldJSONShape(t *testing.T) {
	min, max := 0.0, 16.0
	f := Field{
		Key:         "api_key",
		Label:       "API Key",
		Help:        "上游密钥",
		Group:       GroupCredentials,
		Kind:        KindSecret,
		Default:     "",
		Enum:        []EnumOption{{Value: "a", Label: "A"}},
		Required:    false,
		Advanced:    true,
		Placeholder: "sk-...",
		Pattern:     `^sk-`,
		Min:         &min,
		Max:         &max,
		ShowIf:      &Condition{Key: "anonymous", Equals: "false"},
	}
	raw, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"key", "label", "help", "group", "kind", "default", "enum",
		"required", "advanced", "placeholder", "pattern", "min", "max", "show_if",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("JSON missing %q: %s", key, raw)
		}
	}
	if got["key"] != "api_key" {
		t.Fatalf("key = %v", got["key"])
	}
	if got["kind"] != "secret" {
		t.Fatalf("kind = %v", got["kind"])
	}
	// required is deliberately not omitempty: the panel should see an explicit
	// false rather than having to infer a missing field.
	if v, ok := got["required"]; !ok || v != false {
		t.Fatalf("required = %v (present=%v), want explicit false", v, ok)
	}
	showIf, ok := got["show_if"].(map[string]any)
	if !ok || showIf["key"] != "anonymous" || showIf["equals"] != "false" {
		t.Fatalf("show_if = %#v", got["show_if"])
	}
}

func TestFieldValidate(t *testing.T) {
	validEnum := []EnumOption{{Value: "go", Label: "Go"}, {Value: "zen"}}
	tests := []struct {
		name    string
		field   Field
		wantErr bool
	}{
		{"valid text", Field{Key: "k", Label: "K", Kind: KindText}, false},
		{"valid enum", Field{Key: "k", Label: "K", Kind: KindEnum, Enum: validEnum}, false},
		{"empty key", Field{Label: "K", Kind: KindText}, true},
		{"empty label", Field{Key: "k", Kind: KindText}, true},
		{"unknown kind", Field{Key: "k", Label: "K", Kind: "bogus"}, true},
		{"enum without options", Field{Key: "k", Label: "K", Kind: KindEnum}, true},
		{"multiselect without options", Field{Key: "k", Label: "K", Kind: KindMultiSelect}, true},
		{"empty enum value", Field{Key: "k", Label: "K", Kind: KindEnum, Enum: []EnumOption{{Value: ""}}}, true},
		{"duplicate enum value", Field{Key: "k", Label: "K", Kind: KindEnum, Enum: []EnumOption{{Value: "a"}, {Value: "a"}}}, true},
		{"min above max", Field{Key: "k", Label: "K", Kind: KindNumber, Min: ptrFloat(2), Max: ptrFloat(1)}, true},
		{"bad pattern", Field{Key: "k", Label: "K", Kind: KindText, Pattern: "["}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.field.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateFields(t *testing.T) {
	tests := []struct {
		name    string
		fields  []Field
		wantErr bool
	}{
		{
			name: "valid show_if",
			fields: []Field{
				{Key: "mode", Label: "Mode", Kind: KindEnum, Enum: []EnumOption{{Value: "a"}}},
				{Key: "detail", Label: "Detail", Kind: KindText, ShowIf: &Condition{Key: "mode", Equals: "a"}},
			},
		},
		{
			name: "duplicate key",
			fields: []Field{
				{Key: "k", Label: "K", Kind: KindText},
				{Key: "k", Label: "K2", Kind: KindText},
			},
			wantErr: true,
		},
		{
			name: "show_if self",
			fields: []Field{
				{Key: "k", Label: "K", Kind: KindText, ShowIf: &Condition{Key: "k"}},
			},
			wantErr: true,
		},
		{
			name: "show_if unknown",
			fields: []Field{
				{Key: "k", Label: "K", Kind: KindText, ShowIf: &Condition{Key: "missing"}},
			},
			wantErr: true,
		},
		{
			name: "show_if empty key",
			fields: []Field{
				{Key: "k", Label: "K", Kind: KindText, ShowIf: &Condition{}},
			},
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFields("test", tc.fields)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }
