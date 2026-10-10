package outbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultPoolProbeURL 是代理池探活的默认目标：能通即认为该代理可用。
const DefaultPoolProbeURL = "https://www.gstatic.com/generate_204"

// maxPoolAttempts 单次请求在代理池里最多尝试几个代理。
const maxPoolAttempts = 3

// poolListTimeout 拉取远程代理池 API 的超时。
const poolListTimeout = 20 * time.Second

// poolProbeTimeout 单个代理探活的超时。
const poolProbeTimeout = 8 * time.Second

// poolProbeConcurrency 探活并发度。
const poolProbeConcurrency = 24

// poolTransportCacheMax 每个池最多缓存的「单代理 Transport」数量，超出即重建。
const poolTransportCacheMax = 128

// Selector 为一次出站请求挑代理：Next 返回本次尝试要用的代理（nil=直连），
// Report 汇报本次尝试结果（失败即把该代理踢出可用集），MaxAttempts 是单请求
// 最多尝试几次。*Pool 实现了该接口。
type Selector interface {
	Next() *url.URL
	Report(u *url.URL, err error)
	MaxAttempts() int
}

// PoolStatus 是代理池的可观测快照（供 /omni/healthz、面板展示）。
type PoolStatus struct {
	Name       string `json:"name"`
	Live       int    `json:"live"`
	Candidates int    `json:"candidates"`
	Updated    string `json:"updated,omitempty"`
	LastError  string `json:"last_error,omitempty"`
}

// Pool 是一个命名代理池：显式列表 + 远程 API 定期拉取 → 逐个探活 → 按请求轮询，
// 连接失败自动换下一个（由 failoverTransport 驱动）。
type Pool struct {
	name     string
	explicit []string
	apiURL   string
	scheme   string
	refresh  time.Duration
	probeURL string
	user     string
	pass     string
	logf     func(string, ...any)

	mu       sync.Mutex
	live     []*url.URL
	rr       uint64
	cand     int
	lastErr  string
	updated  time.Time
	firstRun bool
}

func newPool(p Proxy, logf func(string, ...any)) *Pool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	scheme := p.PoolScheme
	if scheme == "" {
		scheme = "http"
	}
	probe := p.ProbeURL
	if probe == "" {
		probe = DefaultPoolProbeURL
	}
	secs := p.RefreshSec
	if secs <= 0 {
		secs = 300
	}
	return &Pool{
		name:     p.Name,
		explicit: append([]string(nil), p.Pool...),
		apiURL:   p.PoolURL,
		scheme:   scheme,
		refresh:  time.Duration(secs) * time.Second,
		probeURL: probe,
		user:     p.Username,
		pass:     p.Password,
		logf:     logf,
	}
}

// run 后台循环：立即刷一次，之后每 refresh 刷一次。
func (p *Pool) run(ctx context.Context) {
	p.refreshOnce(ctx)
	t := time.NewTicker(p.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.refreshOnce(ctx)
		}
	}
}

// refreshOnce 拉取远程列表 + 探活，重建可用集。
func (p *Pool) refreshOnce(ctx context.Context) {
	raw := append([]string(nil), p.explicit...)
	if p.apiURL != "" {
		if list, err := p.fetchList(ctx); err != nil {
			p.logf("代理池「%s」拉取 %s 失败：%v", p.name, p.apiURL, err)
		} else {
			raw = append(raw, list...)
		}
	}

	// 归一化 + 去重
	seen := map[string]bool{}
	var cands []*url.URL
	for _, e := range raw {
		ne, err := normalizeEntry(p.scheme, e)
		if err != nil {
			continue
		}
		if seen[ne] {
			continue
		}
		seen[ne] = true
		u, err := url.Parse(ne)
		if err != nil {
			continue
		}
		if p.user != "" {
			if p.pass != "" {
				u.User = url.UserPassword(p.user, p.pass)
			} else {
				u.User = url.User(p.user)
			}
		}
		cands = append(cands, u)
	}

	live := p.probeAll(ctx, cands)

	p.mu.Lock()
	p.live = live
	p.cand = len(cands)
	p.updated = time.Now()
	switch {
	case len(cands) == 0:
		p.lastErr = "池内没有候选代理"
	case len(live) == 0:
		p.lastErr = fmt.Sprintf("%d 个候选代理全部探活失败", len(cands))
	default:
		p.lastErr = ""
	}
	first := !p.firstRun
	p.firstRun = true
	p.mu.Unlock()

	if first || len(live) == 0 {
		p.logf("代理池「%s」刷新：候选 %d，存活 %d", p.name, len(cands), len(live))
	}
}

