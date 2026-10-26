// Package gateway routes OpenAI-compatible requests to configured providers,
// manages the account pool, and runs check-in / keep-alive tasks.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/imagegen"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/newapi"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/trae"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/toolcall"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// Gateway is the routing core.
type Gateway struct {
	cfg *config.Config
	st  *state.State
	log *logx.Logger

	providers map[string]provider.Provider
	order     []string
	accounts  map[string][]*provider.Account

	accountsMu sync.RWMutex

	modelMu    sync.Mutex
	modelCache map[string]modelEntry
	// modelRefreshing 标记「已有缓存但正在后台刷新」的供应商，避免过期缓存被
	// 每个并发请求各自触发一次上游拉取（见 models 的 stale-while-revalidate）。
	modelRefreshing map[string]bool
	// modelRefreshWG 让测试能等后台刷新落定；生产路径不等待。
	modelRefreshWG sync.WaitGroup

	// modelCachePath 是模型目录的落盘路径（空 = 不持久化）。见 WithModelCachePath。
	// 有了它，容器重启 / 改配置重建网关后，仍能立刻给出带计费价的目录。
	modelCachePath string
	// modelSaveMu 串行化落盘（tmp 文件同名，并发写会互相踩）。
	modelSaveMu sync.Mutex

	// modelCacheBackend / modelCacheKey 可选 KV 后端（PostgreSQL）：非空时模型目录
	// 落库（库为权威），文件退化为首次导入源 + 断库兜底。见 WithModelCacheBackend。
	modelCacheBackend kv.Backend
	modelCacheKey     string
	// modelMapBackend / modelMapKey 全局模型映射表的 KV 后端。见 WithModelMappingBackend。
	modelMapBackend kv.Backend
	modelMapKey     string

	// modelMap is the global alias → target model table (see modelmap.go). Nil =
	// inert (no mapping). Populated from the data dir by New.
	modelMap *ModelMappingStore

	// keyHealth 上游密钥池健康状态（自动禁用 / 连击记账，见 keyhealth.go）。
	// Nil = 不持久化（纯内存）。Populated from the data dir by New.
	keyHealth *keyHealthStore
	// keyHealthBackend / keyHealthKey 健康状态的 KV 后端。见 WithKeyHealthBackend。
	keyHealthBackend kv.Backend
	keyHealthKey     string

	convMu sync.Mutex
	convs  map[string]*conv

	// refreshLocks 按账号串行化 token 轮换：轮换型 refresh_token 是一次性的，
	// 定时刷新与按需刷新并发时会互相作废，并把旧 session 留在内存里。见 refreshToken。
	refreshLocks sync.Map // key: provider+"\x00"+label -> *sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]raccoonPending

	// traePend remembers in-flight TRAE browser logins (keyed by state). TRAE
	// needs the machine/device fingerprint chosen at login time to be reused when
	// the account is finally created, so it cannot share raccoonPending.
	traePendMu sync.Mutex
	traePend   map[string]traePending

	// newapiPend remembers in-flight New API browser-login captures (keyed by
	// state). The panel hands the user a one-time token + snippet; the snippet
	// posts the captured auth bundle back, and the pending entry supplies the
	// label / API key the user typed before starting.
	newapiPendMu sync.Mutex
	newapiPend   map[string]newapiPending

	// proxyFor resolves a per-provider outbound proxy (nil = direct). Installed
	// via WithProxyResolver; consulted once per provider at construction.
	proxyFor func(providerName string) *url.URL

	// poolFor resolves a per-provider outbound proxy pool (nil = none).
	// Installed via WithPoolResolver; takes precedence over proxyFor when a
	// provider routes to a pool (rotation + failover).
	poolFor func(providerName string) outbound.Selector

	// cool 账号级限流冷却台账（进程内）。见 cooldown.go。
	cool *cooldownLedger

	// sess 会话健康台账（进程内，目前用于 runable 会话过期预警）。见 session.go。
	sess *sessionHealth

	// credits 余额快照缓存（进程内，不落盘），供「快过期积分优先」选号与面板角标。
	// 见 credits.go。
	credits *creditCache
}

// raccoonPending remembers an in-flight browser authorization.
type raccoonPending struct {
	provider string
	label    string
	at       time.Time
}

// traePending remembers an in-flight TRAE browser login.
type traePending struct {
	provider  string
	label     string
	machineID string
	deviceID  string
	at        time.Time
}

// newapiPending remembers an in-flight New API browser-login capture. The panel
// pre-fills the chat API key (and, optionally, the label) before starting; the
// snippet only supplies the auth bundle, so we keep the rest here until the
// capture arrives. autoLabel means the user left the name blank: the account is
// then named after the site identity once the capture lands.
type newapiPending struct {
	provider  string
	label     string
	autoLabel bool
	apiKey    string
	at        time.Time
}

type modelEntry struct {
	models []openai.Model
	at     time.Time
}

// modelCacheTTL 是每个 provider 模型目录的缓存时长。到期后重新拉取，拉取失败则
// 继续沿用旧结果（见 models）。
const modelCacheTTL = 10 * time.Minute

type conv struct {
	chatID     string
	systemSent bool
	at         time.Time
}

type chatResult struct {
	text      string
	reasoning string
	finish    string
	toolCalls []openai.ToolCall
	errText   string
	usage     *provider.Usage
	// sawData 表示本次流至少产生过一个「有效帧」（正文 / 思考 / 工具调用）。
	// 全 false = 上游 200 但空流，按失败观测（与非流式空响应同语义）。
	sawData bool
	// sawFinish 表示上游真的发过 finish_reason 帧。注意 res.finish 默认就是
	// "stop"（上游没发 finish 时客户端也要看到一个结束原因），所以判断「上游是否
	// 正常收尾」只能看这个标记，不能看 res.finish 是否为空。
	sawFinish bool
	// sample 是上游「产生不了任何有效帧」的原始片段（非 SSE 的 JSON 体 / 无法识别
	// 的帧）。只在 0 帧失败时用于日志与错误信息，让上游的真实说法不再被静默吞掉。
	sample []byte
}

// Option configures the Gateway at construction time.
type Option func(*Gateway)

// WithProxyResolver installs a per-provider outbound proxy resolver. It is
// consulted once per provider while building the provider map; returning nil
// means direct connection. Providers that support proxying implement SetProxy.
func WithProxyResolver(fn func(providerName string) *url.URL) Option {
	return func(g *Gateway) { g.proxyFor = fn }
}

// WithPoolResolver installs a per-provider outbound proxy *pool* resolver. It is
// consulted once per provider while building the provider map; returning nil
// means "no pool" (fall back to WithProxyResolver / direct). Providers that
// support proxying implement SetProxySelector.
func WithPoolResolver(fn func(providerName string) outbound.Selector) Option {
	return func(g *Gateway) { g.poolFor = fn }
}

// WithModelCachePath 让网关把「最近一次成功的模型目录」持久化到 path：重启 / 改配置
// 重建网关后，仍能立刻提供带计费价（credits / pricing / billing）的目录；上游临时
// 不可用时也用它兜底。空路径 = 不持久化。
func WithModelCachePath(path string) Option {
	return func(g *Gateway) { g.modelCachePath = path }
}

// WithModelCacheBackend 让模型目录落 KV 后端（PostgreSQL）。非空时库为权威：优先读库，
// 写只写库（文件路径退化为首次导入源 + 断库兜底）。见 saveModelCache。
func WithModelCacheBackend(b kv.Backend, key string) Option {
	return func(g *Gateway) {
		g.modelCacheBackend = b
		g.modelCacheKey = key
	}
}

// WithModelMappingBackend 让全局模型映射表落 KV 后端（PostgreSQL）。非空时库为权威。
func WithModelMappingBackend(b kv.Backend, key string) Option {
	return func(g *Gateway) {
		g.modelMapBackend = b
		g.modelMapKey = key
	}
}

// WithKeyHealthPath 让网关把上游密钥池的健康状态（自动禁用 / 连击记账）持久化到
// path：热重载重建 provider、容器重启后，被禁用的死 key 不会「复活」重新进轮换。
// 空路径 = 纯内存。
func WithKeyHealthPath(path string) Option {
	return func(g *Gateway) { g.keyHealth = newKeyHealthStore(path) }
}

// WithKeyHealthBackend 让密钥健康状态落 KV 后端（PostgreSQL）。非空时库为权威：
// 优先读库，写只写库（文件路径退化为首次导入源 + 断库兜底）。见 keyhealth.go。
func WithKeyHealthBackend(b kv.Backend, key string) Option {
	return func(g *Gateway) {
		g.keyHealthBackend = b
		g.keyHealthKey = key
	}
}

// 编译期断言：每个可代理的 Provider 都必须在 Provider 本身上实现 SetProxySelector
// （而不只是它的 Client）。历史上 runable/raccoon 只把该方法实现在 Client 上，导致
// 网关的类型断言静默失败、代理池从未生效（请求悄悄走了直连）。这里用编译期断言把
// 这类回归钉死：谁漏了，编译不过。
var (
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*runable.Provider)(nil)
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*raccoon.Provider)(nil)
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*openaibackend.Provider)(nil)
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*trae.Provider)(nil)
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*newapi.Provider)(nil)
	_ interface {
		SetProxySelector(outbound.Selector)
	} = (*geminiweb.Provider)(nil)
)

