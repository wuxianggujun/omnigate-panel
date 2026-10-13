package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Validation errors for Set (mirrors the reference's 400 responses).
var (
	errModelMappingEmpty = errors.New("alias 与 target 均不能为空")
	errModelMappingSelf  = errors.New("alias 不能等于 target")
)

// ModelMappingStore persists a client-facing model alias → target model mapping,
// ported from gemini2api's app/core/model_mapping.py. An operator can point a
// request model at a different upstream model (e.g. "gpt-4o" →
// "geminiweb/gemini-pro") without touching any client. The target is resolved
// before routing, so it may be a fully-qualified "provider/model" or a bare name.
//
// A zero/nil store is inert: Resolve returns the input unchanged.
type ModelMappingStore struct {
	mu   sync.RWMutex
	path string
	m    map[string]string
}

// NewModelMappingStore constructs the store and loads it from disk (missing file
// = empty table). An empty path keeps it memory-only.
func NewModelMappingStore(path string) *ModelMappingStore {
	s := &ModelMappingStore{path: path, m: map[string]string{}}
	s.load()
	return s
}

func (s *ModelMappingStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var onDisk map[string]string
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return
	}
	for alias, target := range onDisk {
		alias = strings.TrimSpace(alias)
		target = strings.TrimSpace(target)
		if alias == "" || target == "" || alias == target {
			continue
		}
		s.m[alias] = target
	}
}

// saveLocked atomically persists the table. Caller must hold the write lock.
func (s *ModelMappingStore) saveLocked() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
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

// All returns a copy of the alias → target table.
func (s *ModelMappingStore) All() map[string]string {
	out := map[string]string{}
	if s == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for k, v := range s.m {
		out[k] = v
	}
	return out
}

// Resolve maps a model name to its target, or returns it unchanged when there is
// no mapping.
func (s *ModelMappingStore) Resolve(model string) string {
	if s == nil {
		return model
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if t, ok := s.m[model]; ok {
		return t
	}
	return model
}

// Set writes (or overwrites) an alias → target mapping. It rejects an empty
// alias/target or alias == target (mirrors the reference's 400 guard).
func (s *ModelMappingStore) Set(alias, target string) error {
	if s == nil {
		return nil
	}
	alias = strings.TrimSpace(alias)
	target = strings.TrimSpace(target)
	if alias == "" || target == "" {
		return errModelMappingEmpty
	}
	if alias == target {
		return errModelMappingSelf
	}
	s.mu.Lock()
	s.m[alias] = target
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

// SetModelMapping writes one global alias → target mapping.
func (g *Gateway) SetModelMapping(alias, target string) error {
	if g.modelMap == nil {
		return errors.New("模型映射存储未启用")
	}
	return g.modelMap.Set(alias, target)
}

// DeleteModelMapping removes one global alias mapping.
func (g *Gateway) DeleteModelMapping(alias string) bool {
	if g.modelMap == nil {
		return false
	}
	return g.modelMap.Delete(alias)
}
