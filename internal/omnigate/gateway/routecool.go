package gateway

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 路由级失败绕行台账。
//
// 账号冷却与密钥池禁用回答的是「这一把凭据坏了」；它们回答不了另一个问题：
// 一条别名链的主目标一直失败、但链尾还有同模型的备用目标时，下一次请求会不会
// 又先打一次已经挂掉的主目标？没有这层记忆时，答案是「会」——功能上靠兜底仍能
// 成功，但每个请求都白付一次上游往返（额度型错误还会每次都把同一句 quota 再
// 拿回来）。线上最典型的形状就是 deepseek-v4.1-flash 先打 cline-pass，失败后
// 才落到 yijia。
//
// 这里给「provider + 路由后的模型」记一个短期绕行窗口：失败一次，后续请求优先
// 走别名链上下一个可用目标；窗口到期后只放一个探针回来重试，成功即清零。它只
// 改变尝试顺序，不会把目标从配置、密钥 scope 或别名链里删掉，所以不存在「永久
// 锁定某家」。纯进程内状态：短期信号，重启后重新观察即可。
const (
	routeCoolBase = 30 * time.Second
	routeCoolMax  = 5 * time.Minute
	// routeCoolReset 是「安静这么久后重新计数」的门槛。否则一个每天偶发一次
	// 500 的目标会一路把退避顶到上限，而它并不是持续故障。
	routeCoolReset   = 4 * routeCoolMax
	routeCoolEntries = 2048
)

type routeFailureState struct {
	Until   time.Time
	Strikes int
	LastAt  time.Time
	Reason  string
}

// RouteCooldownEntry 是一条对外可观测的路由绕行记录（/healthz 的 route_cooldowns）。
type RouteCooldownEntry struct {
	Provider string    `json:"provider"`
	Model    string    `json:"model"`
	Until    time.Time `json:"until"`
	Strikes  int       `json:"strikes"`
	Reason   string    `json:"reason,omitempty"`
}

type routeFailureLedger struct {
	mu sync.Mutex
	m  map[string]*routeFailureState
}

func newRouteFailureLedger() *routeFailureLedger {
	return &routeFailureLedger{m: map[string]*routeFailureState{}}
}

func routeCoolKey(provider, model string) string {
	return strings.TrimSpace(provider) + "\x00" + strings.TrimSpace(model)
}

// Cooling 报告该目标当前是否处于绕行窗口内。
func (l *routeFailureLedger) Cooling(provider, model string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[routeCoolKey(provider, model)]
	return s != nil && now.Before(s.Until)
}

// Fail 记一次失败并返回绕行截止时间与连续失败次数。退避与账号冷却同形：
// base * 2^(strikes-1)，封顶 routeCoolMax。
func (l *routeFailureLedger) Fail(provider, model, reason string, now time.Time) (time.Time, int) {
	if l == nil {
		return time.Time{}, 0
	}
	key := routeCoolKey(provider, model)
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[key]
	if s == nil || now.Sub(s.LastAt) > routeCoolReset {
		s = &routeFailureState{}
	}
	s.Strikes++
	s.LastAt = now
	s.Reason = truncateReason(reason)
	d := routeCoolBase
	for i := 1; i < s.Strikes && d < routeCoolMax; i++ {
		d *= 2
	}
	if d > routeCoolMax {
		d = routeCoolMax
	}
	s.Until = now.Add(d)
	if l.m[key] == nil {
		l.pruneLocked(now)
	}
	l.m[key] = s
	return s.Until, s.Strikes
}

// Clear 清除绕行状态；返回值用于「恢复」日志（此前没有记录时不打扰）。
func (l *routeFailureLedger) Clear(provider, model string) (strikes int, had bool) {
	if l == nil {
		return 0, false
	}
	key := routeCoolKey(provider, model)
	l.mu.Lock()
	defer l.mu.Unlock()
	s, ok := l.m[key]
	if !ok {
		return 0, false
	}
	delete(l.m, key)
	return s.Strikes, true
}

