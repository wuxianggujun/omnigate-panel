// realm_router.go 请求模型名 → 有序候选域列表（config.json 的 realm_routing 段）。
//
// 背景：WorkBuddy 国内版（cn）与国际版（global）是两套上游，同一模型名可能在两域都
// 存在，也可能只在一个域提供。历史协议用 "[realm:]model" 前缀强制指定域，裸名恒走 cn
// （resolveModel）。本路由器把裸名也纳入域选择：按配置优先级（+ 可选目录探测）给出一个
// **有序**域列表；选号侧按顺序取第一个可用域，从而支持「DeepSeek 优先国际版」这类偏好，
// 并在首选域无号时跨域回退。
//
// 兼容：显式 "cn:"/"global:" 前缀仍是硬指定（单元素列表），老客户端零回归。
package server

import (
	"sort"
	"strings"
	"sync"
)

// 路由域枚举（与 auth.Realm() / pool 的 realm 谓词同一套字面量）。
const (
	RealmCN     = "cn"
	RealmGlobal = "global"
)

// defaultRealmOrder 缺省优先级：cn 优先（= 历史「裸名走 cn」行为），global 兜底。
var defaultRealmOrder = []string{RealmCN, RealmGlobal}

// realmRule 单条「模型 → 优先域」规则。模型名支持结尾 "*" 通配（其余部分按前缀匹配）。
type realmRule struct {
	prefix string // 去掉结尾 "*" 的前缀；"" = 匹配全部模型
	realm  string
}

func (r realmRule) matches(model string) bool {
	return strings.HasPrefix(model, r.prefix)
}

// RealmRouter 线程安全：面板热改（Reconfigure）与请求路径（Ordered）并发。
type RealmRouter struct {
	mu    sync.RWMutex
	order []string
	rules []realmRule
}

// NewRealmRouter 构造。order 空/非法 → 回落缺省；prefer 的域非法则忽略该条。
func NewRealmRouter(order []string, prefer map[string]string) *RealmRouter {
	rr := &RealmRouter{}
	rr.Reconfigure(order, prefer)
	return rr
}

// Reconfigure 热替换优先级与规则（面板保存后调用）。语义同 NewRealmRouter。
func (rr *RealmRouter) Reconfigure(order []string, prefer map[string]string) {
	norm := normalizeRealmOrder(order)
	rules := parseRealmRules(prefer)
	rr.mu.Lock()
	rr.order = norm
	rr.rules = rules
	rr.mu.Unlock()
}

// Order 返回当前优先级快照（面板回显/测试用）。
func (rr *RealmRouter) Order() []string {
	rr.mu.RLock()
	defer rr.mu.RUnlock()
	return append([]string(nil), rr.order...)
}

// normalizeRealmOrder 归一化域顺序：仅保留 cn/global、去重；结果为空 → 缺省。
func normalizeRealmOrder(order []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 2)
	for _, r := range order {
		r = strings.TrimSpace(r)
		if r != RealmCN && r != RealmGlobal {
			continue
		}
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	if len(out) == 0 {
		return append([]string(nil), defaultRealmOrder...)
	}
	return out
}

// parseRealmRules 把 prefer 映射解析成有序规则集。规则之间「第一条命中即生效」，为避免
// map 遍历顺序导致不确定：更长前缀（更具体，如 "gpt-5.6-*"）优先于更短（"gpt-*"），
// 同长度按字典序固定。
func parseRealmRules(prefer map[string]string) []realmRule {
	if len(prefer) == 0 {
		return nil
	}
	rules := make([]realmRule, 0, len(prefer))
	for pat, realm := range prefer {
		pat = strings.TrimSpace(pat)
		realm = strings.TrimSpace(realm)
		if pat == "" || (realm != RealmCN && realm != RealmGlobal) {
			continue
		}
		rules = append(rules, realmRule{prefix: strings.TrimSuffix(pat, "*"), realm: realm})
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].prefix) != len(rules[j].prefix) {
			return len(rules[i].prefix) > len(rules[j].prefix)
		}
		return rules[i].prefix < rules[j].prefix
	})
	return rules
}

// Ordered 解析请求模型名 → (bare, realms)。
//
// provides 可为 nil（不做目录过滤）。非 nil 时 provides(realm, bare) 返回 (known, ok)：
// known=该域目录已有数据；ok=该域确实提供该模型。规则：
//   - 已知不提供该模型的域从候选里剔除——MaxRotate 只有 3，把请求浪费在错的域上会
//     直接打光重试次数（global-only 模型在 cn 域连撞 3 个号必失败）；
//   - 但至少保留一个域（目录可能冷、或模型刚上架），剔除后为空则回退完整顺序。
func (rr *RealmRouter) Ordered(model string, provides func(realm, bare string) (known, ok bool)) (string, []string) {
	if realm, bare, ok := splitRealmPrefix(model); ok {
		return bare, []string{realm}
	}
	rr.mu.RLock()
	order := append([]string(nil), rr.order...)
	rules := append([]realmRule(nil), rr.rules...)
	rr.mu.RUnlock()

	for _, rule := range rules {
		if rule.matches(model) {
			order = moveRealmFront(order, rule.realm)
			break
		}
	}
	if provides != nil {
		kept := make([]string, 0, len(order))
		for _, r := range order {
			known, ok := provides(r, model)
			if known && !ok {
				continue
			}
			kept = append(kept, r)
		}
		if len(kept) > 0 {
			order = kept
		}
	}
	return model, order
}

// moveRealmFront 把 realm 移到列表首位——**仅当它已在候选列表里**。order 定义了允许的
// 域集合，prefer 只调整优先级、不得把未启用的域重新引入（例如 global.enabled=false 时
// order 只剩 ["cn"]，此时 prefer→global 必须被忽略，否则逃生门被绕过）。
func moveRealmFront(order []string, realm string) []string {
	found := false
	for _, r := range order {
		if r == realm {
			found = true
			break
		}
	}
	if !found {
		return order
	}
	out := make([]string, 0, len(order))
	out = append(out, realm)
	for _, r := range order {
		if r != realm {
			out = append(out, r)
		}
	}
	return out
}

// splitRealmPrefix 解析 "[realm:]model" 显式前缀。命中返回 (realm, bare, true)。
func splitRealmPrefix(model string) (realm, bare string, ok bool) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "", model, false
	}
	prefix := model[:idx]
	if prefix != RealmCN && prefix != RealmGlobal {
		return "", model, false
	}
	return prefix, model[idx+1:], true
}
