package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// admin 代理放行的 kind 由描述符派生（OmniAdminKinds），不再是硬编码清单：
// 描述符声明了哪些类型走 admin 传输，面板就放行哪些 kind，其余一律 404。
// 这里注入一个自定义集合，验证「放行集合来自闭包」而非写死的三种。
func TestOmniAdminProxyKindGateFromDescriptor(t *testing.T) {
	reached := 0
	p, tok := mustPanel(t, Config{
		Version:        "test",
		OmniAdminKinds: func() []string { return []string{"raccoon", "customkind"} },
		OmniAdmin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})

	cases := []struct {
		name string
		path string
		want int
	}{
		{"描述符声明的 kind 放行", "/panel/api/omni/admin/raccoon/accounts", http.StatusOK},
		{"闭包新增的 kind 也放行", "/panel/api/omni/admin/customkind/accounts", http.StatusOK},
		{"不在放行集合的 kind 404", "/panel/api/omni/admin/trae/accounts", http.StatusNotFound},
		{"未知 kind 404", "/panel/api/omni/admin/nope/accounts", http.StatusNotFound},
		{"子路径不在白名单 404", "/panel/api/omni/admin/raccoon/evil", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := servePanel(p, req)
			if rec.Code != c.want {
				t.Fatalf("%s = %d want %d body=%s", c.path, rec.Code, c.want, rec.Body.String())
			}
		})
	}
	// 只有两条被放行的请求真的转发到了 OmniAdmin。
	if reached != 2 {
		t.Fatalf("转发次数 = %d, want 2", reached)
	}
}

// 未注入 OmniAdminKinds 时直接读内置目录（login.transport=admin 的类型）：
// 单机/测试场景仍可用，且与生产注入的派生规则一致。
func TestOmniAdminProxyFallsBackToBuiltinKinds(t *testing.T) {
	p, tok := mustPanel(t, Config{
		Version: "test",
		OmniAdmin: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}),
	})
	for _, kind := range []string{"raccoon", "trae", "geminiweb"} {
		req := httptest.NewRequest(http.MethodGet, "/panel/api/omni/admin/"+kind+"/accounts", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := servePanel(p, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("回落 kind %q = %d want 200 body=%s", kind, rec.Code, rec.Body.String())
		}
	}
	// 内置集合之外仍拒绝。
	req := httptest.NewRequest(http.MethodGet, "/panel/api/omni/admin/runable/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	if rec := servePanel(p, req); rec.Code != http.StatusNotFound {
		t.Fatalf("runable 不该被 admin 代理放行，得到 %d", rec.Code)
	}
}
