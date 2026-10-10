package main

import (
	"os"
	"path/filepath"
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

// 命令行设密往返：写文件 → 重新 Load → 账号与哈希可用；保留文件中其它字段与账号。
func TestSetAdminPasswordRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if _, err := WriteDefault(path); err != nil {
		t.Fatal(err)
	}
	// 记下一个非 panel_auth 字段，验证写入不丢字段。
	before, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := SetAdminPassword(path, "admin", panelauth.RoleAdmin, "Sup3rSecret!"); err != nil {
		t.Fatalf("SetAdminPassword: %v", err)
	}
	// 再加一个 viewer。
	if err := SetAdminPassword(path, "guest", panelauth.RoleViewer, "viewerpw1"); err != nil {
		t.Fatalf("SetAdminPassword guest: %v", err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if c.APIKey != before.APIKey {
		t.Errorf("api_key changed by panel auth write (%q → %q)", before.APIKey, c.APIKey)
	}
	if c.Listen != before.Listen {
		t.Errorf("listen changed by panel auth write (%q → %q)", before.Listen, c.Listen)
	}
	if len(c.PanelAuth.Users) != 2 {
		t.Fatalf("users=%d want 2", len(c.PanelAuth.Users))
	}
	admin, _, ok := panelauth.FindUser(c.PanelAuth.Users, "admin")
	if !ok || !panelauth.VerifyPassword(admin.PasswordHash, "Sup3rSecret!") {
		t.Fatal("admin password not persisted/verifiable")
	}
	guest, _, ok := panelauth.FindUser(c.PanelAuth.Users, "guest")
	if !ok || guest.Role != panelauth.RoleViewer || !panelauth.VerifyPassword(guest.PasswordHash, "viewerpw1") {
		t.Fatal("guest not persisted correctly")
	}

	// 重置 admin 密码，不改账号数量。
	if err := SetAdminPassword(path, "admin", panelauth.RoleAdmin, "Rotated!Pass9"); err != nil {
		t.Fatal(err)
	}
	c, _ = Load(path)
	if len(c.PanelAuth.Users) != 2 {
		t.Fatalf("users=%d want 2 after reset", len(c.PanelAuth.Users))
	}
	admin, _, _ = panelauth.FindUser(c.PanelAuth.Users, "admin")
	if !panelauth.VerifyPassword(admin.PasswordHash, "Rotated!Pass9") {
		t.Fatal("reset password not applied")
	}
	if panelauth.VerifyPassword(admin.PasswordHash, "Sup3rSecret!") {
		t.Fatal("old password still valid after reset")
	}

	// 文件权限应收紧为 0600（Linux）。
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm()&0o077 != 0 {
		t.Logf("note: config perms = %v (expected 0600 on Linux)", fi.Mode().Perm())
	}
}
