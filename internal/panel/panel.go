// Package panel 内嵌式 Web 管理面板：单引擎（OmniGate）账号、供应商、路由、
// 模型、用量与请求记录。前端为无构建的原生 ES Module SPA，经 go:embed 与二进制
// 同体部署。
package panel

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	"github.com/wuxianggujun/omnigate-panel/internal/livecfg"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

// recentOverviewLimit 总览「近期请求」卡片展示的条数（前端也是按 8 条渲染）。
const recentOverviewLimit = 8

// Config 面板依赖（main 装配注入）。
type Config struct {
	APIKey  string // 网关密钥（仅用于展示；面板鉴权只认会话）
	Version string // 面板版本号（展示用）

	// Live 运行期可变配置（网关密钥热更新）。
	Live *livecfg.Holder

	// ConfigPath 配置位置（PostgreSQL panel_config）与读写器（系统设置页）。
	ConfigPath string
	// DataDir 持久化根目录（用于「存储」页展示）。
	DataDir       string
	LoadConfig    func() (any, error)
	SaveConfig    func(raw []byte) (restartRequired []string, err error)
	RegenerateKey func() (string, error)

	// Usage / RequestLog 是共享记录器（可为 nil）。
	Usage      *usage.Recorder
	RequestLog *reqlog.Recorder

	// OwnershipFile 账号归属表文件（多租户）。
	OwnershipFile    string
	OwnershipBackend kv.Backend
	OwnershipKey     string

	// OmniConfigPath 引擎配置位置（PostgreSQL panel_config#omnigate）与热重载读写器。
	OmniConfigPath string
	LoadOmniConfig func() (any, error)
	SaveOmniConfig func(raw []byte) (restartRequired []string, err error)

	// LoadOutbound / SaveOutbound 出站代理配置（config.json 的 outbound 段）。
	LoadOutbound func() (any, error)
	SaveOutbound func(raw []byte) (restartRequired []string, err error)

	// 面板登录鉴权运行时。
	PanelAuth     *panelauth.Store
	LoadPanelAuth func() (PanelAuthSection, error)
	SavePanelAuth func(section PanelAuthSection) (restartRequired []string, err error)

	// ExportOmniAccounts / ImportOmniAccounts 引擎账号迁移。
	ExportOmniAccounts func() (json.RawMessage, error)
	ImportOmniAccounts func(raw json.RawMessage) (imported, skipped int, err error)

	// 引擎运行时闭包（进程内直连，不经过 HTTP）。
	OmniAccounts   func(includeBalance bool) (any, error)
	OmniCheckin    func(provider, label string) (any, error)
	OmniRemove     func(provider, label string) (any, error)
	OmniCheckinAll func() (any, error)
	OmniModels     func(ctx context.Context) (any, error)
	// OmniModelIssues 返回「最近一次目录拉取失败」的供应商（网关 ModelIssues）。
	// 面板据此说明模型为什么缺失或陈旧，而不是让它们无声消失。includeError 为
	// false 时不返回上游错误原文：本端点匿名可读。
	OmniModelIssues func(includeError bool) any
	OmniQuota      func(provider string) (any, error)
	// OmniProbeModels 用一份尚未保存的供应商配置探测上游模型目录（添加供应商）。
	// OmniRefreshModels 绕过 TTL 重新拉取已有供应商的上游目录。
	OmniProbeModels   func(raw json.RawMessage) (any, error)
	OmniRefreshModels func(provider string) (any, error)
	OmniKeyHealth     func() (any, error)
	OmniLogin         func(provider, email, password, cookie string) (any, error)
	OmniAdmin         http.Handler
	// OmniProviderTypes 返回插件注册表的元数据（类型 / 字段 schema / 能力 / 动作）。
	OmniProviderTypes func() any
	// OmniAdminKinds 返回走 /omni/admin/<type>/* 代理的供应商类型（即描述符里
	// login.transport=admin 的类型）。面板据此决定代理放行哪些 kind，新增一个
	// admin 传输的插件只需改 catalog，无需再动面板。
	OmniAdminKinds func() []string

	// New API 浏览器登录抓取与统一刷新。
	OmniNewAPIAuthorize     func(provider, label, apiKey string) (any, error)
	OmniNewAPICapture       func(state string, payload json.RawMessage) (any, error)
	OmniNewAPICaptureStatus func(state string) (bool, error)
	OmniNewAPISetKey        func(provider, label, apiKey string) error
	OmniRefresh             func(provider, label string) (any, error)
}

