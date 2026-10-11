// ownership.go 账号归属表（面板多租户的「谁的账号」映射）。
//
// 设计要点：
//   - **纯旁路**：不写进 auths/*.json、也不写进 omnigate.json——凭证/配置格式零
//     改动，删号时顺手清一条记录即可；归属表损坏只会退化成「全部无主」，绝不会
//     影响账号本身的可用性。
//   - **稳定 key**：WorkBuddy = "wb:<uid>"，OmniGate = "omni:<provider>/<label>"。
//     uid / provider+label 是这两个池既有的身份口径（面板各处都在用）。
//   - **空值语义**：没有记录的账号 = 无主/共享——管理员与 viewer 可见可运维，
//     普通 user 看不到（这样存量账号天然只属于管理员）。
//
// 落盘：与 state 文件同目录的 ownership.json（原子写 tmp+rename）。
package panel

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// ownershipVersion 归属表文件版本（未来格式变更时用于迁移）。
const ownershipVersion = 1

// wbOwnerKey WorkBuddy 账号归属键。
func wbOwnerKey(uid string) string { return "wb:" + uid }

// omniOwnerKey OmniGate 账号归属键。
func omniOwnerKey(provider, label string) string { return "omni:" + provider + "/" + label }

// ownershipFile 归属表磁盘形态。
type ownershipFile struct {
	Version int               `json:"version"`
	Owners  map[string]string `json:"owners"`
}

// ownershipStore 账号归属表（内存 + 原子落盘）。path 为空 = 纯内存（测试用）。
type ownershipStore struct {
	mu   sync.RWMutex
	path string
	m    map[string]string
}

// newOwnershipStore 构造归属表；path 非空时尝试加载（失败只记为空表，不报错）。
func newOwnershipStore(path string) *ownershipStore {
	s := &ownershipStore{path: path, m: map[string]string{}}
	s.load()
	return s
}

// load 从磁盘读取归属表。文件缺失/损坏都退化为空表（不阻断面板启动）。
func (s *ownershipStore) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f ownershipFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return
	}
	if f.Owners != nil {
		s.m = f.Owners
	}
}

// Owner 返回账号归属者（空串 = 无主/共享）。
func (s *ownershipStore) Owner(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.m[key]
}

// Set 设置归属（owner 为空 = 清除记录）。
func (s *ownershipStore) Set(key, owner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner == "" {
		delete(s.m, key)
	} else {
		s.m[key] = owner
	}
	s.saveLocked()
}

// Remove 删除一条归属记录（账号被移除时调用）。
func (s *ownershipStore) Remove(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.m[key]; !ok {
		return
	}
	delete(s.m, key)
	s.saveLocked()
}

// DeleteUser 清除某面板用户拥有的全部归属记录（面板账号被删除时调用）。
func (s *ownershipStore) DeleteUser(username string) {
	if username == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k, v := range s.m {
		if v == username {
			delete(s.m, k)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
}

// KeysOf 返回某用户拥有的全部账号键（快照）。
func (s *ownershipStore) KeysOf(username string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for k, v := range s.m {
		if v == username {
			out = append(out, k)
		}
	}
	return out
}

// saveLocked 原子落盘（持锁调用）。无 path（测试）时为空操作；写失败只记日志。
func (s *ownershipStore) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.MarshalIndent(ownershipFile{Version: ownershipVersion, Owners: s.m}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}