// New builds a Gateway from config and state.
func New(cfg *config.Config, st *state.State, log *logx.Logger, opts ...Option) (*Gateway, error) {
	g := &Gateway{
		cfg:             cfg,
		st:              st,
		log:             log,
		providers:       map[string]provider.Provider{},
		accounts:        map[string][]*provider.Account{},
		modelCache:      map[string]modelEntry{},
		modelRefreshing: map[string]bool{},
		convs:           map[string]*conv{},
		pending:         map[string]raccoonPending{},
		traePend:        map[string]traePending{},
		newapiPend:      map[string]newapiPending{},
		cool:            newCooldownLedger(),
		sess:            newSessionHealth(),
		credits:         newCreditCache(),
	}
	for _, o := range opts {
		o(g)
	}
	// Global alias → target model table (model_mapping.py). Optional: absent data
	// dir = inert store.
	if g.modelMap == nil && cfg.DataDir != "" {
		g.modelMap = NewModelMappingStore(filepath.Join(cfg.DataDir, "model-mapping.json"))
	}
	if g.modelMap != nil {
		if err := g.modelMap.SetBackend(g.modelMapBackend, g.modelMapKey); err != nil {
			g.log.Warn("模型映射启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	if g.keyHealth == nil && cfg.DataDir != "" {
		g.keyHealth = newKeyHealthStore(filepath.Join(cfg.DataDir, "key-health.json"))
	}
	if g.keyHealth != nil {
		if err := g.keyHealth.SetBackend(g.keyHealthBackend, g.keyHealthKey); err != nil {
			g.log.Warn("密钥健康状态启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	// 插件注册表：内置类型在网关启动时显式注册一次（无 init 副作用）；
	// 外部插件可在调用 New 之前先 Register 自己的描述符。
	catalog.Register()
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		// 构造走 newProvider（provider_build.go）：与面板「探测模型目录」同一条
		// 路径，避免出现「探测得到、保存后拉不到」。
		prov, err := newProvider(p, plugin.Deps{DataDir: cfg.DataDir, Logger: g.log.Warn})
		if err != nil {
			return nil, err
		}
		// Gemini Web 的进程级附属设施（图片存储 / Gem 映射 / 指纹 / 后台维护）
		// 不属于 provider 协议本身，统一由核心在构造后注入。
		if gw, ok := prov.(*geminiweb.Provider); ok {
			gw.SetImageStore(imagegen.NewStore(filepath.Join(cfg.DataDir, "generated_images")))
			gw.SetGemMappingStore(geminiweb.NewGemMappingStore(filepath.Join(cfg.DataDir, "gem-mapping.json")))
			gw.SetLogger(g.log.Warn)
			if fp := cfg.Fingerprint; fp != nil {
				rt := fingerprint.NewRuntime(fingerprint.Options{
					ConfigPath:          fp.ConfigPath,
					CookieDir:           fp.CookieDir,
					JitterEnabled:       fp.JitterEnabled,
					VersionSyncEnabled:  fp.VersionSyncEnabled,
					VersionSyncInterval: time.Duration(fp.VersionSyncIntervalHours) * time.Hour,
					TLSEnabled:          fp.TLSImpersonationEnabled(),
					Logf:                g.log.Warn,
				})
				gw.SetFingerprint(rt)
				gw.StartFingerprintSync(context.Background())
			}
			if m := cfg.Maintenance; m != nil && m.KeepaliveEnabled {
				provName := p.Name
				gw.StartKeepAlive(context.Background(),
					time.Duration(m.KeepaliveIntervalMinutes)*time.Minute,
					func() []*provider.Account { return g.Accounts(provName) })
			}
			if m := cfg.Maintenance; m != nil && m.ChatCleanupEnabled {
				provName := p.Name
				gw.StartChatCleanup(context.Background(),
					time.Duration(m.ChatCleanupIntervalHours*float64(time.Hour)),
					m.ChatCleanupKeepHours,
					m.ChatCleanupSkipPinnedOrDefault(),
					func() []*provider.Account { return g.Accounts(provName) })
			}
		}
		// 上游密钥池（多密钥轮询）：provider.api_keys 非空时启用（只对支持
		// SetAPIKeys 的后端生效，当前是 openai 类上游）。
		if len(p.APIKeys) > 0 {
			if ks, ok := prov.(interface{ SetAPIKeys([]string) }); ok {
				ks.SetAPIKeys(p.APIKeys)
			}
		}
		// 上游密钥健康状态（自动禁用 / 连击记账）：落库持久化 + 日志留痕。
		// SetHealthStore 是 openai 类上游专属，用它做类型断言；logger 挂在同一
		// 分支内（避免误配到其它也有 SetLogger 的 provider，如 geminiweb 用 Warn）。
		if g.keyHealth != nil {
			if hs, ok := prov.(interface {
				SetHealthStore(openaibackend.HealthStore)
			}); ok {
				hs.SetHealthStore(g.keyHealth)
				if ls, ok := prov.(interface {
					SetLogger(func(string, ...any))
				}); ok {
					ls.SetLogger(g.log.Info)
				}
			}
		}
		// Per-provider outbound proxy (panel config outbound.routes). A proxy
		// pool takes precedence over a single proxy when both resolve.
		if g.poolFor != nil {
			if sel := g.poolFor(p.Name); sel != nil {
				if ps, ok := prov.(interface {
					SetProxySelector(outbound.Selector)
				}); ok {
					ps.SetProxySelector(sel)
				}
			}
		}
		if g.proxyFor != nil {
			if u := g.proxyFor(p.Name); u != nil {
				if ps, ok := prov.(interface{ SetProxy(*url.URL) }); ok {
					ps.SetProxy(u)
				}
			}
		}
		g.providers[p.Name] = prov
		g.order = append(g.order, p.Name)

		var accts []*provider.Account
		seen := map[string]bool{}
		for _, a := range p.Accounts {
			label := a.Label
			if label == "" {
				label = fmt.Sprintf("账号 %d", len(accts)+1)
			}
			seen[label] = true
			accts = append(accts, &provider.Account{
				Label:         label,
				Cookie:        st.Cookie(p.Name, label, a.Cookie),
				Email:         a.Email,
				Password:      a.Password,
				RefreshToken:  st.RefreshToken(p.Name, label, a.RefreshToken),
				UID:           a.UID,
				MachineID:     a.MachineID,
				DeviceID:      a.DeviceID,
				ApiHost:       a.ApiHost,
				APIKey:        a.APIKey,
				ExpiresAt:     a.ExpiresAt,
				SessionCookie: a.SessionCookie,
			})
		}
		// Merge accounts added at runtime (e.g. via the Raccoon web login).
		for _, d := range st.DynAccounts(p.Name) {
			if d.Label == "" || seen[d.Label] {
				continue
			}
			seen[d.Label] = true
			accts = append(accts, &provider.Account{
				Label:         d.Label,
				Cookie:        st.Cookie(p.Name, d.Label, d.AccessToken),
				RefreshToken:  st.RefreshToken(p.Name, d.Label, d.RefreshToken),
				UID:           d.UserID,
				MachineID:     d.MachineID,
				DeviceID:      d.DeviceID,
				ApiHost:       d.ApiHost,
				ExpiresAt:     d.ExpiresAt,
				APIKey:        d.APIKey,
				SessionCookie: d.SessionCookie,
			})
		}
		g.accounts[p.Name] = accts
	}
	// 载入上次落盘的模型目录（过期但可用）：首次访问会尝试刷新，刷新失败则用它
	// 兜底，避免重启后短暂「无价」。
	g.loadModelCache()
	return g, nil
}

// Config exposes the loaded config.
func (g *Gateway) Config() *config.Config { return g.cfg }

// Logger exposes the logger.
func (g *Gateway) Logger() *logx.Logger { return g.log }

// ProviderNames returns configured provider names in order.
// KeyHealthSnapshot 上游密钥健康状态快照（provider → KeyID → 状态），供面板
// 「密钥健康」展示与后台自动清理判定用。
func (g *Gateway) KeyHealthSnapshot() map[string]map[string]openaibackend.Health {
	if g == nil || g.keyHealth == nil {
		return nil
	}
	return g.keyHealth.Snapshot()
}

func (g *Gateway) ProviderNames() []string { return g.order }

// ProviderType returns the type of a provider.
func (g *Gateway) ProviderType(name string) string {
	if p := g.providers[name]; p != nil {
		return p.Type()
	}
	return ""
}

// traitsOf returns the protocol traits a provider type declares in the plugin
// registry. It is the single source of truth for the protocol-level questions
// the gateway used to answer by switching on the type string (tool emulation,
// attachments, reasoning_effort, prompt limit, wire format).
//
// An unregistered type yields the zero Traits — the safe default: no emulation,
// no attachments, no prompt limit, empty wire format — so an unknown type keeps
// behaving exactly as it did before it had a descriptor.
func traitsOf(typ string) plugin.Traits {
	if desc, err := plugin.Get(typ); err == nil {
		return desc.Traits
	}
	return plugin.Traits{}
}

// accountModeOf returns the account mode a type declares ("", none, pool or
// dynamic). An unregistered type yields "" — deliberately not AccountModeNone,
// so a provider without a descriptor still requires a configured account.
func accountModeOf(typ string) plugin.AccountMode {
	if desc, err := plugin.Get(typ); err == nil {
		return desc.AccountMode
	}
	return ""
}

// Accounts returns the account pool for a provider.
func (g *Gateway) Accounts(name string) []*provider.Account {
	g.accountsMu.RLock()
	defer g.accountsMu.RUnlock()
	return g.accounts[name]
}

// models returns a provider's models, cached for 10 minutes. 刷新失败时退回「最近一次
// 成功结果」（哪怕已过期）——目录里带着计费 / 积分价，不能因为账号临时不可用就丢掉。
//
// 有缓存但已过期时走 stale-while-revalidate：立刻返回旧目录，上游刷新放到后台。
// 否则重启后第一次打开模型页（进程内缓存 at 为零值 = 全部过期）要等各家上游的
// 延迟——线上实测 5 家串行 38 秒、并发后仍要等最慢的一家 60 秒，面板一直转圈。
// 没有缓存时才同步拉取：那种情况没有别的东西可展示。
func (g *Gateway) models(ctx context.Context, name string) ([]openai.Model, error) {
	g.modelMu.Lock()
	e, cached := g.modelCache[name]
	if cached && time.Since(e.at) < modelCacheTTL {
		g.modelMu.Unlock()
		return e.models, nil
	}
	if cached && len(e.models) > 0 {
		// 已有目录就永远不等上游：正在刷新时也只是读旧目录。忘了这个分支会让
		// 「后台刷新进行中」的请求掉进下面的同步拉取，等于白做了 stale-while-revalidate
		// （线上表现为第二次打开仍要 3 秒）。
		start := !g.modelRefreshing[name]
		if start {
			// 手工构造的 Gateway（测试）没有这个 map，补一个而不是 panic。
			if g.modelRefreshing == nil {
				g.modelRefreshing = map[string]bool{}
			}
			g.modelRefreshing[name] = true
		}
		g.modelMu.Unlock()
		if start {
			g.modelRefreshWG.Add(1)
			go g.refreshModelsAsync(name)
		}
		return e.models, nil
	}
	g.modelMu.Unlock()

	models, err := g.fetchModels(ctx, name)
	if err != nil {
		// 拉取失败：优先沿用最近一次成功结果（保留计费价）；没有缓存时才退回上游
		// 给的降级目录（如内置模型表，无价格）。降级结果绝不写进缓存，避免把
		// 带价格的目录覆盖掉。
		if e, ok := g.cachedModels(name); ok {
			g.log.Warn("刷新 %s 模型失败，沿用缓存: %v", name, err)
			return e, nil
		}
		if len(models) > 0 {
			g.log.Warn("刷新 %s 模型失败，使用降级目录: %v", name, err)
			return models, nil
		}
		return nil, err
	}
	// 空目录同样不能当成功写进缓存：fetchModels 只在真正拿到列表时才落库，但
	// provider 实现（尤其第三方插件）仍可能返回「无错但为空」。此时保留旧目录
	// 比清空更接近事实，也避免把该供应商从落库快照里抹掉。
	if len(models) == 0 {
		if e, ok := g.cachedModels(name); ok {
			return e, nil
		}
		return nil, fmt.Errorf("%s 的上游模型目录为空", name)
	}
	return models, nil
}

// refreshModelsAsync 在后台刷新某供应商的模型目录。失败只记日志：调用方已经拿到
// 上一次的目录，后台任务不该把它的结果改坏。
func (g *Gateway) refreshModelsAsync(name string) {
	defer g.modelRefreshWG.Done()
	defer func() {
		g.modelMu.Lock()
		delete(g.modelRefreshing, name)
		g.modelMu.Unlock()
	}()
	// 后台刷新没有请求级的 deadline，给一个自己的上限，避免慢上游长期占着标记。
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	before, _ := g.cachedModels(name)
	models, err := g.fetchModels(ctx, name)
	if err != nil {
		g.log.Warn("后台刷新 %s 模型失败，继续沿用缓存: %v", name, err)
		return
	}
	// 成功刷新只在目录真的变了时才说一句：静默成功会让人分不清「刷新没跑」和
	// 「刷新跑过但上游没变」。
	if n, o := len(models), len(before); n != o {
		g.log.Info("后台刷新 %s 模型目录：%d → %d 个", name, o, n)
	}
}

// fetchModels 绕过 TTL 直接向上游拉取某 provider 的模型目录，成功时刷新缓存并
// 落盘。models()（带 TTL 与缓存兜底）与面板的「刷新上游目录」都经它。
func (g *Gateway) fetchModels(ctx context.Context, name string) ([]openai.Model, error) {
	prov := g.providers[name]
	if prov == nil {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	var (
		models []openai.Model
		err    error
	)
	if lister, ok := prov.(provider.AccountModelLister); ok {
		// 需要鉴权的目录（raccoon）：带上一个可用账号才能拿到计费 / 积分价。
		models, err = lister.ListModelsWithAccount(ctx, g.pickAccount(name))
	} else {
		models, err = prov.ListModels(ctx)
	}
	if err != nil {
		return models, err
	}
	// 空目录不落缓存、不落库：见 models 的注释。这里提前拦住，避免「短暂失败 → 空
	// 目录」把上一次的完好目录从进程缓存与落库快照里一起抹掉。
	if len(models) == 0 {
		return nil, fmt.Errorf("%s 的上游模型目录为空", name)
	}
	g.modelMu.Lock()
	g.modelCache[name] = modelEntry{models: models, at: time.Now()}
	g.modelMu.Unlock()
	g.saveModelCache()
	return models, nil
}

// modelCacheFile 是模型目录落盘的格式：provider 名 -> 最近一次成功结果。
type modelCacheFile map[string][]openai.Model

// loadModelCache 从磁盘 / KV 后端载入上次成功的模型目录。载入的条目「过期但可用」：
// 首次访问会尝试刷新（at 为零值 → 不满足 TTL），刷新失败时由 cachedModels 兜底。
func (g *Gateway) loadModelCache() {
	hasBackend := g.modelCacheBackend != nil && g.modelCacheKey != ""
	if g.modelCachePath == "" && !hasBackend {
		return
	}
	var raw []byte
	if hasBackend {
		data, found, err := g.modelCacheBackend.Load(g.modelCacheKey)
		switch {
		case err != nil:
			g.log.Warn("读取模型目录缓存后端失败: %v", err)
		case found:
			raw = data
		case g.modelCachePath != "":
			// 首次启用：回落到文件（稍后由 saveModelCache 导入库）。
			if b, rerr := os.ReadFile(g.modelCachePath); rerr == nil {
				raw = b
			}
		}
	} else {
		b, err := os.ReadFile(g.modelCachePath)
		if err != nil {
			return // 首次启动没有缓存文件，属正常
		}
		raw = b
	}
	if len(raw) == 0 {
		return
	}
	var saved modelCacheFile
	if err := json.Unmarshal(raw, &saved); err != nil {
		g.log.Warn("模型目录缓存解析失败（已忽略）：%v", err)
		return
	}
	n := 0
	g.modelMu.Lock()
	for name, models := range saved {
		if len(models) == 0 {
			continue
		}
		g.modelCache[name] = modelEntry{models: models}
		n++
	}
	g.modelMu.Unlock()
	if n > 0 {
		g.log.Info("已载入 %d 个 provider 的模型目录缓存：%s", n, g.modelCacheLocation())
	}
}

// modelCacheLocation 返回模型目录缓存的位置描述（后端优先，否则文件路径）。
func (g *Gateway) modelCacheLocation() string {
	if g.modelCacheBackend != nil && g.modelCacheKey != "" {
		return "kv:" + g.modelCacheKey
	}
	return g.modelCachePath
}

// saveModelCache 把当前模型目录落盘（尽力而为，失败只记日志）。配置了 KV 后端时
// 写库、不再写文件。原子写：先写临时文件再改名，避免进程中途挂掉留下半个文件。
func (g *Gateway) saveModelCache() {
	hasBackend := g.modelCacheBackend != nil && g.modelCacheKey != ""
	if g.modelCachePath == "" && !hasBackend {
		return
	}
	g.modelSaveMu.Lock()
	defer g.modelSaveMu.Unlock()
	g.modelMu.Lock()
	snapshot := make(modelCacheFile, len(g.modelCache))
	for name, e := range g.modelCache {
		if len(e.models) > 0 {
			snapshot[name] = e.models
		}
	}
	g.modelMu.Unlock()
	if len(snapshot) == 0 {
		return
	}
	raw, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return
	}
	// 库为权威：配置了后端就只写库（文件冻结为最后一次已知良好状态，断库时兜底读）。
	if hasBackend {
		if err := g.modelCacheBackend.Save(g.modelCacheKey, raw); err != nil {
			g.log.Warn("写入模型目录缓存后端失败：%v", err)
		}
		return
	}
	if dir := filepath.Dir(g.modelCachePath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			g.log.Warn("创建模型目录缓存目录失败：%v", err)
			return
		}
	}
	tmp := g.modelCachePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		g.log.Warn("写入模型目录缓存失败：%v", err)
		return
	}
	if err := os.Rename(tmp, g.modelCachePath); err != nil {
		g.log.Warn("替换模型目录缓存失败：%v", err)
	}
}

// cachedModels 返回某 provider 最近一次成功缓存的模型（不判过期）。
func (g *Gateway) cachedModels(name string) ([]openai.Model, bool) {
	g.modelMu.Lock()
	defer g.modelMu.Unlock()
	e, ok := g.modelCache[name]
	if !ok || len(e.models) == 0 {
		return nil, false
	}
	return e.models, true
}

// pickAccount 返回该 provider 的一个可用账号（有凭证即可），否则 nil。
//
// 优先返回有 Cookie 的账号：raccoon / trae 的模型目录就是按 Cookie 拉的，历史上
// 也一直是这个选择，先挑 Cookie 才不会有回归。没有 Cookie 账号时再退回其它凭据——
// Cookie 型站点（AnyRouter / AgentRouter）的账号恰恰没有 Cookie 字段，只认 Cookie
// 会让这类 provider 永远拿不到真实目录，只能静默退回内置清单。
func (g *Gateway) pickAccount(name string) *provider.Account {
	g.accountsMu.RLock()
	defer g.accountsMu.RUnlock()
	var anyCred *provider.Account
	for _, a := range g.accounts[name] {
		if a == nil {
			continue
		}
		if a.Cookie != "" {
			return a
		}
		if anyCred == nil && (a.APIKey != "" || a.SessionCookie != "" || a.RefreshToken != "") {
			anyCred = a
		}
	}
	return anyCred
}

// ListModels aggregates every provider's models, prefixed with "provider/".
func (g *Gateway) ListModels(ctx context.Context) []openai.Model {
	return g.ListModelsFor(ctx, nil)
}

// ListModelsFor 同 ListModels，但只聚合 allowed 中列出的 provider（空 = 全部）。
// 供按供应商收窄的入站密钥使用：客户端只该看到自己有权调用的模型。
func (g *Gateway) ListModelsFor(ctx context.Context, allowed []string) []openai.Model {
	var allow map[string]bool
	if len(allowed) > 0 {
		allow = make(map[string]bool, len(allowed))
		for _, name := range allowed {
			allow[name] = true
		}
	}
	// 并发拉取：各家上游的 /models 相互独立，串行会让「重启后第一次打开模型页」
	// 的等待时间等于各家延迟之和（实测 5 家 38 秒）。这里的顺序只影响结果拼装，
	// 所以按 g.order 收集、按 g.order 输出，保持目录顺序稳定。
	type fetched struct {
		name   string
		models []openai.Model
		err    error
	}
	names := make([]string, 0, len(g.order))
	for _, name := range g.order {
		if allow != nil && !allow[name] {
			continue
		}
		names = append(names, name)
	}
	results := make([]fetched, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			models, err := g.models(ctx, name)
			results[i] = fetched{name: name, models: models, err: err}
		}(i, name)
	}
	wg.Wait()

	out := []openai.Model{}
	for _, r := range results {
		if r.err != nil {
			g.log.Warn("获取 %s 模型失败: %v", r.name, r.err)
			continue
		}
		for _, m := range r.models {
			m.ID = r.name + "/" + m.ID
			// owned_by 是本网关里的「供应商身份」：客户端（面板筛选、scope 计数、
			// 解析预览）都拿它当供应商名用。上游常在这里填自己的内部渠道名
			// （aotera 就返回 "sub2api"），原样透出会在目录里凭空多出一个不存在的
			// 供应商，同时让 aotera 自己的模型数变成 0、被密钥 scope 统计漏掉。
			m.OwnedBy = r.name
			out = append(out, m)
		}
	}
	// Expose configured model aliases (model_mapping) so a client can discover
	// them. An alias is listed only when its target's provider is in scope for
	// this key — otherwise the caller would see a name it cannot actually use.
	seen := make(map[string]bool, len(out))
	for _, m := range out {
		seen[m.ID] = true
	}
	for _, alias := range g.ModelMappings().Aliases() {
		if seen[alias] {
			continue
		}
		// 停用的规则不曝光：留着会让客户端看见一个名字、调下去却走到别处
		// （停用后按 routing.prefer 解析），比不列出来更难排查。
		if g.ModelMappings().IsDisabled(alias) {
			continue
		}
		chain := g.ModelMappings().Chain(alias)
		if len(chain) == 0 {
			continue
		}
		// 别名按「链上第一个有权访问的目标」归属；收窄密钥看到的目标链也只
		// 保留它有权用的那些，避免目录里列出一个调下去必 403 的供应商。
		owned := ""
		visible := make([]string, 0, len(chain))
		for _, target := range chain {
			pn, _, ok := g.routeResolvedScoped(target, allow)
			if !ok || pn == "" {
				continue
			}
			if owned == "" {
				owned = pn
			}
			visible = append(visible, target)
		}
		if owned == "" {
			continue
		}
		seen[alias] = true
		out = append(out, openai.Model{
			ID:      alias,
			Object:  "model",
			OwnedBy: owned,
			AliasOf: visible,
		})
	}
	// 全局模型白名单（MODEL_WHITELIST）：非空时只暴露其中列出的对外模型 ID。
	// 作用点选在这里，让 /v1/models、/v1beta/models 与面板模型目录三处口径一致。
	if len(g.cfg.ModelWhitelist) > 0 {
		kept := out[:0]
		for _, m := range out {
			if g.cfg.ModelWhitelisted(m.ID) {
				kept = append(kept, m)
			}
		}
		out = kept
	}
	return out
}

// ProviderOf 返回该模型会被路由到的 provider 名（与 route 同口径：带 "provider/"
// 前缀且前缀是已知 provider 时用前缀，否则回落默认 provider）。供入站密钥的
// 授权校验使用——校验必须与实际路由同源，否则会出现「放行了却打不到」或
// 「拦住了却能打」。
func (g *Gateway) ProviderOf(model string) string {
	name, _ := g.route(model)
	return name
}

// ProviderFor 返回 model 在 allowed 收窄范围内的实际落点；没有可用目标时返回 ""。
// allowed 为空 = 全量，与 route/ProviderOf 完全同源；收窄时按同一条别名链挑选
// 第一个「供应商在范围内」的目标——这保证授权校验与实际路由不会各说各话。
func (g *Gateway) ProviderFor(model string, allowed []string) string {
	name, _, ok := g.routeScoped(model, allowed)
	if !ok {
		return ""
	}
	return name
}

// route splits a requested model into (provider, upstream model). A configured
// global alias (modelmap.go) is resolved first, so a client can send "gpt-4o"
// and have it route to the mapped upstream model.
func (g *Gateway) route(model string) (string, string) {
	model = g.requestedModel(model)
	if g.modelMap != nil {
		model = g.modelMap.Resolve(model)
	}
	return g.routeResolved(model)
}

// requestedModel 把「没写 model」展开成 default_model。
//
// 不展开的话，空 model 会绕过整张别名表：显式发 default_model 的名字会走别名链，
// 省略 model 却直接落到 default_provider，同一个默认模型出现两种路由结果。
// default_model 为空时保持空串，让 routeResolved 沿用「默认供应商 + 空模型」。
func (g *Gateway) requestedModel(model string) string {
	if model != "" {
		return model
	}
	return g.cfg.DefaultModel
}

// routeResolved splits an already-resolved model into (provider, upstream model)
// WITHOUT consulting the alias table. Chain targets (modelChainFallbacks) go
// through here on purpose: a fallback target that happens to share a name with
// an alias must stay literal, otherwise it would resolve back to the primary and
// the fallback would silently do nothing.
func (g *Gateway) routeResolved(model string) (string, string) {
	if model == "" {
		return g.cfg.DefaultProvider, g.cfg.DefaultModel
	}
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := g.providers[model[:i]]; ok {
			return model[:i], model[i+1:]
		}
	}
	// 裸名：按 routing.prefer 顺序选第一个「模型目录里真有该模型」的供应商。
	if p := g.preferProvider(model); p != "" {
		return p, model
	}
	return g.cfg.DefaultProvider, model
}

