package panel

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 统一账号池：GET /panel/api/omni/accounts 透传注入闭包的归一化数据，并把
// ?balance=1 转成 includeBalance=true。
func TestOmniAccountsEndpoint(t *testing.T) {
	calls := 0
	var gotBalance bool
	p, tok := mustPanel(t, Config{
		Version: "test",
		OmniAccounts: func(withBalance bool) (any, error) {
			calls++
			gotBalance = withBalance
			return map[string]any{"providers": []any{
				map[string]any{"name": "raccoon", "type": "raccoon", "accounts": []any{
					map[string]any{"label": "a1", "has_token": true},
				}},
			}}, nil
		},
	})
	req := httptest.NewRequest("GET", "/panel/api/omni/accounts?balance=1", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !gotBalance {
		t.Errorf("balance=1 未透传为 includeBalance=true")
	}
	if calls != 1 {
		t.Errorf("OmniAccounts calls=%d want 1", calls)
	}
	var resp struct {
		OK   bool `json:"ok"`
		Data struct {
			Providers []struct {
				Name     string `json:"name"`
				Type     string `json:"type"`
				Accounts []struct {
					Label    string `json:"label"`
					HasToken bool   `json:"has_token"`
				} `json:"accounts"`
			} `json:"providers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.OK || len(resp.Data.Providers) != 1 {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	if resp.Data.Providers[0].Name != "raccoon" || len(resp.Data.Providers[0].Accounts) != 1 {
		t.Errorf("providers=%+v", resp.Data.Providers)
	}
}

// OmniGate 未启用（闭包为 nil）→ 501。
func TestOmniAccountsNotEnabled(t *testing.T) {
	p, tok := mustPanel(t, Config{Version: "test"})
	req := httptest.NewRequest("GET", "/panel/api/omni/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 501 {
		t.Fatalf("status=%d want 501", rec.Code)
	}
}

// 单账号签到 / 移除：转发 provider+label；缺字段返回 400。
func TestOmniAccountCheckinRemove(t *testing.T) {
	var gotProv, gotLabel string
	p, tok := mustPanel(t, Config{
		Version: "test",
		OmniCheckin: func(prov, label string) (any, error) {
			gotProv, gotLabel = prov, label
			return map[string]any{"ok": true}, nil
		},
		OmniRemove: func(prov, label string) (any, error) {
			gotProv, gotLabel = prov, label
			return map[string]any{"ok": true, "removed": true}, nil
		},
	})
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/panel/api/omni/account/checkin", `{"provider":"raccoon","label":"a1"}`); rec.Code != 200 {
		t.Fatalf("checkin status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotProv != "raccoon" || gotLabel != "a1" {
		t.Errorf("checkin got %q/%q", gotProv, gotLabel)
	}
	if rec := post("/panel/api/omni/account/remove", `{"provider":"raccoon","label":"a2"}`); rec.Code != 200 {
		t.Fatalf("remove status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotProv != "raccoon" || gotLabel != "a2" {
		t.Errorf("remove got %q/%q", gotProv, gotLabel)
	}
	if rec := post("/panel/api/omni/account/checkin", `{"label":"a1"}`); rec.Code != 400 {
		t.Errorf("missing provider status=%d want 400", rec.Code)
	}
	if rec := post("/panel/api/omni/account/remove", `{"provider":"raccoon"}`); rec.Code != 400 {
		t.Errorf("missing label status=%d want 400", rec.Code)
	}
	if rec := post("/panel/api/omni/account/checkin", `not-json`); rec.Code != 400 {
		t.Errorf("bad body status=%d want 400", rec.Code)
	}
}

// 一键登录：POST /panel/api/omni/account/login 转发 provider/email/password，
// 返回注入闭包给出的账号身份 + 积分；缺 provider 返回 400。
func TestOmniAccountLogin(t *testing.T) {
	var gotProv, gotEmail, gotPwd string
	p, tok := mustPanel(t, Config{
		Version: "test",
		OmniLogin: func(prov, email, password string) (any, error) {
			gotProv, gotEmail, gotPwd = prov, email, password
			return map[string]any{"name": "张三", "email": email, "available": 6600}, nil
		},
	})
	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/panel/api/omni/account/login", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	rec := post(`{"provider":"runable","email":"a@b.com","password":"pw"}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if gotProv != "runable" || gotEmail != "a@b.com" || gotPwd != "pw" {
		t.Errorf("forwarded %q/%q/%q", gotProv, gotEmail, gotPwd)
	}
	if !strings.Contains(rec.Body.String(), `"available":6600`) {
		t.Errorf("body missing credits: %s", rec.Body.String())
	}
	if rec := post(`{"email":"a@b.com"}`); rec.Code != 400 {
		t.Errorf("missing provider status=%d want 400", rec.Code)
	}
	if rec := post(`not-json`); rec.Code != 400 {
		t.Errorf("bad body status=%d want 400", rec.Code)
	}
}

// OmniGate 未启用（OmniLogin 为 nil）→ 一键登录返回 501。
func TestOmniAccountLoginNotEnabled(t *testing.T) {
	p, tok := mustPanel(t, Config{Version: "test"})
	req := httptest.NewRequest("POST", "/panel/api/omni/account/login",
		strings.NewReader(`{"provider":"runable","email":"a@b.com","password":"pw"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != 501 {
		t.Fatalf("status=%d want 501", rec.Code)
	}
}

// 统一账号池端点未鉴权一律 401。
func TestOmniAccountEndpointsUnauthorized(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k", OmniAccounts: func(bool) (any, error) { return nil, nil }})
	for _, c := range []struct{ method, path string }{
		{"GET", "/panel/api/omni/accounts"},
		{"POST", "/panel/api/omni/account/checkin"},
		{"POST", "/panel/api/omni/account/remove"},
		{"POST", "/panel/api/omni/account/login"},
	} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))
		if rec.Code != 401 {
			t.Errorf("%s %s status=%d want 401", c.method, c.path, rec.Code)
		}
	}
}
