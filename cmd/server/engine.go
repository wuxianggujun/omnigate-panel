// engine.go 单引擎（OmniGate）的进程内装配与热重载。
//
// 设计：把「配置 → 网关 → 调度器 → HTTP handler」打包成一个不可变运行时
// （omniRuntime），用 atomic.Pointer 挂载。供应商配置页保存时，重建一份新的
// 运行时并原子替换旧运行时（旧调度器停掉），因此改供应商无需重启进程。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	omniconfig "github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	omnigateway "github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	omnilogx "github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	omnisched "github.com/wuxianggujun/omnigate-panel/internal/omnigate/scheduler"
	omnisrv "github.com/wuxianggujun/omnigate-panel/internal/omnigate/server"
	omnistate "github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

// omniRuntime 是一份自洽的 OmniGate 运行时（构建后只读，替换而非修改）。
type omniRuntime struct {
	cfg   *omniconfig.Config
	gw    *omnigateway.Gateway
	sched *omnisched.Scheduler
	srv   *omnisrv.Server
}

// omniManager 持有当前运行时并负责热重载。enabled=false 时面板自动跳过 OmniGate。
type omniManager struct {
	enabled bool
	path    string
	dataDir string
	apiKey  string
	logger  *omnilogx.Logger

	mu  sync.Mutex // 串行化重载
	st  *omnistate.State
	cur atomic.Pointer[omniRuntime]

	// autoStop 关闭后台上游密钥自动清理循环（Stop 时 close 一次）。
	autoStop chan struct{}

	// outbound 面板出站代理路由（config.json 的 outbound 段）。用于把
	// "omnigate:<provider>" 解析成代理 URL，在 build 时注入各供应商。
	outbound outbound.Config

	// obRT 是 outbound 的运行时（持有代理池的后台刷新 + 探活）。随 outbound
	// 变更重建；nil 表示当前没有代理池。
	obRT *outbound.Runtime

	// reqlog 是进程内共享的请求记录器（面板「请求」页）。
	reqlog *reqlog.Recorder
	clientInfo func() bool

	// usageRec 是进程内共享的用量记录器（面板「用量」页）。nil = 不计入。
	usageRec *usage.Recorder

	// pg 面板状态 KV 后端（PostgreSQL，可为 nil）。OmniGate 状态 / 模型目录缓存 /
	// 全局模型映射经它落库（库为权威，文件仅首次导入 + 断库兜底）。
	pg kv.Backend
}

