package gateway

import (
	"context"
	"errors"
	"regexp"
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
// 走别名链上下一个可用目标；窗口到期后只放一个探针回来重试，成功即清零。额度
// 耗尽类错误会按上游给出的重置时间（如 cline-pass 的 weekly limit）延长窗口，
// 避免每 5 分钟白撞一次。它只改变尝试顺序，不会把目标从配置、密钥 scope 或别名
// 链里删掉，所以不存在「永久锁定某家」。纯进程内状态：短期信号，重启后重新观察即可。
const (
	routeCoolBase = 30 * time.Second
	routeCoolMax  = 5 * time.Minute
	// routeCoolReset 是「安静这么久后重新计数」的门槛。否则一个每天偶发一次
	// 500 的目标会一路把退避顶到上限，而它并不是持续故障。
	routeCoolReset   = 4 * routeCoolMax
	routeCoolEntries = 2048
	// 额度耗尽不是网络抖动：cline-pass 的周额度会明确给出「resets in 14h 24m」。
	// 这类窗口按上游给的重置时间走，上限 24h；拿不到重置时间时保守绕行 30m，
	// 避免每 5 分钟往一个已知没额度的目标上白打一次。
	routeCoolQuotaMax     = 24 * time.Hour
	routeCoolQuotaDefault = 30 * time.Minute
	// routeCoolProviderModel 是供应商级额度窗口在台账里的模型占位符。额度通常是
	// 账号/密钥级而不是单个模型级：一家周额度耗尽后，同供应商的其它模型也不该
	// 每个都先撞一次。它只影响别名链的候选顺序，不会把供应商从配置里删掉。
	routeCoolProviderModel = "*"
)

var (
	routeResetInRe   = regexp.MustCompile(`(?i)reset(?:s)?\s+in\s+([0-9dhms\s]+)`)
	routeResetUnitRe = regexp.MustCompile(`(?i)(\d+)\s*(d|h|m|s)`)
	// 多密钥供应商全池额度耗尽时，openaibackend 返回「最早 10-27 20:26 自动重试」。
	// 这是绝对时间而不是 "resets in 2h"：不解析它，路由层只能按 30 分钟兜底，
	// 会在密钥仍被禁用 16 小时的情况下反复探针。
	routeRetryAtRe = regexp.MustCompile(`(?i)最早\s+(\d{1,2})-(\d{1,2})\s+(\d{1,2}):(\d{2})\s+自动重试`)
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
	Scope    string    `json:"scope,omitempty"` // model / provider
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

// Penalty 返回该目标近期连续失败次数（模型级与供应商级取较大值）。
// 窗口已经到期但失败刚发生不久的候选仍带惩罚：退避到期只表示「可以再探一次」，
// 不表示「已经恢复」；提升主目标时应优先选没有近期失败记录的候选，避免每次
// 退避到期都把用户请求当成探针。超过 routeCoolReset 的旧记录视为已恢复。
func (l *routeFailureLedger) Penalty(provider, model string, now time.Time) int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	penalty := 0
	for _, key := range []string{
		routeCoolKey(provider, model),
		routeCoolKey(provider, routeCoolProviderModel),
	} {
		s := l.m[key]
		if s == nil || s.Strikes <= penalty {
			continue
		}
		if now.Sub(s.LastAt) <= routeCoolReset {
			penalty = s.Strikes
		}
	}
	return penalty
}

// CoolingAny 同时看模型级与供应商级窗口。供应商级窗口来自额度类失败：
// 账号/密钥的额度通常跨模型生效，不能每换一个模型就重新撞一次。
func (l *routeFailureLedger) CoolingAny(provider, model string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range []string{
		routeCoolKey(provider, model),
		routeCoolKey(provider, routeCoolProviderModel),
	} {
		if s := l.m[key]; s != nil && now.Before(s.Until) {
			return true
		}
	}
	return false
}

// Fail 记一次失败并返回绕行截止时间与连续失败次数。退避与账号冷却同形：
// base * 2^(strikes-1)，封顶 routeCoolMax。
func (l *routeFailureLedger) Fail(provider, model, reason string, now time.Time) (time.Time, int) {
	return l.fail(provider, model, reason, now, time.Time{})
}

// FailUntil 与 Fail 相同，但绕行至少持续到 floor。额度耗尽类错误用它把
// 「resets in 2h 12m」这类上游重置时间落成绕行窗口；floor 最多 24h。
func (l *routeFailureLedger) FailUntil(provider, model, reason string, now, floor time.Time) (time.Time, int) {
	if !floor.IsZero() {
		if max := now.Add(routeCoolQuotaMax); floor.After(max) {
			floor = max
		}
	}
	return l.fail(provider, model, reason, now, floor)
}

func (l *routeFailureLedger) fail(provider, model, reason string, now, floor time.Time) (time.Time, int) {
	if l == nil {
		return time.Time{}, 0
	}
	key := routeCoolKey(provider, model)
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.m[key]
	// 长额度窗口内如果又被探针撞了一次（例如链上备用也全冷却），不能因为
	// 退避计数重置就把原本几小时的窗口缩短成 30 秒；保留仍然有效的旧截止时间。
	var prevUntil time.Time
	if s != nil && s.Until.After(now) {
		prevUntil = s.Until
	}
	if s == nil || now.Sub(s.LastAt) > routeCoolReset {
		s = &routeFailureState{Until: prevUntil}
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
	if floor.After(s.Until) {
		s.Until = floor
	}
	if prevUntil.After(s.Until) {
		s.Until = prevUntil
	}
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
		scope := "model"
		if model == routeCoolProviderModel {
			scope = "provider"
		}
		out = append(out, RouteCooldownEntry{
			Provider: provider,
			Model:    model,
			Scope:    scope,
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

// promoteCoolingPrimary 在主目标处于绕行窗口时，把别名链上近期失败最少且在
// 窗口外的候选提为主目标；原主目标与其余候选按「非冷却优先」排进回退链。
//
// 只看窗口是否到期还不够：一个每 5 分钟失败一次的目标，窗口刚好到期时会被
// 反复提升，用户请求就成了探针。Penalty 保留「刚失败过」的记忆，让干净候选
// 先服务；等旧记录超过 routeCoolReset，目标才重新获得被提升的机会。
//
// 只在存在别名链候选时生效：没有同模型备选时，绕行没有意义，保持原有「每次都
// 试一下」的行为，避免把唯一通道也锁在冷却里。
func (g *Gateway) promoteCoolingPrimary(primary fallbackCandidate, chain []fallbackCandidate, now time.Time) (fallbackCandidate, []fallbackCandidate, bool) {
	if g.routeCool == nil || len(chain) == 0 || !g.routeCool.CoolingAny(primary.Provider, primary.Model, now) {
		return primary, chain, false
	}
	best := -1
	bestPenalty := 0
	for i, c := range chain {
		if g.routeCool.CoolingAny(c.Provider, c.Model, now) {
			continue
		}
		penalty := g.routeCool.Penalty(c.Provider, c.Model, now)
		// 同惩罚保留链上原顺序；只有近期确实失败过的候选才让位。
		if best < 0 || penalty < bestPenalty {
			best, bestPenalty = i, penalty
		}
	}
	if best < 0 {
		return primary, chain, false
	}
	i := best
	var warm, cooling []fallbackCandidate
	for j, other := range chain {
		if j == i {
			continue
		}
		if g.routeCool.CoolingAny(other.Provider, other.Model, now) {
			cooling = append(cooling, other)
		} else {
			warm = append(warm, other)
		}
	}
	rest := make([]fallbackCandidate, 0, len(chain))
	rest = append(rest, warm...)
	rest = append(rest, primary) // 原主目标放在非冷却候选之后，作为探针
	rest = append(rest, cooling...)
	return chain[i], rest, true
}

// markRouteFailure 记录一次主目标/候选失败。announce=false 用于兜底候选：调用方
// 已经为它打过一条 Warn，这里不必再重复。
func (g *Gateway) markRouteFailure(provider, model string, err error, announce bool, now time.Time) {
	if g.routeCool == nil || !routeFailurePenalized(err) {
		return
	}
	reason := routeFailureReason(err)
	var until time.Time
	var strikes int
	providerWide := false
	if floor, ok := routeQuotaFloor(err, now); ok {
		until, strikes = g.routeCool.FailUntil(provider, model, reason, now, floor)
		// 额度是账号/密钥级状态，不是模型级：同供应商的其它模型也应优先绕行，
		// 否则每个模型都要先把同一句 quota 撞回来一次。窗口仍只影响候选顺序。
		g.routeCool.FailUntil(provider, routeCoolProviderModel, reason, now, floor)
		providerWide = true
	} else {
		until, strikes = g.routeCool.Fail(provider, model, reason, now)
	}
	if announce && g.log != nil {
		// 不承诺「一定改走备用」：没有别名备用目标时该请求仍会重试它。
		suffix := ""
		if providerWide {
			suffix = "；同供应商其它模型一并优先绕行"
		}
		g.log.Warn("路由目标 %s/%s 暂不可用（%s，连续 %d 次），%s 内记入失败窗口%s；有健康备用目标时优先走备用",
			provider, model, reason, strikes, until.Sub(now).Round(time.Second), suffix)
	}
}

// clearRouteFailure 在主目标/候选恢复后取消绕行，并留下一条恢复日志。
func (g *Gateway) clearRouteFailure(provider, model string) {
	strikes, had := g.routeCool.Clear(provider, model)
	pstrikes, phad := g.routeCool.Clear(provider, routeCoolProviderModel)
	if had && strikes > 0 && g.log != nil {
		g.log.Info("路由目标 %s/%s 已恢复，取消绕行（此前连续 %d 次失败）", provider, model, strikes)
	}
	if phad && pstrikes > 0 && g.log != nil {
		g.log.Info("供应商 %s 的额度绕行窗口已取消（成功请求恢复）", provider)
	}
}

// errEmptyUpstream 是「HTTP 200 但没有任何有效输出」的占位错误：它同样属于
// provider 侧失败，值得短暂绕行，而不是当成请求错误。
var errEmptyUpstream = errors.New("upstream returned no usable output")

// isQuotaText 判断错误文案是否明确属于「额度/订阅上限」，而不是普通限流。
// cline-pass 的 429 是 INFERENCE_CAP_ERROR + weekly limit，正文里没有 quota 字样；
// 只看 "rate limit" 会把它当 30 秒抖动，结果每 5 分钟又去撞一次已经耗尽的周额度。
func isQuotaText(s string) bool {
	if strings.Contains(s, "inference_cap") {
		return true
	}
	if strings.Contains(s, "weekly") &&
		(strings.Contains(s, "limit") || strings.Contains(s, "cap") || strings.Contains(s, "quota")) {
		return true
	}
	for _, marker := range []string{
		"quota", "credit",
		"额度", "余额不足", "积分不足",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// routeQuotaFloor 从额度类错误里解析上游重置时间，返回绕行窗口的下限。
// 解析不到时仍给一个保守窗口（30m）；第二个返回值 false 表示不是额度类错误。
func routeQuotaFloor(err error, now time.Time) (time.Time, bool) {
	if err == nil || upstreamHTTPStatus(err) == 401 {
		return time.Time{}, false
	}
	s := strings.ToLower(err.Error())
	if !isQuotaText(s) {
		return time.Time{}, false
	}
	if m := routeResetInRe.FindStringSubmatch(s); m != nil {
		if d := parseResetDuration(m[1]); d > 0 {
			// 多给 1 分钟余量，避免在上游刚重置的边界上又撞一次。
			d += time.Minute
			if d > routeCoolQuotaMax {
				d = routeCoolQuotaMax
			}
			return now.Add(d), true
		}
	}
	// 全池禁用的错误没有 "resets in"，只有「最早 10-27 20:26 自动重试」。
	if at, ok := parseRouteRetryAt(s, now); ok {
		return at, true
	}
	return now.Add(routeCoolQuotaDefault), true
}

// parseRouteRetryAt 解析「最早 MM-DD HH:MM 自动重试」。最多只接受约 24 小时内的
// 未来时间：额度长禁用上限就是 24h，超出这个范围的多半是过期/错格式文案，宁可
// 退回 30 分钟默认窗口。跨年时（12-31 → 01-01）把日期补到下一年。
func parseRouteRetryAt(s string, now time.Time) (time.Time, bool) {
	m := routeRetryAtRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	month, errM := strconv.Atoi(m[1])
	day, errD := strconv.Atoi(m[2])
	hour, errH := strconv.Atoi(m[3])
	minute, errN := strconv.Atoi(m[4])
	if errM != nil || errD != nil || errH != nil || errN != nil ||
		month < 1 || month > 12 || day < 1 || day > 31 || hour > 23 || minute > 59 {
		return time.Time{}, false
	}
	at := time.Date(now.Year(), time.Month(month), day, hour, minute, 0, 0, now.Location())
	if at.Month() != time.Month(month) || at.Day() != day {
		return time.Time{}, false
	}
	if !at.After(now) {
		next := at.AddDate(1, 0, 0)
		if next.Sub(now) > routeCoolQuotaMax+time.Hour {
			return time.Time{}, false
		}
		at = next
	}
	if at.Sub(now) > routeCoolQuotaMax+time.Hour {
		return time.Time{}, false
	}
	// 与 "resets in" 一样多留 1 分钟，避免在禁用刚到期时又撞一次。
	return at.Add(time.Minute), true
}

func parseResetDuration(s string) time.Duration {
	var total time.Duration
	for _, m := range routeResetUnitRe.FindAllStringSubmatch(s, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		switch strings.ToLower(m[2]) {
		case "d":
			total += time.Duration(n) * 24 * time.Hour
		case "h":
			total += time.Duration(n) * time.Hour
		case "m":
			total += time.Duration(n) * time.Minute
		case "s":
			total += time.Duration(n) * time.Second
		}
	}
	return total
}

// routeFailurePenalized 判断一次失败是否值得把该目标短暂降级。
//
// 关键边界：请求本身写错（400 / 405 / 413 / 422、prompt 超限）换哪家都会得到
// 同一句拒绝，不能因此把供应商标成故障；客户端主动断开也不该记在供应商头上。
// 额度耗尽例外：即使被包成 400 也是目标侧状态。401/403/402/404/408/409/425/429/
// 5xx、网络错误、空响应则都是目标侧问题。
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
	// 额度耗尽有时会被上游包成 400 + invalid_request_error；它是目标侧状态，
	// 不是请求写错，仍应进入绕行（routeQuotaFloor 会给出更长的窗口）。
	if isQuotaText(s) {
		return true
	}
	// 有状态码时先看状态码。否则 403/5xx 带 invalid_request_error 的响应会被
	// 文案兜底误判成请求错误，主目标持续失败却始终不进入绕行。
	if status := upstreamHTTPStatus(err); status != 0 {
		switch status {
		case 400, 405, 413, 422:
			return false
		default:
			return true
		}
	}
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
	case isQuotaText(s):
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
