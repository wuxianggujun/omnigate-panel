package gateway

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
)

// 本文件实现 OmniGate 侧的「快过期积分优先」选号（P4）。
//
// 背景：raccoon / runable 的积分有「每日积分（当天清零）+ 每月会员积分（当月清零）」
// 这类会作废的批次；网关原先只做 ActiveIndex 轮询 + 失败才换号，会把请求钉死在某个
// 账号上，快过期积分白白过期。这里给选号加一层「窗口内仍有快过期批次的账号优先」，
// 与 WorkBuddy 池的 pool.prefer_expiring 同口径，但数据来源不同：
//
//   - WB 池的到期信息来自签到/余额刷新（上游给逐笔到期时间戳）；
//   - OmniGate 上游只给钱包金额（daily_points / monthly_points / reward_points /
//     topup_points），不给逐笔时间戳，故到期时刻由「重置节奏 + 本地时区」推导
//     （见 expiryOf）：daily → 下一个本地零点，monthly → 下月 1 日零点，其余无到期。
//
// 余额快照是 stale-while-revalidate 的：请求路径只用现有快照排序，缺失/过期时在后台
// 单飞刷新（不阻塞请求）。没有快照时保持旧的轮询行为，零回归。

// creditKind 一个积分钱包的时效类别。
type creditKind int

const (
	creditNone    creditKind = iota // 无已知到期（奖励积分 / 充值积分）
	creditDaily                     // 每日重置
	creditMonthly                   // 每月重置
)

// creditWallet 一个钱包的金额与时效类别。
type creditWallet struct {
	Kind   creditKind
	Amount int64
}

// creditSnapshot 某账号最近一次余额快照（运行态，不落盘）。
type creditSnapshot struct {
	At        time.Time
	Available int64
	Wallets   []creditWallet
	// Err 非空表示本次刷新失败；仍写入快照以便按 ttl 节流重试（而不是每个请求都打上游）。
	Err string
}

// creditCache 是 provider\x00label → 余额快照 的进程内缓存。
type creditCache struct {
	mu       sync.Mutex
	m        map[string]creditSnapshot
	inflight map[string]bool
}

func newCreditCache() *creditCache {
	return &creditCache{m: map[string]creditSnapshot{}, inflight: map[string]bool{}}
}

func creditKey(prov, label string) string { return prov + "\x00" + label }

func (c *creditCache) get(key string) (creditSnapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.m[key]
	return s, ok
}

func (c *creditCache) put(key string, s creditSnapshot) {
	c.mu.Lock()
	c.m[key] = s
	c.mu.Unlock()
}

// stale 报告该 provider 是否至少有一个账号缺快照、或快照已过 ttl。
func (c *creditCache) stale(prov string, labels []string, ttl time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range labels {
		s, ok := c.m[creditKey(prov, l)]
		if !ok || now.Sub(s.At) >= ttl {
			return true
		}
	}
	return false
}

// beginRefresh 抢占该 provider 的刷新权（单飞）；已有刷新在跑时返回 false。
func (c *creditCache) beginRefresh(prov string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight[prov] {
		return false
	}
	c.inflight[prov] = true
	return true
}

func (c *creditCache) endRefresh(prov string) {
	c.mu.Lock()
	delete(c.inflight, prov)
	c.mu.Unlock()
}

// copyFrom 把另一份缓存的快照搬进来（热重载时沿用旧网关的余额快照）。
func (c *creditCache) copyFrom(old *creditCache) {
	old.mu.Lock()
	snap := make(map[string]creditSnapshot, len(old.m))
	for k, v := range old.m {
		snap[k] = v
	}
	old.mu.Unlock()
	c.mu.Lock()
	for k, v := range snap {
		c.m[k] = v
	}
	c.mu.Unlock()
}

// ShareCreditsFrom 把旧网关的余额快照搬进本网关。
//
// 为什么需要：保存配置 / 改出站代理都会重建网关，而余额快照是进程内缓存——不搬运的话，
// 每次保存配置后面板上的「快过期」角标与快过期优先选号都会短暂失效（直到下一个请求触发
// 后台刷新）。角标恰好在用户盯着「积分快过期优先」卡片保存时消失，看起来像功能坏了。
func (g *Gateway) ShareCreditsFrom(old *Gateway) {
	if g == nil || old == nil || g.credits == nil || old.credits == nil {
		return
	}
	g.credits.copyFrom(old.credits)
}