// fetchList 从远程代理池 API 拉取一批代理（direct，不走代理）。
func (p *Pool) fetchList(ctx context.Context) ([]string, error) {
	cctx, cancel := context.WithTimeout(ctx, poolListTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, p.apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseProxyList(body), nil
}

// probeAll 并发探活候选代理，返回存活者（保持原顺序）。
func (p *Pool) probeAll(ctx context.Context, cands []*url.URL) []*url.URL {
	if len(cands) == 0 {
		return nil
	}
	ok := make([]bool, len(cands))
	sem := make(chan struct{}, poolProbeConcurrency)
	var wg sync.WaitGroup
	for i, u := range cands {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, u *url.URL) {
			defer wg.Done()
			defer func() { <-sem }()
			ok[i] = p.probeOne(ctx, u)
		}(i, u)
	}
	wg.Wait()
	out := make([]*url.URL, 0, len(cands))
	for i, u := range cands {
		if ok[i] {
			out = append(out, u)
		}
	}
	return out
}

// probeOne 通过代理访问探活目标，能出网即认为可用。
func (p *Pool) probeOne(ctx context.Context, u *url.URL) bool {
	cctx, cancel := context.WithTimeout(ctx, poolProbeTimeout)
	defer cancel()
	tr := &http.Transport{Proxy: http.ProxyURL(u), ForceAttemptHTTP2: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: poolProbeTimeout}
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, p.probeURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	// 4xx 也说明「代理能出网」，只有 5xx / 连接失败才算不可用。
	return resp.StatusCode < 500
}

// Next 轮询返回下一个可用代理；池为空时返回 nil（= 直连）。
func (p *Pool) Next() *url.URL {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.live) == 0 {
		return nil
	}
	u := p.live[int(p.rr)%len(p.live)]
	p.rr++
	return u
}

// Report 汇报一次尝试结果：失败即把该代理踢出可用集。
func (p *Pool) Report(u *url.URL, err error) {
	if err == nil || u == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.live[:0]
	for _, x := range p.live {
		if x.String() != u.String() {
			out = append(out, x)
		}
	}
	p.live = out
	p.lastErr = "代理 " + u.Redacted() + " 连接失败，已剔除：" + err.Error()
}

// MaxAttempts 返回单请求最多尝试次数（至少 1，空池时试一次直连）。
func (p *Pool) MaxAttempts() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.live)
	switch {
	case n == 0:
		return 1
	case n > maxPoolAttempts:
		return maxPoolAttempts
	default:
		return n
	}
}

// Status 返回可观测快照。
func (p *Pool) Status() PoolStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := PoolStatus{Name: p.name, Live: len(p.live), Candidates: p.cand, LastError: p.lastErr}
	if !p.updated.IsZero() {
		st.Updated = p.updated.Format(time.RFC3339)
	}
	return st
}

