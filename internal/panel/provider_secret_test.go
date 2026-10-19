package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// TestProviderSecretAdminOnly 钉住「管理员查看完整密钥」端点的权限与形态。
//
// 背景（用户要求）：上游供应商配置页里 key 一律掩码回显（viewer 也能 GET 配置，
// 明文回显 = 任何登录者都能抄走上游密钥），但管理员需要一个能看回完整 key 的入口
// ——排查「配置的到底是哪把 key」时掩码的前 6 后 4 不够用。折中方案是独立端点：
// 只在管理员显式点击「查看完整」时按供应商取一次明文，no-store 不落缓存，服务端
// 审计留痕（日志里只有供应商名，不落密钥内容）。
//
// 钉四件事：
//  1. 未登录 401 / viewer 403：明文只对管理员开放；
//  2. admin 精确回显明文（不是掩码），且只含白名单字段（zen/go 池没配就不出现）；
//  3. Cache-Control: no-store（密钥不许进任何缓存）；
//  4. 没配密钥字段 / 不存在的供应商 / 缺 name 一律 4xx，不空手 200。
func TestProviderSecretAdminOnly(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "viewer", panelauth.RoleViewer, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")

	const atriaKey = "atr_PK_real_key_1234567890"
	const poolA = "atr_5_real_pool_a_1234567890"
	const poolB = "atr_Oo_real_pool_b_1234567890"
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{"providers": []any{
			map[string]any{
				"name":     "atria",
				"type":     "openai",
				"base_url": "https://api.atria-asi.ai/v1",
				"api_key":  atriaKey,
				"api_keys": []any{poolA, "", poolB},
				"accounts": []any{},
			},
			map[string]any{"name": "raccoon", "type": "raccoon"},
		}}, nil
	}
	admin := login(t, p, "admin", "pw123456")
	viewer := login(t, p, "viewer", "pw123456")

	// ① 未登录 → 401（p.admin 先过 auth 层）。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret?name=atria", "", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anon provider-secret = %d want 401", rec.Code)
	}
	// ② viewer → 403：掩码是 viewer 的天花板，明文只给管理员。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret?name=atria", "", viewer, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer provider-secret = %d want 403", rec.Code)
	}
	// ③ admin → 200：明文逐个精确回显 + no-store。
	rec := do(p, "GET", "/panel/api/omni/provider-secret?name=atria", "", admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin provider-secret = %d body=%s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q want no-store", cc)
	}
	var resp struct {
		OK      bool           `json:"ok"`
		Name    string         `json:"name"`
		Secrets map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if !resp.OK || resp.Name != "atria" {
		t.Fatalf("响应头字段不对：%s", rec.Body.String())
	}
	if got, _ := resp.Secrets["api_key"].(string); got != atriaKey {
		t.Fatalf("api_key = %v want 明文原值", resp.Secrets["api_key"])
	}
	pool, _ := resp.Secrets["api_keys"].([]any)
	if len(pool) != 3 || pool[0] != poolA || pool[1] != "" || pool[2] != poolB {
		t.Fatalf("api_keys 明文池不对：%v", pool)
	}
	// 白名单：没配 zen/go 就不该出现（别把整家供应商的字段都兜出去）。
	if len(resp.Secrets) != 2 {
		t.Fatalf("secrets 应只含已配置的白名单字段，得到 %v", resp.Secrets)
	}
	if strings.Contains(rec.Body.String(), "…(len=") {
		t.Fatalf("响应里出现了掩码串：%s", rec.Body.String())
	}
	// ④ 没配密钥字段的供应商 → 404，别回空 secrets 让前端以为能看。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret?name=raccoon", "", admin, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("no-secret provider = %d want 404", rec.Code)
	}
	// ⑤ 不存在的供应商 → 404。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret?name=nope", "", admin, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown provider = %d want 404", rec.Code)
	}
	// ⑥ 缺 name → 400。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret", "", admin, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing name = %d want 400", rec.Code)
	}
	// ⑦ 前端使用 ?provider=，必须与 ?name= 等价。
	if rec := do(p, "GET", "/panel/api/omni/provider-secret?provider=atria", "", admin, nil); rec.Code != http.StatusOK {
		t.Fatalf("provider= alias = %d want 200 body=%s", rec.Code, rec.Body.String())
	}
}
