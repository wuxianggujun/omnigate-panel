package panelauth

import (
	"errors"
	"testing"
	"time"
)

func mkUser(t *testing.T, name string, role Role, pw string) User {
	t.Helper()
	h, err := hashWithIters(pw, 1000) // 低迭代：单测不必为每次校验付 21 万次
	if err != nil {
		t.Fatal(err)
	}
	return User{Username: name, Role: role, PasswordHash: h}
}

func TestValidateUsers(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	viewer := mkUser(t, "guest", RoleViewer, "pw123456")

	if err := ValidateUsers(nil); err != nil {
		t.Errorf("empty set should be valid (未配置态): %v", err)
	}
	if err := ValidateUsers([]User{admin, viewer}); err != nil {
		t.Errorf("valid set rejected: %v", err)
	}
	if err := ValidateUsers([]User{viewer}); !errors.Is(err, ErrNoAdmin) {
		t.Errorf("viewer-only set = %v want ErrNoAdmin", err)
	}
	if err := ValidateUsers([]User{admin, admin}); err == nil {
		t.Error("duplicate usernames accepted")
	}
	bad := admin
	bad.Role = "root"
	if err := ValidateUsers([]User{bad}); !errors.Is(err, ErrBadRole) {
		t.Errorf("bad role = %v want ErrBadRole", err)
	}
	bad2 := admin
	bad2.PasswordHash = ""
	if err := ValidateUsers([]User{bad2}); err == nil {
		t.Error("empty password hash accepted")
	}
	bad3 := admin
	bad3.Username = "has space"
	if err := ValidateUsers([]User{bad3}); !errors.Is(err, ErrBadUsername) {
		t.Errorf("username with space = %v want ErrBadUsername", err)
	}
}

func TestUpsertUser(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")

	// 新增 viewer。
	out, err := UpsertUser([]User{admin}, "guest", RoleViewer, "pw123456")
	if err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len=%d want 2", len(out))
	}
	if _, _, ok := FindUser(out, "guest"); !ok {
		t.Error("guest not added")
	}

	// 更新角色且不改密码：原哈希保持。
	before, _, _ := FindUser(out, "guest")
	out2, err := UpsertUser(out, "guest", RoleAdmin, "")
	if err != nil {
		t.Fatalf("promote guest: %v", err)
	}
	after, _, _ := FindUser(out2, "guest")
	if after.Role != RoleAdmin {
		t.Error("role not updated")
	}
	if after.PasswordHash != before.PasswordHash {
		t.Error("password changed when empty password supplied")
	}

	// 新建但密码太弱。
	if _, err := UpsertUser([]User{admin}, "x", RoleViewer, "123"); !errors.Is(err, ErrWeakPass) {
		t.Errorf("weak password = %v want ErrWeakPass", err)
	}
}

func TestDeleteUser(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	guest := mkUser(t, "guest", RoleViewer, "pw123456")

	out, err := DeleteUser([]User{admin, guest}, "guest")
	if err != nil || len(out) != 1 {
		t.Fatalf("delete guest: out=%v err=%v", out, err)
	}
	// 删最后一个管理员被拒。
	if _, err := DeleteUser([]User{admin, guest}, "admin"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("delete last admin = %v want ErrLastAdmin", err)
	}
	// 删到只剩 viewer（admin 已先删）——集合内没有管理员，应拒。
	if _, err := DeleteUser([]User{guest}, "guest"); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("delete to empty = %v want ErrLastAdmin", err)
	}
	if _, err := DeleteUser([]User{admin}, "nobody"); !errors.Is(err, ErrUserMissing) {
		t.Errorf("delete missing = %v want ErrUserMissing", err)
	}
}

func TestSetPassword(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	if _, err := SetPassword([]User{admin}, "admin", "123"); !errors.Is(err, ErrWeakPass) {
		t.Errorf("weak = %v want ErrWeakPass", err)
	}
	out, err := SetPassword([]User{admin}, "admin", "newpassword")
	if err != nil {
		t.Fatal(err)
	}
	u, _, _ := FindUser(out, "admin")
	if !VerifyPassword(u.PasswordHash, "newpassword") {
		t.Error("new password does not verify")
	}
	if _, err := SetPassword([]User{admin}, "nobody", "newpassword"); !errors.Is(err, ErrUserMissing) {
		t.Errorf("missing = %v want ErrUserMissing", err)
	}
}

func newTestStore(t *testing.T, users []User, maxFail int, lockFor time.Duration) (*Store, *time.Time) {
	t.Helper()
	now := time.Unix(1_700_000_000, 0)
	s := New(users, time.Hour, maxFail, lockFor)
	s.now = func() time.Time { return now }
	return s, &now
}

