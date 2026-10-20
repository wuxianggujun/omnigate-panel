package plugin

import (
	"fmt"
	"regexp"
	"strings"
)

// FieldKind is the render/parse contract a panel uses for one field.
type FieldKind string

// Field kinds understood by the panel. Keep these strings stable: they are
// serialized as config_schema / account_schema and consumed by the frontend.
const (
	KindText        FieldKind = "text"
	KindSecret      FieldKind = "secret"
	KindTextarea    FieldKind = "textarea"
	KindList        FieldKind = "list"
	KindNumber      FieldKind = "number"
	KindDuration    FieldKind = "duration"
	KindBool        FieldKind = "bool"
	KindEnum        FieldKind = "enum"
	KindMultiSelect FieldKind = "multiselect"
	KindURL         FieldKind = "url"
	KindJSON        FieldKind = "json"
	KindNote        FieldKind = "note"
	KindHidden      FieldKind = "hidden"
)

var fieldKinds = map[FieldKind]struct{}{
	KindText:        {},
	KindSecret:      {},
	KindTextarea:    {},
	KindList:        {},
	KindNumber:      {},
	KindDuration:    {},
	KindBool:        {},
	KindEnum:        {},
	KindMultiSelect: {},
	KindURL:         {},
	KindJSON:        {},
	KindNote:        {},
	KindHidden:      {},
}

// Valid reports whether k is a field kind the panel knows how to render.
func (k FieldKind) Valid() bool {
	_, ok := fieldKinds[k]
	return ok
}

// Common field groups. Free-form strings still work; these keep the built-in
// catalog consistent and give the panel stable section names.
const (
	GroupBasic       = "basic"
	GroupConnection  = "connection"
	GroupCredentials = "credentials"
	GroupModels      = "models"
	GroupBehavior    = "behavior"
	GroupAdvanced    = "advanced"
	GroupFingerprint = "fingerprint"
)

// EnumOption is one choice for KindEnum / KindMultiSelect.
type EnumOption struct {
	Value string `json:"value"`
	Label string `json:"label,omitempty"`
}

// Condition gates a field on another field's value. It is intentionally tiny:
// the panel only needs "show when key == value"; more expressive rules belong in
// provider code, not in a declarative schema.
type Condition struct {
	Key    string `json:"key"`
	Equals string `json:"equals,omitempty"`
}

// Field describes one config or account field. JSON tags are the panel contract:
// keep them stable.
type Field struct {
	Key         string       `json:"key"`
	Label       string       `json:"label"`
	Help        string       `json:"help,omitempty"`
	Group       string       `json:"group,omitempty"`
	Kind        FieldKind    `json:"kind"`
	Default     any          `json:"default,omitempty"`
	Enum        []EnumOption `json:"enum,omitempty"`
	Required    bool         `json:"required"`
	Advanced    bool         `json:"advanced,omitempty"`
	Placeholder string       `json:"placeholder,omitempty"`
	Pattern     string       `json:"pattern,omitempty"`
	Min         *float64     `json:"min,omitempty"`
	Max         *float64     `json:"max,omitempty"`
	ShowIf      *Condition   `json:"show_if,omitempty"`
	// ItemKind, on KindList fields, is the render/parse kind of each item (for
	// example secret for a key pool). Empty = plain text.
	ItemKind FieldKind `json:"item_kind,omitempty"`
}

// Validate checks one field's internal consistency. Referential checks (ShowIf
// targets, duplicate keys) need the whole list and live in validateFields.
func (f Field) Validate() error {
	if f.Key == "" {
		return fmt.Errorf("字段 key 不能为空")
	}
	if strings.TrimSpace(f.Label) == "" {
		return fmt.Errorf("字段 %q: label 不能为空", f.Key)
	}
	if !f.Kind.Valid() {
		return fmt.Errorf("字段 %q: 未知 kind %q", f.Key, f.Kind)
	}
	if f.ItemKind != "" {
		if f.Kind != KindList {
			return fmt.Errorf("字段 %q: item_kind 只能用于 list 字段", f.Key)
		}
		if !f.ItemKind.Valid() {
			return fmt.Errorf("字段 %q: 未知 item_kind %q", f.Key, f.ItemKind)
		}
	}
	if (f.Kind == KindEnum || f.Kind == KindMultiSelect) && len(f.Enum) == 0 {
		return fmt.Errorf("字段 %q: kind=%s 必须提供 enum", f.Key, f.Kind)
	}
	seen := make(map[string]struct{}, len(f.Enum))
	for _, opt := range f.Enum {
		if opt.Value == "" {
			return fmt.Errorf("字段 %q: enum 选项 value 不能为空", f.Key)
		}
		if _, dup := seen[opt.Value]; dup {
			return fmt.Errorf("字段 %q: enum 选项 %q 重复", f.Key, opt.Value)
		}
		seen[opt.Value] = struct{}{}
	}
	if f.Min != nil && f.Max != nil && *f.Min > *f.Max {
		return fmt.Errorf("字段 %q: min (%v) 大于 max (%v)", f.Key, *f.Min, *f.Max)
	}
	if f.Pattern != "" {
		if _, err := regexp.Compile(f.Pattern); err != nil {
			return fmt.Errorf("字段 %q: 非法 pattern: %w", f.Key, err)
		}
	}
	return nil
}

// validateFields validates a whole schema and its cross-field references.
func validateFields(scope string, fields []Field) error {
	keys := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		if _, dup := keys[f.Key]; dup {
			return fmt.Errorf("%s: 字段 key %q 重复", scope, f.Key)
		}
		keys[f.Key] = struct{}{}
		if err := f.Validate(); err != nil {
			return fmt.Errorf("%s: %w", scope, err)
		}
	}
	for _, f := range fields {
		if f.ShowIf == nil {
			continue
		}
		if f.ShowIf.Key == "" {
			return fmt.Errorf("%s: 字段 %q 的 show_if.key 不能为空", scope, f.Key)
		}
		if f.ShowIf.Key == f.Key {
			return fmt.Errorf("%s: 字段 %q 不能依赖自身", scope, f.Key)
		}
		if _, ok := keys[f.ShowIf.Key]; !ok {
			return fmt.Errorf("%s: 字段 %q 的 show_if 指向未知字段 %q", scope, f.Key, f.ShowIf.Key)
		}
	}
	return nil
}
