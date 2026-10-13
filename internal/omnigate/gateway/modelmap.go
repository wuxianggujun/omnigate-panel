package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
)

// Validation errors for Set (mirrors the reference's 400 responses).
var (
	errModelMappingEmpty = errors.New("alias 与 target 均不能为空")
	errModelMappingSelf  = errors.New("alias 不能等于 target")
)

// MappingTargets is the ordered target list for one alias. The first entry is
// the primary target (what route() resolves to); any further entries are
// automatic fallbacks, tried in order when the primary provider has no usable
// account, is out of credits, or fails outright (见 fallback.go 的 tryFallback).
//
// It (un)marshals as a bare string when there is exactly one target, so the
// original single-target table — and the on-disk/API shape every existing
// consumer expects — stays byte-for-byte compatible.
type MappingTargets []string

func (t *MappingTargets) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		var arr []string
		if err := json.Unmarshal(b, &arr); err != nil {
			return err
		}
		*t = arr
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	*t = []string{s}
	return nil
}

func (t MappingTargets) MarshalJSON() ([]byte, error) {
	if len(t) == 1 {
		return json.Marshal(t[0])
	}
	return json.Marshal([]string(t))
}

// sanitizeTargets trims, drops empties and de-duplicates while preserving order.
func sanitizeTargets(raw []string) []string {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, t := range raw {
		t = strings.TrimSpace(t)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// dropSelf removes targets equal to the alias (a self-mapping would loop).
func dropSelf(alias string, targets []string) []string {
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if t == alias {
			continue
		}
		out = append(out, t)
	}
	return out
}

// normalizeAliasTargets validates and canonicalises one alias → targets entry.
// It returns the cleaned chain, or an error when the entry is invalid (empty
// alias/targets, or every target equals the alias).
func normalizeAliasTargets(alias string, raw []string) ([]string, error) {
	if strings.TrimSpace(alias) == "" {
		return nil, errModelMappingEmpty
	}
	sanitized := sanitizeTargets(raw)
	if len(sanitized) == 0 {
		return nil, errModelMappingEmpty
	}
	clean := dropSelf(strings.TrimSpace(alias), sanitized)
	if len(clean) == 0 {
		return nil, errModelMappingSelf
	}
	return clean, nil
}

// ModelMappingStore persists a client-facing model alias → target model chain,
// ported from gemini2api's app/core/model_mapping.py. An operator can point a
// request model at a different upstream model (e.g. "gpt-4o" →
// "geminiweb/gemini-pro") without touching any client. The target is resolved
// before routing, so it may be a fully-qualified "provider/model" or a bare name.
//
// An alias may also map to a *chain* of targets: the request first goes to the
// first target and, if that provider fails or runs out of credits, the gateway
// automatically retries the following ones. This is what lets an operator
// "drain" an expiring account (e.g. prefer raccoon, fall back to runable).
//
// 每条映射还能单独「停用」：停用只是把这条规则从路由里摘掉，配置本身留着，
// 随时可以再打开。之所以需要它——引流链是硬规则，一旦配上就处处盖过
// routing.prefer（别名优先于 prefer，见 gateway.route），用户发现被强推到某个
// 供应商时，除了把整条链删掉再重新配一遍之外没有别的退路。停用就是那个退路。
//
// A zero/nil store is inert: Resolve returns the input unchanged.
type ModelMappingStore struct {
	mu   sync.RWMutex
	path string
	m    map[string]MappingTargets
	// disabled 是被停用的别名集合。刻意与 m 分开存而不是塞进 m 的值里：
	// m 的序列化形状（裸字符串 / 字符串数组）是所有既有消费者和线上数据约定的，
	// 为加一个开关把它变成对象会让存量数据一次性全部不兼容。
	disabled map[string]bool
	// 可选 KV 后端（PostgreSQL）：非空且 key 非空时库为权威。见 SetBackend。
	backend    kv.Backend
	backendKey string
}

// NewModelMappingStore constructs the store and loads it from disk (missing file
// = empty table). An empty path keeps it memory-only.
func NewModelMappingStore(path string) *ModelMappingStore {
	s := &ModelMappingStore{path: path, m: map[string]MappingTargets{}, disabled: map[string]bool{}}
	s.load()
	return s
}

// SetBackend 指定模型映射的 KV 后端（PostgreSQL）。库为权威：命中即用库，未命中则
// 把当前（文件载入的）映射导入库；之后落盘写库、不再写文件。b==nil 或 key=="" 时
// 退化为纯文件模式。
func (s *ModelMappingStore) SetBackend(b kv.Backend, key string) error {
	if s == nil || b == nil || key == "" {
		return nil
	}
	s.mu.Lock()
	s.backend = b
	s.backendKey = key
	s.mu.Unlock()
	raw, found, err := b.Load(key)
	if err != nil {
		return err
	}
	if found {
		targets, off := loadPersisted(raw)
		clean := map[string]MappingTargets{}
		for alias, chain := range targets {
			c, err := normalizeAliasTargets(alias, chain)
			if err != nil {
				continue
			}
			clean[strings.TrimSpace(alias)] = c
		}
		s.mu.Lock()
		s.m = clean
		s.disabled = map[string]bool{}
		for a := range off {
			s.disabled[a] = true
		}
		s.mu.Unlock()
		return nil
	}
	// 首次启用：把文件里的映射整体导入库。
	s.saveLocked()
	return nil
}

func (s *ModelMappingStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	targets, off := loadPersisted(raw)
	for alias, chain := range targets {
		clean, err := normalizeAliasTargets(alias, chain)
		if err != nil {
			continue
		}
		s.m[strings.TrimSpace(alias)] = clean
	}
	for a := range off {
		s.disabled[a] = true
	}
}

// persisted 是落盘/落库的形状。刻意包一层而不是直接把数组写顶层：
// 顶层那个 map 是「别名 → 目标链」的既有约定（含裸字符串单目标的形态），
// 所有存量数据、外部脚本都按它读；停用集合只能另开一个字段，否则加一个开关
// 就要让全部历史数据一次性变得读不出来。
type persisted struct {
	Targets  map[string]MappingTargets `json:"targets"`
	Disabled []string                  `json:"disabled,omitempty"`
}

// loadPersisted 兼读新旧两种形状：新形状是 {targets:{...},disabled:[...]}，
// 旧形状就是裸的「别名 → 目标」map。旧数据读进来时停用集合为空（全都当启用）。
func loadPersisted(raw []byte) (map[string]MappingTargets, map[string]bool) {
	var p persisted
	if err := json.Unmarshal(raw, &p); err == nil && p.Targets != nil {
		off := map[string]bool{}
		for _, a := range p.Disabled {
			off[strings.TrimSpace(a)] = true
		}
		return p.Targets, off
	}
	var flat map[string]MappingTargets
	if err := json.Unmarshal(raw, &flat); err != nil {
		return nil, nil
	}
	return flat, map[string]bool{}
}

// saveLocked atomically persists the table. Caller must hold the write lock.
func (s *ModelMappingStore) saveLocked() {
	if s == nil || (s.path == "" && (s.backend == nil || s.backendKey == "")) {
		return
	}
	p := persisted{Targets: s.m}
	for a, off := range s.disabled {
		if off {
			p.Disabled = append(p.Disabled, a)
		}
	}
	sort.Strings(p.Disabled) // 稳定输出：否则每次保存数组顺序抖一下，diff 全是噪声
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return
	}
	if s.backend != nil && s.backendKey != "" {
		if err := s.backend.Save(s.backendKey, raw); err != nil {
			log.Printf("[omnigate] 模型映射写后端失败: %v", err)
		}
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

// All returns a copy of the alias → target-chain table, including那些被停用的
// 规则（调用方要能看到完整配置才能展示「已停用」状态）。被停用的别名也在结果
// 里，值不变。
func (s *ModelMappingStore) All() map[string]MappingTargets {
	out := map[string]MappingTargets{}
	if s == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.m {
		out[k] = append(MappingTargets(nil), v...)
	}
	return out
}

// DisabledAliases 返回被停用的别名集合（调用方不可改）。
func (s *ModelMappingStore) DisabledAliases() map[string]bool {
	out := map[string]bool{}
	if s == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.disabled {
		out[k] = v
	}
	return out
}

// IsDisabled 报告某条规则是否被停用。停用的规则在 Resolve/Chain 里一律当不存在
// 处理，相当于「这条规则临时没配过」——但配置留着，SetEnabled(false) 可随时恢复。
func (s *ModelMappingStore) IsDisabled(alias string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.disabled[strings.TrimSpace(alias)]
}

// SetEnabled 打开或停用某条已存在的规则。返回 false 表示别名不存在（不让通过
// 这个接口凭空造出一条空规则）。停用不删配置，也不影响别的规则。
func (s *ModelMappingStore) SetEnabled(alias string, on bool) bool {
	if s == nil {
		return false
	}
	alias = strings.TrimSpace(alias)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[alias]; !ok {
		return false
	}
	if on {
		delete(s.disabled, alias)
	} else {
		s.disabled[alias] = true
	}
	s.saveLocked()
	return true
}

// Resolve maps a model name to its primary target, or returns it unchanged when
// there is no mapping (or the mapping is disabled).
func (s *ModelMappingStore) Resolve(model string) string {
	if s == nil {
		return model
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.disabled[model] {
		return model
	}
	if t, ok := s.m[model]; ok && len(t) > 0 {
		return t[0]
	}
	return model
}

// Chain returns the full ordered target list for a model (primary first), or nil
// when the model is not mapped or the mapping is disabled. The caller must not
// mutate the result.
func (s *ModelMappingStore) Chain(model string) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.disabled[model] {
		return nil
	}
	t, ok := s.m[model]
	if !ok || len(t) == 0 {
		return nil
	}
	return append([]string(nil), t...)
}

// Set writes (or overwrites) a single-target alias → target mapping. It rejects
// an empty alias/target or alias == target (mirrors the reference's 400 guard).
func (s *ModelMappingStore) Set(alias, target string) error {
	return s.SetTargets(alias, []string{target})
}

// SetTargets writes (or overwrites) an alias → ordered target chain. The first
// target is the primary; the rest are automatic fallbacks. Empty entries are
// dropped, duplicates de-duplicated, and targets equal to the alias rejected.
//
// 重存一条规则时不会顺手把它停用掉（运维改目标链是常事，不该连带丢掉开关状态），
// 想停用请显式调 SetEnabled。
func (s *ModelMappingStore) SetTargets(alias string, targets []string) error {
	if s == nil {
		return nil
	}
	clean, err := normalizeAliasTargets(alias, targets)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.m[strings.TrimSpace(alias)] = clean
	s.saveLocked()
	s.mu.Unlock()
	return nil
}

// Delete removes a mapping. Returns whether it existed. 停用状态随之清除。
func (s *ModelMappingStore) Delete(alias string) bool {
	if s == nil {
		return false
	}
	alias = strings.TrimSpace(alias)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[alias]; !ok {
		return false
	}
	delete(s.m, alias)
	delete(s.disabled, alias) // 别把停用标记留在表外，将来同名别名会被莫名停用
	s.saveLocked()
	return true
}

// Aliases returns the sorted alias list (for /v1/models exposure).
func (s *ModelMappingStore) Aliases() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.m))
	for k := range s.m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ModelMappings returns the gateway's global alias store (may be nil when the
