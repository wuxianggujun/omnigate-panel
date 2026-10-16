// opencode_quota_test.go 钉住面板「额度」入口的网关接线：type assertion 到
// *opencode.Provider、结果携带 provider 名，明文密钥不出网关层。
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// quotaSrv 起一个假的 OpenCode 额度接口（三个窗口齐全的合法响应）。
func quotaSrv(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{` +
			`"rolling":{"status":"ok","percent":42.5,"resetsAt":"2026-10-16T18:00:00Z"},` +
			`"weekly":{"status":"ok","percent":10,"resetsAt":"2026-10-19T00:00:00Z"},` +
			`"monthly":{"status":"ok","percent":3.2,"resetsAt":"2026-11-01T00:00:00Z"}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOpencodeQuotaRoutesToProvider 额度查询按名字路由到 opencode 供应商并返回
// 掩码结果：网关层不拆 provider 的池，明文密钥也不出门。
func TestOpencodeQuotaRoutesToProvider(t *testing.T) {
	const raw = "sk-gw-quota-0123456789abcdef"
	srv := quotaSrv(t)
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{Name: "oc", Type: "opencode", GoKeys: []string{raw}, GoBaseURL: srv.URL},
	}}
	if err := cfg.Normalize(); err != nil {
		t.Fatalf("配置应合法: %v", err)
	}
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	res, err := g.OpencodeQuota(context.Background(), "oc")
	if err != nil {
		t.Fatalf("OpencodeQuota 不应报错: %v", err)
	}
	// 按面板真正收到的 JSON 契约断言（map 里的 keys 是 []opencode.KeyQuota，
	// 直接类型断言成 []interface{} 会静默拿到 nil）。
	blob, _ := json.Marshal(res)
	if strings.Contains(string(blob), raw) {
		t.Fatal("网关层返回的额度结果里出现了明文密钥")
	}
	var view struct {
		Provider string `json:"provider"`
		Keys     []struct {
			Fingerprint string `json:"fingerprint"`
			Masked      string `json:"masked"`
			Status      string `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(blob, &view); err != nil {
		t.Fatalf("额度结果应为可解析 JSON: %v", err)
	}
	if view.Provider != "oc" {
		t.Fatalf("provider = %v, want oc", view.Provider)
	}
	if len(view.Keys) != 1 {
		t.Fatalf("应返回 1 把密钥，实际 %d（raw=%s）", len(view.Keys), blob)
	}
	if view.Keys[0].Status != "ok" || view.Keys[0].Masked == "" || view.Keys[0].Fingerprint == "" {
		t.Fatalf("密钥条目应完整（status/masked/fingerprint）: %+v", view.Keys[0])
	}
}

// TestOpencodeQuotaRejectsOtherTypes 非 opencode 供应商直接拒绝：额度接口是
// opencode 专属，其它类型没有 /v1/usage 可查，不该静默返回空。
func TestOpencodeQuotaRejectsOtherTypes(t *testing.T) {
	cfg := &config.Config{Providers: []config.ProviderConfig{
		{Name: "oa", Type: "openai", APIKey: "sk-x"},
	}}
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, err := New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpencodeQuota(context.Background(), "oa"); err == nil {
		t.Fatal("openai 类供应商应被拒绝（opencode 专属接口）")
	}
	if _, err := g.OpencodeQuota(context.Background(), "no-such"); err == nil {
		t.Fatal("不存在的供应商应被拒绝")
	}
}
