package outbound

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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

	if got, err := p.Next(); err != nil || got.String() != a.String() {
		t.Fatalf("首次应返回 a，得到 %v err=%v", got, err)
	}
	if got, err := p.Next(); err != nil || got.String() != b.String() {
		t.Fatalf("第二次应返回 b，得到 %v err=%v", got, err)
	}
	if got, err := p.Next(); err != nil || got.String() != a.String() {
		t.Fatalf("应轮询回 a，得到 %v err=%v", got, err)
	}
	if p.MaxAttempts() != 2 {
		t.Fatalf("MaxAttempts 应为 2，得到 %d", p.MaxAttempts())
	}

	// 汇报失败应把该代理踢出可用集。
	p.Report(a, io.ErrUnexpectedEOF)
	if got, err := p.Next(); err != nil || got.String() != b.String() {
		t.Fatalf("a 被剔除后应只返回 b，得到 %v err=%v", got, err)
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

// TestPoolStrict 验证严格模式：池空时不回退直连，而是返回错误。
func TestPoolStrict(t *testing.T) {
	// 非严格：池空 → (nil, nil) = 直连
	lax := newPool(Proxy{Name: "lax", PoolScheme: "http"}, nil)
	if u, err := lax.Next(); u != nil || err != nil {
		t.Fatalf("非严格模式池空应返回 (nil,nil)，得到 %v err=%v", u, err)
	}
	// 严格：池空 → 错误
	strict := newPool(Proxy{Name: "strict", PoolScheme: "http", Strict: true}, nil)
	u, err := strict.Next()
	if u != nil || err == nil {
		t.Fatalf("严格模式池空应返回错误，得到 %v err=%v", u, err)
	}
	if st := strict.Status(); !st.Strict {
		t.Fatalf("Status.Strict 应为 true")
	}
	// 严格模式有存活代理时正常返回
	strict.live = []*url.URL{mustURL(t, "http://10.0.0.1:1")}
	if u, err := strict.Next(); err != nil || u == nil {
		t.Fatalf("严格模式有存活代理应正常返回，得到 %v err=%v", u, err)
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

// TestFailoverStrictEmpty 验证严格模式下池空时请求直接失败（不回退直连）。
func TestFailoverStrictEmpty(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("origin"))
	}))
	defer origin.Close()
	p := newPool(Proxy{Name: "s", PoolScheme: "http", Strict: true}, nil)
	client := &http.Client{Transport: WrapTransport(&http.Transport{}, p), Timeout: 10 * time.Second}
	if _, err := client.Get(origin.URL); err == nil {
		t.Fatal("严格模式池空应请求失败")
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
	if got, err := p.Next(); err != nil || got == nil || got.Host != goodHost {
		t.Fatalf("Next 应返回存活代理，得到 %v err=%v", got, err)
	}
}

// TestReportIgnoresCancellation 验证「请求被取消 / 超时」不会被当成代理故障剔除。
func TestReportIgnoresCancellation(t *testing.T) {
	u := mustURL(t, "http://10.0.0.1:1")
	p := newPool(Proxy{Name: "p", PoolScheme: "http"}, nil)
	p.live = []*url.URL{u}

	p.Report(u, context.Canceled)
	p.Report(u, context.DeadlineExceeded)
	if st := p.Status(); st.Live != 1 {
		t.Fatalf("取消/超时不应剔除代理，live=%d", st.Live)
	}

	// 真正的连接错误仍应剔除。
	p.Report(u, errors.New("connection refused"))
	if st := p.Status(); st.Live != 0 {
		t.Fatalf("连接错误应剔除代理，live=%d", st.Live)
	}
}

// TestFailoverCancelKeepsPool 端到端验证：请求超时（客户端取消）不会清空代理池。
// 回归背景：failoverTransport 曾对 ctx 错误也 Report + 换下一个重试，导致一次断连
// 会带着同一个已取消的 ctx 把池里所有代理逐个试坏并全部剔除。
func TestFailoverCancelKeepsPool(t *testing.T) {
	// 假代理：挂住直到客户端断开，制造「请求被取消」而非「代理故障」。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slow.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()

	p := newPool(Proxy{Name: "p", PoolScheme: "http"}, nil)
	p.live = []*url.URL{mustURL(t, slow.URL)}

	client := &http.Client{Transport: WrapTransport(&http.Transport{}, p)}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Do(req); err == nil {
		t.Fatal("应因请求超时失败")
	}
	if st := p.Status(); st.Live != 1 {
		t.Fatalf("请求超时不应剔除代理，live=%d last_error=%q", st.Live, st.LastError)
	}
}

// TestIsProxyError 覆盖代理层失败的识别：哨兵、HTTP 代理握手、SOCKS、以及反例。
func TestIsProxyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"sentinel", ErrProxy, true},
		{"wrapped sentinel", fmt.Errorf("%w: %w", ErrProxy, errors.New("boom")), true},
		{"proxyconnect", &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"url.Error wraps proxyconnect", &url.Error{Op: "Post", URL: "https://x", Err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("refused")}}, true},
		{"socks connect text", errors.New("socks connect tcp: connection refused"), true},
		{"plain dial", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, false},
		{"connection refused", errors.New("connection refused"), false},
	}
	for _, c := range cases {
		if got := IsProxyError(c.err); got != c.want {
			t.Errorf("%s: IsProxyError=%v want %v (err=%v)", c.name, got, c.want, c.err)
		}
	}
}

// TestStrictEmptyPoolErrorIsProxyError 严格模式空池的自有错误必须判定为代理层失败，
// 这样面板上游才不会把「代理池空」记成账号故障。
func TestStrictEmptyPoolErrorIsProxyError(t *testing.T) {
	p := newPool(Proxy{Name: "p", PoolScheme: "http", Strict: true}, nil)
	_, err := p.Next()
	if err == nil {
		t.Fatal("严格模式空池应返回错误")
	}
	if !IsProxyError(err) {
		t.Fatalf("严格模式池空应判定为代理层失败，得 %v", err)
	}
}

// TestFailoverErrorIsProxyError failoverTransport 的失败必须用 ErrProxy 包装（不依赖
// 错误文本），供上层区分「代理问题」与「账号问题」。
func TestFailoverErrorIsProxyError(t *testing.T) {
	p := newPool(Proxy{Name: "p", PoolScheme: "http"}, nil)
	p.live = []*url.URL{mustURL(t, "http://127.0.0.1:1")}
	client := &http.Client{Transport: WrapTransport(&http.Transport{}, p)}
	resp, err := client.Get("https://example.com/")
	if resp != nil {
		resp.Body.Close()
	}
	if err == nil {
		t.Fatal("死代理应失败")
	}
	if !errors.Is(err, ErrProxy) {
		t.Fatalf("failoverTransport 应包装 ErrProxy，得 %v", err)
	}
	if !IsProxyError(err) {
		t.Fatalf("死代理失败应判定为代理层失败，得 %v", err)
	}
}
