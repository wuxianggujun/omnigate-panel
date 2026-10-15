// Package opencode adapts OpenCode Zen / OpenCode Go (opencode.ai) to the
// provider.Provider interface.
//
// # 设计边界（改这个包之前请先读完）
//
// OpenCode 有三个上游 tier，本项目只使用其中两个：
//
//	zen    付费按量   https://opencode.ai/zen       仅凭 Authorization: Bearer <key>
//	go     订阅套餐   https://opencode.ai/zen/go    需要 key + x-opencode-session
//	anon   匿名免费   https://opencode.ai/inference 需要伪装成 OpenCode 官方客户端
//
// 匿名 tier 用字面量凭证 "public"，但它的门禁卡的是「客户端身份」而不是「凭证」：
// 上游会逐字节比对 tools 的定义、要求 User-Agent 形如 opencode/latest/<ver>/cli、
// x-opencode-client: cli，以及按降序时间戳铸造的 ses_* 会话 id；任一项不符就是
// 403 FreeTierError ——「OpenCode's free tier can only be used from within OpenCode」。
// 这道门禁的**目的**就是排除第三方网关，所以本包不实现它，也不实现其变体
// （不伪造官方 UA、不铸造 ses_* 形态的会话 id、不注入官方工具定义表）。
//
// 我们走的是上游明确欢迎的那条路。OpenCode Go 官方文档（opencode.ai/docs/go）写着：
//
//	"OpenCode Go is designed for OpenCode and other coding agents that produce
//	 similar types of requests. ...
//	 Identify itself with its own user agent, such as my-coding-agent/1.0,
//	 rather than a generic SDK or HTTP-library name.
//	 Send a stable session ID in x-opencode-session for each conversation so we
//	 can optimize routing and prompt caching."
//
// 并列出已通过验证的第三方客户端（Hermes / Claude Code / Codex / ZCode / Pi /
// jcode / Kilo Code CLI）。本包照此执行：User-Agent 如实申报为 omnigate，
// 会话 id 用网关自己的稳定标识。免费模型（cat-jam-free /
// longcat-2.5-preview-free / step-5-preview-free）在 Go 套餐里本来就是 unlimited，
// 用你自己的 key 正常访问即可，不需要匿名通道。
package opencode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// 上游默认地址。两者都已实测可达：/zen/v1/models 与 /zen/go/v1/models 均返回 200
// （模型目录是公开的，不需要密钥）。
const (
	DefaultZenBase = "https://opencode.ai/zen"
	DefaultGoBase  = "https://opencode.ai/zen/go"

	// TierZen / TierGo 是配置里 tier_prefer 接受的取值。
	TierZen = "zen"
	TierGo  = "go"
)

// UserAgent 如实申报本网关。见包注释：上游文档明确要求第三方客户端「用自己的
// UA」，而不是通用 SDK / HTTP 库名，更不是伪装成 opencode 官方 CLI。
const UserAgent = "omnigate/1.0"

// 密钥冷却时长与 openaibackend 保持一致：鉴权失败（密钥被吊销 / 无权限）冷得久
// 一点，限流只短暂绕开——上游恢复后这把密钥还能用。
const (
	keyCooldownAuth = 10 * time.Minute
	keyCooldownRate = time.Minute
)

// pool 是一个 tier 的上游密钥池。
type pool struct {
	name string // TierZen / TierGo
	base string // 形如 https://opencode.ai/zen

	mu   sync.Mutex
	keys []string
	next int
	bad  map[string]time.Time
}

func newPool(name, base string, keys []string) *pool {
	return &pool{name: name, base: strings.TrimRight(base, "/"), keys: clean(keys)}
}

// clean 去掉空白与空串。上游密钥池里出现空串会把「轮询」变成「拿空 key 打上游」。
func clean(keys []string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// snapshot 返回当前池成员（拷贝，避免调用方持锁）。
func (p *pool) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.keys))
	copy(out, p.keys)
	return out
}

