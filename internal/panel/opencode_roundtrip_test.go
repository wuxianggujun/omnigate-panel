package panel

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// TestOpencodeKeysRoundTripThroughPanel 走完整的 HTTP 来回，钉住 opencode 两个密钥池
// 在「打开面板 → 保存」这个动作里不会丢。
//
// 为什么必须走 HTTP 而不是只测 maskSecrets：这条链有两端，任何一端断了都丢密钥，
// 而两端分别属于不同的层——
//  1. GET /panel/api/omni/config 出去的必须是掩码（面板是多人共用的只读接口，
//     viewer 也能 GET；明文回显 = 任何人登录都能抄走付费密钥）。
//  2. 前端把**整份**配置原样 POST 回来（只打开过编辑器、没动密钥也会回传掩码串），
//     落盘前必须还原成真密钥。
//
// 第 2 条尤其隐蔽：保存返回 200、界面显示「已保存并热生效」，直到下次真发请求才
// 发现 Invalid credential——而那时密钥已经被掩码串覆盖掉了，原值不可恢复。
func TestOpencodeKeysRoundTripThroughPanel(t *testing.T) {
	users := []panelauth.User{pwUser(t, "admin", panelauth.RoleAdmin, "pw123456")}
	p, _ := newAuthPanel(t, users, "test-key")

	const zenKey = "sk-zen-real-key-1234567890"
	const goKey = "sk-go-real-key-1234567890"
	saved := ""
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{"providers": []any{
			map[string]any{
				"name":     "opencode",
				"type":     "opencode",
				"zen_keys": []any{zenKey},
				"go_keys":  []any{goKey, ""},
			},
		}}, nil
	}
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) {
		saved = string(raw)
		return nil, nil
	}

	admin := login(t, p, "admin", "pw123456")

	// ① GET：只给掩码，明文绝不出门。
	rec := do(p, "GET", "/panel/api/omni/config", "", admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET = %d body=%s", rec.Code, rec.Body.String())
	}
	view := rec.Body.String()
	if strings.Contains(view, zenKey) || strings.Contains(view, goKey) {
		t.Fatalf("GET 回显了明文密钥：%s", view)
	}
	if n := strings.Count(view, "…(len="); n < 2 {
		t.Fatalf("两个池都该以掩码回显（掩码数 %d）：%s", n, view)
	}

	// 前端只保留 payload.config 这一层（见 app.js 的 loadOmniConfig）。
	var envelope struct {
		Config json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("解析 GET 响应: %v", err)
	}
	if len(envelope.Config) == 0 {
		t.Fatal("GET 响应缺少 config 字段")
	}

	// ② 原样回传（模拟「只打开过编辑器就点保存」）：落盘必须是真密钥。
	rec = do(p, "POST", "/panel/api/omni/config", string(envelope.Config), admin, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(saved, zenKey) {
		t.Fatalf("保存后 zen 密钥丢失/被写成掩码：%s", saved)
	}
	if !strings.Contains(saved, goKey) {
		t.Fatalf("保存后 go 密钥丢失/被写成掩码：%s", saved)
	}
	if strings.Contains(saved, "…(len=") {
		t.Fatalf("掩码串被写进了配置：%s", saved)
	}
}

// 非管理员不能保存（密钥池和其它凭证同权限级别），并且被拒时不写盘。
func TestOpencodeKeysSaveRequiresAdmin(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "viewer", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	saved := ""
	p.cfg.LoadOmniConfig = func() (any, error) {
		return map[string]any{"providers": []any{map[string]any{"name": "opencode", "type": "opencode"}}}, nil
	}
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) {
		saved = string(raw)
		return nil, nil
	}

	viewer := login(t, p, "viewer", "pw123456")
	rec := do(p, "POST", "/panel/api/omni/config",
		`{"providers":[{"name":"opencode","type":"opencode","go_keys":["sk-go"]}]}`, viewer, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("普通用户不该能保存供应商配置，body=%s", rec.Body.String())
	}
	if saved != "" {
		t.Fatalf("被拒的保存不该写盘：%s", saved)
	}
}