// newOmniManager 按 cfg.OmnigateConfig 装配 OmniGate。配置文件缺失/解析失败时
// 返回未启用的 manager（不报错，面板其余功能照常）。requestLog / usageRec 为共享
// 记录器（可为 nil）；clientInfo 报告是否记录调用来源（可为 nil）。
func newOmniManager(cfg *Config, dataDir string, requestLog *reqlog.Recorder, usageRec *usage.Recorder, clientInfo func() bool, obRT *outbound.Runtime, pg kv.Backend) *omniManager {
	m := &omniManager{
		path:       cfg.OmnigateConfig,
		dataDir:    dataDir,
		apiKey:     cfg.APIKey,
		logger:     omnilogx.New(300),
		outbound:   cfg.Outbound,
		obRT:       obRT,
		reqlog:     requestLog,
		usageRec:   usageRec,
		clientInfo: clientInfo,
		pg:         pg,
	}
	if m.path == "" {
		m.path = "omnigate.json"
	}
	if _, err := os.Stat(m.path); err != nil {
		if os.IsNotExist(err) {
			if dir := filepath.Dir(m.path); dir != "" && dir != "." {
				_ = os.MkdirAll(dir, 0o755)
			}
			if werr := os.WriteFile(m.path, []byte("{\n  \"providers\": []\n}\n"), 0o600); werr != nil {
				log.Printf("WARN: [omnigate] 无法生成 %s: %v", m.path, werr)
				return m
			}
			log.Printf("[omnigate] 未找到 %s，已生成空引擎配置（在面板添加供应商即可）", m.path)
		}
		if _, statErr := os.Stat(m.path); statErr != nil {
			log.Printf("[omnigate] 未启用：配置文件 %s 不存在: %v", m.path, statErr)
			return m
		}
	}
	ocfg, err := omniconfig.Load(m.path)
	if err != nil {
		log.Printf("WARN: [omnigate] 加载 %s 失败，已跳过：%v", m.path, err)
		return m
	}
	if ocfg.DataDir == "" || ocfg.DataDir == "data" {
		ocfg.DataDir = filepath.Join(m.dataDir, "omnigate")
	}
	st, err := omnistate.Load(ocfg.DataDir)
	if err != nil {
		log.Printf("WARN: [omnigate] 加载状态失败，已跳过：%v", err)
		return m
	}
	m.st = st
	// OmniGate 状态可选落库：库为权威，文件仅首次导入 + 断库兜底。
	if pg != nil {
		if err := st.SetBackend(pg, "omnigate_state"); err != nil {
			log.Printf("WARN: [storage] OmniGate 状态启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	rt, err := m.build(ocfg)
	if err != nil {
		log.Printf("WARN: [omnigate] 初始化网关失败，已跳过：%v", err)
		return m
	}
	m.cur.Store(rt)
	m.enabled = true
	log.Printf("[omnigate] 已启用：默认 provider=%s，provider 数=%d，数据目录=%s，网关挂载于 /",
		ocfg.DefaultProvider, len(ocfg.Providers), ocfg.DataDir)
	m.logProviders(rt)
	m.startAutoCleanup()
	return m
}

// build 由配置构建一份新运行时（不动旧运行时）。
func (m *omniManager) build(cfg *omniconfig.Config) (*omniRuntime, error) {
	// 面板 api_key 复用为网关密钥（面板与客户端共用同一把全量钥匙；收窄密钥
	// 走 api_keys，不会混进这里）。
	// 它走独立的 PanelKey（不落盘、恒全量），刻意不混进面向客户端的 api_keys：
	// 混进去会被写进 omnigate.json，轮换 api_key 后旧钥匙仍残留在文件里继续可用，
	// 而且它会出现在面板的「网关密钥」卡里、被误当成一把可删的客户端密钥。
	cfg.PanelKey = m.apiKey
	// 兼容历史形态：早期版本把面板密钥写进了 api_keys（当时「api_keys 为空才注入」，而
	// 保存时又整段保留运行时值，于是它被持久化进了文件）。PanelKey 已经覆盖了它，这里
	// 把它从面向客户端的列表里摘掉——否则它会以「一把全量客户端密钥」的形态出现在面板
	// 的密钥卡里，既误导又容易手滑删错。
	cfg.APIKeys = dropAPIKey(cfg.APIKeys, m.apiKey)
	gw, err := omnigateway.New(cfg, m.st, m.logger,
		omnigateway.WithProxyResolver(m.proxyFor),
		omnigateway.WithPoolResolver(m.poolFor),
		// 模型目录跨重启持久化：重启 / 改配置后仍能立刻给出带计费价的目录。
		// 库为权威（omnigate_model_cache），文件路径作首次导入 + 断库兜底。
		omnigateway.WithModelCachePath(filepath.Join(cfg.DataDir, "model-cache.json")),
		omnigateway.WithModelCacheBackend(m.pg, "omnigate_model_cache"),
		// 全局模型映射表（glm-5.2 → atria/... 等）同样落库。
		omnigateway.WithModelMappingBackend(m.pg, "omnigate_model_mapping"),
		// 上游密钥健康状态（自动禁用 / 连击记账）：落库后热重载与容器重启都不会
		// 让死 key 复活（omnigate_key_health；文件 key-health.json 兜底）。
		omnigateway.WithKeyHealthPath(filepath.Join(cfg.DataDir, "key-health.json")),
		omnigateway.WithKeyHealthBackend(m.pg, "omnigate_key_health"),
	)
	if err != nil {
		return nil, err
	}
	// 热重载会重建网关（余额快照是进程内缓存，随之清空）。把旧网关的快照搬过来，
	// 否则每次保存配置后面板上的「快过期」角标与快过期优先选号都会短暂失效。
	if prev := m.cur.Load(); prev != nil {
		gw.ShareCreditsFrom(prev.gw)
	}
	sched := omnisched.New(cfg, gw, m.logger)
	sched.Start()
	srv := omnisrv.New(cfg, gw, m.logger)
	// 请求记录与用量记录：网关请求统一进入进程内共享记录器。
	srv.SetRequestLog(m.reqlog, m.clientInfo)
	srv.SetUsageRecorder(m.usageRec)
	// 代理池状态：让 /healthz 能看到各池的存活/候选数。
	srv.SetPoolStatus(func() any { return m.PoolStatuses() })
	return &omniRuntime{cfg: cfg, gw: gw, sched: sched, srv: srv}, nil
}

// dropAPIKey 返回去掉指定 key 后的列表（key 为空时原样返回）。
func dropAPIKey(list omniconfig.APIKeyList, key string) omniconfig.APIKeyList {
	if key == "" {
		return list
	}
	out := make(omniconfig.APIKeyList, 0, len(list))
	for _, k := range list {
		if k.Key != key {
			out = append(out, k)
		}
	}
	return out
}

// writeConfigFile 原子写配置（tmp + rename）。单文件 bind mount / Windows 目标被占用
// 导致 rename 失败时，退回就地覆盖；权限始终 0600（配置内含全部上游凭证）。
func writeConfigFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err == nil {
		return nil
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	_ = os.Remove(tmp)
	return nil
}

// proxyFor 把某供应商解析成出站代理（来自面板 outbound 路由的
// "omnigate:<provider>"）。未配置返回 nil（直连）。
func (m *omniManager) proxyFor(providerName string) *url.URL {
	return m.obRT.SingleURL(outbound.OmniTarget(providerName))
}

// poolFor 把某供应商解析成出站代理池（同上目标名）。未配置返回 nil。
func (m *omniManager) poolFor(providerName string) outbound.Selector {
	return m.obRT.Pool(outbound.OmniTarget(providerName))
}

// PoolStatuses 返回各代理池的可观测快照（供健康检查/面板）。
func (m *omniManager) PoolStatuses() []outbound.PoolStatus {
	if m.obRT == nil {
		return nil
	}
	return m.obRT.PoolStatuses()
}

// Models 返回汇总模型目录（与 /v1/models 同源，含积分价）。面板「模型」页
// 与请求记录页的积分列用它：进程内直取网关运行时，匿名只读访问也无需、且不会
// 拿到网关 api_key。
func (m *omniManager) Models(ctx context.Context) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未就绪")
	}
	return rt.gw.ListModels(ctx), nil
}

// Quota 查询某供应商的额度（面板供应商行的「额度」按钮）。分派交给网关的
// ProviderOps：只有声明了 quota 动作的类型（当前为 opencode）才会执行，面板侧
// 不再感知 provider 类型。进程内直取当前运行时——出站代理路由与热重载后的密钥池
// 都由网关持有。明文密钥不出 provider 层。
func (m *omniManager) Quota(ctx context.Context, provider string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未就绪")
	}
	ops := rt.gw.Ops(provider)
	if !ops.Supports(plugin.ActionQuota) {
		return nil, fmt.Errorf("provider %q 不支持额度查询", provider)
	}
	res, err := ops.HandleAction(ctx, provider, plugin.ActionRequest{Action: plugin.ActionQuota})
	if err != nil {
		return nil, err
	}
	return res.Data, nil
}

