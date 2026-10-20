package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
)

// 描述符里的 schema key 必须真能落到 ProviderConfig / AccountConfig 的 JSON
// 字段。写错一个 key（例如 device_hdr）不会报错：面板照常渲染输入框，用户填的
// 值却被解码层静默丢弃。这条测试把 schema 与配置结构钉在一起。
func TestCatalogSchemaKeysMapToConfigFields(t *testing.T) {
	configKeys := jsonFieldNames(ProviderConfig{})
	accountKeys := jsonFieldNames(AccountConfig{})

	for _, desc := range catalog.All() {
		for _, f := range desc.ConfigFields {
			if !configKeys[f.Key] {
				t.Errorf("%s: config_schema key %q 不是 ProviderConfig 的 JSON 字段", desc.Type, f.Key)
			}
		}
		for _, f := range desc.AccountFields {
			if !accountKeys[f.Key] {
				t.Errorf("%s: account_schema key %q 不是 AccountConfig 的 JSON 字段", desc.Type, f.Key)
			}
		}
	}
}

// jsonFieldNames 收集结构体的 JSON tag 名（忽略 omitempty 等选项）。
func jsonFieldNames(v any) map[string]bool {
	t := reflect.TypeOf(v)
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out[name] = true
		}
	}
	return out
}
