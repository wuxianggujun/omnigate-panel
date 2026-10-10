// omnigate.go 内置 OmniGate 引擎（Runable / 浣熊 / 任意 OpenAI 兼容上游）的
// 进程内装配与热重载。
//
// 设计：把「配置 → 网关 → 调度器 → HTTP handler」打包成一个不可变运行时
// （omniRuntime），用 atomic.Pointer 挂载。供应商配置页保存时，重建一份新的
// 运行时并原子替换旧运行时（旧调度器停掉），因此改供应商无需重启进程。
//
// 与 WorkBuddy 网关完全隔离：独立配置文件（omnigate.json）、独立数据目录
// （缺省 data/omnigate）、挂载在本服务的 /omni/ 前缀下。
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
	"sync"
	"sync/atomic"
	"time"

	omniconfig "github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	omnigateway "github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	omnilogx "github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	omnisched "github.com/wuxianggujun/omnigate-panel/internal/omnigate/scheduler"
	omnisrv "github.com/wuxianggujun/omnigate-panel/internal/omnigate/server"
	omnistate "github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
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
	apiKey  string
	logger  *omnilogx.Logger

	mu  sync.Mutex // 串行化重载
	st  *omnistate.State
	cur atomic.Pointer[omniRuntime]

	// outbound 面板出站代理路由（config.json 的 outbound 段）。用于把
	// "omnigate:<provider>" 解析成代理 URL，在 build 时注入各供应商。
	outbound outbound.Config
}

// newOmniManager 按 cfg.OmnigateConfig 装配 OmniGate。配置文件缺失/解析失败时
// 返回未启用的 manager（不报错，面板其余功能照常）。
func newOmniManager(cfg *Config) *omniManager {
	m := &omniManager{path: cfg.OmnigateConfig, apiKey: cfg.APIKey, logger: omnilogx.New(300), outbound: cfg.Outbound}
	if m.path == "" {
		return m
	}
	if _, err := os.Stat(m.path); err != nil {
		log.Printf("[omnigate] 未启用：配置文件 %s 不存在（如需启用，参考 omnigate.example.json）", m.path)
		return m
	}
	ocfg, err := omniconfig.Load(m.path)
	if err != nil {
		log.Printf("WARN: [omnigate] 加载 %s 失败，已跳过：%v", m.path, err)
		return m
	}
	// 数据目录隔离：避免与 WorkBuddy 网关的 data/state.json 互相覆盖。
	if ocfg.DataDir == "" || ocfg.DataDir == "data" {
		ocfg.DataDir = filepath.Join("data", "omnigate")
	}
	st, err := omnistate.Load(ocfg.DataDir)
	if err != nil {
		log.Printf("WARN: [omnigate] 加载状态失败，已跳过：%v", err)
		return m
	}
	m.st = st
	rt, err := m.build(ocfg)
	if err != nil {
		log.Printf("WARN: [omnigate] 初始化网关失败，已跳过：%v", err)
		return m
	}
	m.cur.Store(rt)
	m.enabled = true
	log.Printf("[omnigate] 已启用：默认 provider=%s，provider 数=%d，数据目录=%s，挂载于 /omni/",
		ocfg.DefaultProvider, len(ocfg.Providers), ocfg.DataDir)
	m.logProviders(rt)
	return m
}

// build 由配置构建一份新运行时（不动旧运行时）。
func (m *omniManager) build(cfg *omniconfig.Config) (*omniRuntime, error) {
	// 面板 api_key 复用为 OmniGate 的 API key（面板前端同一把钥匙即可调用 /omni/*）。
	if len(cfg.APIKeys) == 0 && m.apiKey != "" {
		cfg.APIKeys = []string{m.apiKey}
	}
	gw, err := omnigateway.New(cfg, m.st, m.logger, omnigateway.WithProxyResolver(m.proxyFor))
	if err != nil {
		return nil, err
	}
	sched := omnisched.New(cfg, gw, m.logger)
	sched.Start()
	srv := omnisrv.New(cfg, gw, m.logger)
	return &omniRuntime{cfg: cfg, gw: gw, sched: sched, srv: srv}, nil
}

// proxyFor 把某供应商解析成出站代理（来自面板 outbound 路由的
// "omnigate:<provider>"）。未配置返回 nil（直连）。
func (m *omniManager) proxyFor(providerName string) *url.URL {
	return m.outbound.For(outbound.OmniTarget(providerName))
}

