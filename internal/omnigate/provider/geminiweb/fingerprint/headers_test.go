package fingerprint

import (
	"net/http"
	"testing"
)

func TestHeaderBuilderOrderAndValues(t *testing.T) {
	b := NewHeaderBuilder(NewManager(""))
	got := b.Build("https://gemini.google.com/app", "POST", "application/x-www-form-urlencoded")

	// 顺序必须与配置一致（只包含被解析出值的项）。
	wantOrder := []string{
		"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform", "User-Agent",
		"X-Same-Domain", "Origin", "Sec-Fetch-Site", "Sec-Fetch-Mode",
		"Sec-Fetch-Dest", "Referer", "Accept-Encoding", "Accept-Language",
	}
	if len(got) != len(wantOrder) {
		t.Fatalf("got %d headers %v", len(got), got)
	}
	for i, name := range wantOrder {
		if got[i].Name != name {
			t.Fatalf("header[%d] = %q want %q", i, got[i].Name, name)
		}
	}

	want := map[string]string{
		"sec-ch-ua":          `"Google Chrome";v="124", "Chromium";v="124", "Not_A Brand";v="24"`,
		"sec-ch-ua-mobile":   "?0",
		"sec-ch-ua-platform": `"Windows"`,
		"User-Agent":         "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.6367.91 Safari/537.36",
		"X-Same-Domain":      "1",
		"Origin":             "https://gemini.google.com",
		"Sec-Fetch-Site":     "same-origin",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Dest":     "empty",
		"Referer":            "https://gemini.google.com/",
		"Accept-Encoding":    "gzip, deflate, br, zstd",
		"Accept-Language":    "en-US,en;q=0.9",
	}
	for name, wantVal := range want {
		var found bool
		for _, h := range got {
			if h.Name == name {
				found = true
				if h.Value != wantVal {
					t.Errorf("%s = %q want %q", name, h.Value, wantVal)
				}
			}
		}
		if !found {
			t.Errorf("missing header %s", name)
		}
	}
}

func TestHeaderBuilderGetUsesNavigationFetch(t *testing.T) {
	b := NewHeaderBuilder(NewManager(""))
	got := b.Build("https://gemini.google.com/app", "GET", "")
	vals := map[string]string{}
	for _, h := range got {
		vals[h.Name] = h.Value
	}
	if vals["Sec-Fetch-Mode"] != "navigate" || vals["Sec-Fetch-Dest"] != "document" {
		t.Fatalf("GET fetch headers = %q/%q", vals["Sec-Fetch-Mode"], vals["Sec-Fetch-Dest"])
	}
}

func TestHeaderBuilderContentTypeAndSkips(t *testing.T) {
	m := NewManager("")
	cfg := m.Config()
	cfg.HeaderOrder = append(cfg.HeaderOrder, "Content-Type", "Content-Length", "Host", "Connection")
	// 直接改配置副本后重建构建器，模拟自定义 header_order。
	rt := NewRuntime(Options{})
	rt.mgr.set(cfg)
	b := rt.Headers()

	// 无 content-type 时不应输出 Content-Type。
	for _, h := range b.Build("", "POST", "") {
		if h.Name == "Content-Type" {
			t.Fatal("Content-Type emitted without a value")
		}
	}
	// 有值时输出。
	var ct string
	for _, h := range b.Build("", "POST", "application/json") {
		if h.Name == "Content-Type" {
			ct = h.Value
		}
		if h.Name == "Content-Length" || h.Name == "Host" || h.Name == "Connection" {
			t.Fatalf("%s must never be emitted", h.Name)
		}
	}
	if ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestHeaderBuilderApplyRespectsSkip(t *testing.T) {
	rt := NewRuntime(Options{})
	// 默认 header_order 不含 Content-Type，这里补上以验证「有值才输出」的接线。
	cfg := rt.Manager().Config()
	cfg.HeaderOrder = append(cfg.HeaderOrder, "Content-Type")
	rt.Manager().set(cfg)

	req, _ := http.NewRequest("POST", "https://gemini.google.com/app", nil)
	rt.Headers().Apply(req, "POST", "application/json", "User-Agent")
	if req.Header.Get("User-Agent") != "" {
		t.Fatal("skipped header was applied")
	}
	if req.Header.Get("Origin") != "https://gemini.google.com" {
		t.Fatal("origin not applied")
	}
	if req.Header.Get("Content-Type") != "application/json" {
		t.Fatal("content-type not applied")
	}
}

func TestHeaderBuilderCustomVersionReflected(t *testing.T) {
	m := NewManager("")
	cfg := m.Config()
	cfg.Chrome = ChromeVersion{Major: 120, Full: "120.0.6099.109", ImpersonateTarget: "chrome120"}
	rt := NewRuntime(Options{})
	rt.mgr.set(cfg)
	got := rt.Headers().Build("", "GET", "")
	for _, h := range got {
		switch h.Name {
		case "User-Agent":
			if h.Value != "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.6099.109 Safari/537.36" {
				t.Fatalf("UA = %q", h.Value)
			}
		case "sec-ch-ua":
			if h.Value != `"Google Chrome";v="120", "Chromium";v="120", "Not_A Brand";v="24"` {
				t.Fatalf("sec-ch-ua = %q", h.Value)
			}
		}
	}
}
