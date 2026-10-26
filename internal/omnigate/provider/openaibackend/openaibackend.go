// Package openaibackend adapts any OpenAI-compatible upstream (OpenAI, DeepSeek,
// GLM, Kimi, OpenRouter, Ollama, ...) to the provider.Provider interface. This is
// how omnigate "supports other" upstreams without touching the core.
package openaibackend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/egress"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/wire"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// Provider is an OpenAI-compatible upstream.
type Provider struct {
	name    string
	baseURL string
	apiKey  string
	models  []string
	http    *http.Client
	// egress is the outbound client identity (UA / extra headers / TLS hello).
	// Zero = honest "omnigate/<ver>".
	egress egress.Profile
	// format is the upstream dialect this provider speaks. Defaults to openai.
	format wire.Format

	// 上游密钥池（可选）。非空时按请求轮询：某个 key 拿到 401/403（key 不可用）
	// 或 429（限流）就冷却并自动切下一个；额度耗尽类失败（quota）则长禁用并记账
	// （见 keyDisableQuota / Health）——连退让轮都不再试它，连续失败达到阈值后由
	// 面板侧自动摘除。空 = 只用单个 api_key（旧写法不变）。
	mu     sync.Mutex
	keys   []string
	next   int
	states map[string]Health // key → 健康状态（冷却 / 禁用 / 连击）
	health HealthStore       // 可选：状态持久化（跨重启与热重载）
	logf   func(format string, args ...any)
}

// New builds an OpenAI-compatible provider. baseURL should include the version
// prefix, e.g. https://api.openai.com/v1.
func New(name, baseURL, apiKey string, models []string) *Provider {
	return NewWithOptions(Options{Name: name, BaseURL: baseURL, APIKey: apiKey, Models: models})
}

// Options configures an OpenAI-compatible provider.
type Options struct {
	Name    string
	BaseURL string
	APIKey  string
	APIKeys []string
	Models  []string
	// Client is the outbound identity. Zero = honest omnigate UA, which is what
	// every existing deployment got before the identity layer existed.
	Client egress.Profile
	// Protocol is the upstream dialect: openai (default), anthropic or
	// responses. Anything unrecognized degrades to openai rather than failing to
	// build, so a hand-edited config never leaves a provider unroutable.
	Protocol string
	// Transport, when non-nil, replaces the built-in one (proxy pools install
	// their selector afterwards; tests inject a stub).
	Transport http.RoundTripper
}

// NewWithOptions builds a provider with an explicit outbound identity/protocol.
func NewWithOptions(o Options) *Provider {
	p := &Provider{
		name:    o.Name,
		baseURL: strings.TrimRight(o.BaseURL, "/"),
		apiKey:  o.APIKey,
		models:  o.Models,
		egress:  o.Client.Normalized(),
		format:  wire.ParseFormat(o.Protocol),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
	if o.Transport != nil {
		p.http.Transport = o.Transport
	}
	p.applyTLS()
	if len(o.APIKeys) > 0 {
		p.SetAPIKeys(o.APIKeys)
	}
	return p
}

// SetClient replaces the outbound identity (hot reload / tests).
func (p *Provider) SetClient(prof egress.Profile) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.egress = prof.Normalized()
	p.mu.Unlock()
	p.applyTLS()
}

// Client returns the resolved outbound identity, for diagnostics and the panel.
func (p *Provider) Client() egress.Resolved {
	if p == nil {
		return egress.Profile{}.Resolve()
	}
	p.mu.Lock()
	prof := p.egress
	p.mu.Unlock()
	return prof.Resolve()
}

// SetProtocol switches the upstream dialect (hot reload).
func (p *Provider) SetProtocol(protocol string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.format = wire.ParseFormat(protocol)
	p.mu.Unlock()
}

// Protocol returns the upstream dialect name.
func (p *Provider) Protocol() string {
	if p == nil {
		return string(wire.FormatOpenAI)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.format)
}

