package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// modelMapBody decodes the shared shape of /admin/model-mapping responses.
func modelMapBody(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var body struct {
		Mappings map[string]string `json:"mappings"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	return body.Mappings
}

// TestModelMappingAdminRoundTrip 覆盖 /admin/model-mapping 的列出 / 写入 / 校验 /
// 删除（model_mapping.py 的功能等价移植）。
func TestModelMappingAdminRoundTrip(t *testing.T) {
	s := fingerprintTestServer(t)

	// 初始为空。
	w := httptest.NewRecorder()
	s.handleModelMapping(w, httptest.NewRequest(http.MethodGet, "/admin/model-mapping", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", w.Code, w.Body.String())
	}
	if m := modelMapBody(t, w); len(m) != 0 {
		t.Fatalf("expected empty mappings, got %+v", m)
	}

	// 写入。
	w = httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/admin/model-mapping",
		strings.NewReader(`{"alias":"gpt-4o","target":"gw/gemini-pro"}`))
	s.handleModelMapping(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d body=%s", w.Code, w.Body.String())
	}
	if m := modelMapBody(t, w); m["gpt-4o"] != "gw/gemini-pro" {
		t.Fatalf("mappings = %+v", m)
	}

	// alias == target 必须 400（与参考实现一致），且不落库。
	w = httptest.NewRecorder()
	r = httptest.NewRequest(http.MethodPost, "/admin/model-mapping",
		strings.NewReader(`{"alias":"same","target":"same"}`))
	s.handleModelMapping(w, r)
	if w.Code == http.StatusOK {
		t.Fatalf("alias==target should fail, got %d", w.Code)
	}

	// DELETE 带子路径。
	w = httptest.NewRecorder()
	s.handleModelMapping(w, httptest.NewRequest(http.MethodDelete, "/admin/model-mapping/gpt-4o", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d body=%s", w.Code, w.Body.String())
	}
	if m := modelMapBody(t, w); len(m) != 0 {
		t.Fatalf("mappings after delete = %+v", m)
	}

	// 再删同一个：不存在 → 非 200。
	w = httptest.NewRecorder()
	s.handleModelMapping(w, httptest.NewRequest(http.MethodDelete, "/admin/model-mapping/gpt-4o", nil))
	if w.Code == http.StatusOK {
		t.Fatalf("deleting a missing alias should fail, got %d", w.Code)
	}
}