// pick 取下一个密钥：第一轮只挑「没在冷却、本次没试过」的；全池冷却时第二轮退让
// 到「只要本次没试过」（宁可冒险也别直接失败）。tried 为 nil 表示不排除任何密钥。
func (p *pool) pick(tried map[string]bool) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.keys)
	if n == 0 {
		return "", false
	}
	if n == 1 {
		if tried[p.keys[0]] {
			return "", false
		}
		return p.keys[0], true
	}
	now := time.Now()
	for pass := 0; pass < 2; pass++ {
		for j := 0; j < n; j++ {
			i := (p.next + j) % n
			k := p.keys[i]
			if tried[k] {
				continue
			}
			if pass == 0 {
				if until, bad := p.bad[k]; bad && now.Before(until) {
					continue
				}
			}
			p.next = (i + 1) % n
			return k, true
		}
	}
	return "", false
}

// markBad 把密钥放进冷却；markGood 清除它的冷却。
func (p *pool) markBad(key string, d time.Duration) {
	if key == "" || d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.bad == nil {
		p.bad = map[string]time.Time{}
	}
	p.bad[key] = time.Now().Add(d)
}

func (p *pool) markGood(key string) {
	if key == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.bad, key)
}

// keyCooldownFor 按上游状态码决定冷却时长；0 = 不参与轮换（本 tier 直接放弃）。
func keyCooldownFor(status int) time.Duration {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return keyCooldownAuth
	case http.StatusTooManyRequests:
		return keyCooldownRate
	}
	return 0
}

// upstreamErrKind 是一次上游失败的性质，决定该不该换密钥再试。
type upstreamErrKind int

const (
	// errKey：这把密钥不可用（无效 / 被吊销 / 被限流）→ 池内换下一把。
	errKey upstreamErrKind = iota
	// errModel：模型或出口地区层面的拒绝 → 换密钥、换 tier 都没用。
	errModel
	// errTier：本 tier 提供不了这个模型 → 回落到下一个 tier。
	errTier
)

// classifyFailure 判定一次失败的性质。
//
// 必须把「密钥的问题」与「模型的问题」分开：两者都是 4xx，处置却完全相反。不区分的
// 后果很具体——一个地区受限的模型会把整池密钥逐个冷却一遍，一次请求就赔掉全部密钥
// （实测 2026-10-15：本机出口请求 cat-jam-free 恒返回 403 RegionError，而它是 go 套餐
// 目录里的模型，用户完全可能勾上它）。
//
// 按响应体里的 type 串判定，而不是按嵌套位置：上游的错误信封有两种形态
// （{"error":{"type":"RegionError"…}} 与 {"type":"error","error":{…}}），按位置解析会漏。
func classifyFailure(status int, body []byte) upstreamErrKind {
	s := string(body)
	switch {
	case strings.Contains(s, "RegionError"),
		strings.Contains(s, "FreeTierError"),
		strings.Contains(s, "MissingSessionID"):
		return errModel
	case strings.Contains(s, "Invalid credential"),
		strings.Contains(s, "AuthError"),
		strings.Contains(s, "Missing API key"):
		return errKey
	}
	// 兜底按状态码，与 openaibackend 同口径。
	if keyCooldownFor(status) > 0 {
		return errKey
	}
	return errTier
}

// modelFailureHint 给「与密钥无关」的拒绝补一句人话，省得用户在密钥池里空找原因。
func modelFailureHint(body []byte) string {
	s := string(body)
	switch {
	case strings.Contains(s, "RegionError"):
		return "该模型在当前出口 IP 的地区不可用；换密钥或换 tier 都不会变，请改用它支持的模型或换出口"
	case strings.Contains(s, "FreeTierError"):
		return "该模型的免费额度只对官方客户端开放；本 provider 如实申报自己的身份、不伪装，请改用套餐内其它模型"
	case strings.Contains(s, "MissingSessionID"):
		return "上游要求 x-opencode-session 头，说明本 provider 的请求装配有问题，请上报"
	}
	return ""
}

