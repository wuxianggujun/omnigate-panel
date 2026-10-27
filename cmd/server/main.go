// main.go omnigate-panel 入口：单引擎（OmniGate）+ 内嵌面板。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/kv"
	"github.com/wuxianggujun/omnigate-panel/internal/livecfg"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin/catalog"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
	"github.com/wuxianggujun/omnigate-panel/internal/panel"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pgstore"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

const appVersion = "2.0.0-refactor"

func main() {
	cfgPath := flag.String("config", "config.json", "bootstrap 配置路径（只含 PostgreSQL DSN）")
	setAdminPW := flag.Bool("set-admin-password", false, "交互设置/重置面板账号密码后退出")
	adminUser := flag.String("admin-user", "admin", "配合 -set-admin-password：目标账号名")
	adminRole := flag.String("admin-role", "admin", "配合 -set-admin-password：角色（admin/viewer）")
	flag.Parse()

	// 插件注册表必须在 Load 之前就绪：配置校验按注册表判断 provider 类型，
	// 新增插件只要在启动早期 Register 即可直接写进配置。
	catalog.Register()

	// config.json 现在只是 bootstrap：必须提供 PostgreSQL DSN。其余配置全部
	// 从数据库读取；数据库不可用时直接启动失败，不再回落到本地 JSON。
	boot, err := Load(*cfgPath)
	if err != nil && errors.Is(err, fs.ErrNotExist) && os.Getenv("OMNIGATE_STORAGE_DSN") != "" {
		boot, err = Load("")
	}
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			log.Fatalf("缺少 bootstrap 配置 %s：请写入 {\"storage\":{\"driver\":\"postgres\",\"dsn\":\"...\"}}，或设置 OMNIGATE_STORAGE_DSN", *cfgPath)
		}
		log.Fatalf("load bootstrap config: %v", err)
	}
	if !boot.Storage.UsePostgres() {
		log.Fatalf("配置已迁移到 PostgreSQL：请在 %s 设置 storage.driver=postgres 与 storage.dsn（或设置 OMNIGATE_STORAGE_DSN）", *cfgPath)
	}

	pg, err := pgstore.Open(context.Background(), boot.Storage.DSN)
	if err != nil {
		log.Fatalf("PostgreSQL 连接失败（配置存储依赖数据库，无法启动）: %v", err)
	}
	defer pg.Close()
	log.Printf("[storage] 配置与状态后端 = postgres (%s)", pg.Label())

	store, err := loadConfigStore(pg, boot, *cfgPath)
	if err != nil {
		log.Fatalf("加载 PostgreSQL 配置失败: %v", err)
	}
	cfg := store.Main()
	// 面板账号：panel_users 表是唯一权威。首次从旧配置迁移时导入一次；必须在
	// -set-admin-password 之前完成，否则命令行设密会把旧账号覆盖掉。
	panelUsers, err := ensurePanelUsers(pg, cfg.PanelAuth.Users)
	if err != nil {
		log.Fatalf("%v", err)
	}
	cfg.PanelAuth.Users = panelUsers
	store.SetUsers(panelUsers)
	if *setAdminPW {
		if err := runSetAdminPassword(pg, *adminUser, *adminRole); err != nil {
			log.Fatalf("set-admin-password: %v", err)
		}
		return
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

	// 请求记录。
	reqLogDir := filepath.Join(cfg.DataDir, "request-logs")
	var reqSink reqlog.Sink
	if pg != nil {
		reqSink = pg
		if cfg.Logging.RequestArchiveEnabled {
			if rows, _, _ := pg.LogStats(); rows == 0 {
				if n, err := pg.ImportRequestLogsFromDir(reqLogDir); err != nil {
					log.Printf("WARN: [storage] 请求记录导入 PostgreSQL 失败: %v", err)
				} else if n > 0 {
					log.Printf("[storage] 已把 %d 条请求记录导入 PostgreSQL", n)
				}
			}
		}
	}
	requestLog := reqlog.New(reqlog.Config{
		Dir:           reqLogDir,
		Enabled:       cfg.Logging.RequestArchiveEnabled,
		RetentionDays: cfg.Logging.RequestRetentionDays,
		MaxBytes:      int64(cfg.Logging.RequestArchiveMaxMB) << 20,
		Sink:          reqSink,
	})
	defer requestLog.Close()

	// 用量记录。
	rec := usage.New(filepath.Join(cfg.DataDir, "usage.json"))
	if pg != nil {
		if err := rec.EnableBackend(pg, "usage"); err != nil {
			log.Printf("WARN: [storage] 用量启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	rec.Start()
	defer rec.Stop()

	live := livecfg.New(livecfg.Snapshot{
		APIKey:           cfg.APIKey,
		RecordClientInfo: cfg.Logging.RequestClientInfo,
	})

	obRT := outbound.NewRuntime(cfg.Outbound, func(f string, a ...any) {
		log.Printf("[outbound] "+f, a...)
	})
	defer obRT.Close()

	// 插件注册表在 main 入口已显式注册；这里不再重复。
	omni := newOmniManager(cfg, cfg.DataDir, store.Omni(), requestLog, rec, func() bool {
		return live.Load().RecordClientInfo
	}, obRT, store, kvBackend(pg))
	if omni.Enabled() {
		defer omni.Stop()
	}

	authStore := panelauth.New(panelUsers, cfg.PanelAuth.SessionTTL(),
		cfg.PanelAuth.MaxFail(), cfg.PanelAuth.LockDuration())
	if err := authStore.EnableSessionBackend(kvBackend(pg), "panel_sessions",
		filepath.Join(cfg.DataDir, "panel_sessions.json")); err != nil {
		log.Printf("panel: 会话持久化未启用：%v", err)
	}

	pcfg := panel.Config{
		APIKey:       cfg.APIKey,
		Version:      appVersion,
		Live:         live,
		ConfigPath:   configStoreRef,
		DataDir:      cfg.DataDir,
		LoadConfig:   func() (any, error) { return store.Main(), nil },
		SaveConfig:   func(raw []byte) ([]string, error) { return saveConfig(raw, store, live, obRT, omni) },
		LoadOutbound: func() (any, error) { return store.Main().Outbound, nil },
		SaveOutbound: func(raw []byte) ([]string, error) {
			var oc outbound.Config
			if err := json.Unmarshal(raw, &oc); err != nil {
				return nil, fmt.Errorf("解析出站代理配置失败: %w", err)
			}
			if err := oc.Normalize(); err != nil {
				return nil, err
			}
			wrapped, err := json.Marshal(map[string]any{"outbound": oc})
			if err != nil {
				return nil, err
			}
			return saveConfig(wrapped, store, live, obRT, omni)
		},
		Usage:            rec,
		RequestLog:       requestLog,
		OwnershipFile:    filepath.Join(cfg.DataDir, "ownership.json"),
		OwnershipBackend: kvBackend(pg),
		OwnershipKey:     "ownership",
		PanelAuth:        authStore,
		LoadPanelAuth: func() (panel.PanelAuthSection, error) {
			c := store.Main()
			users := c.PanelAuth.Users
			if pg != nil {
				got, have, err := pg.LoadPanelUsers(context.Background())
				if err != nil {
					return panel.PanelAuthSection{}, fmt.Errorf("读取 PostgreSQL 面板账号失败: %w", err)
				}
				if have {
					users = got
				}
			}
			return panel.PanelAuthSection{
				SessionHours:      c.PanelAuth.SessionHours,
				MaxFailures:       c.PanelAuth.MaxFailures,
				LockMinutes:       c.PanelAuth.LockMinutes,
				AllowRegistration: c.PanelAuth.AllowRegistration,
				Users:             users,
			}, nil
		},
		SavePanelAuth: func(section panel.PanelAuthSection) ([]string, error) {
			if err := panelauth.ValidateUsers(section.Users); err != nil {
				return nil, err
			}
			if pg != nil {
				if err := pg.SavePanelUsers(context.Background(), section.Users); err != nil {
					return nil, fmt.Errorf("保存面板账号到 PostgreSQL 失败: %w", err)
				}
			}
			wrapped, err := json.Marshal(map[string]any{"panel_auth": section})
			if err != nil {
				return nil, err
			}
			if _, err := saveConfig(wrapped, store, live, obRT, omni); err != nil {
				return nil, err
			}
			ac := PanelAuthConfig{
				SessionHours: section.SessionHours,
				MaxFailures:  section.MaxFailures,
				LockMinutes:  section.LockMinutes,
			}
			authStore.Reconfigure(section.Users, ac.SessionTTL(),
				ac.MaxFail(), ac.LockDuration())
			store.SetUsers(section.Users)
			return nil, nil
		},
		RegenerateKey: func() (string, error) {
			key, err := GenerateAPIKey()
			if err != nil {
				return "", err
			}
			wrapped, err := json.Marshal(map[string]any{"api_key": key})
			if err != nil {
				return "", err
			}
			if _, err := saveConfig(wrapped, store, live, obRT, omni); err != nil {
				return "", err
			}
			if err := omni.SetAPIKey(key); err != nil {
				log.Printf("WARN: [omnigate] 轮换 api_key 后热重载失败（重启后生效）: %v", err)
			}
			return key, nil
		},
	}
	if omni.Enabled() {
		pcfg.OmniConfigPath = configStoreRef + "#omnigate"
		pcfg.LoadOmniConfig = omni.Load
		pcfg.SaveOmniConfig = omni.Save
		pcfg.ExportOmniAccounts = omni.ExportAccountsRaw
		pcfg.ImportOmniAccounts = omni.ImportAccountsRaw
		pcfg.OmniAccounts = func(withBalance bool) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return omni.Accounts(ctx, withBalance)
		}
		pcfg.OmniCheckin = func(provider, label string) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			return omni.Checkin(ctx, provider, label)
		}
		pcfg.OmniRemove = omni.Remove
		pcfg.OmniCheckinAll = func() (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			return omni.CheckinAll(ctx)
		}
		pcfg.OmniModels = omni.Models
		pcfg.OmniModelIssues = omni.ModelIssues
		pcfg.OmniProbeModels = omni.ProbeModels
		pcfg.OmniRefreshModels = omni.RefreshModels
		pcfg.OmniQuota = func(provider string) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			return omni.Quota(ctx, provider)
		}
		pcfg.OmniKeyHealth = omni.KeyHealthReport
		pcfg.OmniLogin = omni.LoginRunable
		pcfg.OmniNewAPIAuthorize = omni.NewAPIAuthorize
		pcfg.OmniNewAPICapture = omni.NewAPICapture
		pcfg.OmniNewAPICaptureStatus = omni.NewAPICaptureStatus
		pcfg.OmniNewAPISetKey = omni.NewAPISetKey
		pcfg.OmniRefresh = omni.Refresh
		pcfg.OmniAdmin = omni.Handler()
		pcfg.OmniProviderTypes = func() any { return catalog.All() }
		// admin 代理放行的供应商类型由描述符派生：新增一个 login.transport=admin
		// 的插件后，面板代理自动放行，无需再改面板路由或白名单。
		pcfg.OmniAdminKinds = func() []string {
			return plugin.TypesByLoginTransport(plugin.LoginTransportAdmin)
		}
	}
	pn := panel.New(pcfg)
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))

	root := http.NewServeMux()
	root.Handle("/panel/", pn)
	if omni.Enabled() {
		root.Handle("/", omni.Handler())
	} else {
		root.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/healthz" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"degraded","reason":"engine not configured"}`)
				return
			}
			http.Error(w, "omnigate engine not configured", http.StatusServiceUnavailable)
		})
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           root,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       cfg.ServerReadTimeoutDur,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("omnigate-panel listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/",
		cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// kvBackend 避免 *pgstore.Store 的 typed-nil 被装进非 nil 接口：未配置 PostgreSQL
// 时必须传真正的 nil，否则各存储层会尝试访问空后端并打出无意义的回退告警。
func kvBackend(pg *pgstore.Store) kv.Backend {
	if pg == nil {
		return nil
	}
	return pg
}

// ensurePanelUsers 读取权威账号表；表为空时把迁移带入的旧账号导入一次。
func ensurePanelUsers(pg *pgstore.Store, fallback []panelauth.User) ([]panelauth.User, error) {
	if pg == nil {
		return nil, errors.New("PostgreSQL 未连接，无法读取面板账号")
	}
	users, have, err := pg.LoadPanelUsers(context.Background())
	if err != nil {
		return nil, fmt.Errorf("读取 PostgreSQL 面板账号失败: %w", err)
	}
	if have {
		log.Printf("[storage] 已从 PostgreSQL 载入 %d 个面板账号", len(users))
		return users, nil
	}
	if len(fallback) > 0 {
		if err := pg.SavePanelUsers(context.Background(), fallback); err != nil {
			return nil, fmt.Errorf("首次导入面板账号失败: %w", err)
		}
		log.Printf("[storage] 已把 %d 个面板账号导入 PostgreSQL", len(fallback))
	}
	return fallback, nil
}

// saveConfig 面板保存配置：校验 → 写 PostgreSQL → 热应用 → 返回需重启字段列表。
func saveConfig(raw []byte, store *configStore, live *livecfg.Holder, obRT *outbound.Runtime, omni *omniManager) ([]string, error) {
	if store == nil {
		return nil, errors.New("配置存储未初始化")
	}
	curCfg := store.Main()
	oldRaw, err := json.Marshal(curCfg)
	if err != nil {
		return nil, fmt.Errorf("encode current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	// storage 是 bootstrap 字段：DSN 只能在 config.json / OMNIGATE_STORAGE_DSN
	// 里改，写进数据库等于把“连接数据库的凭据”放进它自己连不上的地方。
	if v, ok := incoming["storage"]; ok {
		b, _ := json.Marshal(v)
		var submitted StorageConfig
		if err := json.Unmarshal(b, &submitted); err != nil {
			return nil, fmt.Errorf("parse storage: %w", err)
		}
		boot := store.BootstrapStorage()
		if submitted.Driver != boot.Driver || submitted.DSN != boot.DSN {
			return nil, errors.New("storage 是启动引导配置，不能通过面板修改；请编辑 config.json 或 OMNIGATE_STORAGE_DSN 后重启")
		}
	}
	merged := mergeConfigMaps(cur, incoming)
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	newCfg.Storage = store.BootstrapStorage()
	newCfg.OmnigateConfig = configStoreRef + "#omnigate"
	// 面板账号由 panel_users 表与专用鉴权接口管理；主配置表单里回传的是掩码
	// 哈希，不能写回运行时快照。账号集合由 configStore 统一维护。
	newCfg.PanelAuth.Users = store.Users()
	if err := store.SaveMain(newCfg); err != nil {
		return nil, err
	}
	live.Store(livecfg.Snapshot{
		APIKey:           newCfg.APIKey,
		RecordClientInfo: newCfg.Logging.RequestClientInfo,
	})
	if obRT != nil {
		obRT.Reload(newCfg.Outbound)
	}
	if omni != nil && omni.Enabled() {
		if err := omni.SetOutbound(newCfg.Outbound); err != nil {
			log.Printf("WARN: [omnigate] 应用出站代理失败: %v", err)
		}
	}
	return restartRequiredFields(newCfg), nil
}

func restartRequiredFields(c *Config) []string {
	out := []string{
		"listen", "data_dir",
		"server.read_timeout",
		"logging.request_archive_enabled", "logging.request_retention_days",
		"logging.request_archive_max_mb",
	}
	return out
}

var atomicConfigSections = map[string]bool{
	"outbound":   true,
	"panel_auth": true,
}

func mergeConfigMaps(cur, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if atomicConfigSections[k] {
			cur[k] = v
			continue
		}
		if inMap, ok := v.(map[string]any); ok {
			if curMap, ok := cur[k].(map[string]any); ok {
				cur[k] = mergeConfigMaps(curMap, inMap)
				continue
			}
		}
		cur[k] = v
	}
	return cur
}

func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
