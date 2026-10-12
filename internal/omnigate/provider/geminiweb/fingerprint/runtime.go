package fingerprint

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// errTLSDisabled 表示 TLS 伪装目标不可用时的防御性错误。
var errTLSDisabled = errors.New("fingerprint: TLS impersonation disabled")

// Options 配置反检测层。零值 Options 表示「纯内存默认指纹、抖动关闭、TLS 关闭、
// 版本同步关闭」——适合测试与不需要反检测的场景。
type Options struct {
	// ConfigPath 是 fingerprint.json 的路径；空 = 纯内存默认值（不落盘）。
	ConfigPath string
	// CookieDir 是持久化 Cookie 的目录；空 = 纯内存。
	CookieDir string
	// JitterEnabled 打开请求间隔抖动。
	JitterEnabled bool
	// VersionSyncEnabled 打开后台 Chrome 版本同步。
	VersionSyncEnabled bool
	// VersionSyncInterval 是版本同步间隔（<=0 视为 24h）。
	VersionSyncInterval time.Duration
	// TLSEnabled 打开 utls 的 TLS ClientHello 伪装。
	TLSEnabled bool
	// Logf 记录非致命问题（配置损坏、版本同步异常等）。
	Logf func(format string, args ...any)
}

// Runtime 把五个模块打包成一个可挂到客户端上的整体。
type Runtime struct {
	opts    Options
	mgr     *Manager
	headers *HeaderBuilder
	jitter  Jitter
	tls     *TLSImpersonator
	syncer  *VersionSyncer

	startOnce sync.Once
	syncRun   atomic.Bool
}

// NewRuntime 创建并加载指纹配置。
func NewRuntime(opts Options) *Runtime {
	mgr := NewManager(opts.ConfigPath)
	cfg, err := mgr.Load()
	if err != nil && opts.Logf != nil {
		opts.Logf("fingerprint: 指纹配置加载失败，使用默认值: %v", err)
	}
	if cfg == nil {
		cfg = DefaultConfig()
	}
	rt := &Runtime{
		opts:    opts,
		mgr:     mgr,
		headers: NewHeaderBuilder(mgr),
		jitter:  Jitter{Enabled: opts.JitterEnabled},
		syncer:  NewVersionSyncer(mgr, nil, opts.Logf),
	}
	rt.tls = NewTLSImpersonator(cfg.Chrome.ImpersonateTarget)
	if !opts.TLSEnabled {
		rt.tls.Disable()
	}
	// 版本同步把 impersonate 目标改写进配置后，让 TLS 伪装器同步切换，避免
	// 「配置里是 chrome130、实际握手仍是旧 ClientHello」的错位。
	rt.syncer.OnTarget = func(target string) { rt.tls.SetTarget(target) }
	return rt
}

// Manager 返回配置管理器。
func (rt *Runtime) Manager() *Manager { return rt.mgr }

// Headers 返回请求头构建器。
func (rt *Runtime) Headers() *HeaderBuilder { return rt.headers }

// Jitter 返回抖动配置。
func (rt *Runtime) Jitter() Jitter { return rt.jitter }

// TLS 返回 TLS 伪装器。
func (rt *Runtime) TLS() *TLSImpersonator { return rt.tls }

// CookieDir 返回持久化 Cookie 目录。
func (rt *Runtime) CookieDir() string { return rt.opts.CookieDir }

// OpenJar 按账号键打开（可选持久化的）Cookie 容器。
func (rt *Runtime) OpenJar(accountKey string) *Jar { return NewJar(accountKey, rt.opts.CookieDir) }

// StartVersionSync 启动后台版本同步（幂等）。仅在 VersionSyncEnabled 时真正启动。
func (rt *Runtime) StartVersionSync(ctx context.Context) {
	if rt == nil || !rt.opts.VersionSyncEnabled {
		return
	}
	rt.startOnce.Do(func() {
		rt.syncRun.Store(true)
		go rt.syncer.Run(ctx, rt.opts.VersionSyncInterval)
	})
}

// VersionSyncRunning 报告后台同步循环是否已启动（供面板展示）。
func (rt *Runtime) VersionSyncRunning() bool {
	return rt != nil && rt.syncRun.Load()
}

// SyncChromeVersion 立即执行一次版本检查（面板手动「同步」按钮用）。
func (rt *Runtime) SyncChromeVersion(ctx context.Context) {
	if rt == nil {
		return
	}
	rt.syncer.Sync(ctx)
}

// SetChromeTarget 切换 TLS 指纹目标并热更新（面板用）。
func (rt *Runtime) SetChromeTarget(target string) error {
	if !ValidateImpersonateTarget(target) {
		return fmt.Errorf("fingerprint: 不支持的 TLS 目标 %q（可用: %v）", target, SupportedTargets())
	}
	if err := rt.mgr.SetChromeTarget(target); err != nil {
		return err
	}
	rt.tls.SetTarget(target)
	return nil
}

// Status 返回面板展示用的状态快照。
func (rt *Runtime) Status() map[string]any {
	cfg := rt.mgr.Config()
	keys := cfg.VersionKeys()
	vers := make([]int, 0, len(keys))
	vers = append(vers, keys...)
	return map[string]any{
		"chrome_major":         cfg.Chrome.Major,
		"chrome_full":          cfg.Chrome.Full,
		"impersonate_target":   cfg.Chrome.ImpersonateTarget,
		"platform":             cfg.Platform,
		"header_order":         cfg.HeaderOrder,
		"headers":              cfg.Headers,
		"version_map":          cfg.VersionMap,
		"last_updated":         cfg.LastUpdated,
		"config_path":          rt.mgr.Path(),
		"cookie_dir":           rt.opts.CookieDir,
		"jitter_enabled":       rt.jitter.Enabled,
		"version_sync_enabled": rt.opts.VersionSyncEnabled,
		"version_sync_running": rt.VersionSyncRunning(),
		"version_sync_hours":   int(rt.opts.VersionSyncInterval / time.Hour),
		"tls_enabled":          rt.tls.Enabled(),
		"tls_supported":        SupportedTargets(),
	}
}

// SupportedTargets 返回本构建可用的 impersonate 目标名（升序）。
func SupportedTargets() []string {
	majors := SupportedChromeMajors()
	out := make([]string, 0, len(majors))
	for _, m := range majors {
		out = append(out, "chrome"+strconv.Itoa(m))
	}
	return out
}

// baseDialer 供客户端接入 TLS 伪装时复用。
func baseDialer() *net.Dialer {
	return &net.Dialer{Timeout: 20 * time.Second}
}