// Snapshot 返回当前仍在窗口内的绕行记录，按 provider/model 排序，便于阅读。
func (l *routeFailureLedger) Snapshot(now time.Time) []RouteCooldownEntry {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]RouteCooldownEntry, 0, len(l.m))
	for key, s := range l.m {
		if s == nil || !now.Before(s.Until) {
			continue
		}
		provider, model, _ := strings.Cut(key, "\x00")
		out = append(out, RouteCooldownEntry{
			Provider: provider,
			Model:    model,
			Until:    s.Until,
			Strikes:  s.Strikes,
			Reason:   s.Reason,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// pruneLocked 清理已过期条目；仍然超限时丢掉最久没更新的记录。上游模型名由
// 客户端提供，不能让它用「每次换一个 404 模型名」把内存撑大。
func (l *routeFailureLedger) pruneLocked(now time.Time) {
	if len(l.m) < routeCoolEntries {
		return
	}
	for key, s := range l.m {
		if s == nil || !now.Before(s.Until) {
			delete(l.m, key)
		}
	}
	for len(l.m) >= routeCoolEntries {
		var oldestKey string
		var oldest time.Time
		for key, s := range l.m {
			if oldestKey == "" || s.LastAt.Before(oldest) {
				oldestKey, oldest = key, s.LastAt
			}
		}
		if oldestKey == "" {
			return
		}
		delete(l.m, oldestKey)
	}
}

// promoteCoolingPrimary 在主目标处于绕行窗口时，把别名链上第一个同样在窗口外
// 的候选提为主目标；原主目标与其余候选按「非冷却优先」排进回退链。
//
// 只在存在别名链候选时生效：没有同模型备选时，绕行没有意义，保持原有「每次都
// 试一下」的行为，避免把唯一通道也锁在冷却里。
func (g *Gateway) promoteCoolingPrimary(primary fallbackCandidate, chain []fallbackCandidate, now time.Time) (fallbackCandidate, []fallbackCandidate, bool) {
	if g.routeCool == nil || len(chain) == 0 || !g.routeCool.Cooling(primary.Provider, primary.Model, now) {
		return primary, chain, false
	}
	for i, c := range chain {
		if g.routeCool.Cooling(c.Provider, c.Model, now) {
			continue
		}
		var warm, cooling []fallbackCandidate
		for j, other := range chain {
			if j == i {
				continue
			}
			if g.routeCool.Cooling(other.Provider, other.Model, now) {
				cooling = append(cooling, other)
			} else {
				warm = append(warm, other)
			}
		}
		rest := make([]fallbackCandidate, 0, len(chain))
		rest = append(rest, warm...)
		rest = append(rest, primary) // 原主目标放在非冷却候选之后，作为探针
		rest = append(rest, cooling...)
		return c, rest, true
	}
	return primary, chain, false
}

// markRouteFailure 记录一次主目标/候选失败。announce=false 用于兜底候选：调用方
// 已经为它打过一条 Warn，这里不必再重复。
func (g *Gateway) markRouteFailure(provider, model string, err error, announce bool, now time.Time) {
	if g.routeCool == nil || !routeFailurePenalized(err) {
		return
	}
	reason := routeFailureReason(err)
	until, strikes := g.routeCool.Fail(provider, model, reason, now)
	if announce && g.log != nil {
		g.log.Warn("路由目标 %s/%s 暂不可用（%s，连续 %d 次），%s 内优先走备用目标",
			provider, model, reason, strikes, time.Until(until).Round(time.Second))
	}
}

// clearRouteFailure 在主目标/候选恢复后取消绕行，并留下一条恢复日志。
func (g *Gateway) clearRouteFailure(provider, model string) {
	strikes, had := g.routeCool.Clear(provider, model)
	if had && strikes > 0 && g.log != nil {
		g.log.Info("路由目标 %s/%s 已恢复，取消绕行（此前连续 %d 次失败）", provider, model, strikes)
	}
}

// errEmptyUpstream 是「HTTP 200 但没有任何有效输出」的占位错误：它同样属于
// provider 侧失败，值得短暂绕行，而不是当成请求错误。
var errEmptyUpstream = errors.New("upstream returned no usable output")

// routeFailurePenalized 判断一次失败是否值得把该目标短暂降级。
//
// 关键边界：请求本身写错（400 / 405 / 413 / 422、prompt 超限）换哪家都会得到
// 同一句拒绝，不能因此把供应商标成故障；客户端主动断开也不该记在供应商头上。
// 401/403/402/404/408/409/425/429/5xx、网络错误、空响应则都是目标侧问题。
func routeFailurePenalized(err error) bool {
	if err == nil {
		return true // 空响应
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var tooLong *promptTooLongErr
	if errors.As(err, &tooLong) {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, marker := range []string{
		"prompt_limit_exceeded",
		"invalid_request_error",
		"messages_required",
		"context_length_exceeded",
	} {
		if strings.Contains(s, marker) {
			return false
		}
	}
	if status := upstreamHTTPStatus(err); status != 0 {
		switch status {
		case 400, 405, 413, 422:
			return false
		default:
			return true
		}
	}
	return true
}

// routeFailureReason 把失败压成短标签，供日志与 /healthz 阅读。
func routeFailureReason(err error) string {
	if err == nil {
		return "empty"
	}
	s := strings.ToLower(err.Error())
	status := upstreamHTTPStatus(err)
	switch {
	case strings.Contains(s, "quota") || strings.Contains(s, "额度") || strings.Contains(s, "余额不足") || strings.Contains(s, "积分不足"):
		return "quota"
	case status == 401 || status == 403 || strings.Contains(s, "unauthorized") || strings.Contains(s, "invalid api key") || strings.Contains(s, "cookie"):
		return "auth"
	case status == 429 || strings.Contains(s, "rate limit") || strings.Contains(s, "too many requests") || strings.Contains(s, "限流"):
		return "rate"
	case strings.Contains(s, "timeout") || strings.Contains(s, "deadline exceeded"):
		return "timeout"
	case status >= 500:
		return "upstream " + strconv.Itoa(status)
	case status != 0:
		return "http " + strconv.Itoa(status)
	default:
		return truncateReason(err.Error())
	}
}

// upstreamHTTPStatus 从已知的上游错误类型或错误文案里提取 HTTP 状态码。
// 各 provider 早就有自己的强类型错误；这里只做一层兼容读取，避免为了一个状态
// 码把网关和所有 provider 的错误类型强行统一。
func upstreamHTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface{ UpstreamStatus() int }
	var sc statusCoder
	if errors.As(err, &sc) {
		if n := sc.UpstreamStatus(); n != 0 {
			return n
		}
	}
	s := strings.ToLower(err.Error())
	for _, prefix := range []string{"http ", "status "} {
		off := 0
		for {
			i := strings.Index(s[off:], prefix)
			if i < 0 {
				break
			}
			i += off + len(prefix)
			rest := s[i:]
			if len(rest) >= 3 {
				if n, e := strconv.Atoi(rest[:3]); e == nil && n >= 100 && n <= 599 {
					return n
				}
			}
			off = i
		}
	}
	return 0
}