// expiryOf 推导一个钱包的到期时刻：daily → 下一个本地零点；monthly → 下月 1 日
// 本地零点；none → 零值（无已知到期）。上游只给钱包金额、不给逐笔时间戳，所以到期
// 时刻由「重置节奏 + 本地时区」推导（容器 TZ 决定本地时区）。
func expiryOf(kind creditKind, now time.Time) time.Time {
	switch kind {
	case creditDaily:
		y, m, d := now.Date()
		return time.Date(y, m, d+1, 0, 0, 0, 0, now.Location())
	case creditMonthly:
		y, m, _ := now.Date()
		return time.Date(y, m+1, 1, 0, 0, 0, 0, now.Location())
	}
	return time.Time{}
}

// expiringOf 汇总窗口内会过期的钱包金额，以及其中最早的一批到期时刻。
func expiringOf(s creditSnapshot, now time.Time, window time.Duration) (int64, time.Time) {
	if len(s.Wallets) == 0 {
		return 0, time.Time{}
	}
	limit := now.Add(window)
	var total int64
	var earliest time.Time
	for _, w := range s.Wallets {
		if w.Amount <= 0 {
			continue
		}
		exp := expiryOf(w.Kind, now)
		if exp.IsZero() || exp.After(limit) {
			continue
		}
		total += w.Amount
		if earliest.IsZero() || exp.Before(earliest) {
			earliest = exp
		}
	}
	return total, earliest
}

// walletKindOf 把上游钱包类型映射成时效类别。raccoon 的 reward_points / topup_points
// 无已知到期，归 creditNone（不参与快过期偏好）。
func walletKindOf(t string) creditKind {
	switch t {
	case "daily_points", "daily_credits":
		return creditDaily
	case "monthly_points", "monthly_credits":
		return creditMonthly
	}
	return creditNone
}

// raccoonWallets 把 raccoon 的钱包列表映射成 creditWallet（保留金额与时效类别）。
func raccoonWallets(bal *raccoon.Balance) []creditWallet {
	if bal == nil {
		return nil
	}
	out := make([]creditWallet, 0, len(bal.Wallets))
	for _, w := range bal.Wallets {
		out = append(out, creditWallet{Kind: walletKindOf(w.Type), Amount: w.Balance})
	}
	return out
}

// runableWallets 把 runable 的日/月额度映射成 creditWallet。
func runableWallets(cr runable.Credits) []creditWallet {
	return []creditWallet{
		{Kind: creditDaily, Amount: cr.Daily},
		{Kind: creditMonthly, Amount: cr.Monthly},
	}
}

// creditKindOf 把上游声明的重置节奏映射成选号用的时效类别。
func creditKindOf(reset provider.ResetCadence) creditKind {
	switch reset {
	case provider.ResetDaily:
		return creditDaily
	case provider.ResetMonthly:
		return creditMonthly
	}
	return creditNone
}

// balanceWallets 把 provider-neutral 的余额明细映射成 creditWallet。
func balanceWallets(bal *provider.Balance) []creditWallet {
	if bal == nil {
		return nil
	}
	out := make([]creditWallet, 0, len(bal.Wallets))
	for _, w := range bal.Wallets {
		out = append(out, creditWallet{Kind: creditKindOf(w.Resets), Amount: w.Amount})
	}
	return out
}

// noteCredits 记下一个账号的最新余额快照（供快过期优先选号与面板角标）。
func (g *Gateway) noteCredits(prov, label string, available int64, wallets []creditWallet) {
	if g.credits == nil || label == "" {
		return
	}
	g.credits.put(creditKey(prov, label), creditSnapshot{
		At:        time.Now(),
		Available: available,
		Wallets:   wallets,
	})
}

// CreditExpiring 返回某账号当前窗口内的快过期积分与最早到期时刻（供面板角标）。
// ok=false 表示尚无快照（从未刷新过）。
func (g *Gateway) CreditExpiring(prov, label string) (expiring int64, earliest time.Time, ok bool) {
	if g.credits == nil {
		return 0, time.Time{}, false
	}
	s, ok := g.credits.get(creditKey(prov, label))
	if !ok {
		return 0, time.Time{}, false
	}
	e, t := expiringOf(s, time.Now(), g.cfg.CreditExpiringSoon())
	return e, t, true
}

