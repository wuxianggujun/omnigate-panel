package gateway

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
)

// 会话健康台账。
//
// runable 的会话 Cookie 由用户手动从浏览器粘贴，会静默过期（better-auth 会话约
// 30 天）。网关在能拿到会话真值的两个时机记账：签到/浏览器登录校验（FetchSession
// 成功=校验通过，失败=已失效），以及聊天请求被上游回 401（会话已过期）。面板据此
// 提前预警「Cookie 可能过期，请重新粘贴」，而不是等用户聊天失败才发现。
//
// 纯进程内状态：会话状态会随下一次校验刷新，重启后重新观察即可（不落盘）。
type sessionHealth struct {
	mu    sync.Mutex
	state map[string]*sessionState
}

type sessionState struct {
	VerifiedAt time.Time // 最近一次校验成功
	ExpiredAt  time.Time // 最近一次发现失效
	LastErr    string    // 最近一次失效原因（截断）
}

// SessionEntry 是一条对外可观测的会话健康记录。
type SessionEntry struct {
	Provider   string    `json:"provider"`
	Label      string    `json:"label"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	ExpiredAt  time.Time `json:"expired_at,omitempty"`
	LastErr    string    `json:"last_err,omitempty"`
}

func newSessionHealth() *sessionHealth {
	return &sessionHealth{state: map[string]*sessionState{}}
}

// runableUnauthorized 判断 runable 上游错误是否像「会话失效」（401/403）。
// 先按强类型 UpstreamError.Status，再用文案兜底。
func runableUnauthorized(err error) bool {
	if err == nil {
		return false
	}
	var ue *runable.UpstreamError
	if errors.As(err, &ue) {
		return ue.Status == http.StatusUnauthorized || ue.Status == http.StatusForbidden
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "401") || strings.Contains(s, "403") ||
		strings.Contains(s, "unauthorized") || strings.Contains(s, "session expired") ||
		strings.Contains(s, "未登录") || strings.Contains(s, "登录态")
}

func sessionKey(provider, label string) string { return provider + "\x00" + label }

// MarkVerified 记录一次会话校验成功（清除失效标记）。
func (s *sessionHealth) MarkVerified(provider, label string) {
	if s == nil || label == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[sessionKey(provider, label)]
	if st == nil {
		st = &sessionState{}
		s.state[sessionKey(provider, label)] = st
	}
	st.VerifiedAt = time.Now()
	st.ExpiredAt = time.Time{}
	st.LastErr = ""
}

// MarkExpired 记录一次会话失效。
func (s *sessionHealth) MarkExpired(provider, label, errMsg string) {
	if s == nil || label == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[sessionKey(provider, label)]
	if st == nil {
		st = &sessionState{}
		s.state[sessionKey(provider, label)] = st
	}
	st.ExpiredAt = time.Now()
	st.LastErr = truncateReason(errMsg)
}

// Status 返回某账号的会话健康（ok=false 表示从未记账）。
func (s *sessionHealth) Status(provider, label string) (verifiedAt, expiredAt time.Time, lastErr string, ok bool) {
	if s == nil {
		return time.Time{}, time.Time{}, "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state[sessionKey(provider, label)]
	if st == nil {
		return time.Time{}, time.Time{}, "", false
	}
	return st.VerifiedAt, st.ExpiredAt, st.LastErr, true
}

// Snapshot 返回按 provider/label 排序的会话健康台账。
func (s *sessionHealth) Snapshot() []SessionEntry {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionEntry, 0, len(s.state))
	for k, st := range s.state {
		provider, label, _ := strings.Cut(k, "\x00")
		out = append(out, SessionEntry{
			Provider:   provider,
			Label:      label,
			VerifiedAt: st.VerifiedAt,
			ExpiredAt:  st.ExpiredAt,
			LastErr:    st.LastErr,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Label < out[j].Label
	})
	return out
}
