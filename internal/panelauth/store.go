// store.go 面板账号集合 + 服务端会话 + 登录失败限流。
//
// 设计要点：
//   - 账号集合是"纯数据"（[]User），落盘在 config.json 的 panel_auth.users；
//     Validate/Upsert/Delete/SetPassword 都是无副作用的纯函数，便于单测与
//     由调用方（面板 / 命令行）复用计算，再统一交给 SavePanelAuth 落盘。
//   - Store 只负责"鉴权运行时"：校验密码、发/查/销会话、失败限流。它从配置
//     重建（Reconfigure），不直接写盘——写盘由 main 的 SavePanelAuth 闭包完成，
//     保存成功后回读配置再 Reconfigure，保证内存与磁盘一致。
package panelauth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Role 面板角色：admin 可读写，viewer 只读。
type Role string

const (
	RoleAdmin  Role = "admin"
	RoleViewer Role = "viewer"
)

// ValidRole 角色是否合法。
func ValidRole(r Role) bool { return r == RoleAdmin || r == RoleViewer }

// User 一个面板账号（含密码哈希，可与 config.json 直接序列化）。
type User struct {
	Username     string `json:"username"`
	Role         Role   `json:"role"`
	PasswordHash string `json:"password_hash"`
}

// UserView 对外暴露的账号信息（不含哈希）。
type UserView struct {
	Username string `json:"username"`
	Role     Role   `json:"role"`
}

// Session 一次有效登录会话。
type Session struct {
	Username string
	Role     Role
	Expires  time.Time
}

// IsAdmin 是否管理员。
func (s Session) IsAdmin() bool { return s.Role == RoleAdmin }

// 默认参数（配置里缺省/非法时的回落值）。
const (
	DefaultSessionTTL = 72 * time.Hour
	DefaultMaxFail    = 5
	DefaultLockFor    = 15 * time.Minute
	minPasswordLen    = 6
	maxUsernameLen    = 64
)

// 纯函数校验/变更可能返回的错误。
var (
	ErrInvalid     = errors.New("invalid panel auth")
	ErrNoAdmin     = errors.New("至少需要保留一个管理员账号")
	ErrLastAdmin   = errors.New("不能删除或降级最后一个管理员账号")
	ErrUserExists  = errors.New("用户名已存在")
	ErrUserMissing = errors.New("用户名不存在")
	ErrBadUsername = errors.New("用户名不能为空、不能含空白字符，且不超过 64 字符")
	ErrBadRole     = errors.New("角色只能是 admin 或 viewer")
	ErrWeakPass    = errors.New("密码至少 6 个字符")
)

// 鉴权运行时错误。
var (
	ErrInvalidCredentials = errors.New("用户名或密码不正确")
	ErrLockedOut          = errors.New("尝试次数过多，请稍后再试")
)

// ---------------------------------------------------------------------------
// 纯函数：账号集合的校验与变更
// ---------------------------------------------------------------------------

// ValidateUsers 校验账号集合：用户名唯一/合法、角色合法、哈希非空；非空集合
// 至少保留一个管理员。空集合合法（表示"尚未配置面板账号"）。
func ValidateUsers(users []User) error {
	seen := make(map[string]bool, len(users))
	admins := 0
	for _, u := range users {
		if !validUsername(u.Username) {
			return ErrBadUsername
		}
		if seen[u.Username] {
			return fmt.Errorf("%w: 用户名重复 %q", ErrInvalid, u.Username)
		}
		seen[u.Username] = true
		if !ValidRole(u.Role) {
			return ErrBadRole
		}
		if strings.TrimSpace(u.PasswordHash) == "" {
			return fmt.Errorf("%w: 账号 %q 缺少密码", ErrInvalid, u.Username)
		}
		if u.Role == RoleAdmin {
			admins++
		}
	}
	if len(users) > 0 && admins == 0 {
		return ErrNoAdmin
	}
	return nil
}

func validUsername(s string) bool {
	if s == "" || len(s) > maxUsernameLen {
		return false
	}
	return strings.TrimSpace(s) == s && !strings.ContainsAny(s, " \t\r\n")
}

// FindUser 返回用户名对应的账号与下标；不存在时 ok=false。
func FindUser(users []User, username string) (User, int, bool) {
	for i, u := range users {
		if u.Username == username {
			return u, i, true
		}
	}
	return User{}, -1, false
}