// applyTLS installs the utls ClientHello promised by the identity profile. The
// gateway had that capability only inside geminiweb; the identity layer makes it
// available to any provider that asks for it (claude-code / codex presets do).
//
// It is a no-op unless a TLS profile is configured, so nothing changes for the
// providers that never asked for impersonation.
func (p *Provider) applyTLS() {
	if p == nil || p.http == nil {
		return
	}
	tr, ok := p.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	target := p.Client().TLSProfile
	if target == "" {
		tr.DialTLSContext = nil
		tr.CloseIdleConnections()
		return
	}
	imp := fingerprint.NewTLSImpersonator(target)
	if !imp.Enabled() {
		// Unsupported target: keep the standard library handshake instead of
		// silently pretending to impersonate.
		tr.DialTLSContext = nil
		tr.CloseIdleConnections()
		return
	}
	dialer := &net.Dialer{Timeout: 20 * time.Second}
	imp.Apply(tr, dialer)
	// utls speaks HTTP/1.1 or h2 depending on ALPN; net/http needs this to build
	// the h2 stack for a custom DialTLSContext.
	tr.ForceAttemptHTTP2 = true
	tr.CloseIdleConnections()
}

// 密钥池冷却时长：鉴权失败（key 被吊销 / 无权限）冷得久一点，限流（429）只短暂
// 绕开——上游恢复后这把 key 还能用。额度耗尽（quota）不是「过一会就好」：长禁用
// 24 小时，到期自动重试一次；仍失败则连击 +1，连击满 3 次且首见超过清理窗口后
// 由面板侧的后台任务把它从配置里摘除（见 cmd/server 的 CleanupDeadKeys）。
const (
	keyCooldownAuth = 10 * time.Minute
	keyCooldownRate = time.Minute
	keyDisableQuota = 24 * time.Hour
)

// Health 单把上游密钥的健康状态（落库 + 面板展示用）。
type Health struct {
	Reason    string `json:"reason,omitempty"`     // quota / auth / rate：最近一次失败分类
	Until     int64  `json:"until,omitempty"`      // 冷却截止（unix 秒；0 = 无冷却）
	Disabled  bool   `json:"disabled,omitempty"`   // true = 额度型长禁用（Until 到期自动重试）
	Strikes   int    `json:"strikes,omitempty"`    // 连续额度错误次数（任一次成功即清零）
	FirstSeen int64  `json:"first_seen,omitempty"` // 本轮连续额度错误首见时间（unix 秒）
	LastError string `json:"last_error,omitempty"` // 上游错误原文（截断）
	UpdatedAt int64  `json:"updated_at,omitempty"`
}

// HealthStore 密钥健康状态的外部存储（可选）。实现方需保证并发安全；key 以
// KeyID（哈希）引用，不落明文。
type HealthStore interface {
	LoadHealth(provider string) map[string]Health
	SaveHealth(provider, id string, h Health)
	DropHealth(provider, id string)
}

// KeyID 单把 key 的稳定短标识（sha256 前 8 字节 hex）：落库 / 面板引用不存明文。
func KeyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// MaskKey 密钥的日志 / 展示掩码：前 8 位 + … + 后 4 位。绝不回显完整密钥。
func MaskKey(key string) string {
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return ""
	case len(key) <= 14:
		return key[:1] + "…"
	default:
		return key[:8] + "…" + key[len(key)-4:]
	}
}

// quotaErrorMarkers 上游「额度耗尽」类错误的 body 特征（小写匹配）。命中即按
// 死 key（长禁用）处理，而不是短暂限流。
var quotaErrorMarkers = []string{
	"insufficient_quota",
	"quota exhausted",
	"exceeded your current quota",
	"insufficient credit",
	"credits exhausted",
	"credit exhausted",
	"out of credit",
	"no credits",
	"no credit",
	"余额不足",
	"额度用尽",
	"额度耗尽",
	"积分不足",
}

