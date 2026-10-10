package gateway

import (
	"errors"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
)

// 连续限流命中按指数退避放大冷却（30s→60s→…），封顶 cooldownMax；成功清除。
func TestCooldownLedgerBackoffAndClear(t *testing.T) {
	c := newCooldownLedger()
	now := time.Now()

	u1, s1 := c.Note("raccoon", "账号1", "429 too many requests", now)
	if s1 != 1 || !u1.Equal(now.Add(30*time.Second)) {
		t.Fatalf("strike1 until=%v strikes=%d want +30s/1", u1, s1)
	}
	if !c.Cooling("raccoon", "账号1", now) {
		t.Fatal("should be cooling right after note")
	}
	if c.Cooling("raccoon", "账号1", now.Add(31*time.Second)) {
		t.Fatal("should not be cooling after the window")
	}

	u2, s2 := c.Note("raccoon", "账号1", "429", now)
	if s2 != 2 || !u2.Equal(now.Add(60*time.Second)) {
		t.Fatalf("strike2 until=%v strikes=%d want +60s/2", u2, s2)
	}

	// 多打几次后必须封顶，不能无限增长（也不能溢出成负值）。
	for i := 0; i < 40; i++ {
		c.Note("raccoon", "账号1", "429", now)
	}
	if u, _ := c.Until("raccoon", "账号1", now); u.After(now.Add(cooldownMax)) {
		t.Fatalf("cooldown %v exceeds cap %v", u, now.Add(cooldownMax))
	}

	// 成功清除连续计数与冷却；累计 Total 保留作台账。
	c.Clear("raccoon", "账号1")
	if c.Cooling("raccoon", "账号1", now) {
		t.Fatal("clear should drop the cooldown")
	}
	snap := c.Snapshot()
	if len(snap) != 1 || snap[0].Total == 0 || snap[0].Strikes != 0 {
		t.Fatalf("snapshot = %+v, want total>0 strikes=0", snap)
	}
}

func TestCooldownLedgerPerAccountIsolated(t *testing.T) {
	c := newCooldownLedger()
	now := time.Now()
	c.Note("raccoon", "a", "429", now)
	if !c.Cooling("raccoon", "a", now) {
		t.Fatal("a should be cooling")
	}
	if c.Cooling("raccoon", "b", now) || c.Cooling("runable", "a", now) {
		t.Fatal("cooldown must be scoped to (provider,label)")
	}
}

func TestRateLimitedDetection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"raccoon 429", &raccoon.UpstreamError{Status: 429, Message: "too many requests"}, true},
		{"raccoon 500", &raccoon.UpstreamError{Status: 500, Message: "boom"}, false},
		{"text 429", errors.New("HTTP 429: rate limit exceeded"), true},
		{"text zh", errors.New("请求过于频繁"), true},
		{"out of credits", errors.New("out of credits"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rateLimited(tc.err); got != tc.want {
				t.Errorf("rateLimited(%v) = %v want %v", tc.err, got, tc.want)
			}
		})
	}
}
