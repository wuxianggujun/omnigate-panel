package gateway

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
)

// 会话健康：校验成功置 verified、清除失效；失效置 expired + 原因。
func TestSessionHealthVerifiedExpired(t *testing.T) {
	s := newSessionHealth()
	if _, _, _, ok := s.Status("runable", "a"); ok {
		t.Fatal("unknown account should report ok=false")
	}

	s.MarkVerified("runable", "a")
	v, e, lastErr, ok := s.Status("runable", "a")
	if !ok || v.IsZero() || !e.IsZero() || lastErr != "" {
		t.Fatalf("after verify: v=%v e=%v err=%q ok=%v", v, e, lastErr, ok)
	}

	s.MarkExpired("runable", "a", "401 unauthorized")
	v2, e2, lastErr2, _ := s.Status("runable", "a")
	if !v2.Equal(v) || e2.IsZero() || lastErr2 == "" {
		t.Fatalf("after expire: v=%v e=%v err=%q", v2, e2, lastErr2)
	}

	// 再校验成功应清除失效标记。
	s.MarkVerified("runable", "a")
	_, e3, lastErr3, _ := s.Status("runable", "a")
	if !e3.IsZero() || lastErr3 != "" {
		t.Fatalf("verify should clear expiry: e=%v err=%q", e3, lastErr3)
	}
}

func TestRunableUnauthorized(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"401", &runable.UpstreamError{Status: 401}, true},
		{"403", &runable.UpstreamError{Status: 403}, true},
		{"500", &runable.UpstreamError{Status: 500}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runableUnauthorized(tc.err); got != tc.want {
				t.Errorf("runableUnauthorized(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}