// isQuotaFailure 判断一次上游失败是否属于「这把 key 的额度耗尽」。402 直接算；
// 429/403 看 body 特征串。分不清的按短限流走（宁可短暂冷却，不误杀）。
func isQuotaFailure(status int, body []byte) bool {
	if status == http.StatusPaymentRequired {
		return true
	}
	if status != http.StatusTooManyRequests && status != http.StatusForbidden {
		return false
	}
	low := strings.ToLower(string(body))
	for _, m := range quotaErrorMarkers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

// truncateErr 落库用的错误原文截断（body 可能很大，状态里只留线索）。
func truncateErr(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		return s[:240] + "…"
	}
	return s
}

// SetAPIKeys 启用上游密钥池（多密钥轮询）。空池 / 单项池等于没启轮询；调用会重置
// 轮询游标与健康状态（面板保存配置后热重载会重建 provider，这里主要供测试与
// 运行时替换）。持久化的健康状态由随后的 SetHealthStore 载入。
func (p *Provider) SetAPIKeys(keys []string) {
	if p == nil {
		return
	}
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			clean = append(clean, k)
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.keys = clean
	p.next = 0
	p.states = nil
}

// SetHealthStore 接入健康状态持久化（可选）：载入该 provider 已保存的密钥状态，
// 清掉「已不在池里」的陈旧记录；之后每次状态变化写穿。热重载重建 provider 时由
// 网关装配（先 SetAPIKeys 再调用本方法）。
func (p *Provider) SetHealthStore(s HealthStore) {
	if p == nil || s == nil {
		return
	}
	p.mu.Lock()
	p.health = s
	keys := append([]string(nil), p.keys...)
	p.mu.Unlock()
	if len(keys) == 0 {
		return
	}
	saved := s.LoadHealth(p.name)
	valid := make(map[string]string, len(keys)) // id → key
	for _, k := range keys {
		valid[KeyID(k)] = k
	}
	for id := range saved {
		if _, ok := valid[id]; !ok {
			s.DropHealth(p.name, id)
		}
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.states == nil {
		p.states = map[string]Health{}
	}
	for id, k := range valid {
		h, ok := saved[id]
		if !ok {
			continue
		}
		if !now.Before(time.Unix(h.Until, 0)) {
			switch {
			case h.Disabled:
				// 禁用已到期 = 回到「待重试试炼」；保留连击记账（清理判定要它）。
				h.Disabled = false
				h.Until = 0
				s.SaveHealth(p.name, id, h)
			case h.Strikes == 0:
				// 短冷却早过、又没有未结连击：干净状态，不驻留。
				s.DropHealth(p.name, id)
				continue
			}
		}
		p.states[k] = h
	}
}

// SetLogger 接入日志（可选）：密钥自动禁用 / 恢复各留一行，便于事后对账。
func (p *Provider) SetLogger(f func(format string, args ...any)) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.logf = f
	p.mu.Unlock()
}

// candidates 本 provider 可用的候选 key：密钥池优先，否则回落到单个 api_key。
func (p *Provider) candidates() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) > 0 {
		out := make([]string, len(p.keys))
		copy(out, p.keys)
		return out
	}
	if p.apiKey != "" {
		return []string{p.apiKey}
	}
	return nil
}

