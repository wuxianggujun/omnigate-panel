package outbound

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestProxyIsPoolAndNormalize(t *testing.T) {
	c := Config{
		Proxies: []Proxy{
			{Name: "list", Pool: []string{"1.2.3.4:8080", "socks5://5.6.7.8:1080"}, PoolScheme: "http"},
			{Name: "api", PoolURL: "https://example.com/proxies"},
			{Name: "single", URL: "http://127.0.0.1:8888"},
		},
		Routes: map[string]string{
			OmniTarget("runable"): "list",
			OmniTarget("raccoon"): "api",
		},
	}
	if err := c.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !c.Proxies[0].IsPool() || !c.Proxies[1].IsPool() || c.Proxies[2].IsPool() {
		t.Fatalf("IsPool 判定错误")
	}
	if got := c.Proxies[0].Pool[0]; got != "http://1.2.3.4:8080" {
		t.Fatalf("缺 scheme 应补 http，得到 %q", got)
	}
	if got := c.Proxies[0].Pool[1]; got != "socks5://5.6.7.8:1080" {
		t.Fatalf("已带 scheme 不应改写，得到 %q", got)
	}
	if c.Proxies[1].RefreshSec != 300 {
		t.Fatalf("refresh_sec 默认应为 300，得到 %d", c.Proxies[1].RefreshSec)
	}
	if c.Proxies[1].ProbeURL != DefaultPoolProbeURL {
		t.Fatalf("probe_url 默认错误: %q", c.Proxies[1].ProbeURL)
	}
	// 池内的单代理不应被当成单代理解析。
	if u := c.For(OmniTarget("runable")); u != nil {
		t.Fatalf("代理池不应解析出单代理 URL: %+v", u)
	}
	if u := c.For(OmniTarget("raccoon")); u != nil {
		t.Fatalf("代理池不应解析出单代理 URL: %+v", u)
	}
}

func TestProxyPoolNormalizeErrors(t *testing.T) {
	cases := []Config{
		{Proxies: []Proxy{{Name: "p", Pool: []string{""}}}},
		{Proxies: []Proxy{{Name: "p", Pool: []string{"ftp://1.2.3.4:1"}}}},
		{Proxies: []Proxy{{Name: "p", PoolURL: "ftp://example.com/x"}}},
		{Proxies: []Proxy{{Name: "p", PoolURL: "http://"}}},
		{Proxies: []Proxy{{Name: "p", Pool: []string{"1.2.3.4:1"}, PoolScheme: "ftp"}}},
		{Proxies: []Proxy{{Name: "p", URL: ""}}}, // 既不是单代理也不是池
	}
	for i, c := range cases {
		if err := c.Normalize(); err == nil {
			t.Fatalf("case %d 应当报错", i)
		}
	}
}

func TestParseProxyList(t *testing.T) {
	cases := map[string][]string{
		`{"code":200,"data":{"proxies":["1.2.3.4:8080","5.6.7.8:1080"],"count":2}}`: {"1.2.3.4:8080", "5.6.7.8:1080"},
		`{"proxies":["a:1","b:2"]}`:       {"a:1", "b:2"},
		`["x:1","y:2"]`:                   {"x:1", "y:2"},
		"1.1.1.1:1\n2.2.2.2:2\n":          {"1.1.1.1:1", "2.2.2.2:2"},
		"1.1.1.1:1, 2.2.2.2:2":            {"1.1.1.1:1", "2.2.2.2:2"},
		"":                                nil,
	}
	for body, want := range cases {
		got := parseProxyList([]byte(body))
		if len(got) != len(want) {
			t.Fatalf("parseProxyList(%q) = %v, want %v", body, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("parseProxyList(%q)[%d] = %q, want %q", body, i, got[i], want[i])
			}
		}
	}
}

