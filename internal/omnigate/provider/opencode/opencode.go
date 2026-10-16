// Package opencode adapts OpenCode Zen / Go / 匿名免费 (opencode.ai) to the
// provider.Provider interface.
//
// # 设计边界（改这个包之前请先读完）
//
// OpenCode 有三个上游 tier，本包全部使用：
//
//	zen    付费按量   https://opencode.ai/zen       仅凭 Authorization: Bearer <key>
//	go     订阅套餐   https://opencode.ai/zen/go    需要 key + x-opencode-session
//	anon   匿名免费   https://opencode.ai/inference 零密钥（凭证是字面量 "public"）
//
// zen / go 走上游明确欢迎的那条路。OpenCode Go 官方文档（opencode.ai/docs/go）写着：
//
//	"OpenCode Go is designed for OpenCode and other coding agents that produce
//	 similar types of requests. ...
//	 Identify itself with its own user agent, such as my-coding-agent/1.0,
//	 rather than a generic SDK or HTTP-library name.
//	 Send a stable session ID in x-opencode-session for each conversation so we
//	 can optimize routing and prompt caching."
//
// 本包照此执行：zen/go 上 User-Agent 如实申报为 omnigate，会话 id 用网关自己的
// 稳定标识（见 TestKeyLanesDeclareOwnIdentity）。
//
// 匿名 tier 是另一回事：它的门禁卡的是「客户端身份」而不是凭证——上游逐项核验
// 官方工具定义表（五件套 shell/edit/glob/grep/read 缺一不可、名字必须一致）、
// UA 形态（opencode/latest/<ver>/cli）、x-opencode-client: cli、按降序时间戳
// 铸造的 ses_* 会话 id，并点名拒绝 x-opencode-request；任一项不符即
// 403 FreeTierError——「OpenCode's free tier can only be used from within OpenCode」。
// 门禁的目的本是排除第三方网关。
//
// 本包实现匿名通道，是**项目所有者的明确要求**（2026-10-15 决定反转）：面板把
// 它做成默认开启的「零密钥免费通道」，免费模型匿名优先、zen/go 密钥降为付费通道
// 与免费模型回退。协议事实全部来自同一出口 IP 上的实测，并写成测试钉死——
// 上游门禁一变，TestAnonymousLane* 先亮红灯，而不是线上静默 403。
//
// 取舍摆在这里：两条通道的身份策略刻意不同（zen/go 如实申报、anon 镜像客户端），
// 镜像只用于匿名免费层。若上游把门禁收紧到需要复刻整条客户端链路（TLS 指纹、
// 请求时序、TUI 行为……），应当停下来重新评估，而不是无条件加码。
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

// UserAgent 是 zen / go tier 上如实申报本网关的 UA。见包注释：上游文档明确要求
// 第三方客户端「用自己的 UA」，而不是通用 SDK / HTTP 库名。
//
// 匿名通道不适用它——那里的门禁要求官方 CLI 形态的 UA（anonUserAgent，见
// anonymous.go）。两条通道的身份策略刻意分开，改这里前先想清楚改的是哪条。
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
		return "免费门禁拒绝了该请求：模型不在匿名免费范围内，或匿名通道的客户端镜像已过期（上游门禁变动，请上报）；付费模型请在面板给本供应商配置 zen/go 密钥"
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
	// MetaURL 覆盖模型元数据目录地址（上下文长度 / 价格 / 免费标记，见 metadata.go）；
	// 空 = DefaultMetaURL。
	MetaURL string
	// Anonymous 开启匿名免费通道（零密钥，见 anonymous.go）：免费模型匿名优先，
	// zen/go 密钥池降为付费通道与免费模型回退。配置层的默认值见
	// config.ProviderConfig.AnonymousEnabled（未设置 = 开启）。
	Anonymous bool
	// AnonBase 覆盖匿名通道地址；空 = DefaultAnonBase。
	AnonBase string
}

// Provider 是 OpenCode Zen / Go / 匿名免费 的上游适配器。
type Provider struct {
	name string
	// all 是全部 tier 的密钥池（按 prefer 顺序，含没配密钥的）：列公开目录、
	// 纯匿名供应商的目录来源都要用它；pools 只留配了密钥的（轮询用）。
	all   []*pool
	pools []*pool
	anon  string // 匿名免费通道 base；空 = 未启用
	// models 是配置里的兜底模型清单。
	models  []string
	http    *http.Client
	meta    metaCache   // 模型元数据缓存（见 metadata.go）
	anonCat anonCatalog // 匿名免费目录缓存（见 anonymous.go）

	// quotaMu/quotaCache 是 Go 套餐额度查询的结果缓存（见 quota.go）。挂在实例上：
	// 改配置会重建 Provider，旧密钥的条目随旧实例一起消失，不会串到新密钥上。
	// quotaRefreshMu 串行化整个刷新（并发合并，见 GoQuota 的注释）。
	quotaMu        sync.Mutex
	quotaCache     map[string]quotaCacheEntry
	quotaRefreshMu sync.Mutex
}