// providerAllowSet 把入站密钥的供应商白名单转成查询表；空列表返回 nil（= 全量）。
func providerAllowSet(allowed []string) map[string]bool {
	if len(allowed) == 0 {
		return nil
	}
	out := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		out[name] = true
	}
	return out
}

// routeScoped 是 route 的收窄版本：allowed 非空时，只会在这些供应商里选落点。
//
// 与「先 route 再校验」的区别在别名链：一条 deepseek 别名可能同时指向
// cline-pass 和 yijia，只授 yijia 的密钥应该走链上的 yijia，而不是因为链首是
// cline-pass 被整个拒掉。反过来，如果链首就在范围内，也绝不能先打链首再靠兜底
// 回到范围内——那等于让客户端用了没授权的供应商。两个方向都由这里一次决定。
func (g *Gateway) routeScoped(model string, allowed []string) (string, string, bool) {
	allow := providerAllowSet(allowed)
	if allow == nil {
		prov, m := g.route(model)
		return prov, m, true
	}
	for _, target := range g.requestedTargets(model) {
		if g.modelMap != nil {
			if chain := g.modelMap.Chain(target); len(chain) > 0 {
				for _, hop := range chain {
					if prov, m, ok := g.routeResolvedScoped(hop, allow); ok {
						return prov, m, true
					}
				}
				// 这条链上没有权限内目标。省略 model 时继续试下一个写法；
				// 客户端点名要的模型则直接拒绝。
				if model != "" {
					return "", "", false
				}
				continue
			}
		}
		if prov, m, ok := g.routeResolvedScoped(target, allow); ok {
			return prov, m, true
		}
	}
	if model != "" {
		return "", "", false
	}
	// 省略 model 的最后兜底：落点是权限内供应商，模型退回默认模型的裸名。
	// 客户端什么都没点名，用 403 回答「没写 model」只会让人以为密钥坏了。
	prov := g.defaultProviderScoped(allow)
	if prov == "" {
		return "", "", false
	}
	return prov, g.withoutProviderPrefix(g.cfg.DefaultModel), true
}