// SetAPIKey 更新网关 api_key 并热重载（面板「重新生成」密钥后调用，使面板
// 与网关保持同一把）。未启用时仅记住新值，供后续 build 回填。
func (m *omniManager) SetAPIKey(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	oldKey := m.apiKey
	m.apiKey = key
	if m.st == nil {
		return nil
	}
	rt := m.cur.Load()
	if rt == nil {
		return nil
	}
	// 深拷贝当前配置后重建：面向客户端的 api_keys 原样保留，只有 PanelKey 换成新值
	//（见 build）。
	raw, err := json.Marshal(rt.cfg)
	if err != nil {
		return err
	}
	var cfg omniconfig.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	// 轮换后面板旧密钥必须立刻失效：历史版本把它持久化进了 api_keys，不摘掉的话它会以
	// 「一把全量客户端密钥」的形态继续可用（文件里还留着）。build 会再摘一次新值。
	cfg.APIKeys = dropAPIKey(cfg.APIKeys, oldKey)
	newRT, err := m.build(&cfg)
	if err != nil {
		return err
	}
	if old := m.cur.Swap(newRT); old != nil {
		old.sched.Stop()
	}
	log.Printf("[omnigate] api_key 已随面板密钥轮换热重载")
	return nil
}

// SetOutbound 更新出站代理路由并热重载当前运行时（供应商代理即时生效）。
// 代理池运行时的热替换由调用方（saveConfig）负责，这里只重建网关。
func (m *omniManager) SetOutbound(o outbound.Config) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outbound = o
	if m.st == nil {
		return errors.New("omnigate 未启用")
	}
	rt := m.cur.Load()
	if rt == nil {
		return errors.New("omnigate 未启用")
	}
	newRT, err := m.build(rt.cfg)
	if err != nil {
		return err
	}
	prev := m.cur.Swap(newRT)
	if prev != nil {
		prev.sched.Stop()
	}
	log.Printf("[omnigate] 出站代理路由已热重载（%d 条路由）", len(o.Routes))
	return nil
}

