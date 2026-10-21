package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestOmniProbeModelsPassesProviderConfig 覆盖面板 → 引擎的探测编排：请求体里的
// provider 原样透传给注入的闭包，结果包在 {ok,data} 里返回。
func TestOmniProbeModelsPassesProviderConfig(t *testing.T) {
	var gotRaw json.RawMessage
	p, token := mustPanel(t, Config{
		OmniProbeModels: func(raw json.RawMessage) (any, error) {
			gotRaw = raw
			return map[string]any{"models": []map[string]any{{"id": "m-1"}}}, nil
		},
	})
	body := `{"provider":{"name":"probe-openai","type":"openai","base_url":"https://x/v1","api_key":"sk-1"}}`
	rec := do(p, http.MethodPost, "/panel/api/omni/provider-models/probe", body,
		nil, map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusOK {
		t.Fatalf("probe = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatalf("探测闭包收到非法 JSON: %v (%s)", err, gotRaw)
	}
	if got.Name != "probe-openai" || got.Type != "openai" || got.BaseURL != "https://x/v1" || got.APIKey != "sk-1" {
		t.Fatalf("探测闭包收到 %+v，字段未原样透传", got)
	}
	if !strings.Contains(rec.Body.String(), `"m-1"`) {
		t.Errorf("响应缺少模型: %s", rec.Body.String())
	}
}

// TestOmniProbeModelsRequiresProvider 缺 provider 字段直接 400，不打引擎。
func TestOmniProbeModelsRequiresProvider(t *testing.T) {
	called := false
	p, token := mustPanel(t, Config{
		OmniProbeModels: func(json.RawMessage) (any, error) { called = true; return nil, nil },
	})
	rec := do(p, http.MethodPost, "/panel/api/omni/provider-models/probe", `{}`,
		nil, map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 provider 应 400，得到 %d", rec.Code)
	}
	if called {
		t.Errorf("缺 provider 不应调用引擎闭包")
	}
}

// TestOmniRefreshModelsRequiresAdmin 刷新是写操作（改缓存），匿名/非管理员必须被拦。
func TestOmniRefreshModelsRequiresAdmin(t *testing.T) {
	p, _ := mustPanel(t, Config{
		OmniRefreshModels: func(string) (any, error) { return map[string]any{"models": []any{}}, nil },
	})
	rec := do(p, http.MethodPost, "/panel/api/omni/provider-models/refresh", `{"provider":"up"}`, nil, nil)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("匿名刷新应被拒绝，得到 %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestOmniRefreshModelsPassesProvider 管理员刷新：供应商名透传，结果包在 data 里。
func TestOmniRefreshModelsPassesProvider(t *testing.T) {
	var gotName string
	p, token := mustPanel(t, Config{
		OmniRefreshModels: func(name string) (any, error) {
			gotName = name
			return map[string]any{"models": []map[string]any{{"id": "fresh"}}, "warning": "上游抖动"}, nil
		},
	})
	rec := do(p, http.MethodPost, "/panel/api/omni/provider-models/refresh", `{"provider":"up"}`,
		nil, map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d body=%s", rec.Code, rec.Body.String())
	}
	if gotName != "up" {
		t.Errorf("透传的供应商名=%q want up", gotName)
	}
	if !strings.Contains(rec.Body.String(), `"fresh"`) || !strings.Contains(rec.Body.String(), "上游抖动") {
		t.Errorf("响应缺内容: %s", rec.Body.String())
	}
}

// TestOmniProbeModelsNotConfigured 引擎未启用时返回 501 而不是 500。
func TestOmniProbeModelsNotConfigured(t *testing.T) {
	p, token := mustPanel(t, Config{})
	rec := do(p, http.MethodPost, "/panel/api/omni/provider-models/probe",
		`{"provider":{"name":"x","type":"openai"}}`, nil,
		map[string]string{"Authorization": "Bearer " + token})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("未配置应 501，得到 %d body=%s", rec.Code, rec.Body.String())
	}
}
