// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"sort"
	"time"
)

// expiringVirtualSlots 快过期账号在新会话候选集中的虚拟实例权重。
// 3:1 是温和偏好，不是固定比例：账号组成变化会自然改变最终占比。
const expiringVirtualSlots = 3

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// WeightedAvailableUIDsForModelRealm 返回带虚拟实例权重的可用账号列表。
//
// 普通账号出现 1 次；有效快过期账号出现 expiringVirtualSlots 次。重复项按 UID 排序后
// 展开，保证同一账号拓扑下不同进程得到一致列表。
//
// 该方法是现有 AvailableUIDsForModelRealm 的增量入口，不改变旧方法语义，也不
// 修改配置、状态或 Redis schema。prefer_expiring=false 时退化为逐账号一次。
//
// 现状（P1 之后）：新会话的「快过期优先」已由 AssignUIDsForModelRealm 以硬优先层
// 实现，本列表只剩「可用性集合」用途（session.availableSet 会把它转成 set，重复项
// 对集合无影响）。保留重复项是为了不改变既有函数语义与测试口径。
func (p *Pool) WeightedAvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)

	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		slots := 1
		if p.preferExpiring {
			if e := p.byUID[uid]; expiringNow(e, now) {
				slots = expiringVirtualSlots
			}
		}
		for i := 0; i < slots; i++ {
			out = append(out, uid)
		}
	}
	return out
}

// WeightedAvailableUIDsForModelRealms 按 realms 顺序合并各域的加权可用账号（供会话
// 粘性分配用）。各域互斥（一个账号只属一个域），故直接拼接即可，域内虚拟实例重复保留。
// realms 为空 → 退化为不过滤（现状）。
func (p *Pool) WeightedAvailableUIDsForModelRealms(model string, realms []string) []string {
	if len(realms) == 0 {
		return p.WeightedAvailableUIDsForModelRealm(model, "")
	}
	if len(realms) == 1 {
		return p.WeightedAvailableUIDsForModelRealm(model, realms[0])
	}
	var out []string
	for _, realm := range realms {
		out = append(out, p.WeightedAvailableUIDsForModelRealm(model, realm)...)
	}
	return out
}