// UpsertUser 新增或更新账号：已存在时更新角色（plainPassword 为空则保留原密码）；
// 新建时必须提供密码。返回新切片（不改动入参），并做整体校验。
func UpsertUser(users []User, username string, role Role, plainPassword string) ([]User, error) {
	if !validUsername(username) {
		return nil, ErrBadUsername
	}
	if !ValidRole(role) {
		return nil, ErrBadRole
	}
	out := append([]User(nil), users...)
	if u, i, ok := FindUser(out, username); ok {
		u.Role = role
		if plainPassword != "" {
			if err := checkPassword(plainPassword); err != nil {
				return nil, err
			}
			h, err := HashPassword(plainPassword)
			if err != nil {
				return nil, err
			}
			u.PasswordHash = h
		}
		out[i] = u
	} else {
		if err := checkPassword(plainPassword); err != nil {
			return nil, err
		}
		h, err := HashPassword(plainPassword)
		if err != nil {
			return nil, err
		}
		out = append(out, User{Username: username, Role: role, PasswordHash: h})
	}
	if err := ValidateUsers(out); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteUser 删除账号，但拒绝删空集合或删掉最后一个管理员（避免把认证退回
// 未配置的旧模式，或被意外锁死面板）。
func DeleteUser(users []User, username string) ([]User, error) {
	_, i, ok := FindUser(users, username)
	if !ok {
		return nil, ErrUserMissing
	}
	out := append([]User(nil), users[:i]...)
	out = append(out, users[i+1:]...)
	if len(out) == 0 {
		return nil, ErrLastAdmin
	}
	if err := ValidateUsers(out); err != nil {
		if errors.Is(err, ErrNoAdmin) {
			return nil, ErrLastAdmin
		}
		return nil, err
	}
	return out, nil
}

// SetPassword 重置某账号密码。
func SetPassword(users []User, username, plainPassword string) ([]User, error) {
	if err := checkPassword(plainPassword); err != nil {
		return nil, err
	}
	out := append([]User(nil), users...)
	u, i, ok := FindUser(out, username)
	if !ok {
		return nil, ErrUserMissing
	}
	h, err := HashPassword(plainPassword)
	if err != nil {
		return nil, err
	}
	u.PasswordHash = h
	out[i] = u
	return out, nil
}

func checkPassword(pw string) error {
	if len([]rune(pw)) < minPasswordLen {
		return ErrWeakPass
	}
	return nil
}

// ---------------------------------------------------------------------------
// 运行时 Store
// ---------------------------------------------------------------------------

type sessionRec struct {
	username string
	role     Role
	expires  time.Time
}

type attempt struct {
	failures int
	first    time.Time
	lockedTo time.Time
}

// Store 面板鉴权运行时：账号（只读副本）+ 会话表 + 失败限流。
type Store struct {
	mu       sync.Mutex
	users    map[string]User
	order    []string
	sessions map[string]sessionRec
	attempts map[string]*attempt
	ttl      time.Duration
	maxFail  int
	lockFor  time.Duration
	now      func() time.Time
}

// New 用账号集合构造 Store。ttl<=0 回落 72h，maxFail<=0 回落 5，lockFor<=0 回落 15m。
func New(users []User, ttl time.Duration, maxFail int, lockFor time.Duration) *Store {
	s := &Store{now: time.Now}
	s.Reconfigure(users, ttl, maxFail, lockFor)
	return s
}

// Reconfigure 用新账号集合/参数热替换（保存配置后调用）。会话表保留：已删除的
// 用户会在下一次 Authenticate 时失效。
func (s *Store) Reconfigure(users []User, ttl time.Duration, maxFail int, lockFor time.Duration) {
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	if maxFail <= 0 {
		maxFail = DefaultMaxFail
	}
	if lockFor <= 0 {
		lockFor = DefaultLockFor
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users = make(map[string]User, len(users))
	s.order = make([]string, 0, len(users))
	for _, u := range users {
		s.users[u.Username] = u
		s.order = append(s.order, u.Username)
	}
	if s.sessions == nil {
		s.sessions = map[string]sessionRec{}
	}
	if s.attempts == nil {
		s.attempts = map[string]*attempt{}
	}
	s.ttl = ttl
	s.maxFail = maxFail
	s.lockFor = lockFor
}

// HasUsers 是否已配置面板账号（false = 未配置；面板鉴权已与网关 api_key 彻底解耦，
// 未配置账号时 /panel/api/* 一律 401，登录端点返回 panel_auth_not_configured）。
func (s *Store) HasUsers() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.users) > 0
}

// UsersView 返回账号列表（无哈希，稳定顺序）。
func (s *Store) UsersView() []UserView {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]UserView, 0, len(s.order))
	for _, name := range s.order {
		if u, ok := s.users[name]; ok {
			out = append(out, UserView{Username: u.Username, Role: u.Role})
		}
	}
	return out
}