// Panel 管理面板 handler。挂载方式：外层 mux Handle("/panel/", panel)。
type Panel struct {
	cfg     Config
	mux     *http.ServeMux
	started time.Time
	logs    *Ring

	ownership *ownershipStore
	regLimit  *regLimiter

	newapiClaimsMu sync.Mutex
	newapiClaims   map[string]newapiClaim

	// batchRunning 防止重复点击「全部签到」并发打上游。
	batchRunning atomic.Bool
}

// New 构建面板。
func New(cfg Config) *Panel {
	p := &Panel{
		cfg:       cfg,
		mux:       http.NewServeMux(),
		started:   time.Now(),
		logs:      NewRing(500),
		ownership: newOwnershipStore(cfg.OwnershipFile),
		regLimit:  newRegLimiter(5, time.Hour),
	}
	if err := p.ownership.EnableBackend(cfg.OwnershipBackend, cfg.OwnershipKey); err != nil {
		log.Printf("panel: 归属表启用 PostgreSQL 后端失败，回落文件: %v", err)
	}
	p.routes()
	return p
}

// Logs 返回日志环形缓冲（main 经 MultiWriter 镜像 log 进来）。
func (p *Panel) Logs() *Ring { return p.logs }

func (p *Panel) routes() {
	// SPA 与静态资源。
	p.mux.HandleFunc("GET /panel/{$}", p.index)
	p.mux.HandleFunc("GET /panel/theme.css", p.themeStyle)
	p.mux.HandleFunc("GET /panel/js/", p.assets)

	// 会话与账号。
	p.mux.HandleFunc("GET /panel/api/session", p.sessionState)
	p.mux.HandleFunc("POST /panel/api/session", p.login)
	p.mux.HandleFunc("DELETE /panel/api/session", p.logout)
	p.mux.HandleFunc("POST /panel/api/register", p.register)
	p.mux.HandleFunc("POST /panel/api/password", p.auth(p.changeOwnPassword))
	p.mux.HandleFunc("GET /panel/api/admin/users", p.admin(p.listPanelUsers))
	p.mux.HandleFunc("POST /panel/api/admin/users", p.admin(p.savePanelUser))
	p.mux.HandleFunc("POST /panel/api/admin/users/password", p.admin(p.resetPanelUserPassword))
	p.mux.HandleFunc("POST /panel/api/admin/users/delete", p.admin(p.deletePanelUser))
	p.mux.HandleFunc("POST /panel/api/admin/registration", p.admin(p.setRegistration))
	p.mux.HandleFunc("POST /panel/api/regenerate_key", p.admin(p.regenerateKey))

	// 供应商配置与插件元数据。
	p.mux.HandleFunc("GET /panel/api/omni/config", p.privileged(p.getOmniConfig))
	p.mux.HandleFunc("POST /panel/api/omni/config", p.admin(p.saveOmniConfig))
	// 按用户（api_keys[].name）更新供应商范围；同名多把密钥一起改。
	p.mux.HandleFunc("POST /panel/api/access/scope", p.admin(p.updateAccessScope))
	p.mux.HandleFunc("GET /panel/api/omni/provider-types", p.read(p.providerTypes))
	p.mux.HandleFunc("GET /panel/api/omni/provider-secret", p.admin(p.providerSecret))
	p.mux.HandleFunc("GET /panel/api/omni/provider-quota", p.privileged(p.omniProviderQuota))
	p.mux.HandleFunc("GET /panel/api/omni/key-health", p.privileged(p.omniKeyHealth))
	p.mux.HandleFunc("GET /panel/api/omni/outbound", p.privileged(p.getOutbound))
	p.mux.HandleFunc("POST /panel/api/omni/outbound", p.admin(p.saveOutbound))
	p.mux.HandleFunc("GET /panel/api/provider-types", p.read(p.providerTypes))

	// 账号（admin 全量 / user 仅自己的账号）。
	p.mux.HandleFunc("GET /panel/api/omni/accounts", p.read(p.omniAccounts))
	p.mux.HandleFunc("POST /panel/api/omni/account/checkin", p.tenant(p.omniAccountCheckin))
	p.mux.HandleFunc("POST /panel/api/omni/account/remove", p.tenant(p.omniAccountRemove))
	p.mux.HandleFunc("POST /panel/api/omni/account/add", p.tenant(p.omniAccountAdd))
	p.mux.HandleFunc("POST /panel/api/omni/account/login", p.tenant(p.omniAccountLogin))
	p.mux.HandleFunc("POST /panel/api/omni/account/refresh", p.tenant(p.omniAccountRefresh))
	p.mux.HandleFunc("POST /panel/api/omni/newapi/authorize", p.tenant(p.omniNewAPIAuthorize))
	p.mux.HandleFunc("GET /panel/api/omni/newapi/status", p.tenant(p.omniNewAPICaptureStatus))
	p.mux.HandleFunc("POST /panel/api/omni/newapi/capture", p.omniNewAPICapture)
	p.mux.HandleFunc("OPTIONS /panel/api/omni/newapi/capture", p.omniNewAPICapturePreflight)
	p.mux.HandleFunc("POST /panel/api/omni/newapi/setkey", p.tenant(p.omniNewAPISetKey))
	// 走 /omni/admin/<type>/* 代理的供应商类型由描述符（login.transport=admin）
	// 决定，这里只注册一条前缀路由，具体 kind 由 omniAdminProxy 按元数据放行。
	p.mux.HandleFunc("/panel/api/omni/admin/", p.tenant(p.omniAdminProxy))

	// 模型与路由。
	p.mux.HandleFunc("GET /panel/api/omni/models", p.read(p.omniModels))
	p.mux.HandleFunc("GET /panel/api/models", p.read(p.omniModels))
	p.mux.HandleFunc("POST /panel/api/omni/provider-models/probe", p.admin(p.omniProbeModels))
	p.mux.HandleFunc("POST /panel/api/omni/provider-models/refresh", p.admin(p.omniRefreshModels))
	p.mux.HandleFunc("GET /panel/api/omni/model-mapping", p.privileged(p.omniModelMapProxy))
	p.mux.HandleFunc("POST /panel/api/omni/model-mapping", p.admin(p.omniModelMapProxy))
	p.mux.HandleFunc("DELETE /panel/api/omni/model-mapping/", p.admin(p.omniModelMapProxy))

	// 总览 / 用量 / 请求 / 日志。
	p.mux.HandleFunc("GET /panel/api/overview", p.read(p.overview))
	p.mux.HandleFunc("GET /panel/api/logs", p.read(p.logsHandler))
	p.mux.HandleFunc("GET /panel/api/request_metrics", p.read(p.requestMetrics))
	p.mux.HandleFunc("GET /panel/api/request_logs", p.read(p.requestLogs))
	p.mux.HandleFunc("GET /panel/api/usage", p.read(p.usage))
	// 按入站密钥聚合。只给 privileged：归属表里的租户维度是「上游账号」，
	// 没有到入站密钥的映射，放给普通租户等于让他们看到别人的用量。
	p.mux.HandleFunc("GET /panel/api/usage/by-key", p.privileged(p.usageByKey))
	p.mux.HandleFunc("POST /panel/api/usage/save", p.admin(p.usageSave))

	// 批量与迁移。
	p.mux.HandleFunc("POST /panel/api/checkin_all", p.tenant(p.checkinAll))
	p.mux.HandleFunc("GET /panel/api/accounts/export", p.admin(p.exportAccounts))
	p.mux.HandleFunc("POST /panel/api/accounts/import", p.admin(p.importAccounts))

	// 系统配置。
	p.mux.HandleFunc("GET /panel/api/config", p.privileged(p.getConfig))
	p.mux.HandleFunc("POST /panel/api/config", p.admin(p.saveConfig))
}