func TestPoolRotationAndReport(t *testing.T) {
	a := mustURL(t, "http://10.0.0.1:1")
	b := mustURL(t, "http://10.0.0.2:2")
	p := newPool(Proxy{Name: "t", PoolScheme: "http"}, nil)
	p.live = []*url.URL{a, b}

	if got := p.Next(); got.String() != a.String() {
		t.Fatalf("首次应返回 a，得到 %v", got)
	}
	if got := p.Next(); got.String() != b.String() {
		t.Fatalf("第二次应返回 b，得到 %v", got)
	}
	if got := p.Next(); got.String() != a.String() {
		t.Fatalf("应轮询回 a，得到 %v", got)
	}
	if p.MaxAttempts() != 2 {
		t.Fatalf("MaxAttempts 应为 2，得到 %d", p.MaxAttempts())
	}

	// 汇报失败应把该代理踢出可用集。
	p.Report(a, io.ErrUnexpectedEOF)
	if got := p.Next(); got.String() != b.String() {
		t.Fatalf("a 被剔除后应只返回 b，得到 %v", got)
	}
	if p.MaxAttempts() != 1 {
		t.Fatalf("剔除后 MaxAttempts 应为 1，得到 %d", p.MaxAttempts())
	}
	// 汇报成功不应剔除。
	p.Report(b, nil)
	if p.MaxAttempts() != 1 {
		t.Fatalf("汇报成功后仍应为 1，得到 %d", p.MaxAttempts())
	}
	if st := p.Status(); st.Live != 1 || st.LastError == "" {
		t.Fatalf("Status 错误: %+v", st)
	}
}

// TestFailoverTransport 验证「连接失败自动切换下一个代理」。
func TestFailoverTransport(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("origin"))
	}))
	defer origin.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 充当代理：无论请求什么都回 200。
		_, _ = w.Write([]byte("via-proxy"))
	}))
	defer good.Close()

	dead := "http://127.0.0.1:1" // 连接被拒
	p := newPool(Proxy{Name: "t", PoolScheme: "http"}, nil)
	p.live = []*url.URL{mustURL(t, dead), mustURL(t, good.URL)}

	tr := WrapTransport(&http.Transport{}, p)
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("failover 应成功，得到错误: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-proxy" {
		t.Fatalf("应经可用代理返回，得到 %q", body)
	}
	// 死代理应已被剔除。
	if st := p.Status(); st.Live != 1 {
		t.Fatalf("死代理应被剔除，Live=%d", st.Live)
	}
}

// TestRuntimeReload 验证热替换：旧的池停掉，新的池生效。
func TestRuntimeReload(t *testing.T) {
	cfg1 := Config{
		Proxies: []Proxy{{Name: "p1", Pool: []string{"127.0.0.1:1"}, PoolScheme: "http", RefreshSec: 3600}},
		Routes:  map[string]string{OmniTarget("x"): "p1"},
	}
	rt := NewRuntime(cfg1, nil)
	defer rt.Close()
	if rt.Pool(OmniTarget("x")) == nil {
		t.Fatal("reload 前应能解析出代理池")
	}
	if rt.SingleURL(OmniTarget("x")) != nil {
		t.Fatal("代理池不应解析出单代理")
	}

	cfg2 := Config{
		Proxies: []Proxy{{Name: "s", URL: "http://127.0.0.1:9999"}},
		Routes:  map[string]string{OmniTarget("x"): "s"},
	}
	rt.Reload(cfg2)
	if rt.Pool(OmniTarget("x")) != nil {
		t.Fatal("reload 后不应再有代理池")
	}
	u := rt.SingleURL(OmniTarget("x"))
	if u == nil || u.Host != "127.0.0.1:9999" {
		t.Fatalf("reload 后单代理解析错误: %+v", u)
	}
	if fn := rt.ProxyFunc(OmniTarget("x")); fn == nil {
		t.Fatal("ProxyFunc 不应为 nil")
	}
}

// TestPoolRefreshFromAPI 用一个假 API 验证拉取 + 探活闭环。
func TestPoolRefreshFromAPI(t *testing.T) {
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer good.Close()
	goodHost := mustURL(t, good.URL).Host

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 一个可用 + 一个必死
		_, _ = w.Write([]byte(`{"data":{"proxies":["` + goodHost + `","127.0.0.1:1"]}}`))
	}))
	defer api.Close()

	p := newPool(Proxy{
		Name:       "t",
		PoolURL:    api.URL,
		PoolScheme: "http",
		ProbeURL:   good.URL,
	}, nil)
	p.refreshOnce(context.Background())
	st := p.Status()
	if st.Candidates != 2 {
		t.Fatalf("候选数应为 2，得到 %d", st.Candidates)
	}
	if st.Live != 1 {
		t.Fatalf("存活数应为 1，得到 %d (lastErr=%s)", st.Live, st.LastError)
	}
	if got := p.Next(); got == nil || got.Host != goodHost {
		t.Fatalf("Next 应返回存活代理，得到 %v", got)
	}
}