func (m *omniManager) logProviders(rt *omniRuntime) {
	for _, name := range rt.gw.ProviderNames() {
		log.Printf("[omnigate]   · provider %s (%s) · 账号 %d 个", name, rt.gw.ProviderType(name), len(rt.gw.Accounts(name)))
	}
}

// Enabled 报告 OmniGate 是否已装配成功。
func (m *omniManager) Enabled() bool { return m.enabled }

// Handler 返回稳定入口：每次请求读当前运行时（热重载后自动指向新运行时）。
func (m *omniManager) Handler() http.Handler {
	if !m.enabled {
		return nil
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rt := m.cur.Load()
		if rt == nil {
			http.Error(w, "omnigate not ready", http.StatusServiceUnavailable)
			return
		}
		rt.srv.ServeHTTP(w, r)
	})
}

// Stop 停掉当前调度器与后台清理循环，并落盘状态（进程退出时调用）。
func (m *omniManager) Stop() {
	if !m.enabled {
		return
	}
	if m.autoStop != nil {
		close(m.autoStop)
		m.autoStop = nil
	}
	if rt := m.cur.Load(); rt != nil {
		rt.sched.Stop()
	}
	if m.st != nil {
		m.st.Save()
	}
}

// Load 返回当前供应商配置（面板配置页读取用）。
func (m *omniManager) Load() (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	return rt.cfg, nil
}

// Save 校验 + 落盘 + 热重载。校验失败不写盘、不重载。
func (m *omniManager) Save(raw []byte) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveLocked(raw)
}

// saveLocked 是 Save 的加锁内实现（调用方必须持 m.mu）。后台自动清理同样走
// 这条路径，保证「校验 → 落盘 → 热重载」全项目只有一条写入路径。
func (m *omniManager) saveLocked(raw []byte) ([]string, error) {
	if m.st == nil {
		return nil, errors.New("omnigate 未启用")
	}
	var cfg omniconfig.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	// 保留服务端/运行时字段：面板不编辑 server / data_dir（api_keys 现在由面板的
	// 「网关密钥」卡管理，故不再保留——它已不再是纯运行时字段）。
	if rt := m.cur.Load(); rt != nil {
		cfg.Server = rt.cfg.Server
		cfg.DataDir = rt.cfg.DataDir
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	pretty, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	// 先构建成功再写盘：构建失败时磁盘上的旧配置保持原样，不会出现「文件已换、
	// 运行时还是旧的」半截状态。
	rt, err := m.build(&cfg)
	if err != nil {
		return nil, err
	}
	if err := writeConfigFile(m.path, pretty, 0o600); err != nil {
		rt.sched.Stop()
		return nil, fmt.Errorf("写入 %s 失败: %w", m.path, err)
	}
	old := m.cur.Swap(rt)
	if old != nil {
		old.sched.Stop()
	}
	log.Printf("[omnigate] 供应商配置已热重载：provider 数=%d，默认 provider=%s", len(cfg.Providers), cfg.DefaultProvider)
	m.logProviders(rt)
	return nil, nil
}

// ---------------------------------------------------------------------------
// 上游密钥池健康：自动清理（额度耗尽的死 key）
// ---------------------------------------------------------------------------

const (
	// keyAutoCleanupStrikes / keyAutoCleanupAfter 自动清理阈值：连续额度错误
	// 达到该次数（任一次成功即清零），且首见时间超出窗口，才摘除。长禁用 24h
	// 一次试炼——3 次 ≈ 连败三天、首见满七天，稳到不需要人工盯。
	keyAutoCleanupStrikes = 3
	keyAutoCleanupAfter   = 7 * 24 * time.Hour
)

// startAutoCleanup 启动后台上游密钥自动清理循环（进程生命周期）：首次延后
// 30 分钟错开启动高峰，之后每小时检查一次。无候选时零开销。
func (m *omniManager) startAutoCleanup() {
	m.autoStop = make(chan struct{})
	go func() {
		select {
		case <-time.After(30 * time.Minute):
		case <-m.autoStop:
			return
		}
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			m.cleanupOnce()
			select {
			case <-t.C:
			case <-m.autoStop:
				return
			}
		}
	}()
}

