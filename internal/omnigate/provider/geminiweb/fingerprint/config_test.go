package fingerprint

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfigShape(t *testing.T) {
	c := DefaultConfig()
	if c.Chrome.Major != 124 || c.Chrome.Full != "124.0.6367.91" || c.Chrome.ImpersonateTarget != "chrome124" {
		t.Fatalf("unexpected chrome default: %+v", c.Chrome)
	}
	if c.Platform.OS != "Windows" || c.Platform.OSVersion != "10.0" || c.Platform.Arch != "x86_64" {
		t.Fatalf("unexpected platform default: %+v", c.Platform)
	}
	if len(c.HeaderOrder) != 12 || c.HeaderOrder[0] != "sec-ch-ua" || c.HeaderOrder[3] != "User-Agent" {
		t.Fatalf("unexpected header order: %v", c.HeaderOrder)
	}
	if c.Headers["Accept-Encoding"] != "gzip, deflate, br, zstd" {
		t.Fatalf("unexpected accept-encoding: %q", c.Headers["Accept-Encoding"])
	}
	if c.VersionMap["124"] != "chrome124" || len(c.VersionMap) != 4 {
		t.Fatalf("unexpected version map: %v", c.VersionMap)
	}
}

func TestManagerMemoryModeLoadsDefaults(t *testing.T) {
	m := NewManager("")
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Chrome.Major != 124 {
		t.Fatalf("major = %d", cfg.Chrome.Major)
	}
	// Config 返回副本：改动它不应影响内部状态。
	cfg.Chrome.Major = 999
	if m.Config().Chrome.Major != 124 {
		t.Fatal("Config() leaked mutable state")
	}
}

func TestManagerCreatesDefaultFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprint.json")
	m := NewManager(path)
	if _, err := m.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("config file not written: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("written config is not JSON: %v", err)
	}
	if _, ok := got["chrome_version"]; !ok {
		t.Fatalf("missing chrome_version in %s", raw)
	}
}

func TestManagerCorruptFileFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprint.json")
	if err := os.WriteFile(path, []byte(`{"chrome_version": {`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load should tolerate corruption: %v", err)
	}
	if cfg.Chrome.Major != 124 {
		t.Fatalf("major = %d want fallback 124", cfg.Chrome.Major)
	}
	// 损坏文件应被原子重写为合法 JSON。
	raw, _ := os.ReadFile(path)
	var check map[string]any
	if err := json.Unmarshal(raw, &check); err != nil {
		t.Fatalf("corrupt file not repaired: %v (%s)", err, raw)
	}
}

func TestManagerNormalizesPartialConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprint.json")
	// 合法 JSON 但缺字段：应被补齐而不是产生空 UA。
	if err := os.WriteFile(path, []byte(`{"chrome_version":{"major":120}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	cfg, err := m.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Chrome.Full == "" || cfg.Chrome.ImpersonateTarget == "" {
		t.Fatalf("partial config not normalized: %+v", cfg.Chrome)
	}
	if len(cfg.HeaderOrder) == 0 || cfg.Headers == nil || cfg.VersionMap == nil {
		t.Fatalf("partial config missing defaults: %+v", cfg)
	}
}

func TestManagerUpdateChromeVersion(t *testing.T) {
	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	// 124 -> 无变化
	changed, err := m.UpdateChromeVersion(124, "124.0.0.1")
	if err != nil || changed {
		t.Fatalf("no-op update: changed=%v err=%v", changed, err)
	}
	// 125 无对应 target -> 跳过
	changed, err = m.UpdateChromeVersion(125, "125.0.0.1")
	if err != nil || changed {
		t.Fatalf("unknown target should be skipped: changed=%v err=%v", changed, err)
	}
	// 注册 target 后可更新
	if err := m.SetVersionMap(125, "chrome125"); err != nil {
		t.Fatal(err)
	}
	changed, err = m.UpdateChromeVersion(125, "125.0.0.1")
	if err != nil || !changed {
		t.Fatalf("update: changed=%v err=%v", changed, err)
	}
	got := m.Config()
	if got.Chrome.Major != 125 || got.Chrome.Full != "125.0.0.1" || got.Chrome.ImpersonateTarget != "chrome125" {
		t.Fatalf("chrome = %+v", got.Chrome)
	}
	if got.LastUpdated == "" {
		t.Fatal("last_updated not set")
	}
}

func TestVersionKeysDescending(t *testing.T) {
	c := DefaultConfig()
	keys := c.VersionKeys()
	want := []int{124, 123, 120, 119}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v", keys)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("keys = %v want %v", keys, want)
		}
	}
}
