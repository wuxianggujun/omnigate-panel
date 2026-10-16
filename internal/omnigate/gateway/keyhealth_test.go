package gateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
)

// fakeKV 内存 KV 后端（实现 kv.Backend），验证「库为权威」的导入语义。
type fakeKV struct {
	m map[string][]byte
}

func (f *fakeKV) Load(key string) ([]byte, bool, error) {
	v, ok := f.m[key]
	return v, ok, nil
}

func (f *fakeKV) Save(key string, data []byte) error {
	f.m[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeKV) Delete(key string) error {
	delete(f.m, key)
	return nil
}

// TestKeyHealthStoreFileRoundTrip 文件往返：SaveHealth 落盘 → 新 store 载回 →
// DropHealth 清掉后也持久化。KeyID 引用，文件里不得出现明文 key。
func TestKeyHealthStoreFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key-health.json")
	s := newKeyHealthStore(path)
	if got := s.LoadHealth("atria"); len(got) != 0 {
		t.Fatalf("空 store 不该有记录: %v", got)
	}

	const rawKey = "atr_ThisIsTheFullSecretKey_DoNotStore"
	dead := openaibackend.Health{
		Reason: "quota", Disabled: true, Until: time.Now().Add(24 * time.Hour).Unix(),
		Strikes: 2, FirstSeen: time.Now().Add(-8 * 24 * time.Hour).Unix(),
		LastError: "upstream HTTP 429: insufficient_quota",
	}
	s.SaveHealth("atria", openaibackend.KeyID(rawKey), dead)

	// 落盘文件里必须是 KeyID 而非明文。
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取落盘文件: %v", err)
	}
	if strings.Contains(string(raw), rawKey) {
		t.Fatal("健康状态文件里出现了明文 key")
	}

	s2 := newKeyHealthStore(path)
	h, ok := s2.LoadHealth("atria")[openaibackend.KeyID(rawKey)]
	if !ok || !h.Disabled || h.Strikes != 2 || h.Reason != "quota" {
		t.Fatalf("载回状态不对: %+v (ok=%v)", h, ok)
	}

	// Drop 也是持久化的。
	s2.DropHealth("atria", openaibackend.KeyID(rawKey))
	if got := newKeyHealthStore(path).LoadHealth("atria"); len(got) != 0 {
		t.Fatalf("Drop 后不该有记录: %v", got)
	}
	// 再 Drop 一次（已不存在）不 panic、不写坏文件。
	s2.DropHealth("atria", openaibackend.KeyID(rawKey))
}

// TestKeyHealthStoreBackendAuthoritative 库为权威：首次启用把文件状态导入库；
// 库命中后以库为准（完全替换），之后写操作只写库、不再动文件。
func TestKeyHealthStoreBackendAuthoritative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key-health.json")
	if err := os.WriteFile(path, []byte(`{"atria":{"deadid":{"strikes":1,"disabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// ① 首次启用（库为空）：文件状态导入库。
	store := newKeyHealthStore(path)
	back := &fakeKV{m: map[string][]byte{}}
	if err := store.SetBackend(back, "omnigate_key_health"); err != nil {
		t.Fatalf("SetBackend: %v", err)
	}
	raw, ok, _ := back.Load("omnigate_key_health")
	if !ok || !strings.Contains(string(raw), "deadid") {
		t.Fatalf("首次启用应把文件状态导入库，实际 %s (ok=%v)", raw, ok)
	}

	// ② 库命中：以库为准，文件里的旧数据不再混入。
	back.m["omnigate_key_health"] = []byte(`{"atria":{"livedid":{"strikes":5}}}`)
	store2 := newKeyHealthStore(path)
	if err := store2.SetBackend(back, "omnigate_key_health"); err != nil {
		t.Fatal(err)
	}
	m := store2.LoadHealth("atria")
	if _, ok := m["livedid"]; !ok {
		t.Fatalf("库命中时应以库为准: %v", m)
	}
	if _, ok := m["deadid"]; ok {
		t.Fatalf("库命中时不该混入文件旧数据: %v", m)
	}

	// ③ 之后写操作只写库，不再改文件。
	store2.SaveHealth("atria", "newid", openaibackend.Health{Strikes: 3})
	fileRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fileRaw), "newid") {
		t.Fatal("启用库后端后不该再写文件")
	}
	libRaw, _, _ := back.Load("omnigate_key_health")
	if !strings.Contains(string(libRaw), "newid") {
		t.Fatal("写操作应落库")
	}
}

// TestKeyHealthStoreSnapshotIsolated Snapshot 是深拷贝：拿到的副本怎么改都不影响
// store 本体（面板读数据与清理判定的并发安全前提）。
func TestKeyHealthStoreSnapshotIsolated(t *testing.T) {
	s := newKeyHealthStore("") // 纯内存
	s.SaveHealth("p1", "id1", openaibackend.Health{Strikes: 1, FirstSeen: 100})
	snap := s.Snapshot()
	if h := snap["p1"]["id1"]; h.Strikes != 1 {
		t.Fatalf("快照丢失状态: %+v", h)
	}
	snap["p1"]["id1"] = openaibackend.Health{Strikes: 99}
	snap["p1"]["id2"] = openaibackend.Health{Strikes: 7}
	delete(snap, "p1")
	if got := s.LoadHealth("p1"); len(got) != 1 || got["id1"].Strikes != 1 {
		t.Fatalf("副本变更泄漏回 store: %v", got)
	}
}

// TestKeyHealthStoreEmptyPathNoPersist 空路径纯内存：不落盘、不 panic（网关未配
// DataDir 时的退化形态）。
func TestKeyHealthStoreEmptyPathNoPersist(t *testing.T) {
	s := newKeyHealthStore("")
	s.SaveHealth("p1", "id1", openaibackend.Health{Strikes: 2})
	if got := s.LoadHealth("p1")["id1"]; got.Strikes != 2 {
		t.Fatalf("纯内存模式读写不一致: %+v", got)
	}
	// nil store 的安全兜底。
	var nilStore *keyHealthStore
	nilStore.SaveHealth("p1", "id1", openaibackend.Health{})
	nilStore.DropHealth("p1", "id1")
	if nilStore.Snapshot() != nil {
		t.Fatal("nil store 快照应为 nil")
	}
}