func (m *omniManager) cleanupOnce() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("WARN: [omnigate] 自动清理 panic: %v", r)
		}
	}()
	removed, err := m.CleanupDeadKeys(time.Now())
	if err != nil {
		log.Printf("WARN: [omnigate] 自动清理失败: %v", err)
		return
	}
	for _, line := range removed {
		log.Printf("[omnigate] 自动清理额度耗尽密钥：%s（已备份为 %s.bak-auto-cleanup-*）", line, m.path)
	}
}

// CleanupDeadKeys 自动清理「长期额度耗尽」的池内密钥：连续额度错误达到阈值
// （keyAutoCleanupStrikes）且首见超出窗口（keyAutoCleanupAfter）的 key 从
// omnigate.json 摘除并热重载（死 key 立即退出轮换）。摘除前整文件备份；池内若
// 只剩这类 key 则跳过——摘完这家就没密钥可用了，留给人工。返回被摘除描述行。
func (m *omniManager) CleanupDeadKeys(now time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st == nil {
		return nil, nil
	}
	rt := m.cur.Load()
	if rt == nil {
		return nil, nil
	}
	snap := rt.gw.KeyHealthSnapshot()
	if len(snap) == 0 {
		return nil, nil
	}
	raw0, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var fileCfg omniconfig.Config
	if err := json.Unmarshal(raw0, &fileCfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", m.path, err)
	}
	var removed []string
	changed := false
	for i := range fileCfg.Providers {
		p := &fileCfg.Providers[i]
		if len(p.APIKeys) < 2 {
			continue // 单项池没有轮换取舍；摘掉等于废掉这家
		}
		st := snap[p.Name]
		if len(st) == 0 {
			continue
		}
		keep := make([]string, 0, len(p.APIKeys))
		var hits []string
		for _, k := range p.APIKeys {
			h, ok := st[openaibackend.KeyID(strings.TrimSpace(k))]
			if ok && h.Strikes >= keyAutoCleanupStrikes && h.FirstSeen > 0 &&
				now.Sub(time.Unix(h.FirstSeen, 0)) >= keyAutoCleanupAfter {
				hits = append(hits, fmt.Sprintf("%s 的密钥 %s（连续 %d 次额度错误，首见 %s）",
					p.Name, openaibackend.MaskKey(k), h.Strikes,
					time.Unix(h.FirstSeen, 0).Format("2006-01-02")))
				continue
			}
			keep = append(keep, k)
		}
		if len(hits) == 0 {
			continue
		}
		if len(keep) == 0 {
			log.Printf("[omnigate] 自动清理跳过 provider %s：池内密钥全部达到清理阈值，保留到人工处理", p.Name)
			continue
		}
		p.APIKeys = keep
		removed = append(removed, hits...)
		changed = true
	}
	if !changed {
		return nil, nil
	}
	// 备份原文件（整文件、含所有密钥字段）——出问题用 .bak-auto-cleanup-* 回滚。
	bak := fmt.Sprintf("%s.bak-auto-cleanup-%s", m.path, now.Format("20060102-150405"))
	if err := os.WriteFile(bak, raw0, 0o600); err != nil {
		return nil, fmt.Errorf("备份 %s 失败，取消本次清理: %w", bak, err)
	}
	pretty, err := json.MarshalIndent(&fileCfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, err := m.saveLocked(pretty); err != nil {
		return nil, err
	}
	return removed, nil
}

// KeyHealthReport 面板「密钥健康」视图数据：逐 provider 给出池内每把密钥的
// 掩码与状态（谁被自动禁用了、连败几次、预计何时会被自动清理）。不落明文。
func (m *omniManager) KeyHealthReport() (any, error) {
	// 与 Save / 自动清理共用同一把锁：配置文件的写是「整文件覆盖」，读侧不串行
	// 就可能读到写了一半的 JSON。
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	snap := rt.gw.KeyHealthSnapshot()
	raw0, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var fileCfg omniconfig.Config
	if err := json.Unmarshal(raw0, &fileCfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", m.path, err)
	}
	type keyRow struct {
		Key       string `json:"key"` // 掩码形态
		Disabled  bool   `json:"disabled,omitempty"`
		Reason    string `json:"reason,omitempty"`
		Strikes   int    `json:"strikes,omitempty"`
		Until     int64  `json:"until,omitempty"`
		FirstSeen int64  `json:"first_seen,omitempty"`
		CleanupAt int64  `json:"cleanup_at,omitempty"` // 预计自动清理时间（连败达阈值后）
		LastError string `json:"last_error,omitempty"`
	}
	type provRow struct {
		Name string   `json:"name"`
		Keys []keyRow `json:"keys"`
	}
	out := make([]provRow, 0, len(fileCfg.Providers))
	for i := range fileCfg.Providers {
		p := &fileCfg.Providers[i]
		if len(p.APIKeys) == 0 {
			continue
		}
		rows := make([]keyRow, 0, len(p.APIKeys))
		for _, k := range p.APIKeys {
			row := keyRow{Key: openaibackend.MaskKey(k)}
			if h, ok := snap[p.Name][openaibackend.KeyID(strings.TrimSpace(k))]; ok {
				row.Disabled = h.Disabled
				row.Reason = h.Reason
				row.Strikes = h.Strikes
				row.Until = h.Until
				row.FirstSeen = h.FirstSeen
				row.LastError = h.LastError
				if h.Strikes >= keyAutoCleanupStrikes && h.FirstSeen > 0 {
					row.CleanupAt = time.Unix(h.FirstSeen, 0).Add(keyAutoCleanupAfter).Unix()
				}
			}
			rows = append(rows, row)
		}
		out = append(out, provRow{Name: p.Name, Keys: rows})
	}
	return map[string]any{
		"providers":       out,
		"cleanup_strikes": keyAutoCleanupStrikes,
		"cleanup_days":    int(keyAutoCleanupAfter / (24 * time.Hour)),
	}, nil
}

// ExportAccountsRaw 导出运行时添加的账号（state.json 的 dyn_accounts 段），按
// provider 分组返回原始 JSON。仅含运行时账号；配置里声明的账号在 omnigate.json。
func (m *omniManager) ExportAccountsRaw() (json.RawMessage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st == nil {
		return nil, errors.New("omnigate 未启用")
	}
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	out := map[string][]omnistate.DynAccount{}
	for _, name := range rt.gw.ProviderNames() {
		if accs := m.st.DynAccounts(name); len(accs) > 0 {
			out[name] = accs
		}
	}
	return json.Marshal(out)
}

// ImportAccountsRaw 合并导入 dyn_accounts 并重建运行时（导入即生效）。
// 未配置的 provider 整组跳过；label 为空的账号跳过。
func (m *omniManager) ImportAccountsRaw(raw json.RawMessage) (imported, skipped int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.st == nil {
		return 0, 0, errors.New("omnigate 未启用")
	}
	rt := m.cur.Load()
	if rt == nil {
		return 0, 0, errors.New("omnigate 未启用")
	}
	var byProvider map[string][]omnistate.DynAccount
	if err := json.Unmarshal(raw, &byProvider); err != nil {
		return 0, 0, fmt.Errorf("解析 OmniGate 账号失败: %w", err)
	}
	known := map[string]bool{}
	for _, name := range rt.gw.ProviderNames() {
		known[name] = true
	}
	for prov, accs := range byProvider {
		if !known[prov] {
			skipped += len(accs)
			continue
		}
		for _, a := range accs {
			if a.Label == "" {
				skipped++
				continue
			}
			m.st.UpsertDynAccount(prov, a)
			imported++
		}
	}
	if imported > 0 {
		newRT, berr := m.build(rt.cfg)
		if berr != nil {
			return imported, skipped, berr
		}
		if old := m.cur.Swap(newRT); old != nil {
			old.sched.Stop()
		}
		log.Printf("[omnigate] 账号导入完成：新增/更新 %d 个，跳过 %d 个（已热重载）", imported, skipped)
	}
	return imported, skipped, nil
}

// ---------------------------------------------------------------------------
// 统一账号池（面板「账号池」视图聚合 WorkBuddy + OmniGate 全部账号）
// ---------------------------------------------------------------------------

// omniProviderAccounts 是归一化后的「一个 provider 一组账号」视图。
// AccountMode/Keys 让面板区分「账号池」与「供应商级密钥直连」两种形态：
// openai 等密钥型 provider 的 accounts 恒为空，但 keys 可能多把轮询。
type omniProviderAccounts struct {
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	AccountMode string           `json:"account_mode,omitempty"`
	Keys        int              `json:"keys,omitempty"`
	Accounts    []map[string]any `json:"accounts"`
}

// Accounts 汇总全部 provider 的账号（归一化），供面板统一账号池展示。
// withBalance=true 时对支持余额的 provider（raccoon）实时查上游；否则仅内存视图
// （快，适合每次概览轮询）。账号名/用户 ID 等元数据取自运行时 DynAccount。
// 分派由网关的 ProviderOps 完成——本层不再按类型 switch。
func (m *omniManager) Accounts(ctx context.Context, withBalance bool) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	names := rt.gw.ProviderNames()
	out := make([]omniProviderAccounts, 0, len(names))
	for _, name := range names {
		typ := rt.gw.ProviderType(name)
		meta := map[string]omnistate.DynAccount{}
		if m.st != nil {
			for _, d := range m.st.DynAccounts(name) {
				meta[d.Label] = d
			}
		}
		mode := ""
		if desc, err := plugin.Get(typ); err == nil {
			mode = string(desc.AccountMode)
		}
		keys := 0
		if rt.cfg != nil {
			if pc := rt.cfg.Provider(name); pc != nil {
				if typ == "opencode" {
					keys = len(pc.ZenKeys) + len(pc.GoKeys)
				} else {
					keys = len(pc.ProviderKeys())
				}
			}
		}
		pa := omniProviderAccounts{
			Name: name, Type: typ, AccountMode: mode, Keys: keys,
			Accounts: []map[string]any{},
		}
		rows, err := rt.gw.Ops(name).ListAccounts(ctx, name, withBalance)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			enrichOmniAccount(r, meta)
		}
		pa.Accounts = rows
		// 限流冷却：把当前冷却截止时刻并入账号行，面板据此显示「冷却」角标。
		for _, r := range pa.Accounts {
			label, _ := r["label"].(string)
			if label == "" {
				continue
			}
			if until, ok := rt.gw.CooldownUntil(name, label); ok {
				r["cooling_until"] = until
			}
			// 会话健康：runable 会话会静默过期，面板据此做「重新粘贴 Cookie」预警。
			if v, e, lastErr, ok := rt.gw.SessionStatus(name, label); ok {
				if !v.IsZero() {
					r["session_verified_at"] = v
				}
				if !e.IsZero() {
					r["session_expired_at"] = e
				}
				if lastErr != "" {
					r["session_error"] = lastErr
				}
			}
			// 快过期积分：窗口内仍有快过期批次的账号，面板显示「快过期 N」角标
			// （与 WorkBuddy 行同口径）。数据来自网关的余额快照缓存——它由
			// 选号路径的 stale-while-revalidate 刷新与余额查询共同填充。
			if exp, earliest, ok := rt.gw.CreditExpiring(name, label); ok && exp > 0 {
				r["credits_expiring"] = exp
				if !earliest.IsZero() {
					r["credits_earliest_expiry"] = earliest.Format(time.RFC3339)
				}
			}
		}
		out = append(out, pa)
	}
	return map[string]any{"providers": out}, nil
}