// pick 取下一个 key：第一轮只挑「没在冷却、本次没试过」的；全池冷却时第二轮退让
// 到「只要本次没试过」（宁可冒险也别直接失败）。额度型长禁用的 key 例外——它的
// 重试只会再拿回同一句 quota，连退让轮也不试。tried 为 nil 表示不排除任何 key。
func (p *Provider) pick(tried map[string]bool) (string, bool) {
	cands := p.candidates()
	if len(cands) == 0 {
		return "", false
	}
	if len(cands) == 1 {
		if tried[cands[0]] {
			return "", false
		}
		return cands[0], true
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	n := len(cands)
	for pass := 0; pass < 2; pass++ {
		for j := 0; j < n; j++ {
			i := (p.next + j) % n
			k := cands[i]
			if tried[k] {
				continue
			}
			if h, ok := p.states[k]; ok && now.Before(time.Unix(h.Until, 0)) {
				if h.Disabled || pass == 0 {
					continue
				}
			}
			p.next = (i + 1) % n
			return k, true
		}
	}
	return "", false
}

// allDisabledMsg 全池都处于「额度耗尽自动禁用」时，给一句人能看懂的原因，
// 不再把上游的 quota 原文当场抛出来。
func (p *Provider) allDisabledMsg() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) < 2 {
		return ""
	}
	now := time.Now()
	disabled := 0
	var earliest int64
	for _, k := range p.keys {
		h, ok := p.states[k]
		if !ok || !h.Disabled || !now.Before(time.Unix(h.Until, 0)) {
			continue
		}
		disabled++
		if earliest == 0 || h.Until < earliest {
			earliest = h.Until
		}
	}
	if disabled == 0 || disabled != len(p.keys) {
		return ""
	}
	return fmt.Sprintf("上游 %d 把密钥均因额度耗尽被自动禁用（最早 %s 自动重试）", disabled,
		time.Unix(earliest, 0).Format("01-02 15:04"))
}

// touch 在锁内修改 key 的状态并返回修改后的副本；有持久化后端时写穿。
func (p *Provider) touch(key string, fn func(h *Health)) Health {
	p.mu.Lock()
	if p.states == nil {
		p.states = map[string]Health{}
	}
	h := p.states[key]
	fn(&h)
	p.states[key] = h
	store := p.health
	p.mu.Unlock()
	if store != nil {
		store.SaveHealth(p.name, KeyID(key), h)
	}
	return h
}

// markCool 把 key 放进短冷却（auth / rate 类失败）；markQuota 把它长禁用（额度
// 耗尽），markGood 清除状态并在恢复时留一行日志。
func (p *Provider) markCool(key string, d time.Duration, reason, lastErr string) {
	if key == "" || d <= 0 {
		return
	}
	now := time.Now()
	p.touch(key, func(h *Health) {
		h.Reason = reason
		h.Until = now.Add(d).Unix()
		h.Disabled = false
		h.LastError = truncateErr(lastErr)
		h.UpdatedAt = now.Unix()
	})
}

// markQuota 额度耗尽：长禁用 + 连击 +1（首见保持），并留一行日志。
func (p *Provider) markQuota(key, lastErr string) {
	if key == "" {
		return
	}
	now := time.Now()
	h := p.touch(key, func(h *Health) {
		h.Reason = "quota"
		h.Until = now.Add(keyDisableQuota).Unix()
		h.Disabled = true
		h.Strikes++
		if h.FirstSeen == 0 {
			h.FirstSeen = now.Unix()
		}
		h.LastError = truncateErr(lastErr)
		h.UpdatedAt = now.Unix()
	})
	if p.logf != nil {
		p.logf("%s: 上游密钥 %s 额度耗尽，自动禁用（连续第 %d 次，%s 后自动重试一次）",
			p.name, MaskKey(key), h.Strikes, keyDisableQuota)
	}
}

func (p *Provider) markGood(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	h, had := p.states[key]
	if had {
		delete(p.states, key)
	}
	store := p.health
	logf := p.logf
	p.mu.Unlock()
	if !had {
		return
	}
	if store != nil {
		store.DropHealth(p.name, KeyID(key))
	}
	if logf != nil && h.Strikes > 0 {
		logf("%s: 上游密钥 %s 已恢复正常（此前连续 %d 次额度错误）", p.name, MaskKey(key), h.Strikes)
	}
}