// Options 是构造一个 opencode 供应商所需的全部参数。
type Options struct {
	Name string
	// ZenKeys / GoKeys 是两个 tier 各自的密钥池。空池 = 该 tier 不参与轮询。
	ZenKeys []string
	GoKeys  []string
	// Prefer 决定先走哪个 tier（"go" 或 "zen"）；空 = "go"。
	// 默认与上游文档一致：订阅套餐优先，用不完再落到按量付费。
	Prefer string
	// ZenBase / GoBase 覆盖上游地址；空 = 用默认值。
	ZenBase string
	GoBase  string
	// Models 是上游模型目录拉取失败时的兜底清单。
	Models []string
}

// Provider 是 OpenCode Zen / Go 的上游适配器。
type Provider struct {
	name   string
	pools  []*pool
	models []string
	http   *http.Client
}

// New 构造 provider。只保留「配了密钥」的 tier——没有密钥的 tier 留在轮询里
// 只会让每次请求都白跑一趟 401。
func New(o Options) *Provider {
	zenBase := o.ZenBase
	if zenBase == "" {
		zenBase = DefaultZenBase
	}
	goBase := o.GoBase
	if goBase == "" {
		goBase = DefaultGoBase
	}
	byName := map[string]*pool{
		TierZen: newPool(TierZen, zenBase, o.ZenKeys),
		TierGo:  newPool(TierGo, goBase, o.GoKeys),
	}
	order := []string{TierGo, TierZen}
	if strings.EqualFold(o.Prefer, TierZen) {
		order = []string{TierZen, TierGo}
	}
	p := &Provider{
		name:   o.Name,
		models: clean(o.Models),
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
	for _, name := range order {
		if pl := byName[name]; len(pl.snapshot()) > 0 {
			p.pools = append(p.pools, pl)
		}
	}
	return p
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "opencode" }

// Ready 报告是否有任何一个 tier 配了密钥。面板与健康检查用它区分「空壳」。
func (p *Provider) Ready() bool { return len(p.pools) > 0 }

// Tiers 返回参与轮询的 tier 名（按 prefer 顺序），供诊断使用。
func (p *Provider) Tiers() []string {
	out := make([]string, 0, len(p.pools))
	for _, pl := range p.pools {
		out = append(out, pl.name)
	}
	return out
}

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
// selected per request by sel (nil = direct).
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

// decorate 给请求带上鉴权与会话身份。
//
// 会话头按上游文档发送，用于路由与提示词缓存优化；User-Agent 如实申报本网关
// （见包注释，这里刻意不伪装成官方客户端）。
func (p *Provider) decorate(req *http.Request, key, session string) {
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", UserAgent)
	if session != "" {
		req.Header.Set("x-opencode-session", session)
	}
}

// sessionID 派生发往上游的稳定会话标识。
//
// 上游要求「每个会话一个稳定 id」：同一对话必须始终落在同一个 id 上，否则提示词
// 缓存每次都失效。网关只在 prompt 型供应商（runable/geminiweb）上填 ChatID，
// OpenAI 兼容路径是空的，所以这里按「模型 + 首条消息」派生：首条消息在一次对话
// 里不变，因此 id 稳定；同时它天然按对话内容区分（开场相同的会话会共享 id，
// 这与上游「会话亲和」的语义一致，也不影响正确性）。
func sessionID(in provider.ChatInput) string {
	if in.ChatID != "" {
		return in.ChatID
	}
	seed := "opencode-session\x00" + in.Model
	if len(in.Messages) > 0 {
		if b, err := json.Marshal(in.Messages[0]); err == nil {
			sum := sha256.Sum256(b)
			seed += "\x00" + hex.EncodeToString(sum[:8])
		}
	}
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:16])
}