// enrichOmniAccount 用运行时 DynAccount 元数据补全 name / user_id / org。
func enrichOmniAccount(item map[string]any, meta map[string]omnistate.DynAccount) {
	label, _ := item["label"].(string)
	d, ok := meta[label]
	if !ok {
		return
	}
	if d.Name != "" {
		item["name"] = d.Name
	}
	if d.UserID != "" {
		item["user_id"] = d.UserID
	}
	if d.OrgName != "" {
		item["org_name"] = d.OrgName
	}
}

// Checkin 触发某 provider 单账号（label 为空 = 该 provider 全账号）签到。
// 分派交给网关的 ProviderOps：声明了 checkin 动作的类型才会执行。
func (m *omniManager) Checkin(ctx context.Context, provider, label string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	ops := rt.gw.Ops(provider)
	if !ops.Supports(plugin.ActionCheckin) {
		return nil, fmt.Errorf("provider %q 不支持签到", provider)
	}
	res, err := ops.HandleAction(ctx, provider, plugin.ActionRequest{
		Action: plugin.ActionCheckin,
		Params: map[string]string{"label": label},
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"results": res.Data}, nil
}

// Remove 移除某 provider 的运行时账号（含 tokens）。
func (m *omniManager) Remove(provider, label string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	if label == "" {
		return nil, errors.New("label 不能为空")
	}
	removed := rt.gw.Ops(provider).RemoveAccount(provider, label)
	return map[string]any{"ok": true, "removed": removed}, nil
}

// CheckinAll 对全部支持签到的 provider 触发全账号签到（面板「全部签到」覆盖 OmniGate）。
// 是否支持由插件描述符声明的 checkin 动作决定。
func (m *omniManager) CheckinAll(ctx context.Context) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	out := []map[string]any{}
	for _, name := range rt.gw.ProviderNames() {
		ops := rt.gw.Ops(name)
		if !ops.Supports(plugin.ActionCheckin) {
			continue
		}
		item := map[string]any{"provider": name}
		res, err := ops.HandleAction(ctx, name, plugin.ActionRequest{
			Action: plugin.ActionCheckin,
			Params: map[string]string{"label": ""},
		})
		if err != nil {
			item["error"] = err.Error()
		} else {
			item["results"] = res.Data
		}
		out = append(out, item)
	}
	return map[string]any{"providers": out}, nil
}