// requestedTargets 返回一个请求应依次尝试的「已解析目标」。
//
// 非空 model 只有一个目标（就是客户端写的那串）。省略 model 时目标是
// default_model：先试它的完整写法，再试它剥掉供应商前缀后的裸名。
//
// 完整写法优先是为了和「显式发 default_model」完全一致——两者必须是同一条
// 路由，否则改 default_model 之后，不写 model 的客户端会悄悄走到别的供应商。
// 裸名是收窄范围时的退路：default_model 常写成 "provider/model"，而该
// provider 完全可能不在某个入站密钥的 providers 里，此时靠裸名仍能在范围内
// 解析出落点与别名链，而不是直接 403。
func (g *Gateway) requestedTargets(model string) []string {
	if model != "" {
		return []string{model}
	}
	expanded := g.cfg.DefaultModel
	if expanded == "" {
		return nil
	}
	if bare := g.withoutProviderPrefix(expanded); bare != expanded {
		return []string{expanded, bare}
	}
	return []string{expanded}
}

// withoutProviderPrefix 剥掉形如 "已知供应商/" 的前缀；不是已知前缀时原样返回。
func (g *Gateway) withoutProviderPrefix(model string) string {
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := g.providers[model[:i]]; ok {
			return model[i+1:]
		}
	}
	return model
}

// routeResolvedScoped 在已知的 allow 表下解析一个「已经过别名解析」的目标。
// 语义与 routeResolved 一致，只是所有裸名回落都限制在 allow 内。
func (g *Gateway) routeResolvedScoped(model string, allow map[string]bool) (string, string, bool) {
	if allow == nil {
		prov, m := g.routeResolved(model)
		return prov, m, true
	}
	if model == "" {
		prov := g.defaultProviderScoped(allow)
		if prov == "" {
			return "", "", false
		}
		return prov, g.defaultModelScoped(prov), true
	}
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := g.providers[model[:i]]; ok {
			if !allow[model[:i]] {
				return "", "", false
			}
			return model[:i], model[i+1:], true
		}
	}
	if p := g.preferProviderIn(model, allow); p != "" {
		return p, model, true
	}
	if allow[g.cfg.DefaultProvider] {
		return g.cfg.DefaultProvider, model, true
	}
	// 默认供应商不在范围内：按配置顺序取第一个允许的供应商。模型目录可能还没
	// 缓存这个裸名，此时宁可让请求落到权限内的供应商，也不该用 403 假装模型不存在。
	if p := g.firstAllowedProvider(allow); p != "" {
		return p, model, true
	}
	return "", "", false
}

// defaultProviderScoped 返回范围内的默认供应商；默认不在范围内时取第一个允许项。
func (g *Gateway) defaultProviderScoped(allow map[string]bool) string {
	if allow[g.cfg.DefaultProvider] {
		return g.cfg.DefaultProvider
	}
	return g.firstAllowedProvider(allow)
}

// defaultModelScoped 返回落在 prov 上的默认模型裸名。
//
// default_model 通常写成 "provider/model"。当请求没带 model、且默认供应商不在
// 该密钥范围内时，落点会被换成第一个允许的供应商；若仍把带原默认前缀的
// "fengshao1227/deepseek-v4.1-flash" 交给它，上游会收到一个自己根本不存在的
// 模型名。这里把「指向别家供应商」的显式前缀剥掉，让落点按自己的目录解析这个
// 裸名；前缀本来就是落点自己、或本来就是裸名时保持原样。
func (g *Gateway) defaultModelScoped(prov string) string {
	model := g.cfg.DefaultModel
	if model == "" || prov == "" {
		return model
	}
	if i := strings.Index(model, "/"); i > 0 {
		if _, ok := g.providers[model[:i]]; ok && model[:i] != prov {
			return model[i+1:]
		}
	}
	return model
}

func (g *Gateway) firstAllowedProvider(allow map[string]bool) string {
	for _, name := range g.order {
		if allow[name] {
			return name
		}
	}
	return ""
}

// preferProvider 在裸模型名下按 routing.prefer 顺序返回第一个「模型目录里真有该
// 模型」的供应商名；没有偏好、或都没命中时返回 ""（调用方回落 default_provider）。
//
// 只看缓存目录（cachedModels，不触发网络拉取）——路由必须是无阻塞的纯内存判断。
// 目录由 models() 在列举 / 兜底时刷新，并在启动时从落盘缓存（文件或 KV 后端）恢复。
func (g *Gateway) preferProvider(model string) string {
	return g.preferProviderIn(model, nil)
}

// preferProviderIn 是 preferProvider 的收窄版本：allow 非空时跳过范围外供应商。
func (g *Gateway) preferProviderIn(model string, allow map[string]bool) string {
	prefer := g.cfg.PreferProviders()
	if len(prefer) == 0 || model == "" {
		return ""
	}
	for _, name := range prefer {
		if allow != nil && !allow[name] {
			continue
		}
		if _, ok := g.providers[name]; !ok {
			continue
		}
		models, ok := g.cachedModels(name)
		if !ok {
			continue
		}
		if modelInList(models, model) {
			return name
		}
	}
	return ""
}

// modelInList 判断模型目录里是否存在该模型 id：精确匹配，或目录条目的最后一段
// 等于该名字（与 resolveModel 的短名解析同口径，故 prefer 命中后 resolveModel
// 一定能把短名解析成完整 id）。
func modelInList(models []openai.Model, model string) bool {
	for _, m := range models {
		if m.ID == model {
			return true
		}
	}
	for _, m := range models {
		if i := strings.LastIndex(m.ID, "/"); i >= 0 && m.ID[i+1:] == model {
			return true
		}
	}
	return false
}

// modelChainFallbacks returns the ordered fallback candidates declared by a
// model alias chain: every target after the primary. A request whose model has a
// chain tries the primary first and, on failure / exhausted credits, walks this
// list (see tryFallback). Returns nil when the model is unmapped, maps to a
// single target, or the gateway has no alias store.
//
// Chain targets are split with routeResolved (no alias re-resolution): they are
// explicit "provider/model" ids and must not be bounced back through the alias
// table.
func (g *Gateway) modelChainFallbacks(model string) []fallbackCandidate {
	if g.modelMap == nil {
		return nil
	}
	chain := g.aliasChain(model)
	if len(chain) <= 1 {
		return nil
	}
	primProv, primModel := g.route(model)
	return g.chainFallbacksFrom(chain, primProv, primModel, nil)
}

// aliasChain 返回该请求所请求模型对应的别名链。
//
// 省略 model 时按 default_model 找链：那条链就是「默认模型」真正会走的路径，
// 拿空串去查链只会得到 nil，等于让「没写 model」的请求永远拿不到回退目标。
func (g *Gateway) aliasChain(model string) []string {
	if g.modelMap == nil {
		return nil
	}
	for _, target := range g.requestedTargets(model) {
		if chain := g.modelMap.Chain(target); len(chain) > 0 {
			return chain
		}
	}
	return nil
}

// modelChainFallbacksScoped 同 modelChainFallbacks，但以「收窄后实际选中的
// 主目标」为排除项，并只保留权限内的链目标。链首可能因为权限被跳过，这时
// 主目标其实是链上的第二个目标；若仍按链首排除，会在失败后把同一个目标再
// 试一遍，白白多打一次上游。
func (g *Gateway) modelChainFallbacksScoped(model, primaryProvider, primaryModel string, allowed []string) []fallbackCandidate {
	if g.modelMap == nil {
		return nil
	}
	chain := g.aliasChain(model)
	if len(chain) <= 1 {
		return nil
	}
	return g.chainFallbacksFrom(chain, primaryProvider, primaryModel, allowed)
}

func (g *Gateway) chainFallbacksFrom(chain []string, primProv, primModel string, allowed []string) []fallbackCandidate {
	allow := providerAllowSet(allowed)
	out := make([]fallbackCandidate, 0, len(chain)-1)
	seen := map[string]bool{}
	for _, target := range chain {
		pn, m, ok := g.routeResolvedScoped(target, allow)
		if !ok || pn == "" || m == "" {
			continue
		}
		if pn == primProv && m == primModel {
			continue
		}
		q := pn + "/" + m
		if seen[q] {
			continue
		}
		seen[q] = true
		out = append(out, fallbackCandidate{Provider: pn, Model: m})
	}
	return out
}

// resolveModel maps a short name to a full upstream model id.
func (g *Gateway) resolveModel(ctx context.Context, providerName, model string) string {
	if model == "" {
		return g.cfg.DefaultModel
	}
	models, err := g.models(ctx, providerName)
	if err == nil {
		for _, m := range models {
			if m.ID == model {
				return model
			}
		}
		for _, m := range models {
			if i := strings.LastIndex(m.ID, "/"); i >= 0 && m.ID[i+1:] == model {
				return m.ID
			}
		}
	}
	if traitsOf(g.ProviderType(providerName)).FallbackDefaultModel && g.cfg.DefaultModel != "" {
		return g.cfg.DefaultModel
	}
	return model
}