// Login 校验凭据并发会话。clientKey 用于失败限流（通常取客户端 IP）。
// 失败一律返回 ErrInvalidCredentials（不区分用户不存在/密码错），超限返回 ErrLockedOut。
func (s *Store) Login(username, password, clientKey string) (Session, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	u, err := s.verifyLocked(username, password, clientKey, now)
	if err != nil {
		return Session{}, "", err
	}
	token, err := newToken()
	if err != nil {
		return Session{}, "", err
	}
	exp := now.Add(s.ttl)
	s.sessions[token] = sessionRec{username: u.Username, role: u.Role, expires: exp}
	return Session{Username: u.Username, Role: u.Role, Expires: exp}, token, nil
}

// Verify 仅校验凭据（与 Login 同限流口径），不创建会话。用于"改自己密码前
// 校验当前密码"——避免每次改密都在会话表里留下一个用不上的令牌，也让失败
// 计数语义与登录完全一致（不能借改密接口绕过登录限流）。
func (s *Store) Verify(username, password, clientKey string) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purgeLocked(now)
	return s.verifyLocked(username, password, clientKey, now)
}

// verifyLocked 在持锁状态下校验凭据并做失败计数（Login/Verify 共用）。
// 成功返回账号并清零该用户/来源的失败计数；失败按限流策略记账。
func (s *Store) verifyLocked(username, password, clientKey string, now time.Time) (User, error) {
	uk := "u:" + username
	ik := "ip:" + clientKey
	if wait, locked := s.lockedFor(uk, now); locked {
		return User{}, &LockedError{RetryAfter: wait}
	}
	if wait, locked := s.lockedFor(ik, now); locked {
		return User{}, &LockedError{RetryAfter: wait}
	}

	u, ok := s.users[username]
	if !ok {
		dummyVerify(password)
		s.recordFailure(uk, now)
		s.recordFailure(ik, now)
		return User{}, ErrInvalidCredentials
	}
	if !VerifyPassword(u.PasswordHash, password) {
		s.recordFailure(uk, now)
		s.recordFailure(ik, now)
		return User{}, ErrInvalidCredentials
	}
	delete(s.attempts, uk)
	delete(s.attempts, ik)
	return u, nil
}

// Authenticate 校验会话令牌并在有效时滑动续期（expires = now + ttl）。
// 角色以当前账号集合为准（改角色即时生效）；账号被删除则会话立即失效。
func (s *Store) Authenticate(token string) (Session, bool) {
	if token == "" {
		return Session{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	rec, ok := s.sessions[token]
	if !ok || now.After(rec.expires) {
		delete(s.sessions, token)
		return Session{}, false
	}
	u, ok := s.users[rec.username]
	if !ok {
		delete(s.sessions, token)
		return Session{}, false
	}
	exp := now.Add(s.ttl)
	s.sessions[token] = sessionRec{username: u.Username, role: u.Role, expires: exp}
	return Session{Username: u.Username, Role: u.Role, Expires: exp}, true
}

// Logout 注销会话令牌（幂等）。
func (s *Store) Logout(token string) {
	if token == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// SessionTTL 当前会话有效期（供 Set-Cookie Max-Age）。
func (s *Store) SessionTTL() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ttl
}

// --- 内部：限流 ---

// LockedError 携带剩余锁定时间。
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string { return ErrLockedOut.Error() }
func (e *LockedError) Unwrap() error { return ErrLockedOut }

func (s *Store) lockedFor(key string, now time.Time) (time.Duration, bool) {
	a, ok := s.attempts[key]
	if !ok || now.After(a.lockedTo) || a.lockedTo.IsZero() {
		return 0, false
	}
	return a.lockedTo.Sub(now), true
}

func (s *Store) recordFailure(key string, now time.Time) {
	a := s.attempts[key]
	if a == nil {
		a = &attempt{}
		s.attempts[key] = a
	}
	// 超过两倍锁定时长的旧窗口视为过期，重新计数。
	if !a.first.IsZero() && now.Sub(a.first) > 2*s.lockFor {
		a.failures = 0
	}
	if a.failures == 0 {
		a.first = now
	}
	a.failures++
	if a.failures >= s.maxFail {
		a.lockedTo = now.Add(s.lockFor)
	}
}

func (s *Store) purgeLocked(now time.Time) {
	for k, a := range s.attempts {
		if !a.lockedTo.IsZero() && now.After(a.lockedTo) || a.failures == 0 && now.Sub(a.first) > 2*s.lockFor {
			delete(s.attempts, k)
		}
	}
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("panelauth: gen session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