// LoginRunable 校验 runable 账号：用粘贴的 session_token Cookie（或邮箱/密码）做一次
// 服务端会话探测，返回账号身份与当前积分。供面板「浏览器登录」使用——runable 只支持
// Google/Facebook 登录，且会话是 httpOnly Cookie（网页读不到），面板无法自动抓取，
// 只能由用户粘贴。仅校验，不落盘；账号由面板随后写入 omnigate.json 并热生效。
func (m *omniManager) LoginRunable(provider, email, password, cookie string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	return rt.gw.RunableVerify(ctx, provider, email, password, cookie)
}

// NewAPIAuthorize 登记一次 New API 浏览器抓取会话（返回 state + 登录页）。
func (m *omniManager) NewAPIAuthorize(provider, label, apiKey string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	return rt.gw.NewAPIAuthorize(provider, label, apiKey)
}

// NewAPICapture 接收浏览器脚本回传的鉴权包并落库账号。
func (m *omniManager) NewAPICapture(stateID string, payload json.RawMessage) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	var p omnigateway.NewAPICapturePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, fmt.Errorf("解析回传数据失败: %w", err)
	}
	// 兼容毫秒级时间戳（不同 New API 版本可能回毫秒）。
	if p.ExpiresAt > 1e12 {
		p.ExpiresAt /= 1000
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return rt.gw.NewAPICapture(ctx, stateID, p)
}