// SetAPIKey 更新 OmniGate 复用的网关 api_key 并热重载（面板「重新生成」密钥后
// 调用，使 /omni/* 与网关 api_key 保持同一把）。未启用时仅记住新值，供后续
// build 回填。返回错误不致命——调用方记日志，重启后自然对齐。
func (m *omniManager) SetAPIKey(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiKey = key
	if m.st == nil {
		return nil
	}
	rt := m.cur.Load()
	if rt == nil {
		return nil
	}
	// 深拷贝当前配置并清空 api_keys：build 会用新的 m.apiKey 回填（见 build）。
	raw, err := json.Marshal(rt.cfg)
	if err != nil {
		return err
	}
	var cfg omniconfig.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	cfg.APIKeys = nil
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
	old := m.cur.Swap(newRT)
	if old != nil {
		old.sched.Stop()
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
// 未启用时返回 nil（外层 mountRoot 直接透传给 WorkBuddy handler）。
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

// Stop 停掉当前调度器并落盘状态（进程退出时调用）。
func (m *omniManager) Stop() {
	if !m.enabled {
		return
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
	if m.st == nil {
		return nil, errors.New("omnigate 未启用")
	}
	var cfg omniconfig.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置失败: %w", err)
	}
	// 保留服务端/运行时字段：面板不编辑 server/data_dir/api_keys。
	if rt := m.cur.Load(); rt != nil {
		cfg.Server = rt.cfg.Server
		cfg.DataDir = rt.cfg.DataDir
		cfg.APIKeys = rt.cfg.APIKeys
	}
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	pretty, err := json.MarshalIndent(&cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(m.path, pretty, 0o644); err != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", m.path, err)
	}
	rt, err := m.build(&cfg)
	if err != nil {
		return nil, err
	}
	old := m.cur.Swap(rt)
	if old != nil {
		old.sched.Stop()
	}
	log.Printf("[omnigate] 供应商配置已热重载：provider 数=%d，默认 provider=%s", len(cfg.Providers), cfg.DefaultProvider)
	m.logProviders(rt)
	return nil, nil
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
type omniProviderAccounts struct {
	Name     string           `json:"name"`
	Type     string           `json:"type"`
	Accounts []map[string]any `json:"accounts"`
}

// Accounts 汇总全部 provider 的账号（归一化），供面板统一账号池展示。
// withBalance=true 时对支持余额的 provider（raccoon）实时查上游；否则仅内存视图
// （快，适合每次概览轮询）。账号名/用户 ID 等元数据取自运行时 DynAccount。
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
		pa := omniProviderAccounts{Name: name, Type: typ, Accounts: []map[string]any{}}
		switch {
		case withBalance && typ == "raccoon":
			rows, err := rt.gw.RaccoonAccounts(ctx, name)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				enrichOmniAccount(r, meta)
			}
			pa.Accounts = rows
		case withBalance && typ == "runable":
			// runable 是积分（credits）制：withBalance 时实时查上游余额，
			// 与 raccoon 对齐——账号池的「积分」列由此填充。
			rows, err := rt.gw.RunableAccounts(ctx, name)
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				enrichOmniAccount(r, meta)
			}
			pa.Accounts = rows
		default:
			for _, a := range rt.gw.Accounts(name) {
				item := map[string]any{"label": a.Label, "has_token": a.Cookie != ""}
				enrichOmniAccount(item, meta)
				pa.Accounts = append(pa.Accounts, item)
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
// 按 provider 类型分派：raccoon=桌面登录奖励，runable=会话验活 + 积分刷新。
func (m *omniManager) Checkin(ctx context.Context, provider, label string) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	switch rt.gw.ProviderType(provider) {
	case "raccoon":
		res, err := rt.gw.RaccoonCheckin(ctx, provider, label)
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": res}, nil
	case "runable":
		res, err := rt.gw.RunableCheckin(ctx, provider, label)
		if err != nil {
			return nil, err
		}
		return map[string]any{"results": res}, nil
	default:
		return nil, fmt.Errorf("provider %q 不支持签到", provider)
	}
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
	removed := rt.gw.RaccoonRemoveAccount(provider, label)
	return map[string]any{"ok": true, "removed": removed}, nil
}

// CheckinAll 对全部支持签到的 provider 触发全账号签到（面板「全部签到」覆盖 OmniGate）。
// raccoon 走桌面登录奖励，runable 走会话验活 + 积分刷新。
func (m *omniManager) CheckinAll(ctx context.Context) (any, error) {
	rt := m.cur.Load()
	if rt == nil {
		return nil, errors.New("omnigate 未启用")
	}
	out := []map[string]any{}
	for _, name := range rt.gw.ProviderNames() {
		item := map[string]any{"provider": name}
		switch rt.gw.ProviderType(name) {
		case "raccoon":
			if res, err := rt.gw.RaccoonCheckin(ctx, name, ""); err != nil {
				item["error"] = err.Error()
			} else {
				item["results"] = res
			}
		case "runable":
			if res, err := rt.gw.RunableCheckin(ctx, name, ""); err != nil {
				item["error"] = err.Error()
			} else {
				item["results"] = res
			}
		default:
			continue
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
