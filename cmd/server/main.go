// main.go omnigate-panel 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
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
	"strings"
	"syscall"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
	"github.com/wuxianggujun/omnigate-panel/internal/credithist"
	"github.com/wuxianggujun/omnigate-panel/internal/livecfg"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
	"github.com/wuxianggujun/omnigate-panel/internal/panel"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pgstore"
	"github.com/wuxianggujun/omnigate-panel/internal/pool"
	"github.com/wuxianggujun/omnigate-panel/internal/redisstore"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/scheduler"
	"github.com/wuxianggujun/omnigate-panel/internal/server"
	"github.com/wuxianggujun/omnigate-panel/internal/session"
	"github.com/wuxianggujun/omnigate-panel/internal/upstream"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

// appVersion 网关版本（fork 版：面板 + 任务体系），透出到 /panel/api/overview。
const appVersion = "1.13.0-panel"

// usagePathFor 由 state 文件路径推出用量文件路径：同目录、文件名 usage.json。
// 这样 config 里改 state_file 时用量数据跟着走，不需要额外配置项。
func usagePathFor(stateFile string) string { return stateSibling(stateFile, "usage.json") }

// stateSibling 返回与 state 文件同目录的指定文件名路径（相对路径场景回落当前目录）。
// usage.json（用量记录）与 output_probes.json（模型上限探测）共用本规则。
func stateSibling(stateFile, name string) string {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return name
	}
	return filepath.Join(dir, name)
}