// ServeHTTP 统一入口：先写安全响应头再分发。
func (p *Panel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	p.mux.ServeHTTP(w, r)
}

// apiKey 当前生效密钥（Live 优先，回落静态字段）。
func (p *Panel) apiKey() string {
	if p.cfg.Live != nil {
		return p.cfg.Live.Load().APIKey
	}
	return p.cfg.APIKey
}

// providerTypes 返回插件注册表的 UI 元数据。
func (p *Panel) providerTypes(w http.ResponseWriter, r *http.Request) {
	types := any([]any{})
	if p.cfg.OmniProviderTypes != nil {
		types = p.cfg.OmniProviderTypes()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "types": types})
}

// ---------------------------------------------------------------------------
// 总览
// ---------------------------------------------------------------------------

type omniAccountRow struct {
	Label          string `json:"label"`
	HasToken       *bool  `json:"has_token,omitempty"`
	AuthInvalid    bool   `json:"auth_invalid,omitempty"`
	CoolingUntil   any    `json:"cooling_until,omitempty"`
	SessionError   string `json:"session_error,omitempty"`
	SessionExpired any    `json:"session_expired_at,omitempty"`
	Credits        any    `json:"credits,omitempty"`
}

type omniProviderRow struct {
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	AccountMode string           `json:"account_mode,omitempty"`
	Keys        int              `json:"keys,omitempty"`
	Accounts    []omniAccountRow `json:"accounts"`
}