// parseProxyList 尽力从任意响应里解析出代理字符串列表：
//   - {"data":{"proxies":[...]}}（scdn.io 等）
//   - {"proxies":[...]} / {"list":[...]}
//   - JSON 数组
//   - 纯文本（按行/逗号/空白切分）
func parseProxyList(body []byte) []string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return nil
	}
	var v any
	if json.Unmarshal(body, &v) == nil {
		if out := proxiesFromAny(v); len(out) > 0 {
			return out
		}
	}
	var out []string
	for _, tok := range strings.FieldsFunc(s, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ' ' || r == '\t' || r == ';'
	}) {
		if tok = strings.TrimSpace(tok); tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

func proxiesFromAny(v any) []string {
	switch t := v.(type) {
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case map[string]any:
		for _, key := range []string{"data", "proxies", "list", "result"} {
			if raw, ok := t[key]; ok {
				if out := proxiesFromAny(raw); len(out) > 0 {
					return out
				}
			}
		}
	}
	return nil
}

// failoverTransport 用 Selector 给 base 包一层：每个请求按池轮询挑代理，连接失败
// 自动换下一个（请求体可重放时才重试）。每个代理各持一份 *http.Transport 克隆，
// 避免并发改写同一个 Transport.Proxy。
type failoverTransport struct {
	tmpl *http.Transport
	sel  Selector

	mu    sync.Mutex
	trans map[string]*http.Transport
}

// WrapTransport 用 sel 包装 base（base 或 sel 为 nil 时原样返回）。
func WrapTransport(base *http.Transport, sel Selector) http.RoundTripper {
	if base == nil || sel == nil {
		return base
	}
	return &failoverTransport{tmpl: base, sel: sel, trans: map[string]*http.Transport{}}
}

func (t *failoverTransport) transportFor(u *url.URL) *http.Transport {
	key := ""
	if u != nil {
		key = u.String()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.trans[key]; ok {
		return tr
	}
	if len(t.trans) >= poolTransportCacheMax {
		t.trans = map[string]*http.Transport{}
	}
	tr := t.tmpl.Clone()
	if u != nil {
		tr.Proxy = http.ProxyURL(u)
	} else {
		tr.Proxy = nil
	}
	t.trans[key] = tr
	return tr
}

// CloseIdleConnections 让底层各代理的 Transport 释放空闲连接。
func (t *failoverTransport) CloseIdleConnections() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.trans {
		tr.CloseIdleConnections()
	}
	t.tmpl.CloseIdleConnections()
}

func (t *failoverTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.sel.MaxAttempts()
	var lastErr error
	for i := 0; i < attempts; i++ {
		u := t.sel.Next()
		tr := t.transportFor(u)
		r := req
		if i > 0 {
			if req.Body != nil {
				if req.GetBody == nil {
					break
				}
				body, err := req.GetBody()
				if err != nil {
					break
				}
				r = req.Clone(req.Context())
				r.Body = body
			} else {
				r = req.Clone(req.Context())
			}
		}
		resp, err := tr.RoundTrip(r)
		if err == nil {
			t.sel.Report(u, nil)
			return resp, nil
		}
		t.sel.Report(u, err)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("outbound: 代理池无可用代理")
	}
	return nil, lastErr
}

// Runtime 是 Config 的运行时视图：持有代理池（后台刷新 + 探活），并按目标解析出站方案。
// 可通过 Reload 热替换配置（停掉旧代理池、按新配置重建）。
type Runtime struct {
	logf func(string, ...any)

	mu      sync.Mutex
	cfg     Config
	pools   map[string]*Pool
	cancels []context.CancelFunc
	wg      sync.WaitGroup
}

// NewRuntime 依据 cfg 构建运行时并启动各代理池的后台刷新。logf 可为 nil。
func NewRuntime(cfg Config, logf func(string, ...any)) *Runtime {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Runtime{logf: logf, pools: map[string]*Pool{}}
	r.Reload(cfg)
	return r
}

// Reload 用新配置替换运行时：停掉旧代理池（含在途拉取/探活），按新配置重建。
func (r *Runtime) Reload(cfg Config) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cancels {
		c()
	}
	r.wg.Wait()
	r.cancels = nil
	r.pools = make(map[string]*Pool)
	r.cfg = cfg
	for i := range cfg.Proxies {
		p := cfg.Proxies[i]
		if !p.IsPool() {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		pl := newPool(p, r.logf)
		r.pools[p.Name] = pl
		r.cancels = append(r.cancels, cancel)
		r.wg.Add(1)
		go func(pl *Pool) {
			defer r.wg.Done()
			pl.run(ctx)
		}(pl)
	}
}

// Close 停止所有后台刷新。
func (r *Runtime) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.cancels {
		c()
	}
	r.wg.Wait()
	r.cancels = nil
	r.pools = map[string]*Pool{}
}

// SingleURL 返回目标路由到的「单代理」；目标是代理池或无路由时返回 nil。
func (r *Runtime) SingleURL(target string) *url.URL {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.TrimSpace(r.cfg.Routes[target])
	if name == "" {
		return nil
	}
	for i := range r.cfg.Proxies {
		p := &r.cfg.Proxies[i]
		if p.Name == name && !p.IsPool() {
			u, err := p.parse()
			if err != nil {
				return nil
			}
			return u
		}
	}
	return nil
}

// Pool 返回目标路由到的代理池；无则返回 nil。
func (r *Runtime) Pool(target string) Selector {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.TrimSpace(r.cfg.Routes[target])
	if name == "" {
		return nil
	}
	if pl, ok := r.pools[name]; ok && pl != nil {
		return pl
	}
	return nil
}

// ProxyFunc 返回目标的出站 Proxy 函数（单代理固定、代理池轮询）；无路由返回 nil。
// 用于不支持 RoundTripper 包装的调用方（如面板上游 WorkBuddy）。
func (r *Runtime) ProxyFunc(target string) func(*http.Request) (*url.URL, error) {
	if u := r.SingleURL(target); u != nil {
		return http.ProxyURL(u)
	}
	if pl := r.Pool(target); pl != nil {
		return func(*http.Request) (*url.URL, error) { return pl.Next(), nil }
	}
	return nil
}

// PoolStatuses 返回全部代理池状态（供健康检查/面板展示）。
func (r *Runtime) PoolStatuses() []PoolStatus {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]PoolStatus, 0, len(r.pools))
	for _, p := range r.pools {
		out = append(out, p.Status())
	}
	return out
}