// NewAPICaptureStatus 报告一次抓取 state 是否仍在等待（前端轮询）。
func (m *omniManager) NewAPICaptureStatus(stateID string) (bool, error) {
	rt := m.cur.Load()
	if rt == nil {
		return false, errors.New("omnigate 未启用")
	}
	return rt.gw.NewAPIPending(stateID), nil
}

// Refresh 统一刷新：轮换某 provider / 账号的会话 access_token。
// 是否支持由插件描述符声明的 refresh 动作决定（当前为 newapi）。
func (m *omniManager) Refresh(provider, label string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	ops := rt.gw.Ops(provider)
	if !ops.Supports(plugin.ActionRefresh) {
		return nil, fmt.Errorf("provider %q 不支持统一刷新", provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := ops.HandleAction(ctx, provider, plugin.ActionRequest{
		Action: plugin.ActionRefresh,
		Params: map[string]string{"label": label},
	})
	if err != nil {
		return nil, err
	}
	if label != "" {
		return res.Data, nil
	}
	return map[string]any{"results": res.Data}, nil
}

// NewAPISetKey 更新某 New API 账号的聊天 API Key（sk-）。
func (m *omniManager) NewAPISetKey(provider, label, apiKey string) error {
	rt := m.cur.Load()
	if rt == nil {
		return errors.New("omnigate 未启用")
	}
	if rt.gw.ProviderType(provider) != "newapi" {
		return fmt.Errorf("provider %q 不是 New API", provider)
	}
	return rt.gw.NewAPISetAPIKey(provider, label, apiKey)
}
