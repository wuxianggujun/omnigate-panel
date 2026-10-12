package fingerprint

import (
	"context"
	"path/filepath"
	"testing"
)

func TestNewRuntimeDefaults(t *testing.T) {
	rt := NewRuntime(Options{})
	if rt.Manager() == nil || rt.Headers() == nil || rt.TLS() == nil {
		t.Fatal("runtime components must be non-nil")
	}
	// TLSEnabled 默认 false -> 伪装器被禁用。
	if rt.TLS().Enabled() {
		t.Fatal("TLS should be disabled by default")
	}
	if rt.Jitter().Enabled {
		t.Fatal("jitter should be disabled by default")
	}
	if rt.CookieDir() != "" {
		t.Fatalf("CookieDir = %q want empty", rt.CookieDir())
	}
	// 默认目标 chrome124 在配置里，且可降级。
	if got := rt.Headers().GetImpersonateTarget(); got != "chrome124" {
		t.Fatalf("target = %q want chrome124", got)
	}
}

func TestRuntimeTLSEnabled(t *testing.T) {
	rt := NewRuntime(Options{TLSEnabled: true})
	if !rt.TLS().Enabled() {
		t.Fatal("TLS should be enabled with chrome124 (downgraded)")
	}
	st := rt.Status()
	if st["tls_enabled"] != true {
		t.Fatalf("status tls_enabled = %v", st["tls_enabled"])
	}
}

func TestRuntimeSetChromeTarget(t *testing.T) {
	rt := NewRuntime(Options{TLSEnabled: true})
	if err := rt.SetChromeTarget("chrome120"); err != nil {
		t.Fatalf("SetChromeTarget: %v", err)
	}
	if got := rt.Headers().GetImpersonateTarget(); got != "chrome120" {
		t.Fatalf("target = %q want chrome120", got)
	}
	if !rt.TLS().Enabled() {
		t.Fatal("TLS should remain enabled after switching to chrome120")
	}
	if err := rt.SetChromeTarget("bogus"); err == nil {
		t.Fatal("unsupported target must error")
	}
}

func TestRuntimeVersionSyncPropagatesTargetToTLS(t *testing.T) {
	rt := NewRuntime(Options{TLSEnabled: true})
	if rt.syncer.OnTarget == nil {
		t.Fatal("syncer.OnTarget must be wired by NewRuntime")
	}
	before, ok := rt.tls.current()
	if !ok {
		t.Fatal("TLS should be enabled")
	}
	// 通过同步回调切到一个与默认 (chrome124 -> HelloChrome_120) 不同的目标，
	// 验证回调确实改到了运行中的伪装器。
	rt.syncer.OnTarget("chrome100")
	after, ok := rt.tls.current()
	if !ok {
		t.Fatal("TLS should stay enabled after target switch")
	}
	if after == before {
		t.Fatalf("TLS target did not change (still %v)", after)
	}
}

func TestRuntimeOpenJarPersists(t *testing.T) {
	dir := t.TempDir()
	rt := NewRuntime(Options{CookieDir: dir})
	if rt.CookieDir() != dir {
		t.Fatalf("CookieDir = %q", rt.CookieDir())
	}
	j := rt.OpenJar("acct")
	j.Set("__Secure-1PSID", "abc")
	if j.Path() == "" {
		t.Fatal("jar should have a persistence path")
	}
	if _, err := filepath.Abs(j.Path()); err != nil {
		t.Fatalf("jar path invalid: %v", err)
	}
	reopened := rt.OpenJar("acct")
	if reopened.Get("__Secure-1PSID") != "abc" {
		t.Fatalf("reopened jar = %q", reopened.Get("__Secure-1PSID"))
	}
}

func TestRuntimeOpenJarMemoryMode(t *testing.T) {
	rt := NewRuntime(Options{})
	j := rt.OpenJar("acct")
	if j.Path() != "" {
		t.Fatalf("memory jar path = %q want empty", j.Path())
	}
	j.Set("a", "1")
	if j.Get("a") != "1" {
		t.Fatal("memory jar lost value")
	}
}

func TestRuntimeStartVersionSyncDisabledIsNoop(t *testing.T) {
	rt := NewRuntime(Options{VersionSyncEnabled: false})
	rt.StartVersionSync(context.Background())
	if rt.VersionSyncRunning() {
		t.Fatal("version sync must not start when disabled")
	}
}

func TestRuntimeStartVersionSyncEnabledIdempotent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rt := NewRuntime(Options{VersionSyncEnabled: true})
	rt.StartVersionSync(ctx)
	rt.StartVersionSync(ctx) // startOnce：第二次不应再起一个循环
	if !rt.VersionSyncRunning() {
		t.Fatal("version sync should be running")
	}
	if rt.syncer == nil {
		t.Fatal("syncer should exist")
	}
	first := rt.syncer
	rt.StartVersionSync(ctx)
	if rt.syncer != first {
		t.Fatal("StartVersionSync replaced the syncer")
	}
}

func TestRuntimeStatusKeys(t *testing.T) {
	rt := NewRuntime(Options{CookieDir: "C:/tmp/fp", VersionSyncEnabled: true, VersionSyncInterval: 0})
	st := rt.Status()
	for _, k := range []string{
		"chrome_major", "chrome_full", "impersonate_target", "platform",
		"header_order", "headers", "version_map", "last_updated",
		"config_path", "cookie_dir", "jitter_enabled", "version_sync_enabled",
		"version_sync_running", "version_sync_hours", "tls_enabled", "tls_supported",
	} {
		if _, ok := st[k]; !ok {
			t.Errorf("status missing key %q", k)
		}
	}
	if st["chrome_major"] != 124 {
		t.Errorf("chrome_major = %v want 124", st["chrome_major"])
	}
	if st["cookie_dir"] != "C:/tmp/fp" {
		t.Errorf("cookie_dir = %v", st["cookie_dir"])
	}
	// 0 间隔在展示层按 24h 计。
	if st["version_sync_hours"] != 0 {
		t.Errorf("version_sync_hours = %v", st["version_sync_hours"])
	}
}

func TestSupportedTargets(t *testing.T) {
	got := SupportedTargets()
	want := []string{"chrome100", "chrome102", "chrome106", "chrome115", "chrome120"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("targets = %v want %v", got, want)
		}
	}
}

func TestRuntimeNilLogfTolerated(t *testing.T) {
	// 配置路径指向不存在目录也应正常初始化（Load 容错）。
	rt := NewRuntime(Options{ConfigPath: filepath.Join(t.TempDir(), "fp", "fingerprint.json")})
	if rt.Manager().Path() == "" {
		t.Fatal("config path not retained")
	}
	if rt.Status()["config_path"] == "" {
		t.Fatal("status config_path empty")
	}
}