// ListModels 拉取各 tier 的 /v1/models 并取并集。
//
// 目录是公开的（实测无需密钥即返回 200），两个 tier 的模型集并不相同——zen 86 个、
// go 37 个——所以逐个 tier 拉、按 prefer 顺序去重合并；全部失败才回落到配置里写死
// 的模型清单。某个 tier 拉不到不算错误：另一个 tier 还能提供模型。
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	seen := map[string]bool{}
	out := make([]openai.Model, 0, 64)
	var lastErr error
	for _, pl := range p.pools {
		models, err := p.listTier(ctx, pl)
		if err != nil {
			lastErr = err
			continue
		}
		for _, m := range models {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	for _, id := range p.models {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, openai.Model{ID: id, Object: "model", OwnedBy: p.name})
	}
	if len(out) > 0 {
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("opencode: 未配置 zen/go 密钥池，无法获取模型目录")
}

// listTier 拉取单个 tier 的模型目录。
func (p *Provider) listTier(ctx context.Context, pl *pool) ([]openai.Model, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pl.base+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	if key, ok := pl.pick(nil); ok {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("opencode/%s 拉取模型目录失败: %w", pl.name, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("opencode/%s 拉取模型目录 HTTP %d: %s",
			pl.name, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ml openai.ModelList
	if err := json.Unmarshal(body, &ml); err != nil {
		return nil, fmt.Errorf("opencode/%s 模型目录解析失败: %w", pl.name, err)
	}
	out := make([]openai.Model, 0, len(ml.Data))
	for _, m := range ml.Data {
		if m.ID == "" {
			continue
		}
		if m.Object == "" {
			m.Object = "model"
		}
		if m.OwnedBy == "" {
			m.OwnedBy = p.name
		}
		out = append(out, m)
	}
	return out, nil
}

// StreamChat 发一次流式 chat completion。
//
// 轮询是两层：外层按 prefer 顺序走 tier，内层走该 tier 的密钥池。失败按性质分三类
// （见 classifyFailure）：密钥类在池内轮换；模型类当场失败（换密钥、换 tier 都是
// 白打，还会把好密钥冷却掉）；其余 4xx（例如该模型不在这个 tier 上）放弃本 tier、
// 回落到下一个 tier——这样「模型只在 go 上」和「模型只在 zen 上」都能自动落到对的
// 一侧，不需要在配置里逐模型标注。
func (p *Provider) StreamChat(ctx context.Context, acc *provider.Account, in provider.ChatInput) (provider.Stream, error) {
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
	raw, _ := json.Marshal(body)
	session := sessionID(in)

	var lastErr error
	for _, pl := range p.pools {
		tried := map[string]bool{}
		for {
			key, ok := pl.pick(tried)
			if !ok {
				break
			}
			tried[key] = true

			req, err := http.NewRequestWithContext(ctx, http.MethodPost,
				pl.base+"/v1/chat/completions", bytes.NewReader(raw))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "text/event-stream")
			p.decorate(req, key, session)

			resp, err := p.http.Do(req)
			if err != nil {
				// 网络层错误与密钥无关：不轮换，否则一次网络抖动会把整池密钥冷却掉。
				return nil, err
			}
			if resp.StatusCode >= 400 {
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				lastErr = fmt.Errorf("opencode/%s HTTP %d: %s",
					pl.name, resp.StatusCode, strings.TrimSpace(string(b)))
				kind := classifyFailure(resp.StatusCode, b)
				if kind == errModel {
					// 模型/地区层面的拒绝与密钥无关：换密钥既没用，又会让一次请求
					// 把整池密钥逐个冷却掉，所以当场失败并补一句人话。
					if hint := modelFailureHint(b); hint != "" {
						lastErr = fmt.Errorf("%w｜%s", lastErr, hint)
					}
					return nil, lastErr
				}
				// 只有「这把密钥的问题」才换密钥重试；密钥池只有一个成员时也直接
				// 放弃本 tier（换无可换）。
				if kind == errKey && len(pl.snapshot()) > 1 {
					if d := keyCooldownFor(resp.StatusCode); d > 0 {
						pl.markBad(key, d)
						continue
					}
				}
				break
			}
			pl.markGood(key)
			return provider.NewOpenAIStream(resp.Body), nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("opencode: 没有可用的上游密钥（请在面板的「上游供应商 → %s」里填写 zen / go 密钥池）", p.name)
	}
	return nil, lastErr
}