func main() {
	cfgPath := flag.String("config", "config.json", "配置文件路径（默认当前目录 config.json；不存在时自动生成推荐配置）")
	setAdminPW := flag.Bool("set-admin-password", false, "交互设置/重置面板账号密码后退出（首次引导或忘记密码兜底）")
	adminUser := flag.String("admin-user", "admin", "配合 -set-admin-password：目标账号名")
	adminRole := flag.String("admin-role", "admin", "配合 -set-admin-password：角色（admin/viewer）")
	flag.Parse()

	// 命令行设密：独立于网关 api_key 的面板引导入口。写完即退出，不启动服务。
	if *setAdminPW {
		if err := runSetAdminPassword(*cfgPath, *adminUser, *adminRole); err != nil {
			log.Fatalf("set-admin-password: %v", err)
		}
		return
	}

	cfg, err := Load(*cfgPath)
	if err != nil {
		// errors.Is 才能看穿 Load 里 fmt.Errorf("%w") 的包装；os.IsNotExist 不行。
		if errors.Is(err, fs.ErrNotExist) {
			// 首次运行：目录下没有配置 → 自动落一份推荐配置（含随机 api_key）再加载。
			// 双击 exe / 裸跑 docker 即开，无需先手工复制样例。
			if key, werr := WriteDefault(*cfgPath); werr == nil {
				log.Printf("config %s 不存在，已生成推荐配置（api_key=%s，记录在该文件里，可自行修改）", *cfgPath, key)
				cfg, err = Load(*cfgPath)
			}
			if err != nil {
				// 生成失败（目录只读等）：退回纯默认 + env（旧行为兜底），不阻塞启动。
				log.Printf("config %s not found (auto-generate failed), using defaults+env: %v", *cfgPath, err)
				cfg, err = Load("")
			}
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// ── 状态存储后端：面板全部状态优先落用户自己的 PostgreSQL ──────────────────
	// 数据库是权威存储，本地文件退化为镜像 + 断库兜底（详见 internal/pgstore）：
	// 数据库连不上时仍用镜像启动，谁都登不进来这种情况不会发生。面板账号、会话、
	// 账号归属、用量、积分历史、池状态、OmniGate 状态与缓存全部经它落库。
	var pg *pgstore.Store
	if cfg.Storage.UsePostgres() {
		st, err := pgstore.Open(context.Background(), cfg.Storage.DSN)
		if err != nil {
			log.Printf("WARN: [storage] PostgreSQL 不可用，全部状态回落本地文件: %v", err)
		} else {
			pg = st
			defer pg.Close()
			log.Printf("[storage] 状态存储后端 = postgres (%s)", st.Label())
		}
	} else {
		log.Printf("[storage] 状态存储后端 = file（本地 data/）")
	}

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	// 停机序：先 pool.Close()（最后一次 Flush → SaveState 已提交到 store），
	// 再 store.Close() 排空在途异步写（最后一笔 Redis 镜像必须写完才关连接）。
	defer func() {
		p.Close()
		_ = store.Close()
	}()
	p.SetStore(store)
	// 池状态可选落库：库为权威，文件仅首次导入 + 断库兜底。必须在恢复/对齐之前。
	if pg != nil {
		if err := p.SetBackend(pg, "pool_state"); err != nil {
			log.Printf("WARN: [storage] 池状态启用 PostgreSQL 后端失败，回落 state.json: %v", err)
		}
	}
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 熔断器 + 在途上限（含 global 分档）+ 连败降权 + 闲置补偿调优（从 config 注入，
	// 非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域 WAF 风控分档（P1-1）
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(cfg.SoftRateMaxDur)                 // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetCreditFloor(cfg.Pool.CreditFloor)               // 积分保底（默认 0 = 关闭）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(cfg.Pool.PreferExpiring)

	// 域路由（config.json 的 realm_routing 段）：裸模型名的 cn/global 优先级 + 模型
	// 优先域规则。面板保存后热更新（同一 *RealmRouter 实例，Reconfigure 原子替换）。
	// global.enabled=false（逃生门）时从候选里彻底移除 global。
	realmOrder := cfg.RealmRouting.Order
	if !cfg.Global.Enabled {
		realmOrder = []string{"cn"}
	}
	realmRouter := server.NewRealmRouter(realmOrder, cfg.RealmRouting.Prefer)

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 域感知闭包：模型名按 RealmRouter 解析成有序候选域，再按域过滤可用账号
			// （跨 realm 不泄漏）；裸名按 realm_routing 优先级，显式前缀仍硬指定。
			AvailableForModel: realmAwareAvailableForModel(p, realmRouter),
			// 首次分配走快过期硬优先层（层为空时内部回落 AvailableForModel 同集）。
			AssignForModel: realmAwareAssignForModel(p, realmRouter),
			// 粘性让路（P2）：冷会话给快过期层让位，0 = 关闭。
			YieldColdAfter: cfg.SessionYieldColdAfter,
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()

	// 出站代理（config.json 的 outbound 段）：面板上游目标 "workbuddy" 启动即
	// 生效（nil = 直连）。保存配置时 saveConfig 会热更新同一 Transport。
	// obRT 是代理池的运行时（后台刷新 + 探活），面板上游与 OmniGate 共用一份。
	obRT := outbound.NewRuntime(cfg.Outbound, func(f string, a ...any) {
		log.Printf("[outbound] "+f, a...)
	})
	defer obRT.Close()
	applyUpstreamProxy(up, obRT)

	// 积分保底的「收费」兜底判据：接上游模型目录的积分倍率表。本地实测台账无观测
	// 时用它判收费——否则「没学过」恒等于「放行」，高价新模型会把触底号一笔打穿
	// （kimi-k3-1 实案：全池无观测 → 保底全放行 → 两笔打穿并硬冷却到次日 04:00）。
	// 位于 up 装配之后：倍率表由探测下发，闭包每次调用读实时快照。
	p.SetModelRateOf(func(realm, model string) string { return up.ModelRate(realm, model) })

	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints.Store(cfg.Features.SanitizeBlacklistFingerprints)
	// 出站 UA 与归属头（issue #42 + 上游同步）：
	// UserAgent 非空则完全覆盖；ClientVersion/CliVersion 缺省对齐官方形态；
	// ClientName 非空时 chat 路径注入 X-IDE-* 四头（用量归因对齐官方桌面端）。
	up.UserAgent = cfg.Upstream.UserAgent
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	up.ClientName = cfg.Upstream.ClientName
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 路由（config global 段）：上游侧开关（第一道闸）+ base 覆盖；
	// auth 侧开关（auth.SetGlobalEnabled）是第二道闸，两者同 config global.enabled。
	up.GlobalEnabled = cfg.Global.Enabled
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	auth.SetGlobalEnabled(cfg.Global.Enabled)
	// model.json 本地缓存接线（context_length/max_output_tokens 四级查找链第 3 级）：
	// 数据目录与 state.json 同风格（Docker volume 持久化路径）。首次缺失/损坏自动
	// 回落仓库内嵌种子；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(stateSibling(cfg.StateFile, "model.json"))

	// 余额快照历史（「近 N 小时消耗」/用量对账的数据源）：与 state 文件同目录，
	// 缺省 data/credits_history.json。只记录、不参与任何路由决策。
	creditHist := credithist.New(stateSibling(cfg.StateFile, "credits_history.json"))
	if pg != nil {
		if err := creditHist.EnableBackend(pg, "credits_history"); err != nil {
			log.Printf("WARN: [storage] 积分历史启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	creditHist.Start()
	defer creditHist.Stop()
	log.Printf("[credithist] 余额快照历史已启用: %s (%s)",
		stateSibling(cfg.StateFile, "credits_history.json"), creditHist.Describe())

	sch := scheduler.New(scheduler.Config{
		Pool:           p,
		Upstream:       up,
		History:        creditHist,
		CheckinHours:   cfg.Schedule.CheckinHours,
		TravelHours:    cfg.Schedule.TravelHours,
		ActivityHours:  cfg.Schedule.ActivityHours,
		KeepaliveHours: cfg.Schedule.KeepaliveHours,
		BlackcatHours:  cfg.Schedule.BlackcatHours,
		GrowthHours:    cfg.Schedule.GrowthHours,
		// 快过期积分优先消耗：签到/余额刷新按此窗口分桶（issue:积分过期）。
		ExpiringSoonWindow: cfg.ExpiringSoonDur,
		CheckinDisabled:    !cfg.Schedule.CheckinEnabled,
		TravelDisabled:     !cfg.Schedule.TravelEnabled,
		ActivityDisabled:   !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:  !cfg.Schedule.KeepaliveEnabled,
		BlackcatDisabled:   !cfg.Schedule.BlackcatEnabled,
		GrowthDisabled:     !cfg.Schedule.GrowthEnabled,
		// 保号类四任务是否覆盖禁用账号（缺省 false = 禁用即跳过，保持既有行为）。
		IncludeDisabledInTasks: cfg.Schedule.IncludeDisabledInTasks,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每日 1 次，点亮连登 + 解锁 first_buddy）", cfg.Schedule.ActivityHours)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	switch {
	case !cfg.Schedule.BlackcatEnabled:
		log.Printf("夜猫子已禁用（schedule.blackcat_enabled=false）")
	default:
		log.Printf("夜猫子已启用：%v 点（23:00–08:00 窗口 glm-5.2 对话补足）", cfg.Schedule.BlackcatHours)
	}
	switch {
	case !cfg.Schedule.BalanceRefreshEnabled:
		log.Printf("余额后台刷新已禁用（schedule.balance_refresh_enabled=false）")
	case cfg.BalanceRefreshInterval > 0:
		log.Printf("余额后台刷新：每 %s（签到时点照常额外刷新）", cfg.BalanceRefreshInterval)
	}
	if cfg.Schedule.IncludeDisabledInTasks {
		log.Printf("保号任务覆盖禁用账号（schedule.include_disabled_in_tasks=true）：禁用号仍签到 / 活跃 / 保活 / 刷新余额，但不参与选号")
	}

	// 管理面板日志镜像：标准 log（stderr）与 chat 表格日志（stdout）双路复制进
	// 面板环形缓冲，供 /panel/api/logs 读取；控制台输出行为完全不变。
	// live 承载可热改字段（api_key/soft_rate/脱敏开关），面板保存配置时在线替换。
	live := livecfg.New(livecfg.Snapshot{
		APIKey:               cfg.APIKey,
		SoftCooldown:         cfg.SoftRateDur,
		SanitizeFingerprints: cfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     cfg.Logging.RequestClientInfo,
	})
	// 用量记录器：与 state 文件同目录，随 state_file 配置一起搬移。
	// datapath 由 state 文件路径推出，避免再加一个配置项。
	usagePath := usagePathFor(cfg.StateFile)
	rec := usage.New(usagePath)
	if pg != nil {
		if err := rec.EnableBackend(pg, "usage"); err != nil {
			log.Printf("WARN: [storage] 用量启用 PostgreSQL 后端失败，回落文件: %v", err)
		}
	}
	rec.Start()
	defer rec.Stop()
	log.Printf("[usage] 逐请求用量记录已启用: %s (%s)", usagePath, rec.Describe())

	// 请求指标始终启用；归档默认写脱敏元数据。配置了 PostgreSQL 时改为落库
	// （request_logs 表）：首次启用把既有 JSONL 一次性导入，之后不再写文件。
	reqLogDir := stateSibling(cfg.StateFile, "request-logs")
	var reqSink reqlog.Sink
	if pg != nil {
		reqSink = pg
		if cfg.Logging.RequestArchiveEnabled {
			if rows, _, _ := pg.LogStats(); rows == 0 {
				if n, err := pg.ImportRequestLogsFromDir(reqLogDir); err != nil {
					log.Printf("WARN: [storage] 请求记录导入 PostgreSQL 失败（先按空表运行）: %v", err)
				} else if n > 0 {
					log.Printf("[storage] 首次启用：已把 %d 条请求记录导入 PostgreSQL", n)
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
	rs := requestLog.Snapshot().Archive
	switch {
	case !rs.Enabled:
		log.Printf("[reqlog] 请求指标已启用；归档已关闭")
	case rs.Backend != "":
		log.Printf("[reqlog] 请求指标已启用；归档落 %s（保留 %d 天，上限 %d MiB）",
			rs.Backend, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	default:
		log.Printf("[reqlog] 请求指标已启用；JSONL 归档 %s（保留 %d 天，上限 %d MiB）",
			rs.Dir, cfg.Logging.RequestRetentionDays, cfg.Logging.RequestArchiveMaxMB)
	}

	// 内置 OmniGate 引擎（Runable / 浣熊 / 任意 OpenAI 兼容上游）：先装配，
	// 好把供应商配置读写器注入面板（保存即热重载，无需重启进程）。
	omni := newOmniManager(cfg, requestLog, rec, func() bool { return live.Load().RecordClientInfo }, obRT, pg)
	if omni.Enabled() {
		defer omni.Stop()
	}

	// 面板账号：以库为准；库里还没有（首次启用）就把现状导进去。
	panelUsers := cfg.PanelAuth.Users
	if pg != nil {
		users, have, err := pg.LoadPanelUsers(context.Background())
		switch {
		case err != nil:
			log.Printf("WARN: [storage] 读 PostgreSQL 面板账号失败，本次启动用 config.json 镜像: %v", err)
		case have:
			panelUsers = users
			log.Printf("[storage] 已从 PostgreSQL 载入 %d 个面板账号", len(users))
		default:
			if err := pg.SavePanelUsers(context.Background(), cfg.PanelAuth.Users); err != nil {
				log.Printf("WARN: [storage] 首次导入面板账号失败（本次仍用 config.json）: %v", err)
			} else {
				log.Printf("[storage] 首次启用：已把 %d 个面板账号导入 PostgreSQL", len(cfg.PanelAuth.Users))
			}
		}
	}

	// 面板登录鉴权运行时（独立于网关 api_key）：账号来自上述存储。面板只认密码
	// 登录（api_key 不能打开面板）；未配置任何账号时 /panel/api/* 一律 401
	// （登录端点返回 panel_auth_not_configured），不退回旧 api_key 门。
	// 账号管理页保存后 Reconfigure 热重建（会话保留，按当前账号集合即时校验）。
	authStore := panelauth.New(panelUsers, cfg.PanelAuth.SessionTTL(),
		cfg.PanelAuth.MaxFail(), cfg.PanelAuth.LockDuration())
	// 会话落盘：部署/重启容器后已登录用户不掉线（与 state 文件同目录，
	// 缺省 data/panel_sessions.json）。库为权威时落 PostgreSQL（panel_sessions），
	// 文件仅首次导入 + 断库兜底。失败只记日志——纯内存会话仍可正常工作。
	if err := authStore.EnableSessionBackend(pg, "panel_sessions", stateSibling(cfg.StateFile, "panel_sessions.json")); err != nil {
		log.Printf("panel: 会话持久化未启用：%v", err)
	}

	pcfg := panel.Config{
		Pool:        p,
		Usage:       rec,
		History:     creditHist,
		RequestLog:  requestLog,
		Upstream:    up,
		Scheduler:   sch,
		AuthDir:     cfg.AuthDir,
		APIKey:      cfg.APIKey,
		RedisMode:   redisMode,
		StickyCount: sessCount,
		Version:     appVersion,
		Live:        live,
		// 模型上限探测数据（scripts/probe_max_tokens.py --panel-out 写入）：
		// 与 state 文件同目录，缺省 data/output_probes.json。
		ProbeFile:  stateSibling(cfg.StateFile, "output_probes.json"),
		// 账号归属（多租户）：谁拥有哪个账号。与 state 文件同目录，缺省
		// data/ownership.json；缺失/损坏时退化为空映射（全部视为管理员/共享）。
		OwnershipFile: stateSibling(cfg.StateFile, "ownership.json"),
		// 归属表可选落库：库为权威（ownership），文件仅首次导入 + 断库兜底。
		OwnershipBackend: pg,
		OwnershipKey:     "ownership",
		ConfigPath:    *cfgPath,
		LoadConfig: func() (any, error) {
			return Load(*cfgPath)
		},
		SaveConfig: func(raw []byte) ([]string, error) {
			return saveConfig(raw, *cfgPath, live, p, up, sch, obRT)
		},
		// 出站代理配置（config.json 的 outbound 段）读写：面板 OmniGate 页
		// 「出站代理」卡片用。保存后热应用到面板上游 + 内置 OmniGate 供应商。
		LoadOutbound: func() (any, error) {
			c, err := Load(*cfgPath)
			if err != nil {
				return nil, err
			}
			return c.Outbound, nil
		},
		SaveOutbound: func(raw []byte) ([]string, error) {
			wrapped := make([]byte, 0, len(raw)+16)
			wrapped = append(wrapped, `{"outbound":`...)
			wrapped = append(wrapped, raw...)
			wrapped = append(wrapped, '}')
			if _, err := saveConfig(wrapped, *cfgPath, live, p, up, sch, obRT); err != nil {
				return nil, err
			}
			if omni.Enabled() {
				if c, err := Load(*cfgPath); err == nil {
					if err := omni.SetOutbound(c.Outbound); err != nil {
						log.Printf("WARN: [omnigate] 应用出站代理失败: %v", err)
					}
				}
			}
			return nil, nil
		},
		// 域路由配置（config.json 的 realm_routing 段）读写：面板「模型与档位」页
		// 「域优先级」卡片用。保存后热更新同一 *RealmRouter（Reconfigure 原子替换，
		// 请求路径立即生效，无需重启）。
		LoadRealmRouting: func() (any, error) {
			c, err := Load(*cfgPath)
			if err != nil {
				return nil, err
			}
			return c.RealmRouting, nil
		},
		SaveRealmRouting: func(raw []byte) ([]string, error) {
			wrapped := make([]byte, 0, len(raw)+24)
			wrapped = append(wrapped, `{"realm_routing":`...)
			wrapped = append(wrapped, raw...)
			wrapped = append(wrapped, '}')
			if _, err := saveConfig(wrapped, *cfgPath, live, p, up, sch, obRT); err != nil {
				return nil, err
			}
			// 热生效：重读配置、重建规则（global 逃生门时移除 global）。
			if c, err := Load(*cfgPath); err == nil {
				order := c.RealmRouting.Order
				if !c.Global.Enabled {
					order = []string{"cn"}
				}
				realmRouter.Reconfigure(order, c.RealmRouting.Prefer)
			}
			return nil, nil
		},
		// 面板鉴权配置读写：账号管理页用。开关（会话时长 / 锁定 / 允许注册）仍在
		// config.json；账号集合在 driver=postgres 时以数据库为准，config.json 只作
		// 镜像兜底。LoadPanelAuth 返回含密码哈希的原始段（面板内部用于保留未改密码
		// 账号的哈希，对前端只暴露用户名/角色）；SavePanelAuth 完成「校验 → 落库
		// （+镜像）→ 热重建同一 *panelauth.Store」，全部热生效，需重启列表恒空。
		PanelAuth: authStore,
		LoadPanelAuth: func() (panel.PanelAuthSection, error) {
			c, err := Load(*cfgPath)
			if err != nil {
				return panel.PanelAuthSection{}, err
			}
			users := c.PanelAuth.Users // 镜像兜底（数据库不可用时用这份）
			if pg != nil {
				got, have, err := pg.LoadPanelUsers(context.Background())
				switch {
				case err != nil:
					log.Printf("WARN: [storage] 读 PostgreSQL 面板账号失败，本次用 config.json 镜像: %v", err)
				case have:
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
			// 1) 权威存储：PostgreSQL。写库失败直接上报，且不写镜像——避免库与镜像
			// 分叉（镜像始终是「库的最近一次已知良好状态」）。
			if pg != nil {
				if err := pg.SavePanelUsers(context.Background(), section.Users); err != nil {
					return nil, fmt.Errorf("保存面板账号到 PostgreSQL 失败（本次未落任何存储）: %w", err)
				}
			}
			// 2) 镜像：config.json（数据库故障时的登录兜底 + 兼容既有读配置路径）。
			wrapped, err := json.Marshal(map[string]any{"panel_auth": section})
			if err != nil {
				return nil, err
			}
			if _, err := saveConfig(wrapped, *cfgPath, live, p, up, sch, obRT); err != nil {
				return nil, err
			}
			// 热重建鉴权运行时（内存与磁盘对齐）。
			if c, err := Load(*cfgPath); err == nil {
				authStore.Reconfigure(c.PanelAuth.Users, c.PanelAuth.SessionTTL(),
					c.PanelAuth.MaxFail(), c.PanelAuth.LockDuration())
			}
			return nil, nil
		},
		// 重新生成网关 api_key（管理员）：轮换防泄漏被滥用。生成新 key → 合并落盘
		// → 热生效（live 快照 + 内置 OmniGate 复用的 api_keys）。返回新 key 供前端
		// 展示一次；之后 /panel/api/config 仍会回显（管理员可见）。
		RegenerateKey: func() (string, error) {
			key, err := GenerateAPIKey()
			if err != nil {
				return "", err
			}
			wrapped, err := json.Marshal(map[string]any{"api_key": key})
			if err != nil {
				return "", err
			}
			if _, err := saveConfig(wrapped, *cfgPath, live, p, up, sch, obRT); err != nil {
				return "", err
			}
			if omni.Enabled() {
				if err := omni.SetAPIKey(key); err != nil {
					log.Printf("WARN: [omnigate] 轮换 api_key 后热重载失败（/omni/* 将在重启后生效）: %v", err)
				}
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
		// 统一账号池：把 OmniGate 各供应商账号聚合进面板「账号池」视图，并支持
		// 单账号签到/移除、全量签到。全部走进程内运行时，不经过 /omni/* HTTP。
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
		// 模型目录只读代理：面板「模型与档位」页匿名也能看 OmniGate 模型与积分价，
		// 但不经过需要 api_key 的 /omni/* HTTP。
		pcfg.OmniModels = omni.Models
		// runable 登录校验（服务端会话探测 + 取积分）：runable 只支持 Google/Facebook
		// 登录、会话是 httpOnly Cookie（网页读不到），面板无法自动抓取，只能由用户粘贴。
		pcfg.OmniLogin = omni.LoginRunable
		// New API（justwoker 等）浏览器登录抓取 + 统一刷新：该站登录开了 Cloudflare
		// Turnstile，服务端直连密码登录会被拦，故账号靠「浏览器登录 + 控制台脚本回传」
		// 建立。抓取端点公开但靠一次性 state 校验（见 internal/panel/omni_newapi.go）。
		pcfg.OmniNewAPIAuthorize = omni.NewAPIAuthorize
		pcfg.OmniNewAPICapture = omni.NewAPICapture
		pcfg.OmniNewAPICaptureStatus = omni.NewAPICaptureStatus
		pcfg.OmniNewAPISetKey = omni.NewAPISetKey
		pcfg.OmniRefresh = omni.NewAPIRefresh
		// 面板 → OmniGate 管理端点代理：raccoon/TRAE 的授权/回调/账号列表/签到/
		// 移除由面板注入 api_key 转发（多租户下普通 user 不持有网关 api_key）。
		pcfg.OmniAdmin = omni.Handler()
	}
	pn := panel.New(pcfg)
	// 成长任务队列每日自动执行（与「执行全部待办」同管线）：Sequential 族零点解锁后
	// 无需手动扫描；hook 返回即启动（异步执行），已在跑时内部跳过。
	sch.SetGrowthHook(pn.RunGrowthQueueOnce)
	// 面板「运行日志」只收标准日志（任务 / 系统：签到 / 保活 / 调度等）。
	// 聊天表格行是逐请求流水，属于「请求记录」页（从归档读取）——不再镜像进
	// 环形缓冲，否则每个请求一行会把运行日志淹没、签到记录被迅速挤出。
	log.SetOutput(io.MultiWriter(os.Stderr, pn.Logs()))

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		Panel:        pn,
		Live:         live,
		Usage:        rec,
		RequestLog:   requestLog,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// 来源记录开关经 livecfg 热生效；此处同时填静态字段，供 Live 为 nil 的
		// 裸用/测试路径拿到同一缺省值。
		RecordClientInfo: cfg.Logging.RequestClientInfo,
		// handler 侧第三道闸（global realm）：false（显式逃生门）时不列 global 模型名。
		GlobalEnabled: cfg.Global.Enabled,
		// 域路由：裸模型名 → 有序候选域列表（realm_routing）。同一实例与面板共享，
		// 面板保存后热更新。
		RealmRouter: realmRouter,
	})

	// 内置 OmniGate 引擎挂到本服务的 /omni/ 前缀下（同源，面板前端直接调用）。
	// 供应商配置页保存后由 omniManager 进程内重建运行时，无需重启。
	omniRoot := omni.Handler()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)
	sch.StartBalanceRefresh(ctx, cfg.BalanceRefreshInterval)

	// 启动即预热模型积分倍率表：倍率只在 FetchModels/FetchGlobalModelInfos 成功时
	// 填充（两者均懒触发），重启后到首次 /v1/models 或面板模型页被访问之前，
	// ModelRate 恒返回空串——积分保底的目录兜底在这段空窗期内形同虚设，触底号
	// 会被当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费
	// 模型归零；倍率表当时尚未建立）。
	// 异步执行：不阻塞监听启动；失败仅记日志（下一轮懒触发或本轮重试仍可补上）。
	go warmModelRates(ctx, up, p)
	// 异步预热模型目录（CN + global）：供域路由判断「该域是否提供该模型」——目录冷时
	// 只能按 realm_routing 顺序走、靠选号回退兜底，global-only 模型会被 cn 域白撞。
	// 周期性刷新（目录本身带 TTL：CN 10min / global 1h）。
	go warmModelCatalog(ctx, h)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           mountRoot(h, omniRoot),
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body 上传）：防慢速 body 拖死连接。
		// 请求体已无网关侧上限（max_body_mb 移除）。缺省 300s（issue #100：旧固定
		// 60s 会掐掉大上下文/文件块经反代链的慢速上传，客户端收到
		// 400 "read body: ... i/o timeout"）；server.read_timeout="0" 显式关闭。
		// 改动需重启进程。
		ReadTimeout: cfg.ServerReadTimeoutDur,
		// IdleTimeout keep-alive 空闲连接回收：配合 chat 出站 ctx 传播防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("omnigate-panel listening on %s (api_key=%v)，管理面板 http://127.0.0.1%s/panel/", cfg.Listen, cfg.APIKey != "", panelListenPath(cfg.Listen))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// warmModelRates 启动预热各域模型积分倍率表（供积分保底的目录兜底判定）。
//
// 为什么需要：倍率表只在 FetchModels（CN）/ FetchGlobalModelInfos（global）成功时
// 填充，两者都是懒触发（被 /v1/models 或面板模型页访问才跑）。重启后到首次触发
// 之间的空窗期里 ModelRate 恒返回空串，保底的目录兜底判不出收费，触底号会被
// 当成「收费未知」放行并打穿（实测：重启后 2 分钟，97 分的账号打收费模型归零）。
//
// 失败处理：单域失败只记 WARN（不阻塞、不致命——后续懒触发仍会补上）；global 域
// 仅在其路由开关开启时预热（逃生门关锁时按 CN 处理，无需探测）。
func warmModelRates(ctx context.Context, up *upstream.Client, p *pool.Pool) {
	// 预热不得拖住进程退出：ctx 取消（SIGINT/SIGTERM）时立刻放弃剩余域。
	if ctx.Err() != nil {
		return
	}
	// CN：有可用 CN 账号才拉（与面板 models 同口径，避免无谓上游调用）。
	if uids := p.AvailableUIDsForRealm("cn"); len(uids) > 0 {
		if a := p.AuthByUID(uids[0]); a != nil {
			if _, err := up.FetchModels(a); err != nil {
				log.Printf("WARN: [upstream] warm model rates (cn): %v", err)
			} else {
				log.Printf("[upstream] warm model rates: cn ok")
			}
		}
	}
	// global：独立目录端点（workbuddy.ai），倍率按 "global" 域键存储。
	if up.GlobalEnabled && ctx.Err() == nil {
		if uids := p.AvailableUIDsForRealm("global"); len(uids) > 0 {
			if a := p.AuthByUID(uids[0]); a != nil {
				// FetchGlobalModelInfos 无错误返回（内部负缓存自行节流），
				// 仅按结果条数判断是否拿到目录。
				if infos := up.FetchGlobalModelInfos(a); len(infos) == 0 {
					log.Printf("WARN: [upstream] warm model rates (global): empty model list")
				} else {
					log.Printf("[upstream] warm model rates: global ok (%d models)", len(infos))
				}
			}
		}
	}
}

// warmModelCatalog 启动预热 + 周期刷新模型目录缓存（CN 动态 + global），供域路由的
// 「该域是否提供该模型」探测使用。目录冷时域路由只能按 realm_routing 顺序走、靠选号
// 回退兜底——global-only 模型在 cn 域会白撞（MaxRotate 只有 3）。这里保证目录尽快可用。
//
// 周期 10min 对齐 CN 目录 TTL；global 侧内部 1h 缓存，重复调用代价可忽略。失败静默
// （h.WarmModels 内部走负缓存节流，不阻塞）。
func warmModelCatalog(ctx context.Context, h *server.Handler) {
	refresh := func() {
		if ctx.Err() != nil {
			return
		}
		h.WarmModels()
	}
	refresh() // 启动即预热
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

// panelListenPath 从 listen 地址提取 ":port" 形式，用于启动日志拼面板 URL
// （":7863" 或 "0.0.0.0:7863" → ":7863"；异常输入原样返回）。
func panelListenPath(listen string) string {
	for i := len(listen) - 1; i >= 0; i-- {
		if listen[i] == ':' {
			return listen[i:]
		}
	}
	return listen
}

// mountRoot 把内置 OmniGate 引擎挂到 /omni/ 前缀下（剥前缀后交给 OmniGate 路由），
// 其余请求全部交给 WorkBuddy 网关 handler。omni 为 nil 时直接返回 h。
func mountRoot(h http.Handler, omni http.Handler) http.Handler {
	if omni == nil {
		return h
	}
	omniStripped := http.StripPrefix("/omni", omni)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/omni" || strings.HasPrefix(r.URL.Path, "/omni/") {
			omniStripped.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// saveConfig 面板保存配置：校验 → 落盘 → 热应用 → 返回需重启的字段列表。
//
// 热生效范围（设计取舍）：
//   - api_key / cooldown.soft_rate / features.sanitize_blacklist_fingerprints → livecfg 快照
//   - pool.* → pool.SetBreaker/SetMaxInFlight/SetSoftRateMax/SetWeights/SetCostExploreInterval/SetPreferExpiring/SetCreditFloor
//   - schedule.* → scheduler.Reconfigure/SetBalanceInterval/SetExpiringSoonWindow
//
// 需重启（涉及监听地址、HTTP client 超时、auth_dir 等装配期依赖）：
//   - listen / auth_dir / state_file / upstream.* / upstash.* / session_sticky.*（TTL 类）
//
// 落盘用"先写 tmp 再 rename"原子替换，且优先保留磁盘上的原始 JSON 结构（只改
// 面板表单覆盖到的键），避免把用户手写的注释性字段/未知键洗掉——这里直接整体
// 序列化校验后的配置，未知键在 json.Unmarshal 时已丢失，故先合并原始 map。
func saveConfig(raw []byte, path string, live *livecfg.Holder, p *pool.Pool, up *upstream.Client, sch *scheduler.Scheduler, obRT *outbound.Runtime) ([]string, error) {
	// 1) 解析原始 JSON 为 map（保留用户手写的未知键），再叠加面板提交的键。
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

	// 2) 校验（与启动同一套 Default+normalize），失败直接返回、不落盘。
	newCfg, err := ParseConfig(mergedJSON(merged))
	if err != nil {
		return nil, err
	}

	// 3) 落盘（原子替换）。
	out, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal config: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return nil, fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// A single-file Docker bind mount cannot be renamed over its mount
		// target (Linux returns EBUSY / "device or resource busy"). Keep the
		// atomic path for regular files, but update the mounted file in place
		// for this specific deployment shape.
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
		// 写失败时保留 tmp（挂载文件已被 O_TRUNC 破坏，tmp 里是完整新内容，
		// 可手工恢复）；写成功才清理。
		if writeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback, 完整新内容保留在 %s): %w", tmp, writeErr)
		}
		_ = os.Remove(tmp)
		if closeErr != nil {
			return nil, fmt.Errorf("replace config (bind mount fallback): %w", closeErr)
		}
	}

	// 4) 热应用：能立即生效的字段全部应用，并列出仍需重启的字段。
	live.Store(livecfg.Snapshot{
		APIKey:               newCfg.APIKey,
		SoftCooldown:         newCfg.SoftRateDur,
		SanitizeFingerprints: newCfg.Features.SanitizeBlacklistFingerprints,
		RecordClientInfo:     newCfg.Logging.RequestClientInfo,
	})
	up.SanitizeFingerprints.Store(newCfg.Features.SanitizeBlacklistFingerprints)
	p.SetBreaker(newCfg.Pool.BreakerThreshold, newCfg.BreakerCooldownDur, newCfg.BreakerCooldownMaxD)
	p.SetMaxInFlight(newCfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(newCfg.Pool.MaxInFlightGlobal)
	p.SetDegrade(newCfg.Pool.DegradeThreshold, newCfg.DegradeCooldownDur, newCfg.DegradeCooldownMaxD)
	p.SetSoftRateMax(newCfg.SoftRateMaxDur)
	p.SetCostExploreInterval(newCfg.CostExploreIntervalDur) // costTier 探索窗口热生效（0 关停）
	p.SetCreditFloor(newCfg.Pool.CreditFloor)               // 积分保底热生效（0 = 关闭）
	p.SetWeights(newCfg.Pool.IdleWeightPerHour, newCfg.Pool.IdleWeightMax)
	p.SetPreferExpiring(newCfg.Pool.PreferExpiring)
	sch.SetExpiringSoonWindow(newCfg.ExpiringSoonDur)
	sch.Reconfigure(
		newCfg.Schedule.CheckinHours, newCfg.Schedule.TravelHours,
		newCfg.Schedule.ActivityHours, newCfg.Schedule.KeepaliveHours, newCfg.Schedule.BlackcatHours,
		newCfg.Schedule.GrowthHours,
		!newCfg.Schedule.CheckinEnabled, !newCfg.Schedule.TravelEnabled,
		!newCfg.Schedule.ActivityEnabled, !newCfg.Schedule.KeepaliveEnabled, !newCfg.Schedule.BlackcatEnabled,
		!newCfg.Schedule.GrowthEnabled)
	sch.SetBalanceInterval(newCfg.BalanceRefreshInterval)
	sch.SetIncludeDisabledInTasks(newCfg.Schedule.IncludeDisabledInTasks)
	// 出站代理（outbound）：热更新代理池运行时 + 面板上游 Transport 的代理
	// （清空闲池即时生效）。
	if obRT != nil {
		obRT.Reload(newCfg.Outbound)
		applyUpstreamProxy(up, obRT)
	} else {
		up.SetProxy(newCfg.Outbound.ProxyFunc(outbound.TargetWorkbuddy))
	}

	return restartRequiredFields(newCfg), nil
}

// applyUpstreamProxy 把当前出站配置应用到面板上游（WorkBuddy 目标）：路由到代理池
// 时安装带故障切换的 Selector（与 OmniGate 供应商一致）；否则用单代理 / 直连。
// 每次保存都先 SetProxySelector 复位，避免「池 → 单代理」切换时残留池包装。
func applyUpstreamProxy(up *upstream.Client, obRT *outbound.Runtime) {
	if up == nil || obRT == nil {
		return
	}
	if pl := obRT.Pool(outbound.TargetWorkbuddy); pl != nil {
		up.SetProxySelector(pl)
		return
	}
	up.SetProxySelector(nil)
	up.SetProxy(obRT.ProxyFunc(outbound.TargetWorkbuddy))
}

// restartRequiredFields 返回本次改动中无法热生效、需要重启进程的字段名。
// 恒返回完整清单中的"与当前进程装配期依赖相关"的项——面板据此提示用户。
func restartRequiredFields(c *Config) []string {
	var out []string
	// 这些字段在进程内被监听地址/HTTP client/目录句柄等装配期对象捕获。
	if c.Listen != "" {
		out = append(out, "listen")
	}
	if c.AuthDir != "" {
		out = append(out, "auth_dir")
	}
	if c.StateFile != "" {
		out = append(out, "state_file")
	}
	out = append(out, "upstream.timeout_seconds", "upstream.header_timeout_seconds", "upstream.idle_timeout_seconds")
	// upstream.user_agent 在装配期被写进出站 client（main.go 的 up.UserAgent = ...），
	// 之后不再读取——不在 livecfg 热快照里，也无法热改。此前漏列，导致面板改完
	// 显示"已保存"却不提示需要重启，用户以为没生效（issue #102 附带发现 2）。
	out = append(out, "upstream.user_agent")
	if c.Upstash.URL != "" || c.Upstash.Token != "" {
		out = append(out, "upstash")
	}
	out = append(out, "session_sticky.ttl", "session_sticky.gc_interval", "session_sticky.yield_cold_after")
	out = append(out, "logging.request_archive_enabled", "logging.request_retention_days", "logging.request_archive_max_mb")
	out = append(out, "server.read_timeout")
	return out
}

// atomicConfigSections 整段替换（不深合并）的顶层配置段。这些段由一个面板卡片一次性
// 提交，内部含可增删的集合（命名代理、域优先规则），深合并会让删除项残留在旧配置里。
var atomicConfigSections = map[string]bool{
	"outbound":      true, // 命名代理列表 + 「目标 → 代理」路由是一组，删项必须真正生效
	"realm_routing": true, // 域优先级 + 模型优先域规则同上（删规则必须真正生效）
	"panel_auth":    true, // 面板账号列表含密码哈希，整段替换（删账号/改角色必须真正生效）
}

// mergeConfigMaps 把 incoming 深合并进 cur（原地），返回 cur。
// 对嵌套对象逐键覆盖而不是整体替换：面板表单只提交它管理的键，
// 未提交的兄弟键（含用户手写的未知键）保持原样。atomicConfigSections 里的段例外，
// 整段替换。
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

// mergedJSON 把合并后的 map 序列化回 JSON（供 ParseConfig 校验）。
func mergedJSON(m map[string]any) []byte {
	b, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return b
}