// keyCooldownFor 按上游状态码决定冷却时长；0 = 不参与轮询（直接报错）。
func keyCooldownFor(status int) time.Duration {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return keyCooldownAuth
	case http.StatusTooManyRequests:
		return keyCooldownRate
	}
	return 0
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// SetProxy routes this provider's outbound requests through u (nil = direct).
func (p *Provider) SetProxy(u *url.URL) {
	if u == nil {
		return
	}
	if tr, ok := p.http.Transport.(*http.Transport); ok {
		tr.Proxy = http.ProxyURL(u)
		tr.CloseIdleConnections()
	}
}

// SetProxySelector routes this provider's outbound requests through the proxy pool
// selected per request by sel (nil = direct). Unlike SetProxy it adds
// connection-level failover (retry the next proxy on a connection error).
func (p *Provider) SetProxySelector(sel outbound.Selector) {
	if p == nil || p.http == nil {
		return
	}
	base, ok := p.http.Transport.(*http.Transport)
	if !ok {
		return
	}
	p.http.Transport = outbound.WrapTransport(base, sel)
}

// Type returns the provider type.
func (p *Provider) Type() string { return "openai" }

// ListModels 返回供应商的模型目录。
//
// 显式配置了 models 时，它就是目录白名单：直接返回该清单，绝不访问上游 /models。
// 否则「手动只加了一个模型」会被自动发现覆盖成上游的全部模型。models 留空才自动发现。
//
// 自动发现失败必须返回 error，不能吞成「成功但目录为空」：网关拿「成功」当作
// 「这个供应商确实没有模型」，会用它覆盖掉上一次的完好目录，并把该供应商从落库
// 的模型目录快照里删掉。一次网络抖动就足以让 aotera 的 7 个模型从线上目录消失
// （重启后日志写「已载入 5 个 provider」，库里却只剩 4 个）。空目录同理：上游偶发
// 返回 data:[] 时保留旧目录比清空它更接近事实。
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	if len(p.models) > 0 {
		out := make([]openai.Model, 0, len(p.models))
		for _, id := range p.models {
			out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: p.name})
		}
		return out, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("拉取 %s 模型目录失败：%w", p.name, err)
	}
	key, _ := p.pick(nil)
	// 模型目录请求走同一套出站身份：只收 Claude Code 流量的中转站在
	// /v1/models 上同样会校验客户端形态。这里用 openai 方言的鉴权（Bearer），
	// 因为 /models 本身是 OpenAI 形态的端点，与聊天所用的方言无关。
	p.stamp(req, wire.FormatOpenAI, key)
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 %s 模型目录失败：%w", p.name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("拉取 %s 模型目录失败：上游返回 HTTP %d", p.name, resp.StatusCode)
	}
	var ml openai.ModelList
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ml); err != nil {
		return nil, fmt.Errorf("解析 %s 模型目录失败：%w", p.name, err)
	}
	out := make([]openai.Model, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.Object == "" {
			m.Object = "model"
		}
		if m.OwnedBy == "" {
			m.OwnedBy = p.name
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("拉取 %s 模型目录失败：上游返回空目录", p.name)
	}
	return out, nil
}

