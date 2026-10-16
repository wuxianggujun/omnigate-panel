package gateway

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
)

// keyHealthStore 持久化上游密钥池的健康状态（自动禁用 / 连击记账，见
// openaibackend.Health）：热重载重建 provider、容器重启后，被禁用的死 key
// 不会「复活」重新进轮换。与 modelmap.go 同范式：KV 后端（PostgreSQL）为权威，
// 文件仅首次导入 + 断库兜底。数据量小（每 provider 几把键），全量保存。
//
// 落盘形状：{"<provider>": {"<KeyID>": {Health}}}。KeyID 是 sha256 前 8 字节
// hex——状态里不存明文 key。
type keyHealthStore struct {
	mu         sync.Mutex
	path       string
	data       map[string]map[string]openaibackend.Health
	backend    kv.Backend
	backendKey string
}

// newKeyHealthStore constructs the store and loads it from disk (missing file =
// empty). An empty path keeps it memory-only.
func newKeyHealthStore(path string) *keyHealthStore {
	s := &keyHealthStore{path: path, data: map[string]map[string]openaibackend.Health{}}
	s.load()
	return s
}

func (s *keyHealthStore) load() {
	if s == nil || s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var m map[string]map[string]openaibackend.Health
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return
	}
	s.data = m
}

// SetBackend 指定健康状态的 KV 后端（PostgreSQL）。库为权威：命中即用库，
// 未命中把当前（文件载入的）状态导入库；之后写库、不再写文件。b==nil 或
// key=="" 时退化为纯文件模式。
func (s *keyHealthStore) SetBackend(b kv.Backend, key string) error {
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
		var m map[string]map[string]openaibackend.Health
		if err := json.Unmarshal(raw, &m); err == nil && m != nil {
			s.mu.Lock()
			s.data = m
			s.mu.Unlock()
			return nil
		}
	}
	s.saveLocked() // 首次启用：把文件里的状态导入库
	return nil
}

// LoadHealth 返回某 provider 的全部健康状态（深拷贝，可安全读）。
func (s *keyHealthStore) LoadHealth(provider string) map[string]openaibackend.Health {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]openaibackend.Health, len(s.data[provider]))
	for id, h := range s.data[provider] {
		out[id] = h
	}
	return out
}

// SaveHealth 写入 / 更新一条健康状态（KeyID 引用）。
func (s *keyHealthStore) SaveHealth(provider, id string, h openaibackend.Health) {
	if s == nil || provider == "" || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data[provider] == nil {
		s.data[provider] = map[string]openaibackend.Health{}
	}
	s.data[provider][id] = h
	s.saveLocked()
}

// DropHealth 删除一条健康状态（key 恢复 / 已不在池里）。
func (s *keyHealthStore) DropHealth(provider, id string) {
	if s == nil || provider == "" || id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.data[provider]
	if m == nil {
		return
	}
	if _, ok := m[id]; !ok {
		return
	}
	delete(m, id)
	if len(m) == 0 {
		delete(s.data, provider)
	}
	s.saveLocked()
}

// Snapshot 返回全量健康状态（深拷贝），供面板展示与自动清理判定。
func (s *keyHealthStore) Snapshot() map[string]map[string]openaibackend.Health {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]map[string]openaibackend.Health, len(s.data))
	for prov, m := range s.data {
		mm := make(map[string]openaibackend.Health, len(m))
		for id, h := range m {
			mm[id] = h
		}
		out[prov] = mm
	}
	return out
}

// saveLocked 原子落盘 / 落库。调用方必须持锁。
func (s *keyHealthStore) saveLocked() {
	if s == nil || (s.path == "" && (s.backend == nil || s.backendKey == "")) {
		return
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return
	}
	if s.backend != nil && s.backendKey != "" {
		if err := s.backend.Save(s.backendKey, raw); err != nil {
			log.Printf("[omnigate] 密钥健康状态写后端失败: %v", err)
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