// HandleChat serves POST /v1/chat/completions.
func (g *Gateway) HandleChat(ctx context.Context, req *openai.ChatRequest, w http.ResponseWriter) {
	if len(req.Messages) == 0 {
		writeJSON(w, 400, openai.ErrorJSON("messages_required", "messages is required"))
		return
	}
	// 入口 allowModel 已按同一范围校验过；这里再取一次范围，是为了让「别名链首在
	// 权限外、链上另一个目标在权限内」的请求真的落到权限内的那个目标，而不是被
	// 入口拦掉或先打权限外目标再兜底。直接调用 HandleChat 的路径（单测、内部
	// 研究流程）没有 ReqMeta 时等价于全量。
	var keyProviders []string
	if m := ReqMetaFrom(ctx); m != nil {
		keyProviders = m.KeyProviders
	}
	provName, model, routeOK := g.routeScoped(req.Model, keyProviders)
	if !routeOK {
		writeJSON(w, http.StatusForbidden, openai.ErrorJSON("model_not_allowed",
			"api key is scoped to providers ["+strings.Join(keyProviders, ", ")+"]; model "+req.Model+" has no allowed target"))
		return
	}
	// 模型别名引流链：主目标失败/额度耗尽时的有序回退候选（见 modelChainFallbacks）。
	chainFB := g.modelChainFallbacksScoped(req.Model, provName, model, keyProviders)
	prov := g.providers[provName]
	if prov == nil {
		writeJSON(w, 400, openai.ErrorJSON("unknown_provider", "未知 provider: "+provName))
		return
	}
	upstreamModel := g.resolveModel(ctx, provName, model)
	// Gemini Web 自定义 Gem（issue #2）：模型名命中 gem 映射时，改用该 Gem 的
	// 底层模型，并把请求固定到拥有该 Gem 的账号（gem_id 是账号私有的）。
	var gemID, gemAccount string
	if gp, ok := prov.(*geminiweb.Provider); ok {
		if gm, ok := gp.ResolveGem(model); ok {
			gemID = gm.GemID
			gemAccount = gm.AccountID
			if gm.BaseModel != "" {
				upstreamModel = gm.BaseModel
			}
		}
	}
	incognito := g.cfg.Mode == "incognito"
	hasTools := len(req.Tools) > 0

	displayModel := req.Model
	if displayModel == "" {
		displayModel = provName + "/" + upstreamModel
	}
	// 请求记录：回填路由结果（provider + 展示模型名 + 真实上游模型），供 server 出口落盘。
	if m := ReqMetaFrom(ctx); m != nil {
		m.Provider = provName
		m.Model = displayModel
		// 真实上游模型：别名/目录解析后的最终 ID。与 displayModel 不同时，
		// 「这条别名到底有没有生效」才第一次变得可观测。
		m.UpstreamModel = upstreamModel
		// 思考档位：仅当上游会真正收到 reasoning_effort 时记录（raccoon/openai
		// 型会透传；runable 自研协议不支持，记了会误导）。
		if req.ReasoningEffort != "" && supportsEffort(prov.Type()) {
			m.Effort = req.ReasoningEffort
		}
	}

	id := openai.NewID()
	created := time.Now().Unix()

	// 上游对单条 prompt 有字符数硬上限（runable = 8000，见 runable.MaxPromptChars）。
	// 超限不再整条拒掉，而是在 buildInput 里从中间截断后照跑：用户拿得到回答，
	// 不用自己数字数（代价是静默丢一点上下文）。截断发生在拼 prompt 的那一步，
	// 所以流式/非流式、工具路径/非工具路径走的是同一套口径。
	in := g.buildInput(prov, upstreamModel, req, incognito, hasTools)
	in.GemID = gemID
	in.PreferredAccount = gemAccount

	if req.Stream {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		h.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(200)
		// ew 记录首个写失败（客户端断连）：HTTP 头已发 200，写失败改不了状态码，
		// 但请求记录要能区分「人已走」与「上游失败」。它同时把写入串行化：正文帧
		// 来自 runChat 的后台 goroutine，保活 ping 来自主 goroutine，两者必须经同一
		// 把锁，否则会写坏帧（net/http 的 ResponseWriter 不支持并发写）。
		ew := &errWriter{w: w}
		flush := ew.Flush
		// 末尾 usage chunk 按 OpenAI 规范 gate：仅当客户端显式要
		// stream_options.include_usage 时才补发。用量本身照常记进请求记录
		// （下方 m.Usage = res.usage），不受此开关影响。
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		_ = openai.EmitRole(ew, id, displayModel, created)
		flush()

		// contentEmitted 记录是否已经流出过正文：只有「零正文」的失败/空流才允许
		// 改用第三方兜底（与参考实现仅在 streamed_any 为假时兜底同口径——role 首帧
		// 不算正文）。
		contentEmitted := false
		// lastOut 记录「上次真的往外写了东西」的时刻（UnixNano）。保活只在真正
		// 静默超过阈值时才发：既避免正文流动时插噪音帧，也让「静默」的定义基于
		// 实际写出而不是定时器相位。
		var lastOut atomic.Int64
		lastOut.Store(time.Now().UnixNano())
		markOut := func() { lastOut.Store(time.Now().UnixNano()) }
		onText := func(s string) {
			contentEmitted = true
			_ = openai.EmitContent(ew, id, displayModel, created, s)
			flush()
			markOut()
		}
		onReasoning := func(s string) {
			if g.cfg.Reasoning {
				_ = openai.EmitReasoning(ew, id, displayModel, created, s)
				flush()
				markOut()
			}
		}
		// 上游长时间静默（扩展思考 / 长 prompt 预填充）时，每 sseKeepAliveInterval 发
		// 一帧**空 delta 的合法 chat chunk**，避免中间层按读超时掐断流。
		//
		// 这里曾经发的是 SSE 注释帧（": ping\n\n"）：那句注释只能穿透「直连网关」的
		// 场景。线上链路里还有 new-api 一层 SSE 再编码器，它的 StreamScannerHandler
		// 只转发 `data:` / `[DONE]` 行、把注释行 `continue` 掉——注释帧在网关这端
		// 「写成功」，客户端却永远收不到，于是静默期客户端按自己的读超时断连，请求
		// 记录里出现大量 200+interrupted（实测 24h 内 96 次，ttfb≈5ms、token 全 0）。
		// 空 delta 帧是真实上游在思考阶段也会发的形状，任何 OpenAI 客户端都忽略其
		// 语义，但它是 `data:` 行，能原样穿过再编码层。
		type chatOutcome struct {
			res *chatResult
			err error
		}
		outcome := make(chan chatOutcome, 1)
		go func() {
			r, e := g.runChat(ctx, provName, prov, in, hasTools, onText, onReasoning)
			outcome <- chatOutcome{r, e}
		}()
		ticker := time.NewTicker(sseKeepAliveInterval)
		var res *chatResult
		var err error
	wait:
		for {
			select {
			case oc := <-outcome:
				res, err = oc.res, oc.err
				break wait
			case <-ticker.C:
				// 阈值留 25% 余量。markOut 记录的是「帧真正写完」的时刻，比 tick
				// 时刻略晚；若直接拿 sseKeepAliveInterval 当阈值，下一个 tick 算出的
				// since 恰好落在 9.99…s，被判成「刚输出过」，于是每两个 tick 才发
				// 一帧——设定 10s 实际变成 20s（实测抓到过这个 2 倍退化）。
				if time.Since(time.Unix(0, lastOut.Load())) < sseKeepAliveInterval*3/4 {
					continue // 期间已有输出，不必再补保活
				}
				_ = openai.EmitKeepAlive(ew, id, displayModel, created)
				flush()
				markOut()
			}
		}
		ticker.Stop()
		// 失败或零正文、且还没流出任何正文 → 第三方兜底（把兜底结果转成 SSE 发出）。
		if (err != nil || !resultHasOutput(res)) && !contentEmitted {
			if fb := g.tryFallback(ctx, req, provName, upstreamModel, hasTools, chainFB); fb != nil {
				if fb.text != "" {
					_ = openai.EmitContent(ew, id, displayModel, created, fb.text)
				}
				if len(fb.toolCalls) > 0 {
					_ = openai.EmitToolCalls(ew, id, displayModel, created, fb.toolCalls)
				}
				if m := ReqMetaFrom(ctx); m != nil {
					m.Usage = fb.usage
					m.Reasoning = fb.reasoning != ""
				}
				_ = openai.EmitFinish(ew, id, displayModel, created, fb.finish)
				if includeUsage && fb.usage != nil {
					_ = openai.EmitUsage(ew, id, displayModel, created, usageMap(fb.usage))
				}
				_ = openai.Done(ew)
				flush()
				if ew.err != nil || ctx.Err() != nil {
					markStreamFailure(ctx, reqlog.OutcomeInterrupted, 0, disconnectReason(ctx, ew.err))
				}
				g.log.Info("✓ 完成 %s · 流式兜底 · %s", displayModel, fb.finish)
				return
			}
		}
		if err != nil {
			// 分两类：ctx 取消 / 写失败 = 客户端断连（人已走，归 interrupted，不改
			// 观测状态码）；其余 = 上游 error 帧 / 读失败（用 503 观测，与 WorkBuddy
			// 侧流式 error 帧同语义）。此前一律记 200 success，是假成功。
			if ctx.Err() != nil || ew.err != nil {
				markStreamFailure(ctx, reqlog.OutcomeInterrupted, 0, disconnectReason(ctx, ew.err))
			} else {
				markStreamFailure(ctx, reqlog.OutcomeStreamError, http.StatusServiceUnavailable, err.Error())
			}
			g.log.Error("对话失败 %s: %v", displayModel, err)
			fmt.Fprintf(ew, "data: %s\n\n", openai.ErrorJSON("upstream_error", err.Error()))
			_ = openai.EmitFinish(ew, id, displayModel, created, "stop")
			_ = openai.Done(ew)
			flush()
			return
		}
		if !res.sawData && !res.sawFinish {
			// 上游 200 但空流（0 有效帧）、且连 finish_reason 都没发：不是成功。写
			// error 帧兜底，并用 502 观测（与非流式空响应同语义）。上游原文一并带出：
			// 这类失败最常见的成因是上游用 200 回了一个非 SSE 的 JSON 错误体（限流 /
			// 审核 / 参数错），只报 "empty" 会把真实原因彻底吞掉。
			g.log.Error("对话失败 %s: 上游空流（200+0 帧）finish=%q%s", displayModel, res.finish, sampleSuffix(res.sample))
			markStreamFailure(ctx, reqlog.OutcomeStreamError, http.StatusBadGateway, "upstream returned an empty stream"+sampleSuffix(res.sample))
			fmt.Fprintf(ew, "data: %s\n\n", openai.ErrorJSON("upstream_error", "empty upstream stream"+sampleSuffix(res.sample)))
			_ = openai.EmitFinish(ew, id, displayModel, created, "stop")
			_ = openai.Done(ew)
			flush()
			return
		}
		if !res.sawData {
			// 收到过 finish_reason：合法空回复（成因见非流式分支同名注释）。不写 error
			// 帧，照常发 finish + [DONE]，让客户端拿到一个「空但正常结束」的流。
			g.log.Warn("空回复 %s · finish=%s · 上游正常收尾但无正文", displayModel, res.finish)
		}
		if len(res.toolCalls) > 0 {
			_ = openai.EmitToolCalls(ew, id, displayModel, created, res.toolCalls)
		}
		if m := ReqMetaFrom(ctx); m != nil {
			m.Usage = res.usage
			m.Reasoning = res.reasoning != ""
		}
		_ = openai.EmitFinish(ew, id, displayModel, created, res.finish)
		if includeUsage && res.usage != nil {
			_ = openai.EmitUsage(ew, id, displayModel, created, usageMap(res.usage))
		}
		_ = openai.Done(ew)
		flush()
		if ew.err != nil || ctx.Err() != nil {
			// 客户端断连：上游帧无恙，只是没人接了——归为 interrupted（非上游故障，
			// 保留实际状态码 200，与 WorkBuddy 侧同口径）。
			markStreamFailure(ctx, reqlog.OutcomeInterrupted, 0, disconnectReason(ctx, ew.err))
		}
		g.log.Info("✓ 完成 %s · 流式 · %s", displayModel, res.finish)
		return
	}

	res, err := g.runChat(ctx, provName, prov, in, hasTools, nil, nil)
	// 失败或空回复 → 第三方兜底（与参考实现 is_empty_result / 异常路径同口径）。
	if err != nil || !resultHasOutput(res) {
		if fb := g.tryFallback(ctx, req, provName, upstreamModel, hasTools, chainFB); fb != nil {
			res, err = fb, nil
		}
	}
	if err != nil {
		g.log.Error("对话失败 %s: %v", displayModel, err)
		writeJSON(w, 502, openai.ErrorJSON("upstream_error", err.Error()))
		return
	}
	if !res.sawData && !res.sawFinish {
		// 上游 200 但空响应（0 有效帧）、且连 finish_reason 都没发：上游确实断了，不是
		// 成功。上游原文一并带出，原因见流式分支的同名注释。
		g.log.Error("对话失败 %s: 上游空响应 finish=%q%s", displayModel, res.finish, sampleSuffix(res.sample))
		writeJSON(w, 502, openai.ErrorJSON("upstream_error", "empty upstream response"+sampleSuffix(res.sample)))
		return
	}
	if !res.sawData {
		// 收到过 finish_reason：上游正常收尾，只是这次补全一个正文/思考字节都没有。
		// 实测成因：max_tokens 很小时模型把预算全花在上游不上报为 reasoning_content 的
		// 内部推理上，上游于是只发空 delta 帧 + finish_reason=length。这是合法空回复，
		// 按 OpenAI 语义回 200 + 空正文 + finish_reason；当成上游故障会让客户端拿到
		// 502，误以为网关或上游坏了（此前就是如此）。
		g.log.Warn("空回复 %s · finish=%s · 上游正常收尾但无正文", displayModel, res.finish)
	}
	if m := ReqMetaFrom(ctx); m != nil {
		m.Usage = res.usage
		m.Reasoning = res.reasoning != ""
	}
	msg := openai.Message{Role: "assistant"}
	if res.text != "" {
		msg.Content = openai.Content{Text: res.text}
	}
	if len(res.toolCalls) > 0 {
		msg.ToolCalls = res.toolCalls
	}
	writeJSON(w, 200, openai.Completion(id, displayModel, created, msg, res.finish, usageMap(res.usage)))
	g.log.Info("✓ 完成 %s · %s", displayModel, res.finish)
}

