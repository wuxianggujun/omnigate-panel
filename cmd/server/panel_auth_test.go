package main

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// 面板鉴权配置的归一化：会话/限流参数回落缺省；账号集合校验（用户名唯一、
// 角色合法、至少一个管理员）；空集合合法（未配置态）。
func TestPanelAuthNormalize(t *testing.T) {
	// 空集合：合法，参数回落缺省。
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatalf("empty panel_auth: %v", err)
	}
	if c.PanelAuth.SessionHours != 72 || c.PanelAuth.MaxFailures != 5 || c.PanelAuth.LockMinutes != 15 {
		t.Fatalf("defaults = %+v want 72/5/15", c.PanelAuth)
	}
	if c.PanelAuth.SessionTTL().Hours() != 72 || c.PanelAuth.MaxFail() != 5 || c.PanelAuth.LockDuration().Minutes() != 15 {
		t.Fatalf("accessors mismatch: %+v", c.PanelAuth)
	}

	hash, err := panelauth.HashPassword("pw123456")
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"panel_auth":{"session_hours":12,"max_failures":3,"lock_minutes":30,"users":[{"username":"admin","role":"admin","password_hash":"` + hash + `"}]}}`)
	c, err = ParseConfig(raw)
	if err != nil {
		t.Fatalf("parse configured: %v", err)
	}
	if c.PanelAuth.SessionHours != 12 || c.PanelAuth.MaxFailures != 3 || c.PanelAuth.LockMinutes != 30 {
		t.Fatalf("configured params = %+v", c.PanelAuth)
	}
	if len(c.PanelAuth.Users) != 1 || c.PanelAuth.Users[0].Username != "admin" {
		t.Fatalf("users = %+v", c.PanelAuth.Users)
	}

	// 只有 viewer → 无管理员，拒绝启动。
	viewerOnly := []byte(`{"panel_auth":{"users":[{"username":"v","role":"viewer","password_hash":"` + hash + `"}]}}`)
	if _, err := ParseConfig(viewerOnly); err == nil {
		t.Fatal("viewer-only account set should be rejected (no admin)")
	}
	// 非法角色。
	badRole := []byte(`{"panel_auth":{"users":[{"username":"x","role":"root","password_hash":"` + hash + `"}]}}`)
	if _, err := ParseConfig(badRole); err == nil {
		t.Fatal("bad role should be rejected")
	}
}