// gateway was built without a data dir; every store method is nil-safe).
func (g *Gateway) ModelMappings() *ModelMappingStore { return g.modelMap }

// SetModelMapping writes one global single-target alias → target mapping.
func (g *Gateway) SetModelMapping(alias, target string) error {
	if g.modelMap == nil {
		return errors.New("模型映射存储未启用")
	}
	return g.modelMap.Set(alias, target)
}

// SetModelMappingTargets writes one global alias → ordered target chain.
func (g *Gateway) SetModelMappingTargets(alias string, targets []string) error {
	if g.modelMap == nil {
		return errors.New("模型映射存储未启用")
	}
	return g.modelMap.SetTargets(alias, targets)
}

// DeleteModelMapping removes one global alias mapping.
func (g *Gateway) DeleteModelMapping(alias string) bool {
	if g.modelMap == nil {
		return false
	}
	return g.modelMap.Delete(alias)
}

// SetModelMappingEnabled 停用/启用一条已存在的规则。停用 = 路由时当它不存在
// （按 routing.prefer 走），但别名和目标链都留着，随时能恢复。
func (g *Gateway) SetModelMappingEnabled(alias string, on bool) bool {
	if g.modelMap == nil {
		return false
	}
	return g.modelMap.SetEnabled(alias, on)
}