// supportsEffort reports whether a provider type forwards reasoning_effort
// upstream. The fact lives in the plugin descriptor (Traits.ReasoningEffort), so
// adding a type that accepts the OpenAI thinking tier is a catalog-only change.
func supportsEffort(provType string) bool {
	return traitsOf(provType).ReasoningEffort
}

// defaultPromptLimit 是各供应商类型「单条 prompt 字符数」的默认上限（上游硬限制）。
// 0 = 无默认上限。默认值来自插件描述符（Traits.MaxPromptChars）。
func defaultPromptLimit(provType string) int {
	return traitsOf(provType).MaxPromptChars
}

// promptLimit 返回该 provider 的单条 prompt 字符上限（0 = 不检查）。供应商配置里的
// max_prompt_chars 优先（负数表示显式关闭），未设置时用该类型的默认值。
func (g *Gateway) promptLimit(prov provider.Provider) int {
	if prov == nil {
		return 0
	}
	for i := range g.cfg.Providers {
		p := &g.cfg.Providers[i]
		if p.Name != prov.Name() {
			continue
		}
		if p.MaxPromptChars < 0 {
			return 0
		}
		if p.MaxPromptChars > 0 {
			return p.MaxPromptChars
		}
		break
	}
	return defaultPromptLimit(prov.Type())
}

// promptTooLongErr 表示请求因单条 prompt 超过上游上限被拒。Actual == 0 表示长度
// 未知（是上游回绝的，不是本地预检算出来的）。错误文案会原样透给客户端，所以必须
// 是「用户看得懂 + 知道怎么办」的说法。
type promptTooLongErr struct {
	Provider string
	Limit    int
	Actual   int
}

func (e *promptTooLongErr) Error() string {
	msg := fmt.Sprintf("请求过长：%s 上游限制单次 prompt 最多 %d 字符", e.Provider, e.Limit)
	if e.Actual > 0 {
		msg += fmt.Sprintf("，本次约 %d 字符", e.Actual)
	}
	return msg + "。请缩短本次消息（或精简系统提示词 / 工具定义）后重试，长上下文建议改用其它分组。"
}

// friendlyUpstreamErr 把上游的技术性错误码换成用户能理解的说法。目前只处理
// PROMPT_LIMIT_EXCEEDED：原样透出时客户端只会报 "stream disconnected before
// completion: upstream HTTP 400: PROMPT_LIMIT_EXCEEDED"，看不出该改什么。
func friendlyUpstreamErr(provName string, limit int, err error) error {
	if err == nil || !strings.Contains(err.Error(), "PROMPT_LIMIT_EXCEEDED") {
		return err
	}
	return &promptTooLongErr{Provider: provName, Limit: limit}
}

// runablePrompt 组装本轮发给 runable 的单条 prompt。advance=true 时推进会话状态
// （标记 system 已发送）；advance=false 只算不改，供写响应头之前的长度预检使用——
// 被拒的首轮绝不能把 systemSent 置位，否则下一轮就漏发 system 了。
// runable 侧按 chatID 保存历史，所以每轮只发「最后一条用户消息」，首轮附带 system。
func (g *Gateway) runablePrompt(model string, msgs []openai.Message, advance bool) string {
	c := g.getConv(toolcall.ConvKey(model, msgs))
	prompt := toolcall.LastUserText(msgs)
	if !c.systemSent {
		if sys := toolcall.SystemText(msgs); sys != "" {
			prompt = sys + "\n\n" + prompt
		}
		if advance {
			c.systemSent = true
		}
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = "你好"
	}
	return prompt
}

// clampPrompt 把单条 prompt 压到上游硬上限以内：超长时保留开头与结尾、从中间省略，
// 并插入一条说明省略了多少字符的标记（见 util.ClipMiddle）。limit <= 0 表示不限制。
// 第二个返回值是原始字符数，供日志使用。
func clampPrompt(s string, limit int) (string, int) {
	n := utf8.RuneCountInString(s)
	if limit <= 0 || n <= limit {
		return s, n
	}
	return util.ClipMiddle(s, limit), n
}

// buildInput 组装发给上游的 ChatInput。单条 prompt 形态的供应商（runable）在这里
// 统一套用上游字符上限：超长从中间截断后照跑（见 clampPrompt），而不是拒掉请求。
func (g *Gateway) buildInput(prov provider.Provider, model string, req *openai.ChatRequest, incognito, hasTools bool) provider.ChatInput {
	limit := g.promptLimit(prov)
	clamp := func(s string) string {
		out, raw := clampPrompt(s, limit)
		if out != s {
			g.log.Warn("截断超长请求 %s/%s: %d → %d 字符", prov.Name(), model, raw, utf8.RuneCountInString(out))
		}
		return out
	}
	// Prompt-based providers need emulated tool calling.
	if hasTools && traitsOf(prov.Type()).ToolEmulation {
		in := provider.ChatInput{
			ChatID:          util.UUID(),
			Model:           model,
			Prompt:          clamp(toolcall.BuildToolPrompt(req.Messages, req.Tools)),
			ReasoningEffort: req.ReasoningEffort,
			Incognito:       incognito,
			Mode:            g.cfg.Mode,
		}
		in.Attachments = collectAttachments(prov, req.Messages)
		g.applySystemPrompt(&in, prov)
		return in
	}

	var in provider.ChatInput
	in.Model = model
	in.Incognito = incognito
	in.Mode = g.cfg.Mode
	in.ReasoningEffort = req.ReasoningEffort
	// 标准采样/结构控制字段：不转发就等于静默丢弃（ChatRequest 解析时就没字段）。
	in.Temperature = req.Temperature
	in.TopP = req.TopP
	in.MaxTokens = req.MaxTokens
	if in.MaxTokens == nil {
		in.MaxTokens = req.MaxCompletionTokens // OpenAI 的新别名
	}
	in.Stop = req.Stop
	in.ParallelToolCalls = req.ParallelToolCalls
	in.ToolChoice = req.ToolChoice
	in.ResponseFormat = req.ResponseFormat
	in.User = req.User
	if hasTools {
		in.Messages = req.Messages
		in.Tools = req.Tools
	} else if traitsOf(prov.Type()).WireFormat == plugin.WireRunable {
		in.ChatID = g.getConv(toolcall.ConvKey(model, req.Messages)).chatID
		in.Prompt = clamp(g.runablePrompt(model, req.Messages, true))
	} else if traitsOf(prov.Type()).WireFormat == plugin.WireGeminiWeb {
		// Gemini Web 不认 OpenAI 消息数组，也没有可复用的服务端会话 id：把整段
		// 历史（含 system）压成一条 prompt，每次自带上下文。
		p := toolcall.Flatten(req.Messages)
		if sys := toolcall.SystemText(req.Messages); sys != "" {
			p = sys + "\n\n" + p
		}
		in.Prompt = clamp(p)
		in.Attachments = collectAttachments(prov, req.Messages)
	} else {
		in.Messages = req.Messages
	}
	// 供应商级系统提示词（provider.system_prompt）：在分支之后统一注入，覆盖
	// runable / geminiweb 的 prompt 形态与其余消息形态。
	g.applySystemPrompt(&in, prov)
	return in
}

// applySystemPrompt 注入供应商级系统提示词（provider.system_prompt）。
//   - 消息型上游（openai/newapi/raccoon/trae）：在 messages 头部插一条 system 消息；
//   - 单条 prompt 型上游（runable/geminiweb）：拼到 prompt 开头；
//   - 留空则完全不动（保持上游原样）。
//
// 只加不改：客户端自带的 system 消息保留在后（上游自行决定如何合并），避免我们把
// 用户显式写的系统提示悄悄顶掉。
func (g *Gateway) applySystemPrompt(in *provider.ChatInput, prov provider.Provider) {
	if in == nil || prov == nil {
		return
	}
	cfg := g.cfg.Provider(prov.Name())
	if cfg == nil {
		return
	}
	sys := strings.TrimSpace(cfg.SystemPrompt)
	if sys == "" {
		return
	}
	if in.Prompt != "" {
		in.Prompt = sys + "\n\n" + in.Prompt
		return
	}
	in.Messages = append([]openai.Message{{Role: "system", Content: openai.Content{Text: sys}}}, in.Messages...)
}

// collectAttachments gathers the non-text parts of every message, for providers
// that can actually carry them (Traits.Attachments). A text-only provider gets
// nothing: handing it attachments would only invite it to pretend it forwarded
// something.
func collectAttachments(p provider.Provider, msgs []openai.Message) []openai.Attachment {
	if !traitsOf(p.Type()).Attachments {
		return nil
	}
	var out []openai.Attachment
	for _, m := range msgs {
		out = append(out, m.Content.Attachments...)
	}
	return out
}

// markStreamFailure 回填流式请求的失败观测。HTTP 头（200）在首字节前已发出、无法
// 再改状态码，但请求记录必须能区分假成功：outcome 覆盖成 stream_error/interrupted，
// status>0 时用 5xx 作为观测状态码（与 WorkBuddy 侧一致），status==0 时保留实际状态码
// （客户端断连不改状态码）。
//
// reason 必填：只写 outcome 而不写原因，等于把「客户端主动取消」和「中间层按读超时
// 掐断」记成同一件事，线上排查时根本分不清责任方（实测踩过：96 次 interrupted 全是
// 200、ttfb≈5ms、token 全 0，却看不出是谁断的）。
func markStreamFailure(ctx context.Context, outcome string, status int, reason string) {
	m := ReqMetaFrom(ctx)
	if m == nil {
		return
	}
	m.Outcome = outcome
	if status != 0 {
		m.Status = status
	}
	if reason != "" {
		m.Failure = reason
	}
}

// disconnectReason 归纳一次「写失败 / 上下文取消」的具体成因，用于把
// interrupted 记录变成可定位的证据。区分三类：
//   - 客户端主动取消（浏览器关页 / IDE 停止）：ctx.Err()==context.Canceled
//   - 中间层或客户端按读超时断开：写失败（broken pipe / connection reset）
//   - 上游未被读完：上游读错误（由调用方另走 stream_error）
func disconnectReason(ctx context.Context, writeErr error) string {
	switch {
	case ctx.Err() == context.Canceled:
		return "client canceled the request (context canceled)"
	case ctx.Err() == context.DeadlineExceeded:
		return "request deadline exceeded (context deadline exceeded)"
	case writeErr != nil:
		return "client closed the connection while writing: " + writeErr.Error()
	default:
		return "stream ended without a successful write"
	}
}