// New 构造 provider。只保留「配了密钥」的 tier 进轮询——没有密钥的 tier 留在
// 轮询里只会让每次请求都白跑一趟 401；但它们仍记在 all 里，因为匿名通道开启时
// 纯匿名供应商要用它们的公开目录列模型（目录本身不需要密钥）。
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
	metaURL := strings.TrimSpace(o.MetaURL)
	if metaURL == "" {
		metaURL = DefaultMetaURL
	}
	p := &Provider{
		name:   o.Name,
		models: clean(o.Models),
		meta:   metaCache{url: metaURL},
		http: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 20 * time.Second}).DialContext,
				ResponseHeaderTimeout: 300 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		},
	}
	for _, name := range order {
		pl := byName[name]
		p.all = append(p.all, pl)
		if len(pl.snapshot()) > 0 {
			p.pools = append(p.pools, pl)
		}
	}
	if o.Anonymous {
		base := strings.TrimSpace(o.AnonBase)
		if base == "" {
			base = DefaultAnonBase
		}
		p.anon = strings.TrimRight(base, "/")
	}
	return p
}

// Name returns the provider name.
func (p *Provider) Name() string { return p.name }

// Type returns the provider type.
func (p *Provider) Type() string { return "opencode" }

// Ready 报告这个供应商有没有任何一条能服务的通道：任一 tier 配了密钥，或匿名
// 免费通道已启用（零密钥也真能服务免费模型，面板不按空壳隐藏）。
func (p *Provider) Ready() bool { return len(p.pools) > 0 || p.anon != "" }

// Tiers 返回配了密钥、参与轮询的 tier 名（按 prefer 顺序），供诊断使用。
// 匿名通道不是密钥 tier，不出现在这里。
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

// decorate 给 zen/go 请求带上鉴权与会话身份。
//
// 会话头按上游文档发送，用于路由与提示词缓存优化；User-Agent 如实申报本网关
// （见包注释）。匿名通道不走这里——它有自己的一套镜像头（streamAnonymous）。
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

