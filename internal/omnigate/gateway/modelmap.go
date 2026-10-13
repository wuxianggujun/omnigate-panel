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
// A zero/nil store is inert: Resolve returns the input unchanged.
type ModelMappingStore struct {
	mu   sync.RWMutex
	path string
	m    map[string]MappingTargets
	// 可选 KV 后端（PostgreSQL）：非空且 key 非空时库为权威。见 SetBackend。
	backend    kv.Backend
	backendKey string
}

// NewModelMappingStore constructs the store and loads it from disk (missing file
// = empty table). An empty path keeps it memory-only.
func NewModelMappingStore(path string) *ModelMappingStore {
	s := &ModelMappingStore{path: path, m: map[string]MappingTargets{}}
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
		var onDisk map[string]MappingTargets
		if err := json.Unmarshal(raw, &onDisk); err == nil {
			clean := map[string]MappingTargets{}
			for alias, targets := range onDisk {
				chain, err := normalizeAliasTargets(alias, targets)
				if err != nil {
					continue
				}
				clean[strings.TrimSpace(alias)] = chain
			}
			s.mu.Lock()
			s.m = clean
			s.mu.Unlock()
		}
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
	var onDisk map[string]MappingTargets
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return
	}
	for alias, targets := range onDisk {
		chain, err := normalizeAliasTargets(alias, targets)
		if err != nil {
			continue
		}
		s.m[strings.TrimSpace(alias)] = chain
	}
}

// saveLocked atomically persists the table. Caller must hold the write lock.
func (s *ModelMappingStore) saveLocked() {
	if s == nil || (s.path == "" && (s.backend == nil || s.backendKey == "")) {
		return
	}
	raw, err := json.MarshalIndent(s.m, "", "  ")
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

// All returns a copy of the alias → target-chain table.
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

// Resolve maps a model name to its primary target, or returns it unchanged when
// there is no mapping.
func (s *ModelMappingStore) Resolve(model string) string {
	if s == nil {
		return model
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.m[model]; ok && len(t) > 0 {
		return t[0]
	}
	return model
}

// Chain returns the full ordered target list for a model (primary first), or nil
// when the model is not mapped. The caller must not mutate the result.
func (s *ModelMappingStore) Chain(model string) []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// Delete removes a mapping. Returns whether it existed.
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