// failure message: whitespace is flattened so the text stays readable both in a
// log line and inside a JSON error body, and it is truncated.
func sampleSuffix(sample []byte) string {
	if len(sample) == 0 {
		return ""
	}
	s := strings.Join(strings.Fields(string(sample)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	if s == "" {
		return ""
	}
	return " · 上游原文: " + s
}

// sseKeepAliveInterval 是网关等待上游事件的静默上限：超过就发一帧保活，防止中间
// 代理（nginx / 负载均衡 / SSE 再编码层）按读超时掐断长静默的流。包级变量便于
// 测试缩短。
var sseKeepAliveInterval = 10 * time.Second

// errWriter 记录首个写失败，并把写入串行化。SSE 逐帧写入时客户端可能中途断连，
// 写失败本身不改变 HTTP 状态码（头已发），但请求记录要据此归为 interrupted；同时
// 正文帧与保活 ping 来自不同 goroutine，必须经同一把锁避免写坏帧。
type errWriter struct {
	mu  sync.Mutex
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	if err != nil {
		e.err = err
	}
	return n, err
}

// Flush 在与 Write 同一把锁下刷新底层 writer（若支持 http.Flusher），保证保活
// ping 不会插进正文帧的中间。
func (e *errWriter) Flush() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if f, ok := e.w.(http.Flusher); ok {
		f.Flush()
	}
}

// usageMap renders provider usage as an OpenAI-format usage object (nil → zero).
func usageMap(u *provider.Usage) map[string]any {
	if u == nil {
		return nil
	}
	m := map[string]any{
		"prompt_tokens":     u.PromptTokens,
		"completion_tokens": u.CompletionTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		m["prompt_tokens_details"] = map[string]any{"cached_tokens": u.CachedTokens}
	}
	if u.ReasoningTokens > 0 {
		m["completion_tokens_details"] = map[string]any{"reasoning_tokens": u.ReasoningTokens}
	}
	return m
}

func (g *Gateway) runChat(ctx context.Context, provName string, prov provider.Provider, in provider.ChatInput, hasTools bool, onText, onReasoning func(string)) (*chatResult, error) {
	res := &chatResult{finish: "stop"}
	// 上游对单条 prompt 的字符上限（0 = 不检查）。正常路径已在 HandleChat 里预检过，
	// 这里只用于把上游回绝的错误码换成用户看得懂的说法。
	limit := g.promptLimit(prov)

	// Prompt-based providers need emulated tool calling.
	if hasTools && traitsOf(prov.Type()).ToolEmulation {
		stream, err := g.openStream(ctx, provName, prov, in)
		if err != nil {
			return nil, friendlyUpstreamErr(provName, limit, err)
		}
		defer stream.Close()
		parser := toolcall.NewParser(
			func(s string) {
				res.sawData = true
				res.text += s
				if onText != nil {
					onText(s)
				}
			},
			func(c openai.ToolCall) {
				res.sawData = true
				res.toolCalls = append(res.toolCalls, c)
			},
		)
		err = g.drain(ctx, stream, func(e provider.Event) {
			switch e.Type {
			case provider.EventText:
				parser.Feed(e.Text)
			case provider.EventReasoning:
				res.sawData = true
				res.reasoning += e.Text
				if onReasoning != nil {
					onReasoning(e.Text)
				}
			case provider.EventFinish:
				res.finish = e.Finish
				res.sawFinish = true
			case provider.EventUsage:
				res.usage = e.Usage
			case provider.EventError:
				res.errText = e.Text
			}
		})
		parser.Flush()
		if err != nil {
			return nil, friendlyUpstreamErr(provName, limit, err)
		}
		if res.errText != "" {
			return nil, friendlyUpstreamErr(provName, limit, fmt.Errorf("%s", res.errText))
		}
		if len(res.toolCalls) > 0 {
			res.finish = "tool_calls"
		}
		return res, nil
	}

	stream, err := g.openStream(ctx, provName, prov, in)
	if err != nil {
		return nil, friendlyUpstreamErr(provName, limit, err)
	}
	defer stream.Close()

	err = g.drain(ctx, stream, func(e provider.Event) {
		switch e.Type {
		case provider.EventText:
			res.sawData = true
			res.text += e.Text
			if onText != nil {
				onText(e.Text)
			}
		case provider.EventReasoning:
			res.sawData = true
			res.reasoning += e.Text
			if onReasoning != nil {
				onReasoning(e.Text)
			}
		case provider.EventToolCalls:
			res.sawData = true
			res.toolCalls = append(res.toolCalls, e.ToolCalls...)
		case provider.EventFinish:
			res.finish = e.Finish
			res.sawFinish = true
		case provider.EventUsage:
			res.usage = e.Usage
		case provider.EventError:
			res.errText = e.Text
		}
	})
	if err != nil {
		return nil, friendlyUpstreamErr(provName, limit, err)
	}
	// 上游「无有效帧」时的原文样本（诊断用；实现 provider.Sampler 的流才有）。
	// 放在 drain 之后：样本是边读边攒的。
	if sp, ok := stream.(provider.Sampler); ok {
		res.sample = sp.UpstreamSample()
	}
	if res.errText != "" {
		return nil, fmt.Errorf("%s", res.errText)
	}
	if len(res.toolCalls) > 0 {
		res.finish = "tool_calls"
	}
	return res, nil
}

// TokenRefresher is implemented by token-based providers (raccoon, trae) that can
// renew an expired access token from a refresh token. The gateway uses it to
// refresh proactively (before a call) and reactively (once, on a 401).
type TokenRefresher interface {
	// TokenExpired reports whether acc's access token is missing or near expiry.
	TokenExpired(acc *provider.Account) bool
	// RefreshAccount mints a fresh access token, updating acc in place.
	RefreshAccount(ctx context.Context, acc *provider.Account) error
	// Unauthorized reports whether err is an auth failure worth a refresh+retry.
	Unauthorized(err error) bool
}

// refreshToken mints a fresh access token via the provider and persists it.
//
// Rotation is serialized per account: rotating refresh tokens are single-use, so
// two concurrent refreshes (a scheduled one and an on-demand one) would each send
// the same session id, and the loser would invalidate the winner's session. Under
// the lock the second caller observes the already-rotated credentials (accounts
// are shared pointers) and rotates the new pair instead.
func (g *Gateway) refreshToken(ctx context.Context, provName string, tr TokenRefresher, acc *provider.Account) error {
	mu := g.accountRefreshLock(provName, acc.Label)
	mu.Lock()
	defer mu.Unlock()
	if err := tr.RefreshAccount(ctx, acc); err != nil {
		return err
	}
	g.st.SetTokens(provName, acc.Label, acc.Cookie, acc.RefreshToken)
	return nil
}

// accountRefreshLock returns the per-account rotation mutex (created on demand).
func (g *Gateway) accountRefreshLock(provName, label string) *sync.Mutex {
	key := provName + "\x00" + label
	m, _ := g.refreshLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// openStream picks an account (switching on exhausted credits) and opens the stream.
func (g *Gateway) openStream(ctx context.Context, provName string, prov provider.Provider, in provider.ChatInput) (provider.Stream, error) {
	g.accountsMu.RLock()
	accts := g.accounts[provName]
	g.accountsMu.RUnlock()

	// A Gem turn is account-scoped: pin to the account that owns the Gem. A
	// gem_id is private to one Google account, so falling back to another account
	// would run the wrong (or no) Gem.
	pinned := false
	if in.PreferredAccount != "" {
		var only []*provider.Account
		for _, a := range accts {
			if a.Label == in.PreferredAccount {
				only = append(only, a)
			}
		}
		if len(only) == 0 {
			return nil, fmt.Errorf("Gem 绑定的账号「%s」不存在", in.PreferredAccount)
		}
		accts = only
		pinned = true
	}

	// Stateless upstreams (api-key based) need no account pool.
	if len(accts) == 0 {
		// 密钥型上游（openai / opencode）的凭证在供应商自己的密钥池里
		// （opencode 是 zen_keys / go_keys 两套），账号池天然为空。AccountMode
		// 为 none 就表示「没有账号记录、凭证在供应商级」，漏掉这个分支会让它在
		// 运行时直接报「no accounts configured」——配置界面上却完全正常。
		if accountModeOf(prov.Type()) == plugin.AccountModeNone {
			if m := ReqMetaFrom(ctx); m != nil {
				m.Account = "api-key"
			}
			return prov.StreamChat(ctx, &provider.Account{}, in)
		}
		return nil, fmt.Errorf("provider %q has no accounts configured", provName)
	}

	n := len(accts)
	start := g.st.ActiveIndex(provName)
	if start < 0 || start >= n {
		start = 0
	}
	now := time.Now()
	// 快过期积分优先（P4）：先用现有余额快照排序，缺失/过期时后台单飞刷新。
	g.ensureCredits(provName)
	var lastErr error
	// 分两趟：第一趟跳过冷却中的账号（避免对刚被限流的账号连打）；若第一趟跳过了
	// 一些账号且都没试成，第二趟只补试这些被跳过的——冷却只是「优先绕开」，不该把
	// 可用账号彻底挡死，但也不重复试已试过的账号。
	var skipped []int
	for pass := 0; pass < 2; pass++ {
		var order []int
		if pass == 0 {
			for k := 0; k < n; k++ {
				order = append(order, (start+k)%n)
			}
			// 快过期积分优先：把窗口内有快过期批次的账号提到最前（层内按最早到期、
			// 金额排序），其余保持轮询顺序。无快照时原样返回 = 旧行为。
			order = g.prioritizeExpiring(provName, accts, order, now)
		} else {
			if len(skipped) == 0 {
				break
			}
			order = skipped
			g.log.Warn("provider「%s」可用账号都在冷却，兜底补试", provName)
		}
		for _, idx := range order {
			acc := accts[idx]
			if pass == 0 && g.cool.Cooling(provName, acc.Label, now) {
				skipped = append(skipped, idx)
				continue
			}
			// Token-based providers refresh before the call when the access token
			// is missing or about to expire.
			tr, _ := prov.(TokenRefresher)
			if tr != nil && acc.RefreshToken != "" && tr.TokenExpired(acc) {
				if err := g.refreshToken(ctx, provName, tr, acc); err != nil {
					g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, err)
				}
			}
			if acc.Cookie == "" && acc.APIKey == "" {
				if !g.tryLogin(ctx, provName, prov, acc) {
					continue
				}
			}
			stream, err := prov.StreamChat(ctx, acc, in)
			// On an auth failure, refresh once and retry.
			if err != nil && tr != nil && tr.Unauthorized(err) {
				if rerr := g.refreshToken(ctx, provName, tr, acc); rerr == nil {
					stream, err = prov.StreamChat(ctx, acc, in)
				}
			}
			// 上游在响应里轮换了 Cookie（geminiweb）：变了才落盘，重启后不必重新粘贴。
			if err == nil && acc.Cookie != "" {
				if cr, ok := prov.(provider.CookieRotator); ok && cr.RotatesCookie() {
					if g.st.Cookie(provName, acc.Label, "") != acc.Cookie {
						g.st.SetCookie(provName, acc.Label, acc.Cookie)
					}
				}
			}
			// 会话型凭据（SessionCookie）由用户手动粘贴，过期时上游回 401/403：
			// 记账供面板预警。
			if err != nil && traitsOf(prov.Type()).SessionCookie && runableUnauthorized(err) {
				g.sess.MarkExpired(provName, acc.Label, err.Error())
			}
			if err == nil {
				// A pinned (Gem) turn must not move the provider's global active
				// index: idx is relative to the filtered one-account slice.
				if !pinned && idx != start {
					g.st.SetActive(provName, idx)
					g.log.Info("⇄ 自动切换账号「%s」", acc.Label)
				}
				// 请求记录：回填实际服务的账号 label。
				if m := ReqMetaFrom(ctx); m != nil {
					m.Account = acc.Label
				}
				// 成功即解除该账号冷却（连续命中计数清零）。
				g.cool.Clear(provName, acc.Label)
				return stream, nil
			}
			lastErr = err
			// 积分耗尽 / 限流：换下一个账号；限流额外把该账号冷却一段时间。
			if provider.OutOfCredits(err) {
				g.log.Warn("账号「%s」积分耗尽，尝试下一个", acc.Label)
				continue
			}
			if rateLimited(err) {
				until, strikes := g.cool.Note(provName, acc.Label, err.Error(), now)
				g.log.Warn("账号「%s」被限流，冷却至 %s（连续 %d 次），尝试下一个",
					acc.Label, until.Format("15:04:05"), strikes)
				continue
			}
			// 其它错误换号也大概率一样，直接返回。
			return nil, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("provider %q has no usable account", provName)
	}
	return nil, lastErr
}

// tryLogin refreshes an account cookie from stored credentials.
func (g *Gateway) tryLogin(ctx context.Context, provName string, prov provider.Provider, acc *provider.Account) bool {
	if acc.Email == "" || acc.Password == "" {
		return false
	}
	rp, ok := prov.(*runable.Provider)
	if !ok {
		return false
	}
	cookie, err := rp.Client().SignIn(ctx, acc.Email, acc.Password)
	if err != nil {
		g.log.Warn("账号「%s」登录失败: %v", acc.Label, err)
		return false
	}
	acc.Cookie = cookie
	g.st.SetCookie(provName, acc.Label, cookie)
	return true
}

func (g *Gateway) drain(ctx context.Context, s provider.Stream, fn func(provider.Event)) error {
	for {
		e, err := s.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(e)
		if e.Type == provider.EventFinish {
			return nil
		}
	}
}

func (g *Gateway) getConv(key string) *conv {
	g.convMu.Lock()
	defer g.convMu.Unlock()
	if len(g.convs) > 4096 {
		cutoff := time.Now().Add(-2 * time.Hour)
		for k, c := range g.convs {
			if c.at.Before(cutoff) {
				delete(g.convs, k)
			}
		}
	}
	c := g.convs[key]
	if c == nil {
		c = &conv{chatID: util.UUID()}
		g.convs[key] = c
	}
	c.at = time.Now()
	return c
}

// RunableCheckinOutcome mirrors RaccoonCheckinOutcome so the panel renders
// runable check-in results with the same shape ({label, success, msg, error}).
type RunableCheckinOutcome struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Message string `json:"msg"`
	Error   string `json:"error,omitempty"`
}

// RunableCheckin verifies the provider's Runable accounts (all, or just the one
// with the given label), refreshing/persisting cookies when possible and
// recording the current credit balance. It is the runable counterpart of
// RaccoonCheckin, so the panel's per-account / 全部签到 buttons work for both.
func (g *Gateway) RunableCheckin(ctx context.Context, providerName, label string) ([]RunableCheckinOutcome, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	out := []RunableCheckinOutcome{}
	matched := false
	for _, acc := range g.accounts[providerName] {
		if label != "" && acc.Label != label {
			continue
		}
		matched = true
		cookie := acc.Cookie
		if cookie == "" && acc.Email != "" && acc.Password != "" {
			c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password)
			if err != nil {
				out = append(out, RunableCheckinOutcome{Label: acc.Label, Error: "登录失败：" + err.Error()})
				continue
			}
			cookie = c
		}
		if cookie == "" {
			out = append(out, RunableCheckinOutcome{Label: acc.Label, Error: "没有可用凭证，请重新复制 Cookie"})
			continue
		}
		sess := rp.Client().FetchSession(ctx, cookie)
		if !sess.Alive && acc.Email != "" && acc.Password != "" {
			if c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password); err == nil {
				cookie = c
				sess = rp.Client().FetchSession(ctx, cookie)
			}
		}
		if !sess.Alive {
			g.sess.MarkExpired(providerName, acc.Label, "会话无效或已过期")
			out = append(out, RunableCheckinOutcome{Label: acc.Label, Error: "会话无效或已过期，请重新在 runable.com 登录后复制 Cookie"})
			continue
		}
		acc.Cookie = cookie
		g.st.SetCookie(providerName, acc.Label, cookie)
		g.sess.MarkVerified(providerName, acc.Label)
		oc := RunableCheckinOutcome{Label: acc.Label, Success: true, Message: "会话有效"}
		if cr := rp.Client().FetchCredits(ctx, cookie); cr.OK {
			oc.Message = fmt.Sprintf("会话有效，积分 %d（日 %d + 月 %d）", cr.Total, cr.Daily, cr.Monthly)
			// 保活顺带刷新快过期优先选号的余额快照。
			g.noteCredits(providerName, acc.Label, cr.Total, runableWallets(cr))
		} else if cr.Error != "" {
			oc.Message = "会话有效（读取积分失败：" + cr.Error + "）"
		}
		out = append(out, oc)
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}