// ensureCredits 在快照缺失/过期时异步刷新该 provider 的余额（单飞）。
// stale-while-revalidate：本次请求先用现有快照排序，刷新在后台进行——绝不阻塞请求。
func (g *Gateway) ensureCredits(name string) {
	if g.credits == nil || !g.cfg.CreditRoutingEnabled() {
		return
	}
	accts := g.Accounts(name)
	if len(accts) == 0 {
		return
	}
	labels := make([]string, 0, len(accts))
	for _, a := range accts {
		labels = append(labels, a.Label)
	}
	now := time.Now()
	if !g.credits.stale(name, labels, g.cfg.CreditRefreshInterval(), now) {
		return
	}
	if !g.credits.beginRefresh(name) {
		return
	}
	go func() {
		defer g.credits.endRefresh(name)
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		g.refreshCredits(ctx, name)
	}()
}

// refreshCredits 查上游余额并把结果写进缓存。能力接口驱动：provider 实现
// provider.BalanceReader 就走通用路径（token 型凭据顺带续期），未实现的类型
// 静默跳过（选号自动退化为旧行为）。
func (g *Gateway) refreshCredits(ctx context.Context, name string) {
	prov := g.providers[name]
	br, ok := prov.(provider.BalanceReader)
	if !ok {
		return
	}
	tr, canRefresh := prov.(TokenRefresher)
	for _, acc := range g.Accounts(name) {
		if acc.Cookie == "" {
			continue
		}
		// token 型凭据在余额查询前先续期（与请求路径同一套 TokenRefresher）。
		if canRefresh && tr.TokenExpired(acc) {
			if rerr := g.refreshToken(ctx, name, tr, acc); rerr != nil {
				g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, rerr)
			}
			if acc.Cookie == "" {
				continue
			}
		}
		bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		bal, berr := br.QueryBalance(bctx, acc)
		cancel()
		if berr != nil {
			g.credits.put(creditKey(name, acc.Label), creditSnapshot{At: time.Now(), Err: berr.Error()})
			continue
		}
		g.noteCredits(name, acc.Label, bal.Available, balanceWallets(bal))
	}
}

// prioritizeExpiring 把候选顺序里「窗口内有快过期积分」的账号提到最前（层内按最早
// 到期升序、金额降序），其余账号保持原顺序。没有快照 / 没有快过期积分时原样返回
// ——等价于旧的轮询行为，零回归。
//
// 为什么用「排序」而不是「硬过滤」：openStream 本就是「按顺序试、失败就换下一个」
// 的循环，把快过期号排到最前已经等价于硬优先（在途占满 / 积分耗尽会自然溢出到
// 普通层），又不会在快过期号临时失败时把请求挡死。
func (g *Gateway) prioritizeExpiring(name string, accts []*provider.Account, order []int, now time.Time) []int {
	if g.credits == nil || !g.cfg.CreditRoutingEnabled() {
		return order
	}
	window, minCredits := g.cfg.CreditExpiringSoon(), g.cfg.CreditMinExpiring()
	type cand struct {
		idx      int
		expiring int64
		earliest time.Time
	}
	var hot []cand
	rest := make([]int, 0, len(order))
	for _, idx := range order {
		var acc *provider.Account
		if idx >= 0 && idx < len(accts) {
			acc = accts[idx]
		}
		if acc == nil || acc.Label == "" {
			rest = append(rest, idx)
			continue
		}
		snap, ok := g.credits.get(creditKey(name, acc.Label))
		if !ok {
			rest = append(rest, idx)
			continue
		}
		total, earliest := expiringOf(snap, now, window)
		if total < minCredits {
			rest = append(rest, idx)
			continue
		}
		hot = append(hot, cand{idx: idx, expiring: total, earliest: earliest})
	}
	if len(hot) == 0 {
		return order
	}
	sort.SliceStable(hot, func(i, j int) bool {
		if !hot[i].earliest.Equal(hot[j].earliest) {
			return hot[i].earliest.Before(hot[j].earliest)
		}
		return hot[i].expiring > hot[j].expiring
	})
	out := make([]int, 0, len(order))
	for _, c := range hot {
		out = append(out, c.idx)
	}
	return append(out, rest...)
}
