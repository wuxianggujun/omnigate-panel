package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 未注入闭包（非 main 装配路径）时，realm_routing 接口返回 501 而非 panic。
func TestRealmRoutingEndpointNotAvailable(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	req := httptest.NewRequest("GET", "/panel/api/realm_routing", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("code=%d want 501", rec.Code)
	}
}

// GET 透传注入的配置；POST 原样把 body 交给 SaveRealmRouting 闭包。
func TestRealmRoutingRoundTrip(t *testing.T) {
	var saved []byte
	p := New(Config{
		Version: "test", APIKey: "k",
		LoadRealmRouting: func() (any, error) {
			return map[string]any{
				"order":  []string{"global", "cn"},
				"prefer": map[string]string{"deepseek-*": "global"},
			}, nil
		},
		SaveRealmRouting: func(raw []byte) ([]string, error) {
			saved = append([]byte(nil), raw...)
			return nil, nil
		},
	})

	req := httptest.NewRequest("GET", "/panel/api/realm_routing", nil)
	req.Header.Set("Authorization", "Bearer k")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v", err)
	}
	rr, ok := resp["realm_routing"].(map[string]any)
	if !ok {
		t.Fatalf("realm_routing type=%T", resp["realm_routing"])
	}
	if o, _ := rr["order"].([]any); len(o) != 2 || o[0] != "global" {
		t.Fatalf("order=%v", rr["order"])
	}

	body := strings.NewReader(`{"order":["cn","global"],"prefer":{"glm-*":"cn"}}`)
	req = httptest.NewRequest("POST", "/panel/api/realm_routing", body)
	req.Header.Set("Authorization", "Bearer k")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("POST code=%d body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(string(saved), "glm-*") {
		t.Fatalf("saved=%s", saved)
	}
}