// ListModels 拉取各来源的 /v1/models 并取并集。
//
// 来源与顺序：
//  1. 配了密钥的 tier 按轮询顺序拉（带密钥优先，见 listTier）；
//  2. 没配密钥的 tier——只在匿名通道开启时——用公开目录补上（纯匿名供应商的
//     唯一目录来源就是它：免费模型就分布在这两份目录里）；
//  3. 匿名免费目录（/inference/v1/models，Bearer public）：权威的「匿名通道真能
//     服务」清单，同时给纯匿名供应商兜底（见 anonymous.go）。
//
// 目录都是公开的（实测无需有效密钥即返回 200），两个 tier 的模型集并不相同——
// zen 86 个、go 37 个——所以逐个来源拉、按顺序去重合并；全部失败才回落到配置里
// 写死的模型清单。某个来源拉不到不算错误：别的来源还能提供模型。
//
// 并集之后用上游元数据目录补齐 /v1/models 不带的字段（上下文长度、输出上限、价格、
// 免费标记，见 metadata.go）。这一步是纯增益：抓不到就保持 bare，不影响目录本身。
func (p *Provider) ListModels(ctx context.Context) ([]openai.Model, error) {
	seen := map[string]bool{}
	out := make([]openai.Model, 0, 64)
	var lastErr error
	add := func(models []openai.Model) {
		for _, m := range models {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	for _, pl := range p.all {
		if len(pl.snapshot()) == 0 {
			if p.anon == "" {
				continue // 没密钥、又没匿名通道：这个 tier 拿它没辙
			}
			models, err := p.listPublic(ctx, pl.name, pl.base)
			if err != nil {
				lastErr = err
				continue
			}
			p.enrich(pl.name, models)
			add(models)
			continue
		}
		models, err := p.listTier(ctx, pl)
		if err != nil {
			lastErr = err
			continue
		}
		p.enrich(pl.name, models)
		add(models)
	}
	if ids := p.anonCatalogIDs(); len(ids) > 0 {
		free := make([]openai.Model, 0, len(ids))
		for _, id := range ids {
			free = append(free, openai.Model{ID: id, Object: "model", OwnedBy: p.name, IsFree: true})
		}
		p.enrichMerged(free)
		add(free)
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
		p.enrichMerged(out)
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("opencode: 没有配密钥池、匿名通道也未开启，无法获取模型目录")
}

// listTier 拉取单个配了密钥的 tier 的模型目录。
//
// 带密钥失败（401/403）时摘掉 Authorization 重试一次：目录本身是公开的——实测不带
// 鉴权返回完整目录，而带一把**无效**密钥会被直接拒（401 Invalid credential）。不重试
// 的后果很误导：用户密钥填错时模型下拉是空的，他会去查网络，而不是去查那把密钥。
//
// 带有效密钥时仍优先带密钥请求：付费账号可能看到比公开目录更多的模型。
func (p *Provider) listTier(ctx context.Context, pl *pool) ([]openai.Model, error) {
	key, _ := pl.pick(nil)
	models, status, body, err := p.fetchModels(ctx, pl.name, pl.base, key)
	if err != nil {
		return nil, err
	}
	if status < 400 {
		return models, nil
	}
	if (status == http.StatusUnauthorized || status == http.StatusForbidden) && key != "" {
		if m2, st2, _, err2 := p.fetchModels(ctx, pl.name, pl.base, ""); err2 == nil && st2 < 400 {
			return m2, nil
		}
	}
	return nil, fmt.Errorf("opencode/%s 拉取模型目录 HTTP %d: %s",
		pl.name, status, clipBody(body))
}

// listPublic 拉没配密钥的 tier 的公开目录（匿名通道开启时用）。
func (p *Provider) listPublic(ctx context.Context, tier, base string) ([]openai.Model, error) {
	models, status, body, err := p.fetchModels(ctx, tier, base, "")
	if err != nil {
		return nil, err
	}
	if status >= 400 {
		return nil, fmt.Errorf("opencode/%s 拉取公开模型目录 HTTP %d: %s", tier, status, clipBody(body))
	}
	return models, nil
}

// fetchModels 发一次模型目录请求，返回模型、状态码与响应体（出错时 body 原样带回，
// 供上层拼报错）。key 为空时不带 Authorization（公开目录）。
func (p *Provider) fetchModels(ctx context.Context, tier, base, key string) ([]openai.Model, int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("opencode/%s 拉取模型目录失败: %w", tier, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, string(raw), nil
	}
	var ml openai.ModelList
	if err := json.Unmarshal(raw, &ml); err != nil {
		return nil, resp.StatusCode, string(raw), fmt.Errorf("opencode/%s 模型目录解析失败: %w", tier, err)
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
	return out, resp.StatusCode, string(raw), nil
}

// clipBody 把响应体裁到报错可读的长度：上游出错时可能回一整页 HTML。
func clipBody(s string) string {
	s = strings.TrimSpace(s)
	const max = 300
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// StreamChat 发一次流式 chat completion。
//
// 通道顺序：免费模型匿名优先（零密钥，见 anonymous.go）；失败或不在免费范围时
// 回落密钥轮询——外层按 prefer 顺序走 tier，内层走该 tier 的密钥池。失败按性质
// 分三类（见 classifyFailure）：密钥类在池内轮换；模型类当场失败（换密钥、换
// tier 都是白打，还会把好密钥冷却掉）；其余 4xx（例如该模型不在这个 tier 上）
// 放弃本 tier、回落到下一个 tier——这样「模型只在 go 上」和「模型只在 zen 上」
// 都能自动落到对的一侧，不需要在配置里逐模型标注。
//
// 匿名通道只试一次、不重试也不冷却：它没有凭证可轮换，被门禁拒了就该回落密钥池；
// 密钥池也没有就报错，并把两条路都写进提示里（见 noChannelError）。
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
	if p.anon != "" && p.isAnonEligible(in.Model) {
		st, err := p.streamAnonymous(ctx, raw, in, session)
		if err == nil {
			return st, nil
		}
		if ctx.Err() != nil {
			// 客户端断开之类的中止：继续打密钥池没有意义。
			return nil, err
		}
		lastErr = err
	}
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
		lastErr = p.noChannelError(in.Model)
	}
	return nil, lastErr
}

// noChannelError 是「一条通道都没跑成」时的兜底报错，按配置情况给可操作的话。
// 能走到这里的组合只有两种：没开匿名且没配密钥（一个通道都没有），或开了匿名、
// 模型不在免费范围、密钥又全在冷却中（本次没有可用通道）。其它失败路径都有
// 各自的 lastErr（匿名/密钥的原始报错），不会落进这个兜底。
func (p *Provider) noChannelError(model string) error {
	if p.anon == "" {
		return fmt.Errorf("opencode: 没有可用的上游密钥（请在面板的「上游供应商 → %s」里填写 zen / go 密钥池，或开启匿名免费通道）", p.name)
	}
	return fmt.Errorf("opencode: 模型 %q 不属于匿名免费范围，可用的 zen / go 密钥又都在冷却中；请稍后重试，或改用免费模型（零密钥直连匿名通道）", model)
}
