package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

func accountCfgJSON(label string) map[string]any {
	accts := []any{}
	if label != "" {
		accts = append(accts, map[string]any{"label": label, "cookie": "old"})
	}
	return map[string]any{"providers": []any{
		map[string]any{"name": "raccoon", "type": "raccoon", "accounts": accts},
	}}
}

// 动态 schema 表单提交的所有账号字段都必须落进配置，而不是只保留 cookie。
func TestAccountAddPersistsAllSchemaFields(t *testing.T) {
	p, tok := mustPanel(t, Config{Version: "test"})
	cfg := accountCfgJSON("")
	var saved []byte
	p.cfg.LoadOmniConfig = func() (any, error) { return cfg, nil }
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) { saved = append([]byte(nil), raw...); return nil, nil }

	body := `{"provider":"raccoon","label":"a1","access_token":"at-123","refresh_token":"rt-456","uid":"u-1"}`
	rec := servePanel(p, newJSONRequest(http.MethodPost, "/panel/api/omni/account/add", body, tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("add = %d body=%s", rec.Code, rec.Body.String())
	}
	if !json.Valid(saved) {
		t.Fatalf("saved config not json: %s", saved)
	}
	for _, want := range []string{"access_token", "at-123", "refresh_token", "rt-456", "uid", "u-1"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("saved config missing %q: %s", want, saved)
		}
	}
}

// 「登录校验并添加」必须在 label 存在时直接保存，不能只返回校验结果。
func TestAccountLoginSavesWhenLabelPresent(t *testing.T) {
	p, tok := mustPanel(t, Config{
		Version: "test",
		OmniLogin: func(provider, email, password, cookie string) (any, error) {
			return map[string]any{"name": "张三"}, nil
		},
	})
	cfg := accountCfgJSON("")
	var saved []byte
	p.cfg.LoadOmniConfig = func() (any, error) { return cfg, nil }
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) { saved = append([]byte(nil), raw...); return nil, nil }

	body := `{"provider":"raccoon","label":"r1","cookie":"session=abc"}`
	rec := servePanel(p, newJSONRequest(http.MethodPost, "/panel/api/omni/account/login", body, tok))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(saved), "session=abc") {
		t.Fatalf("login 未保存会话: %s", saved)
	}
}

// 普通用户不能覆盖管理员维护的无主账号。
func TestAccountAddRejectsSharedOverwriteForUser(t *testing.T) {
	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	p, _ := newAuthPanel(t, users, "test-key")
	cfg := accountCfgJSON("shared")
	p.cfg.LoadOmniConfig = func() (any, error) { return cfg, nil }
	p.cfg.SaveOmniConfig = func(raw []byte) ([]string, error) { return nil, nil }
	alice := login(t, p, "alice", "pw123456")

	body := `{"provider":"raccoon","label":"shared","cookie":"stolen"}`
	req := newJSONRequest(http.MethodPost, "/panel/api/omni/account/add", body, "")
	req.AddCookie(alice)
	rec := servePanel(p, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("shared overwrite = %d want 409 body=%s", rec.Code, rec.Body.String())
	}
}

func newJSONRequest(method, path, body, token string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req
}

func servePanel(p *Panel, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	return rec
}