// RunableAccounts lists a Runable provider's accounts with a best-effort credit
// balance (available = 每日额度 + 每月额度). The panel account pool asks for this
// on demand (withBalance); the plain listing stays a cheap in-memory view so the
// 5s overview poll never hits the upstream. Accounts without a usable cookie are
// returned without a balance rather than failing the whole listing.
func (g *Gateway) RunableAccounts(ctx context.Context, providerName string) ([]map[string]any, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		item := map[string]any{"label": acc.Label, "has_token": acc.Cookie != ""}
		cookie := acc.Cookie
		if cookie == "" && acc.Email != "" && acc.Password != "" {
			if c, err := rp.Client().SignIn(ctx, acc.Email, acc.Password); err == nil {
				cookie = c
				acc.Cookie = c
				g.st.SetCookie(providerName, acc.Label, c)
			}
		}
		if cookie != "" {
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			cr := rp.Client().FetchCredits(bctx, cookie)
			cancel()
			if cr.OK {
				item["available"] = cr.Total
				item["wallets"] = []map[string]any{
					{"type": "monthly_credits", "displayName": "每月额度", "balance": cr.Monthly},
					{"type": "daily_credits", "displayName": "每日额度", "balance": cr.Daily},
				}
				// 顺带把余额喂给快过期优先选号的缓存。
				g.noteCredits(providerName, acc.Label, cr.Total, runableWallets(cr))
			} else if cr.Error != "" {
				item["balance_error"] = cr.Error
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// RunableVerify 校验 runable 账号并返回账号身份 + 当前积分（用于面板「浏览器登录」）。
//
// runable 只支持 Google/Facebook 登录（邮箱密码已全局关闭：服务端直连会得到
// EMAIL_PASSWORD_DISABLED），且登录产物是 runable.com 域下的 httpOnly Cookie——任何
// 网页（含面板）都读不到，Google 也不会把结果交回面板。所以面板无法自动登录/抓取，
// 只能由用户在浏览器登录 runable.com 后，把 session_token 贴进来，这里做一次服务端
// 校验（get-session）并取积分。
//
// 若给了 email/password（runable 未来若重开密码登录）则先登录再校验；否则直接用 cookie。
func (g *Gateway) RunableVerify(ctx context.Context, providerName, email, password, cookie string) (map[string]any, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "runable" {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	rp, ok := g.providers[providerName].(*runable.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 runable 类型", providerName)
	}
	email = strings.TrimSpace(email)
	candidates := runableCookieCandidates(cookie)
	if len(candidates) == 0 && (email == "" || password == "") {
		return nil, fmt.Errorf("请粘贴 session_token（Cookie）")
	}
	lctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	if len(candidates) == 0 {
		c, err := rp.Client().SignIn(lctx, email, password)
		if err != nil {
			return nil, err
		}
		candidates = []string{c}
	}
	// runable 用 better-auth，真实 Cookie 名是 __Secure-better-auth.session_token；
	// 用户往往只复制「值」，所以裸值要依次试真实名，命中哪个就用哪个（并把命中的
	// 完整 Cookie 头回写，供后续聊天直接使用）。
	var sess runable.Session
	cookie = ""
	for _, cand := range candidates {
		if s := rp.Client().FetchSession(lctx, cand); s.Alive {
			sess, cookie = s, cand
			break
		}
	}
	if !sess.Alive {
		return nil, fmt.Errorf("会话无效或已过期：请重新在 runable.com 登录后复制 session_token")
	}
	out := map[string]any{"name": sess.Name, "cookie": cookie}
	if sess.Email != "" {
		out["email"] = sess.Email
	} else {
		out["email"] = email
	}
	if cr := rp.Client().FetchCredits(lctx, cookie); cr.OK {
		out["available"] = cr.Total
		out["monthly"] = cr.Monthly
		out["daily"] = cr.Daily
	}
	return out, nil
}

// runableCookieCandidates 把用户粘贴的内容展开成候选 Cookie 头，按可能性排序：
//   - 已含 "=" → 视为完整 Cookie（`名称=值` 或整段 Cookie 头），原样返回；
//   - 只给值（无 "="）→ 依次尝试 better-auth 的真实 Cookie 名。
//
// runable 的会话 Cookie 实测名为 `__Secure-better-auth.session_token`（better-auth
// 在生产 https 下加的 `__Secure-` 前缀）；只写 `session_token` 会被服务端判为未登录，
// 所以裸值必须优先补真实名，否则「粘贴值」这条最常用的路径会失败。
func runableCookieCandidates(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.Contains(raw, "=") {
		return []string{raw}
	}
	return []string{
		"__Secure-better-auth.session_token=" + raw,
		"better-auth.session_token=" + raw,
		"session_token=" + raw,
	}
}

// LastCheckin returns when a named check-in task last ran.
func (g *Gateway) LastCheckin(name string) time.Time { return g.st.LastCheckinAt(name) }

// MarkCheckin records that a named check-in task just ran.
func (g *Gateway) MarkCheckin(name string) { g.st.MarkCheckin(name) }

// CooldownUntil 报告某账号是否处于限流冷却中（供面板账号池展示）。
func (g *Gateway) CooldownUntil(providerName, label string) (time.Time, bool) {
	return g.cool.Until(providerName, label, time.Now())
}

// SessionStatus 报告某账号的会话健康（供面板账号池做过期预警）。
// ok=false 表示从未记账。
func (g *Gateway) SessionStatus(providerName, label string) (verifiedAt, expiredAt time.Time, lastErr string, ok bool) {
	return g.sess.Status(providerName, label)
}

// Info returns a small status snapshot for /healthz.
func (g *Gateway) Info(ctx context.Context) map[string]any {
	now := time.Now()
	providers := map[string]any{}
	for _, name := range g.order {
		accts := g.Accounts(name)
		labels := make([]string, 0, len(accts))
		for _, a := range accts {
			state := "no-cookie"
			if a.Cookie != "" {
				state = "ready"
			}
			// 限流冷却优先级最高：冷却中的账号即便有 cookie 也不该被当成可用。
			if until, ok := g.cool.Until(name, a.Label, now); ok {
				state = "cooling-until-" + until.Format("15:04:05")
			}
			labels = append(labels, a.Label+"("+state+")")
		}
		providers[name] = map[string]any{
			"type":     g.ProviderType(name),
			"accounts": labels,
			"active":   g.st.ActiveIndex(name),
		}
	}
	return map[string]any{
		"status":    "ok",
		"service":   "omnigate",
		"mode":      g.cfg.Mode,
		"providers": providers,
		"cooldowns": g.cool.Snapshot(),
		"sessions":  g.sess.Snapshot(),
	}
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// MarshalIndent is a tiny helper used by the CLI to pretty-print JSON.
func MarshalIndent(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}
