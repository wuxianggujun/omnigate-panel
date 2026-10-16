package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// TestOmniKeyHealthEndpoint 钉住「上游密钥健康」端点的权限与形态。
//
// 背景：密钥池的自动禁用（额度耗尽）状态由内核轮换层记账，面板要能看到
// 「谁被禁了、连败几次、何时自动清理」。数据是掩码形态的只读视图，故与
// 「上游供应商」配置页同权限（admin/viewer），而不是「查看完整密钥」那种
// admin-only。钉四件事：
//  1. 未登录 401；viewer 可看（200）；
//  2. 响应 no-store（状态随每次请求变化，缓存读到的是旧世界）；
//  3. payload 原样透传（providers / cleanup_strikes / cleanup_days）；
//  4. 未接入 OmniGate（回调为 nil）→ 501；回调报错 → 502。
func TestOmniKeyHealthEndpoint(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "viewer", panelauth.RoleViewer, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")

	// ① 未登录 → 401（鉴权层先拦，压根到不了 handler）。
	if rec := do(p, "GET", "/panel/api/omni/key-health", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon key-health = %d want 401", rec.Code)
	}
	viewer := login(t, p, "viewer", "pw123456")
	admin := login(t, p, "admin", "pw123456")

	// ② 已登录但 OmniGate 未接入 → 501（不是 500，也不是空 200）。
	if rec := do(p, "GET", "/panel/api/omni/key-health", "", viewer, nil); rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil-callback key-health = %d want 501", rec.Code)
	}

	// ③ 接入后 viewer 可看：payload 原样透传 + no-store。
	const masked = "atr_OoG9tEee…sDCA"
	p.cfg.OmniKeyHealth = func() (any, error) {
		return map[string]any{
			"providers": []any{
				map[string]any{"name": "atria", "keys": []any{
					map[string]any{"key": masked, "disabled": true, "reason": "quota",
						"strikes": 3, "until": 1760000000, "first_seen": 1759000000, "cleanup_at": 1760500000},
					map[string]any{"key": "atr_PKQzVd_Q…5OyU"},
				}},
			},
			"cleanup_strikes": 3,
			"cleanup_days":    7,
		}, nil
	}
	rec := do(p, "GET", "/panel/api/omni/key-health", "", viewer, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer key-health = %d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q want no-store", cc)
	}
	var resp struct {
		OK        bool `json:"ok"`
		Providers []struct {
			Name string `json:"name"`
			Keys []struct {
				Key      string `json:"key"`
				Disabled bool   `json:"disabled"`
				Strikes  int    `json:"strikes"`
			} `json:"keys"`
		} `json:"providers"`
		CleanupStrikes int `json:"cleanup_strikes"`
		CleanupDays    int `json:"cleanup_days"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(resp.Providers) != 1 || resp.Providers[0].Name != "atria" || len(resp.Providers[0].Keys) != 2 {
		t.Fatalf("providers 形态不对：%s", rec.Body.String())
	}
	if k := resp.Providers[0].Keys[0]; !k.Disabled || k.Strikes != 3 {
		t.Fatalf("首行应为禁用 + 连败 3 次：%+v", k)
	}
	if resp.CleanupStrikes != 3 || resp.CleanupDays != 7 {
		t.Fatalf("清理阈值透传不对：strikes=%d days=%d", resp.CleanupStrikes, resp.CleanupDays)
	}

	// ④ admin 同样 200（admin ⊇ viewer 权限）。
	if rec := do(p, "GET", "/panel/api/omni/key-health", "", admin, nil); rec.Code != http.StatusOK {
		t.Fatalf("admin key-health = %d want 200", rec.Code)
	}

	// ⑤ 回调报错 → 502（读不到就说读不到，别装空）。
	p.cfg.OmniKeyHealth = func() (any, error) {
		return nil, errors.New("key health unavailable")
	}
	if rec := do(p, "GET", "/panel/api/omni/key-health", "", admin, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("error-callback key-health = %d want 502", rec.Code)
	}
}