func (p *Panel) overview(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{
		"version":       p.cfg.Version,
		"uptime_sec":    int(time.Since(p.started).Seconds()),
		"build":         panelBuild,
		"auth_required": p.apiKey() != "",
	}
	// 近期请求：直接取请求归档的最近 8 条（多租户下按归属过滤，与 /panel/api/request_logs
	// 同一可见性口径）。此前是单引擎重构遗留的 `[]any{}` 桩，导致总览「近期请求」永远为空。
	// 趋势序列不在这里返回：前端已回落到 /panel/api/usage 的 series（同一次加载里就有）。
	// 放在 OmniAccounts 早退之前，保证没装配账号回调时（如精简部署）也照常返回。
	if p.cfg.RequestLog != nil {
		rows, err := p.cfg.RequestLog.ReadArchive(recentOverviewLimit, reqlog.Filter{})
		if err == nil {
			if visible, all := p.reqlogVisible(r); !all {
				filtered := make([]reqlog.Event, 0, len(rows))
				for _, e := range rows {
					if visible(e) {
						filtered = append(filtered, e)
					}
				}
				rows = filtered
			}
			if rows == nil {
				rows = []reqlog.Event{}
			}
			body["recent"] = rows
		}
	}
	// 密钥锁/冷却：只下发给能看到平台配置的角色。
	//
	// 之前总览的「模型锁与冷却」卡片从不拿后端数据，永远渲染「没有模型锁」——
	// 而线上 atria 两把密钥正因额度耗尽被自动禁用。那张卡片因此是纯粹的假阴性：
	// 越需要它的时候越说没事。数据与 /panel/api/omni/key-health 同源，但这里只
	// 提炼成「哪家供应商、几个密钥、因为什么、何时恢复」，不外泄掩码与错误原文。
	// 匿名与普通用户拿不到平台级状态，前端据此显示「不可见」而不是「没有」。
	// 放在 OmniAccounts 早退之前：精简部署同样要能报警。
	if role := roleOf(r); role.IsAdmin() || role == panelauth.RoleViewer {
		if keys := p.lockedUpstreamKeys(); keys != nil {
			body["model_locks"] = keys
		}
	}
	if p.cfg.OmniAccounts == nil {
		writeJSON(w, http.StatusOK, body)
		return
	}
	d, err := p.cfg.OmniAccounts(false)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list accounts: "+err.Error())
		return
	}
	providers := decodeOmniProviders(d)
	owned, all := p.ownFilter(r)
	anon := !authed(r)

	total, healthy, cooling, failed := 0, 0, 0, 0
	rows := make([]map[string]any, 0, len(providers))
	for _, prov := range providers {
		ph, pc, pf := 0, 0, 0
		for _, a := range prov.Accounts {
			if !all && !owned[omniOwnerKey(prov.Name, a.Label)] {
				continue
			}
			total++
			if a.AuthInvalid || a.SessionError != "" {
				failed++
				pf++
				continue
			}
			if isFutureTime(a.CoolingUntil) {
				cooling++
				pc++
				continue
			}
			healthy++
			ph++
		}
		if !anon {
			rows = append(rows, map[string]any{
				"name": prov.Name, "type": prov.Type,
				"account_mode": prov.AccountMode, "keys": prov.Keys,
				"accounts": len(prov.Accounts), "healthy": ph,
				"cooling": pc, "failed": pf,
			})
		}
	}
	body["totals"] = map[string]any{
		"accounts": total, "healthy": healthy,
		"cooling": cooling, "failed": failed,
	}
	if !anon {
		body["providers"] = rows
	}
	if cfg, err := p.loadOmniConfigMap(); err == nil {
		if v, ok := cfg["default_provider"].(string); ok {
			body["default_provider"] = v
		}
		if v, ok := cfg["default_model"].(string); ok {
			body["default_model"] = v
		}
	}
	storage := map[string]any{
		"data_dir":    p.cfg.DataDir,
		"config_path": p.cfg.ConfigPath,
	}
	if p.cfg.RequestLog != nil {
		a := p.cfg.RequestLog.Snapshot().Archive
		storage["request_archive_files"] = a.Files
		storage["request_archive_bytes"] = a.Bytes
		storage["dropped_writes"] = a.DroppedWrites
		if a.Dir != "" {
			storage["request_archive_dir"] = a.Dir
		}
	}
	if p.cfg.DataDir != "" {
		if fi, err := os.Stat(filepath.Join(p.cfg.DataDir, "usage.json")); err == nil {
			storage["usage_file_bytes"] = fi.Size()
		}
	}
	body["storage"] = storage
	writeJSON(w, http.StatusOK, body)
}

