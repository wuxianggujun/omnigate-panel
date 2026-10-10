package gateway

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
)

// 账号级限流冷却台账。
//
// 上游对某账号回 429（或明确的限流文案）时，把这个账号冷却一段时间，后续请求优先
// 绕开它，避免对同一账号连打 429——既打不进去，又可能把上游的封禁窗口越拖越长。
// 冷却时长按连续命中次数指数退避：base * 2^(strikes-1)，封顶 max；成功一次即清零
// 连续计数（累计命中 Total 保留，作为台账）。
//
// 纯进程内状态：冷却本就是短期信号，不落盘（重启后重新观察即可）。台账经
// Gateway.Info（/omni/healthz）与面板账号池的 cooling_until 字段对外可见。
const (
	cooldownBase = 30 * time.Second
	cooldownMax  = 15 * time.Minute
)

type cooldownState struct {
	Until   time.Time // 冷却截止（零值 = 未冷却）
	Strikes int       // 连续命中次数（指数退避用）
	LastAt  time.Time // 最近一次命中
	Reason  string    // 最近一次命中原因（截断）
	Total   int       // 累计命中次数（台账，不清零）
}

// CooldownEntry 是一条对外可观测的冷却台账记录。
type CooldownEntry struct {
	Provider string    `json:"provider"`
	Label    string    `json:"label"`
	Until    time.Time `json:"until"`
	Strikes  int       `json:"strikes"`
	Total    int       `json:"total"`
	LastAt   time.Time `json:"last_at"`
	Reason   string    `json:"reason,omitempty"`
}

type cooldownLedger struct {
	mu     sync.Mutex
	states map[string]*cooldownState
}

func newCooldownLedger() *cooldownLedger {
	return &cooldownLedger{states: map[string]*cooldownState{}}
}

func cooldownKey(provider, label string) string { return provider + "\x00" + label }

// Cooling 报告账号此刻是否处于冷却中。
func (c *cooldownLedger) Cooling(provider, label string, now time.Time) bool {
	_, ok := c.Until(provider, label, now)
	return ok
}

// Until 返回账号的冷却截止时刻（仅在仍冷却时 ok=true）。
func (c *cooldownLedger) Until(provider, label string, now time.Time) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.states[cooldownKey(provider, label)]
	if s != nil && now.Before(s.Until) {
		return s.Until, true
	}
	return time.Time{}, false
}

// Note 记录一次限流命中，返回冷却截止时刻与连续命中次数。
func (c *cooldownLedger) Note(provider, label, reason string, now time.Time) (time.Time, int) {
	if c == nil {
		return time.Time{}, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := cooldownKey(provider, label)
	s := c.states[key]
	if s == nil {
		s = &cooldownState{}
		c.states[key] = s
	}
	s.Strikes++
	s.Total++
	s.LastAt = now
	s.Reason = truncateReason(reason)
	shift := s.Strikes - 1
	if shift > 20 {
		shift = 20 // 防溢出；该档早已封顶
	}
	d := cooldownBase << shift
	if d > cooldownMax {
		d = cooldownMax
	}
	s.Until = now.Add(d)
	return s.Until, s.Strikes
}

// Clear 成功一次即解除冷却并清零连续计数（累计 Total 保留作台账）。
func (c *cooldownLedger) Clear(provider, label string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if s := c.states[cooldownKey(provider, label)]; s != nil {
		s.Strikes = 0
		s.Until = time.Time{}
	}
}

// Snapshot 返回按 provider/label 排序的冷却台账（仅包含曾命中过的账号）。
func (c *cooldownLedger) Snapshot() []CooldownEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]CooldownEntry, 0, len(c.states))
	for k, s := range c.states {
		provider, label, _ := strings.Cut(k, "\x00")
		out = append(out, CooldownEntry{
			Provider: provider,
			Label:    label,
			Until:    s.Until,
			Strikes:  s.Strikes,
			Total:    s.Total,
			LastAt:   s.LastAt,
			Reason:   s.Reason,
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

// rateLimited 判断错误是否像「上游限流」（429 / too many requests / 请求过于频繁）。
//
// 与 provider.OutOfCredits 同思路：先按强类型 UpstreamError.Status 判定，再用文案
// 兜底——上游各式各样，宁松勿漏（误判的代价只是少打一次该账号，冷却会自动恢复）。
func rateLimited(err error) bool {
	if err == nil {
		return false
	}
	var re *raccoon.UpstreamError
	if errors.As(err, &re) && re.Status == http.StatusTooManyRequests {
		return true
	}
	var ue *runable.UpstreamError
	if errors.As(err, &ue) && ue.Status == http.StatusTooManyRequests {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "429") ||
		strings.Contains(s, "rate limit") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "too many requests") ||
		strings.Contains(s, "请求过于频繁") ||
		strings.Contains(s, "频率过高") ||
		strings.Contains(s, "限流")
}

func truncateReason(s string) string {
	s = strings.TrimSpace(s)
	const max = 200
	if len(s) > max {
		return s[:max]
	}
	return s
}
