package geminiweb

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// GemMapping binds a client-facing model name to a custom Gem on a specific
// account. Ported from gemini2api's app/core/gem_mapping.py (issue #2): it lets a
// caller invoke a saved Gem by name — e.g. POST model "code-reviewer" instead of
// "gemini-pro" — and pins the request to the account that owns the Gem, because a
// gem_id is private to one Google account.
type GemMapping struct {
	// Model is the client-facing alias (the key). Filled on read.
	Model string `json:"model"`
	// GemID is the upstream Gem id the alias resolves to.
	GemID string `json:"gem_id"`
	// BaseModel is the real model the Gem runs on (default "gemini-pro").
	BaseModel string `json:"base_model"`
	// AccountID is the label of the account that owns the Gem.
	AccountID string `json:"account_id"`
}

// gemMappingDisk 是落盘形态：{model: {gem_id, base_model, account_id}}，与参考
// 实现的 gem-mapping.json 完全一致（键即模型名，故不再冗余存 Model 字段）。
type gemMappingDisk struct {
	GemID     string `json:"gem_id"`
	BaseModel string `json:"base_model"`
	AccountID string `json:"account_id"`
}

// defaultGemBaseModel 是映射未指定底层模型时的兜底。
const defaultGemBaseModel = "gemini-pro"

// GemMappingStore 持久化「模型名 -> GemMapping」。空路径 = 只存内存（不落盘）。
// 读写都加锁：网关可能并发地从聊天请求解析、从管理 RPC 修改。
type GemMappingStore struct {
	mu   sync.RWMutex
	path string
	m    map[string]GemMapping
}

// NewGemMappingStore 构造并立即从磁盘载入（文件不存在 = 空表）。
func NewGemMappingStore(path string) *GemMappingStore {
	s := &GemMappingStore{path: path, m: map[string]GemMapping{}}
	s.load()
	return s
}

func (s *GemMappingStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return // 首次启动没有文件，属正常
	}
	var onDisk map[string]gemMappingDisk
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		return
	}
	for name, v := range onDisk {
		name = strings.TrimSpace(name)
		if name == "" || strings.TrimSpace(v.GemID) == "" {
			continue
		}
		if strings.TrimSpace(v.BaseModel) == "" {
			v.BaseModel = defaultGemBaseModel
		}
		s.m[name] = GemMapping{
			Model:     name,
			GemID:     v.GemID,
			BaseModel: v.BaseModel,
			AccountID: strings.TrimSpace(v.AccountID),
		}
	}
}

// saveLocked 原子落盘（先写临时文件再改名）。调用方须持写锁。
func (s *GemMappingStore) saveLocked() {
	if s == nil || s.path == "" {
		return
	}
	out := make(map[string]gemMappingDisk, len(s.m))
	for name, v := range s.m {
		out[name] = gemMappingDisk{GemID: v.GemID, BaseModel: v.BaseModel, AccountID: v.AccountID}
	}
	raw, err := json.MarshalIndent(out, "", "  ")
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

// All 返回全部映射（按模型名排序）。
func (s *GemMappingStore) All() []GemMapping {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]GemMapping, 0, len(s.m))
	for _, v := range s.m {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// Resolve 查一个模型名的映射。第二个返回值为 false 表示没有该映射。
func (s *GemMappingStore) Resolve(model string) (GemMapping, bool) {
	if s == nil {
		return GemMapping{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.m[model]
	return v, ok
}

// Set 写入/覆盖一条映射并落盘。模型名为空或 gem_id 为空时忽略（返回 false）。
func (s *GemMappingStore) Set(model string, v GemMapping) bool {
	if s == nil {
		return false
	}
	model = strings.TrimSpace(model)
	if model == "" || strings.TrimSpace(v.GemID) == "" {
		return false
	}
	if strings.TrimSpace(v.BaseModel) == "" {
		v.BaseModel = defaultGemBaseModel
	}
	v.Model = model
	v.AccountID = strings.TrimSpace(v.AccountID)
	s.mu.Lock()
	s.m[model] = v
	s.saveLocked()
	s.mu.Unlock()
	return true
}

// Delete 删除一条映射。返回是否真的删掉了。
func (s *GemMappingStore) Delete(model string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[model]; !ok {
		return false
	}
	delete(s.m, model)
	s.saveLocked()
	return true
}

// DeleteByGem 删除所有指向同一 (gem_id, account_id) 的映射，返回被删的模型名。
// 用于删除 Gem 时联动清理：避免留下指向已失效 Gem 的死模型名。
func (s *GemMappingStore) DeleteByGem(gemID, accountID string) []string {
	if s == nil {
		return nil
	}
	gemID = strings.TrimSpace(gemID)
	accountID = strings.TrimSpace(accountID)
	if gemID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var removed []string
	for name, v := range s.m {
		if v.GemID == gemID && strings.TrimSpace(v.AccountID) == accountID {
			removed = append(removed, name)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	for _, name := range removed {
		delete(s.m, name)
	}
	s.saveLocked()
	sort.Strings(removed)
	return removed
}
