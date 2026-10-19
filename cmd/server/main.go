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
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	setAdminPW := flag.Bool("set-admin-password", false, "交互设置/重置面板账号密码后退出")
	adminUser := flag.String("admin-user", "admin", "配合 -set-admin-password：目标账号名")
	adminRole := flag.String("admin-role", "admin", "配合 -set-admin-password：角色（admin/viewer）")
	flag.Parse()

	// 插件注册表必须在 Load 之前就绪：配置校验按注册表判断 provider 类型，
	// 新增插件只要在启动早期 Register 即可直接写进配置。
	catalog.Register()

	if *setAdminPW {
		if err := runSetAdminPassword(*cfgPath, *adminUser, *adminRole); err != nil {
			log.Fatalf("set-admin-password: %v", err)
		}
		return
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				log.Printf("config %s not found (auto-generate failed), using defaults: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	// 状态存储后端：PostgreSQL 可选，文件为兜底。
	var pg *pgstore.Store
	if cfg.Storage.UsePostgres() {
		st, err := pgstore.Open(context.Background(), cfg.Storage.DSN)
		if err != nil {
			log.Printf("WARN: [storage] PostgreSQL 不可用，状态回落本地文件: %v", err)
		} else {
			pg = st
			defer pg.Close()
			log.Printf("[storage] 状态存储后端 = postgres (%s)", st.Label())
		}
	} else {
		log.Printf("[storage] 状态存储后端 = file (%s)", cfg.DataDir)
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
	omni := newOmniManager(cfg, cfg.DataDir, requestLog, rec, func() bool {
		return live.Load().RecordClientInfo
	}, obRT, kvBackend(pg))
	if omni.Enabled() {
		defer omni.Stop()
	}

	// 面板账号：库为权威，config.json 作镜像。
	panelUsers := cfg.PanelAuth.Users
	if pg != nil {
		users, have, err := pg.LoadPanelUsers(context.Background())
		switch {
		case err != nil:
			log.Printf("WARN: [storage] 读 PostgreSQL 面板账号失败，本次用 config.json 镜像: %v", err)
		case have:
			panelUsers = users
			log.Printf("[storage] 已从 PostgreSQL 载入 %d 个面板账号", len(users))
		default:
			if err := pg.SavePanelUsers(context.Background(), cfg.PanelAuth.Users); err != nil {
				log.Printf("WARN: [storage] 首次导入面板账号失败: %v", err)
			}
		}
	}
	authStore := panelauth.New(panelUsers, cfg.PanelAuth.SessionTTL(),
		cfg.PanelAuth.MaxFail(), cfg.PanelAuth.LockDuration())
	if err := authStore.EnableSessionBackend(kvBackend(pg), "panel_sessions",
		filepath.Join(cfg.DataDir, "panel_sessions.json")); err != nil {
		log.Printf("panel: 会话持久化未启用：%v", err)
	}

	pcfg := panel.Config{
		APIKey:           cfg.APIKey,
		Version:          appVersion,
		Live:             live,
		ConfigPath:       *cfgPath,
		DataDir:          cfg.DataDir,
		LoadConfig:       func() (any, error) { return Load(*cfgPath) },
		SaveConfig:       func(raw []byte) ([]string, error) { return saveConfig(raw, *cfgPath, live, obRT, omni) },
		Usage:            rec,
		RequestLog:       requestLog,
		OwnershipFile:    filepath.Join(cfg.DataDir, "ownership.json"),
		OwnershipBackend: kvBackend(pg),
		OwnershipKey:     "ownership",
		PanelAuth:        authStore,
		LoadPanelAuth: func() (panel.PanelAuthSection, error) {
			c, err := Load(*cfgPath)
			if err != nil {
				return panel.PanelAuthSection{}, err
			}
			users := c.PanelAuth.Users
			if pg != nil {
				if got, have, err := pg.LoadPanelUsers(context.Background()); err == nil && have {
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
			if _, err := saveConfig(wrapped, *cfgPath, live, obRT, omni); err != nil {
				return nil, err
			}
			if c, err := Load(*cfgPath); err == nil {
				authStore.Reconfigure(c.PanelAuth.Users, c.PanelAuth.SessionTTL(),
					c.PanelAuth.MaxFail(), c.PanelAuth.LockDuration())
			}
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
			if _, err := saveConfig(wrapped, *cfgPath, live, obRT, omni); err != nil {
				return "", err
			}
			if err := omni.SetAPIKey(key); err != nil {
				log.Printf("WARN: [omnigate] 轮换 api_key 后热重载失败（重启后生效）: %v", err)
			}
			return key, nil
		},
	}
	if omni.Enabled() {
		pcfg.OmniConfigPath = cfg.OmnigateConfig
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
		pcfg.OmniQuota = func(provider string) (any, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			return omni.OpencodeQuota(ctx, provider)
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

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启字段列表。
func saveConfig(raw []byte, path string, live *livecfg.Holder, obRT *outbound.Runtime, omni *omniManager) ([]string, error) {
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read current config: %w", err)
	}
	var cur, incoming map[string]any
	if err := json.Unmarshal(oldRaw, &cur); err != nil {
		cur = map[string]any{}
	}
	if err := json.Unmarshal(raw, &incoming); err != nil {
		return nil, fmt.Errorf("parse submitted config: %w", err)
	}
	merged := mergeConfigMaps(cur, incoming)
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		if !errors.Is(err, syscall.EBUSY) {
			return nil, fmt.Errorf("replace config: %w", err)
		}
		f, openErr := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
		if openErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", openErr)
		}
		_, writeErr := f.Write(out)
		if writeErr == nil {
			writeErr = f.Sync()
		}
		closeErr := f.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
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
		"listen", "data_dir", "omnigate_config", "storage",
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