func decodeOmniProviders(d any) []omniProviderRow {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	var payload struct {
		Providers []omniProviderRow `json:"providers"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	return payload.Providers
}

func (p *Panel) loadOmniConfigMap() (map[string]any, error) {
	if p.cfg.LoadOmniConfig == nil {
		return nil, io.EOF
	}
	v, err := p.cfg.LoadOmniConfig()
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func isFutureTime(v any) bool {
	switch t := v.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339, t)
		return err == nil && parsed.After(time.Now())
	case float64:
		return int64(t) > time.Now().Unix()
	case int64:
		return t > time.Now().Unix()
	case json.Number:
		n, err := t.Int64()
		return err == nil && n > time.Now().Unix()
	}
	return false
}

// ---------------------------------------------------------------------------
// 日志 / 请求
// ---------------------------------------------------------------------------

func (p *Panel) logsHandler(w http.ResponseWriter, r *http.Request) {
	entries := p.logs.Snapshot()
	if visible, all := p.logLineVisible(r); !all {
		filtered := make([]LogEntry, 0, len(entries))
		for _, e := range entries {
			if visible(e.Text) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (p *Panel) requestMetrics(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	snap := p.cfg.RequestLog.Snapshot()
	if visible, all := p.reqlogVisible(r); !all {
		recent := make([]reqlog.Event, 0, len(snap.Recent))
		for _, e := range snap.Recent {
			if visible(e) {
				recent = append(recent, e)
			}
		}
		snap.Recent = recent
	}
	writeJSON(w, http.StatusOK, snap)
}

func (p *Panel) requestLogs(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "request logger not available")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 1000 {
		limit = 1000
	}
	q := r.URL.Query()
	rows, err := p.cfg.RequestLog.ReadArchive(limit, reqlog.Filter{
		Outcome:   q.Get("outcome"),
		Account:   q.Get("account"),
		Model:     q.Get("model"),
		Provider:  q.Get("provider"),
		ClientIP:  q.Get("client_ip"),
		UserAgent: q.Get("user_agent"),
		From:      parseTimeParam(q.Get("from")),
		To:        parseTimeParam(q.Get("to")),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if visible, all := p.reqlogVisible(r); !all {
		filtered := make([]reqlog.Event, 0, len(rows))
		for _, e := range rows {
			if visible(e) {
				filtered = append(filtered, e)
			}
		}
		rows = filtered
	}
	if rows == nil {
		rows = []reqlog.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows, "limit": limit})
}

// ---------------------------------------------------------------------------
// 用量
// ---------------------------------------------------------------------------

func (p *Panel) usage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	q := r.URL.Query()
	win := usage.Window{}
	win.From = parseTimeParam(q.Get("from"))
	win.To = parseTimeParam(q.Get("to"))
	if win.From.IsZero() && win.To.IsZero() {
		win.Hours = 72
		if v := q.Get("hours"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n >= 0 {
				win.Hours = n
			}
		}
		if win.Hours > 1440 {
			win.Hours = 1440
		}
	}
	// 普通用户只看自己账号的用量。用量桶只带账号 label，不带 provider：
	// 用归属键的 label 部分匹配（同 label 跨供应商时略宽，但不会泄露别人的明细）。
	owned, all := p.ownFilter(r)
	var allowUID func(uid string) bool
	if !all {
		labels := map[string]bool{}
		for k := range owned {
			if _, label, ok := strings.Cut(k, "/"); ok {
				labels[label] = true
			}
		}
		allowUID = func(uid string) bool { return labels[uid] }
	}
	writeJSON(w, http.StatusOK, p.cfg.Usage.SnapshotWindowFiltered(win, nil, nil, allowUID))
}

func (p *Panel) usageSave(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Usage == nil {
		writeErr(w, http.StatusNotImplemented, "usage recorder not available")
		return
	}
	p.cfg.Usage.Save()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseTimeParam(v string) time.Time {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		if n <= 0 {
			return time.Time{}
		}
		if n > 1e12 {
			return time.UnixMilli(n)
		}
		return time.Unix(n, 0)
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t
	}
	if t, err := time.ParseInLocation("2006-01-02T15:04", v, time.Local); err == nil {
		return t
	}
	return time.Time{}
}

// ---------------------------------------------------------------------------
// 批量 / 迁移
// ---------------------------------------------------------------------------

func safeGo(name string, fn func()) {
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panel: %s panic: %v", name, rec)
			}
		}()
		fn()
	}()
}

func (p *Panel) checkinAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniCheckinAll == nil && p.cfg.OmniCheckin == nil {
		writeErr(w, http.StatusNotImplemented, "checkin not available")
		return
	}
	if !p.batchRunning.CompareAndSwap(false, true) {
		writeErr(w, http.StatusTooManyRequests, "batch_in_progress")
		return
	}
	if roleOf(r).IsAdmin() {
		if p.cfg.OmniCheckinAll == nil {
			p.batchRunning.Store(false)
			writeErr(w, http.StatusNotImplemented, "checkin not available")
			return
		}
		safeGo("全量签到", func() {
			defer p.batchRunning.Store(false)
			if _, err := p.cfg.OmniCheckinAll(); err != nil {
				log.Printf("panel: 全量签到失败: %v", err)
				return
			}
			log.Printf("panel: 全量签到完成")
		})
	} else {
		accts := p.ownedOmniAccounts(r)
		safeGo("个人签到", func() {
			defer p.batchRunning.Store(false)
			for _, ac := range accts {
				if _, err := p.cfg.OmniCheckin(ac[0], ac[1]); err != nil {
					log.Printf("panel: 签到 %s/%s 失败: %v", ac[0], ac[1], err)
				}
			}
			log.Printf("panel: 个人签到完成（%d 个账号）", len(accts))
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "started": true})
}

func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ExportOmniAccounts == nil {
		writeErr(w, http.StatusNotImplemented, "export not available")
		return
	}
	raw, err := p.cfg.ExportOmniAccounts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=omnigate-accounts.json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (p *Panel) importAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ImportOmniAccounts == nil {
		writeErr(w, http.StatusNotImplemented, "import not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	imported, skipped, err := p.cfg.ImportOmniAccounts(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "imported": imported, "skipped": skipped})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}