func TestStoreLoginAndSession(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	guest := mkUser(t, "guest", RoleViewer, "pw123456")
	s, _ := newTestStore(t, []User{admin, guest}, 5, 15*time.Minute)

	sess, tok, err := s.Login("admin", "pw123456", "1.2.3.4")
	if err != nil || tok == "" {
		t.Fatalf("login: %v tok=%q", err, tok)
	}
	if !sess.IsAdmin() {
		t.Error("admin session not admin")
	}
	if got, ok := s.Authenticate(tok); !ok || got.Username != "admin" {
		t.Error("session token did not authenticate")
	}
	i, ok := s.Authenticate("bogus")
	if ok || i.Username != "" {
		t.Error("bogus token authenticated")
	}

	// 错误密码/未知用户都返回同一个错误。
	if _, _, err := s.Login("admin", "nope", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("wrong pw = %v", err)
	}
	if _, _, err := s.Login("ghost", "whatever", "1.2.3.4"); !errors.Is(err, ErrInvalidCredentials) {
		t.Errorf("unknown user = %v", err)
	}

	// 注销后失效。
	s.Logout(tok)
	if _, ok := s.Authenticate(tok); ok {
		t.Error("session still valid after logout")
	}
}

// 角色热变更 / 删除账号后，既有会话以当前账号集合为准。
func TestStoreReconfigureAffectsSession(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	boss := mkUser(t, "boss", RoleAdmin, "pw123456")
	s, _ := newTestStore(t, []User{admin, boss}, 5, time.Minute)
	_, tok, _ := s.Login("admin", "pw123456", "ip")

	// admin 降级为 viewer（boss 仍为管理员）：同一 token 立即变 viewer。
	s.Reconfigure([]User{
		{Username: "admin", Role: RoleViewer, PasswordHash: admin.PasswordHash},
		{Username: "boss", Role: RoleAdmin, PasswordHash: boss.PasswordHash},
	}, time.Hour, 5, time.Minute)
	if got, ok := s.Authenticate(tok); !ok || got.IsAdmin() {
		t.Error("role change not reflected in session")
	}

	// 删除 admin：会话立即失效。
	s.Reconfigure([]User{{Username: "boss", Role: RoleAdmin, PasswordHash: boss.PasswordHash}}, time.Hour, 5, time.Minute)
	if _, ok := s.Authenticate(tok); ok {
		t.Error("session valid after user removed")
	}
}

func TestStoreSessionExpiry(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	now := time.Unix(1_700_000_000, 0)
	s := New([]User{admin}, 30*time.Minute, 5, time.Minute)
	s.now = func() time.Time { return now }
	_, tok, err := s.Login("admin", "pw123456", "ip")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(20 * time.Minute)
	if _, ok := s.Authenticate(tok); !ok {
		t.Error("session expired too early")
	}
	// 滑动续期：再过 20 分钟仍在（因为上次访问已续到 +30）。
	now = now.Add(20 * time.Minute)
	if _, ok := s.Authenticate(tok); !ok {
		t.Error("sliding renewal did not extend session")
	}
	// 空置超过 ttl 后过期。
	now = now.Add(31 * time.Minute)
	if _, ok := s.Authenticate(tok); ok {
		t.Error("session should have expired after idle > ttl")
	}
}

func TestStoreLockout(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	s, nowp := newTestStore(t, []User{admin}, 3, 10*time.Minute)

	for i := 0; i < 3; i++ {
		if _, _, err := s.Login("admin", "bad", "1.1.1.1"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	// 第 4 次即使密码正确也应被锁定。
	_, _, err := s.Login("admin", "pw123456", "2.2.2.2")
	var le *LockedError
	if !errors.As(err, &le) {
		t.Fatalf("expected lockout, got %v", err)
	}
	if le.RetryAfter <= 0 {
		t.Error("lockout RetryAfter should be positive")
	}
	// 锁定窗口过后可再次登录。
	*nowp = nowp.Add(11 * time.Minute)
	if _, tok, err := s.Login("admin", "pw123456", "1.1.1.1"); err != nil || tok == "" {
		t.Fatalf("login after lockout window: %v", err)
	}
}

// Verify 只校验凭据、不创建会话：改密前校验当前密码不会在会话表里留令牌，
// 且失败计数与 Login 同口径（不能借改密接口绕过登录限流）。
func TestStoreVerifyNoSession(t *testing.T) {
	admin := mkUser(t, "admin", RoleAdmin, "pw123456")
	s, _ := newTestStore(t, []User{admin}, 5, time.Minute)

	u, err := s.Verify("admin", "pw123456", "ip")
	if err != nil || u.Username != "admin" {
		t.Fatalf("verify ok = %v user=%q", err, u.Username)
	}
	s.mu.Lock()
	n := len(s.sessions)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("Verify created %d session(s), want 0", n)
	}
	if _, err := s.Verify("admin", "nope", "ip"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("verify wrong = %v", err)
	}

	// 达到上限后 Verify 同样返回锁定错误（与 Login 同口径）。
	boss := mkUser(t, "boss", RoleAdmin, "pw123456")
	s2, _ := newTestStore(t, []User{boss}, 2, time.Minute)
	for i := 0; i < 2; i++ {
		_, _ = s2.Verify("boss", "bad", "ip2")
	}
	if _, err := s2.Verify("boss", "pw123456", "ip2"); err == nil {
		t.Fatal("Verify should be locked out after repeated failures")
	}
}