// StreamChat sends a streaming chat completion and normalizes the stream. When an
// upstream key pool is configured it rotates on auth/rate failures, so one dead
// or throttled key doesn't take the whole provider down.
//
// The request body and the response codec both come from the configured protocol
// (openai / anthropic / responses), so this method has no per-dialect branch and
// a relay that only accepts /v1/messages is reachable through the same adapter.
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	format, endpoint, raw, err := p.buildRequest(in)
	if err != nil {
		return nil, err
	}

	tried := map[string]bool{}
	var lastErr error
	for {
		key, ok := p.pick(tried)
		if !ok {
			if lastErr != nil {
				return nil, lastErr
			}
			if msg := p.allDisabledMsg(); msg != "" {
				return nil, errors.New(msg)
			}
			return nil, fmt.Errorf("no upstream api key available")
		}
		tried[key] = true
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		p.stamp(req, format, key)
		resp, err := p.http.Do(req)
		if err != nil {
			// 网络层错误与 key 无关：不轮换，否则一次网络抖动会把整池 key 冷却掉。
			return nil, err
		}
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			status := resp.StatusCode
			lastErr = fmt.Errorf("upstream HTTP %d: %s", status, string(b))
			// 只有「这把 key 的问题」才换 key 重试：401/403（失效/无权限）、
			// 429（限流）；额度耗尽类（402，或带 quota 文案的 429/403）直接长
			// 禁用——重试换 key 有意义，原地再打只是重演同一句报错。单 key 或
			// 其余错误码直接上报，行为与从前一致。
			if len(p.candidates()) > 1 {
				if isQuotaFailure(status, b) {
					p.markQuota(key, string(b))
					continue
				}
				if d := keyCooldownFor(status); d > 0 {
					reason := "rate"
					if status == http.StatusUnauthorized || status == http.StatusForbidden {
						reason = "auth"
					}
					p.markCool(key, d, reason, string(b))
					continue
				}
			}
			return nil, lastErr
		}
		p.markGood(key)
		return wrapStream(format, resp.Body), nil
	}
}

// buildRequest encodes in for the configured protocol and returns the endpoint
// suffix together with the serialized body.
func (p *Provider) buildRequest(in provider.ChatInput) (wire.Format, string, []byte, error) {
	p.mu.Lock()
	format := p.format
	p.mu.Unlock()
	if format == "" {
		format = wire.FormatOpenAI
	}
	var (
		raw []byte
		err error
	)
	switch format {
	case wire.FormatAnthropic:
		raw, err = wire.EncodeAnthropic(in)
	case wire.FormatResponses:
		raw, err = wire.EncodeResponses(in)
	default:
		format = wire.FormatOpenAI
		raw, err = encodeChat(in)
	}
	if err != nil {
		return format, "", nil, err
	}
	return format, format.Path(), raw, nil
}

// encodeChat is the pre-existing chat/completions body builder.
func encodeChat(in provider.ChatInput) ([]byte, error) {
	body := map[string]any{
		"model":    in.Model,
		"messages": in.Messages,
		"stream":   true,
	}
	if len(in.Tools) > 0 {
		body["tools"] = in.Tools
	}
	if in.ReasoningEffort != "" {
		body["reasoning_effort"] = in.ReasoningEffort
	}
	provider.ApplySampling(body, in)
	return json.Marshal(body)
}

// stamp applies the outbound identity and auth to one request. Auth stays
// format-specific: chat/Responses want "Bearer", Anthropic wants "x-api-key".
func (p *Provider) stamp(req *http.Request, format wire.Format, key string) {
	p.mu.Lock()
	prof := p.egress
	p.mu.Unlock()
	prof.Resolve().Apply(req)
	req.Header.Set("Content-Type", "application/json")
	if format == wire.FormatAnthropic {
		// Anthropic authenticates with x-api-key; the browser-style "Bearer"
		// header is not accepted by the real API and relays mirror that.
		req.Header.Set("x-api-key", key)
		req.Header.Del("Authorization")
		if req.Header.Get("anthropic-version") == "" {
			req.Header.Set("anthropic-version", "2023-06-01")
		}
		// Every request built here asks for stream=true, and a CLI preset sets
		// Accept: application/json because that is what a real client sends for
		// non-streaming calls. Leaving it would contradict the body we just
		// built, so the streaming Accept wins.
		req.Header.Set("Accept", "text/event-stream")
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "text/event-stream")
}

// wrapStream picks the decoder for the dialect the request was made in.
func wrapStream(format wire.Format, body io.ReadCloser) provider.Stream {
	switch format {
	case wire.FormatAnthropic:
		return wire.NewAnthropicStream(body)
	case wire.FormatResponses:
		return wire.NewResponsesStream(body)
	default:
		return provider.NewOpenAIStream(body)
	}
}
