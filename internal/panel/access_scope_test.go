package panel

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// 按用户名更新范围时，同名多把密钥必须一起改；范围按 providers 声明顺序排列；
// 未命中的同名密钥保持原样。
func TestUpdateAccessScopeUpdatesAllKeysByName(t *testing.T) {
	p, cookies := newUsagePanel(t, &fakeKeyUsageSink{})
	omni := map[string]any{
		"providers": []any{
			map[string]any{"name": "a"},
			map[string]any{"name": "b"},
			map[string]any{"name": "c"},
		},
		"api_keys": []any{
			map[string]any{"key": "sk-1", "name": "u", "providers": []any{"a"}},
			map[string]any{"key": "sk-2", "name": "u", "providers": []any{"a"}},
			map[string]any{"key": "sk-3", "name": "other", "providers": []any{"b"}},
		},
	}
	var saved map[string]any
	p.cfg.LoadOmniConfig = func() (any, error) { return omni, nil }
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) {
		if err := json.Unmarshal(raw, &saved); err != nil {
			return nil, err
		}
		return []string{"some-field"}, nil
	}

	rec := do(p, http.MethodPost, "/panel/api/access/scope",
		`{"name":"u","providers":["c","b","b","  "]}`, cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK        bool     `json:"ok"`
		Name      string   `json:"name"`
		Providers []string `json:"providers"`
		Updated   int      `json:"updated"`
		Restart   []string `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.OK || got.Name != "u" || got.Updated != 2 {
		t.Fatalf("resp = %+v", got)
	}
	if !reflect.DeepEqual(got.Providers, []string{"b", "c"}) {
		t.Errorf("providers = %v want [b c]", got.Providers)
	}
	if !reflect.DeepEqual(got.Restart, []string{"some-field"}) {
		t.Errorf("restart_required = %v", got.Restart)
	}

	list, _ := saved["api_keys"].([]any)
	if len(list) != 3 {
		t.Fatalf("saved api_keys = %v", saved["api_keys"])
	}
	for i, want := range [][]string{{"b", "c"}, {"b", "c"}, {"b"}} {
		obj, _ := list[i].(map[string]any)
		provs, _ := stringList(obj["providers"])
		if !reflect.DeepEqual(provs, want) {
			t.Errorf("key[%d] providers = %v want %v", i, provs, want)
		}
	}
}

// 不勾任何供应商 = 全量：保存时删掉 providers 字段，而不是写空数组。
func TestUpdateAccessScopeEmptyMeansFull(t *testing.T) {
	p, cookies := newUsagePanel(t, &fakeKeyUsageSink{})
	omni := map[string]any{
		"providers": []any{map[string]any{"name": "a"}},
		"api_keys": []any{
			map[string]any{"key": "sk-1", "name": "u", "providers": []any{"a"}},
		},
	}
	var saved map[string]any
	p.cfg.LoadOmniConfig = func() (any, error) { return omni, nil }
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) {
		if err := json.Unmarshal(raw, &saved); err != nil {
			return nil, err
		}
		return nil, nil
	}

	rec := do(p, http.MethodPost, "/panel/api/access/scope",
		`{"name":"u","providers":[]}`, cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	list, _ := saved["api_keys"].([]any)
	obj, _ := list[0].(map[string]any)
	if _, exists := obj["providers"]; exists {
		t.Errorf("全量应删除 providers 字段，得到 %v", obj)
	}
}

// 两个 JSON 对象 / 尾部垃圾必须拒绝：拒绝比「静默忽略第二个对象」安全。拼错
// 字段名由 DisallowUnknownFields 拦住；否则 {"name":"u"} 会解成「providers 为空」，
// 等于把用户从收窄静默放开成全量。
func TestUpdateAccessScopeRejectsTrailingBody(t *testing.T) {
	p, cookies := newUsagePanel(t, &fakeKeyUsageSink{})
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{
			"providers": []any{map[string]any{"name": "a"}},
			"api_keys": []any{
				map[string]any{"key": "sk-1", "name": "u", "providers": []any{"a"}},
			},
		}, nil
	}
	saved := false
	p.cfg.SaveOmniConfig = func([]byte) ([]string, error) {
		saved = true
		return nil, nil
	}

	for _, body := range []string{
		`{"name":"u","provider":["a"]}`,    // 字段名拼错
		`{"name":"u","providers":["a"]}{}`, // 多个对象
		`{"name":"u","providers":["a"]} x`, // 尾部垃圾
	} {
		rec := do(p, http.MethodPost, "/panel/api/access/scope", body, cookies[panelauth.RoleAdmin], nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s status = %d want 400", body, rec.Code)
		}
	}
	if saved {
		t.Error("非法请求体不应该触发保存")
	}
}

func TestUpdateAccessScopeRejectsUnknown(t *testing.T) {
	p, cookies := newUsagePanel(t, &fakeKeyUsageSink{})
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{
			"providers": []any{map[string]any{"name": "a"}},
			"api_keys": []any{
				map[string]any{"key": "sk-1", "name": "u", "providers": []any{"a"}},
			},
		}, nil
	}
	p.cfg.SaveOmniConfig = func([]byte) ([]string, error) { return nil, nil }

	// 未知供应商 → 400。
	rec := do(p, http.MethodPost, "/panel/api/access/scope",
		`{"name":"u","providers":["nope"]}`, cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("未知供应商 status = %d body=%s", rec.Code, rec.Body.String())
	}
	// 未知用户 → 404。
	rec = do(p, http.MethodPost, "/panel/api/access/scope",
		`{"name":"nobody","providers":["a"]}`, cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("未知用户 status = %d body=%s", rec.Code, rec.Body.String())
	}
	// 非管理员 → 403；viewer 也不能写。
	for _, role := range []panelauth.Role{panelauth.RoleViewer, panelauth.RoleUser} {
		rec = do(p, http.MethodPost, "/panel/api/access/scope",
			`{"name":"u","providers":["a"]}`, cookies[role], nil)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s 写范围 status = %d body=%s", role, rec.Code, rec.Body.String())
		}
	}
}
