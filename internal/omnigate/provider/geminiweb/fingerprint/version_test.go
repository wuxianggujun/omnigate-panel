package fingerprint

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckLatestParsesVersion(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"version":"126.0.6478.126"}]}`))
	}))
	defer ts.Close()

	s := NewVersionSyncer(NewManager(""), ts.Client(), nil)
	s.URL = ts.URL
	major, full, err := s.CheckLatest(context.Background())
	if err != nil {
		t.Fatalf("CheckLatest: %v", err)
	}
	if major != 126 || full != "126.0.6478.126" {
		t.Fatalf("got %d / %q", major, full)
	}
}

func TestCheckLatestErrors(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
	}{
		{"http error", http.StatusInternalServerError, "boom"},
		{"empty list", http.StatusOK, `{"versions":[]}`},
		{"bad json", http.StatusOK, `not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			s := NewVersionSyncer(NewManager(""), ts.Client(), nil)
			s.URL = ts.URL
			if _, _, err := s.CheckLatest(context.Background()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestFindBestTarget(t *testing.T) {
	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewVersionSyncer(m, nil, nil)

	// 精确命中（chrome120 是 utls 支持的）。
	if got := s.FindBestTarget(120); got != "chrome120" {
		t.Fatalf("120 -> %q want chrome120", got)
	}
	// 124 的目标名在 version_map 里存在，且校验时按「最近低版本 120」判定可用，
	// 因此直接返回 chrome124（真正的 ClientHello 降级发生在 tls.go）。
	if got := s.FindBestTarget(124); got != "chrome124" {
		t.Fatalf("124 -> %q want chrome124", got)
	}
	// 123 同理，返回映射里的 chrome123。
	if got := s.FindBestTarget(123); got != "chrome123" {
		t.Fatalf("123 -> %q want chrome123", got)
	}
	// 119 映射到 chrome119（校验时降级到 115 判定可用）。
	if got := s.FindBestTarget(119); got != "chrome119" {
		t.Fatalf("119 -> %q want chrome119", got)
	}
	// 过低版本无可用目标。
	if got := s.FindBestTarget(50); got != "" {
		t.Fatalf("50 -> %q want empty", got)
	}
}

func TestVersionSyncerTickUpdatesConfig(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"version":"126.0.6478.126"}]}`))
	}))
	defer ts.Close()

	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewVersionSyncer(m, ts.Client(), nil)
	s.URL = ts.URL
	s.tick(context.Background())

	got := m.Config()
	if got.Chrome.Major != 126 {
		t.Fatalf("major = %d want 126", got.Chrome.Major)
	}
	// 126 无精确目标，但 version_map 里最高的 124 -> chrome124 校验可用（降级），
	// 因此注册为 chrome124。
	if got.Chrome.ImpersonateTarget != "chrome124" {
		t.Fatalf("target = %q want chrome124", got.Chrome.ImpersonateTarget)
	}
	if got.VersionMap["126"] != "chrome124" {
		t.Fatalf("version_map not updated: %v", got.VersionMap)
	}
}

func TestVersionSyncerTickNoDowngrade(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"version":"100.0.0.0"}]}`))
	}))
	defer ts.Close()

	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewVersionSyncer(m, ts.Client(), nil)
	s.URL = ts.URL
	s.tick(context.Background())
	if m.Config().Chrome.Major != 124 {
		t.Fatalf("major = %d want unchanged 124", m.Config().Chrome.Major)
	}
}

func TestVersionSyncerTickFiresOnTarget(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"version":"126.0.6478.126"}]}`))
	}))
	defer ts.Close()

	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewVersionSyncer(m, ts.Client(), nil)
	s.URL = ts.URL
	var got string
	s.OnTarget = func(target string) { got = target }
	s.tick(context.Background())
	// 126 无精确目标，降级到 version_map 里最高的 chrome124。
	if got != "chrome124" {
		t.Fatalf("OnTarget = %q want chrome124", got)
	}
}

func TestVersionSyncerTickNoTargetWhenUnchanged(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"versions":[{"version":"100.0.0.0"}]}`))
	}))
	defer ts.Close()

	m := NewManager("")
	if _, err := m.Load(); err != nil {
		t.Fatal(err)
	}
	s := NewVersionSyncer(m, ts.Client(), nil)
	s.URL = ts.URL
	called := false
	s.OnTarget = func(string) { called = true }
	s.tick(context.Background())
	if called {
		t.Fatal("OnTarget must not fire when the version did not change")
	}
}

func TestVersionSyncerRunStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s := NewVersionSyncer(NewManager(""), nil, nil)
	done := make(chan struct{})
	go func() { s.Run(ctx, time.Hour); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancelled context")
	}
}
