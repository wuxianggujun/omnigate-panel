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

// TestModelMappingAdminToggle 覆盖停用/启用端点：引流链是硬规则（别名优先于
// routing.prefer），配上就处处盖过 prefers。被强推到某个供应商时，运维需要的是
// 「先停用、别删配置」——这就是下面要钉的东西。
func TestModelMappingAdminToggle(t *testing.T) {
	s := fingerprintTestServer(t)

	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.handleModelMapping(w, httptest.NewRequest(http.MethodPost, "/admin/model-mapping", strings.NewReader(body)))
		return w
	}
	disabledSet := func(w *httptest.ResponseRecorder) map[string]bool {
		var body struct {
			Disabled map[string]bool `json:"disabled"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("body not json: %v (%s)", err, w.Body.String())
		}
		return body.Disabled
	}

	// 建一条单目标规则。
	w := post(`{"alias":"gpt-4o","target":"gw/gemini-pro"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST status = %d", w.Code)
	}
	if len(disabledSet(w)) != 0 {
		t.Fatal("新规则默认应为启用")
	}

	// 停用：只带 alias + enabled=false，不带 targets。
	w = post(`{"alias":"gpt-4o","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("停用 status = %d body=%s", w.Code, w.Body.String())
	}
	if !disabledSet(w)["gpt-4o"] {
		t.Fatalf("停用未生效: %s", w.Body.String())
	}
	// 配置仍在（区别于删除）。
	if m := modelMapBody(t, w); m["gpt-4o"] != "gw/gemini-pro" {
		t.Fatalf("停用后配置被丢了: %+v", m)
	}

	// 重新启用。
	w = post(`{"alias":"gpt-4o","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("启用 status = %d", w.Code)
	}
	if disabledSet(w)["gpt-4o"] {
		t.Fatal("启用未生效")
	}

	// 对不存在的别名只改开关 → 非 200（不能凭空造规则）。
	if w = post(`{"alias":"ghost","enabled":false}`); w.Code == http.StatusOK {
		t.Fatal("停用不存在的别名应失败")
	}

	// 保存目标链时同时带 enabled=false = 存一份先不生效的配置。
	w = post(`{"alias":"later","target":"gw/x","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("带 enabled 保存 status = %d", w.Code)
	}
	if !disabledSet(w)["later"] {
		t.Fatal("带 enabled=false 保存应立即停用")
	}
	if m := modelMapBody(t, w); m["later"] != "gw/x" {
		t.Fatalf("目标链没存住: %+v", m)
	}

	// 删除后停用标记一并消失。
	post(`{"alias":"later","enabled":true}`)
	w = httptest.NewRecorder()
	s.handleModelMapping(w, httptest.NewRequest(http.MethodDelete, "/admin/model-mapping/later", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("DELETE status = %d", w.Code)
	}
	if len(disabledSet(w)) != 0 {
		t.Fatalf("删除后仍留着停用标记: %s", w.Body.String())
	}
}