// availableUIDsForModelRealmLocked 返回 realm 内「该模型可用」的账号 uid（healthyForModel
// 且未占满在途）有序列表。调用方必须已持 p.mu（读锁即可）。
func (p *Pool) availableUIDsForModelRealmLocked(model, realm string, now time.Time) []string {
	uids := make([]string, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if realm != "" && e.a.Realm() != realm {
			continue
		}
		if !e.healthyForModel(now, model) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
//
// 语义是「可用性全集」：会话粘性校验（已绑号是否仍可用）必须用它，不能用
// AssignUIDsForModelRealm——后者会按快过期优先层收窄，用在校验上会让普通号上的
// 既有绑定被判失效而重绑（= 粘性给快过期让路，见 AssignUIDsForModelRealm 注释）。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.availableUIDsForModelRealmLocked(model, realm, time.Now())
}

// bestCostTierOfUIDsLocked 返回 uid 列表里对 model 的最优成本层（0 免费 / 1 无观测 / 2 收费）。
// ok=false 表示列表为空（或所有 uid 都已不在了）。调用方必须已持 p.mu。
//
// 口径与 pickLocked 的成本分层一致（共用 costTierOf）：成本层是最高优先级，故任何
// 收窄候选的决策（快过期优先层）都必须先过这一层，否则会为了烧快过期积分去付费。
func (p *Pool) bestCostTierOfUIDsLocked(uids []string, model string, now time.Time) (int, bool) {
	best, ok := 2, false
	for _, uid := range uids {
		e := p.byUID[uid]
		if e == nil {
			continue
		}
		if ti, _ := costTierOf(e, model, now); !ok || ti < best {
			best, ok = ti, true
		}
	}
	return best, ok
}

// expiringTierInBestCostTierLocked 在 all 里挑出「处于**最优成本层** 且 在快过期优先层」的 uid。
//
// 为什么必须先过成本层（成本层 > 快过期）：线上案例是 cn 的快过期号在
// deepseek-v4.1-flash 上实测收费（tier 2），而 14 个国际版号实测免费（tier 0）。若只按
// 快过期收窄，所有新会话会被绑到那个收费号上，每次请求都在花钱——「能免费就不该为了
// 烧快过期积分去付费」。层为空（最优成本层里没有有效快过期号）时返回 nil，交回调用方
// 退到全集/域序。调用方必须已持 p.mu。
func (p *Pool) expiringTierInBestCostTierLocked(all []string, model string, now time.Time) []string {
	best, ok := p.bestCostTierOfUIDsLocked(all, model, now)
	if !ok {
		return nil
	}
	tier := make([]string, 0, len(all))
	for _, uid := range all {
		e := p.byUID[uid]
		if e == nil {
			continue
		}
		if ti, _ := costTierOf(e, model, now); ti != best {
			continue
		}
		if expiringPreferred(e, now) {
			tier = append(tier, uid)
		}
	}
	return tier
}

// bestCostTierForRealmsLocked 返回 realms 内「当前可服务 reqModel」账号的最优成本层（0/1/2）。
// 可用性判据与 realmHasCapacityLocked 同口径（healthyForModel + 未触积分保底 + 在途未满）——
// 只统计真能被选中的号，否则会被不可用的便宜号误导而白白抖动粘性绑定。
// realms 为空 → 不过滤（全池）。ok=false = 域内没有可服务该模型的号。调用方必须已持 p.mu。
func (p *Pool) bestCostTierForRealmsLocked(reqModel string, realms []string, now time.Time) (int, bool) {
	inRealms := func(realm string) bool {
		if len(realms) == 0 {
			return true
		}
		for _, r := range realms {
			if r == realm {
				return true
			}
		}
		return false
	}
	best, ok := 2, false
	for _, e := range p.byUID {
		if !inRealms(e.a.Realm()) {
			continue
		}
		e.pruneExpiredModelCooldowns(now)
		e.pruneExpiredModelCosts(now)
		healthy := e.healthy(now)
		if reqModel != "" {
			healthy = e.healthyForModel(now, reqModel)
		}
		if !healthy {
			continue
		}
		if p.floorBlockedForRealmModel(e, reqModel, e.a.Realm(), now) {
			continue
		}
		if p.inFlightFull(e) {
			continue
		}
		if ti, _ := costTierOf(e, reqModel, now); !ok || ti < best {
			best, ok = ti, true
		}
	}
	return best, ok
}

// AssignUIDsForModelRealm 会话**首次分配**用的候选列表（P1 快过期硬优先层的粘性侧）：
// 存在快过期优先层成员时只返回它们，否则退回全部可用账号（与 AvailableUIDsForModelRealm 同集）。
//
// 为什么要与可用性校验分开：校验必须用全集，否则已绑在普通号上的会话会被判「不可用」
// 而重绑——那等于让粘性给快过期让路，会换号丢 prompt 缓存（新号按全价重算 prompt），
// 是刻意未启用的另一档策略。本方法只决定「新会话绑到谁」。
//
// 收窄必须先过成本层（成本层 > 快过期，见 expiringTierInBestCostTierLocked）：否则新会话
// 会被绑到一个对该模型收费的快过期号上，粘性侧就绕过了成本分层。
//
// 在途占满的号已被排除，故收窄不会堵死：层内号打满在途后新会话自然落到普通号上。
func (p *Pool) AssignUIDsForModelRealm(model, realm string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	now := time.Now()
	all := p.availableUIDsForModelRealmLocked(model, realm, now)
	if !p.preferExpiring {
		return all
	}
	if tier := p.expiringTierInBestCostTierLocked(all, model, now); len(tier) > 0 {
		return tier
	}
	return all
}

// AssignUIDsForModelRealms 按 realms 顺序合并各域的分配候选（供会话粘性分配用）。
// 各域互斥（一个账号只属一个域），直接拼接即可。realms 为空 → 退化为不过滤。
//
// P6 跨域快过期优先：先看全池（跨域）有没有该模型的快过期优先层成员，有就整份返回——
// 否则新会话会被粘性分配绑到 global，cn 的快过期号永远接不到新会话。层为空时照旧按域序拼接。
func (p *Pool) AssignUIDsForModelRealms(model string, realms []string) []string {
	if len(realms) == 0 {
		return p.AssignUIDsForModelRealm(model, "")
	}
	if len(realms) == 1 {
		return p.AssignUIDsForModelRealm(model, realms[0])
	}
	if tier := p.assignExpiringTierForModel(model); len(tier) > 0 {
		return tier
	}
	var out []string
	for _, realm := range realms {
		out = append(out, p.AssignUIDsForModelRealm(model, realm)...)
	}
	return out
}

// assignExpiringTierForModel 返回全池（跨域）内该模型的快过期优先层 uid（P6 的粘性侧入口）。
// 层口径与 AssignUIDsForModelRealm 一致（preferExpiring 关闭 / 层为空时返回 nil），
// 且同样先过成本层：最优成本层里没有快过期号时返回 nil，调用方退到按域序拼接。
func (p *Pool) assignExpiringTierForModel(model string) []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.preferExpiring {
		return nil
	}
	now := time.Now()
	all := p.availableUIDsForModelRealmLocked(model, "", now)
	return p.expiringTierInBestCostTierLocked(all, model, now)
}
