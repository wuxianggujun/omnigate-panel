// Package server exposes the OpenAI-compatible HTTP surface.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/imagegen"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

const maxBody = 32 << 20 // 32 MiB

// Server wraps an *http.Server with omnigate's routes.
type Server struct {
	cfg      *config.Config
	gw       *gateway.Gateway
	log      *logx.Logger
	http     *http.Server
	started  time.Time
	requests atomic.Int64

	// reqlog 与 WorkBuddy 网关共用同一份请求记录器（面板「请求记录」页）。
	// nil = 关闭 OmniGate 请求记录。clientInfo 报告是否记录调用来源
	// （客户端 IP / UA），复用面板 logging.request_client_info 开关。
	reqlog     *reqlog.Recorder
	clientInfo func() bool

	// usage 与 WorkBuddy 面板「用量」页共用同一份逐请求用量记录器：OmniGate 的
	// raccoon/runable 请求也计入，让「所有 AI 请求」在一处可见。nil = 不计入。
	usage *usage.Recorder

	// poolStatus 返回各出站代理池的运行时快照（供 /healthz 观测）。nil = 不展示。
	poolStatus func() any

	// images 托管 AI 生成的图片（geminiweb 抽图后落盘），供 /images/{id} 路由读取。
	// 与 gateway 构造 provider 时用的是同一个目录。
	images *imagegen.Store

	// rl 是可选入站限流器（rate_limit.enabled 时才生效）。见 ratelimit.go。
	rl *rateLimiter
}

// New builds a Server.
func New(cfg *config.Config, gw *gateway.Gateway, log *logx.Logger) *Server {
	s := &Server{
		cfg:     cfg,
		gw:      gw,
		log:     log,
		started: time.Now(),
		images:  imagegen.NewStore(filepath.Join(cfg.DataDir, "generated_images")),
		rl:      newRateLimiter(),
	}
	s.http = &http.Server{
		Addr:              net.JoinHostPort(cfg.Server.Host, strconv.Itoa(cfg.Server.Port)),
		Handler:           s,
		ReadHeaderTimeout: 15 * time.Second,
	}
	return s
}

// Addr returns the listen address.
func (s *Server) Addr() string { return s.http.Addr }

// SetRequestLog 注入请求记录器（与 WorkBuddy 网关共用同一份）。rec 为 nil 时
// 关闭 OmniGate 请求记录。clientInfo 报告是否记录调用来源（客户端 IP / UA），
// 复用面板 logging.request_client_info 开关；nil 视为不记录来源。构造后、开始
// 服务前调用一次即可。
func (s *Server) SetRequestLog(rec *reqlog.Recorder, clientInfo func() bool) {
	s.reqlog = rec
	s.clientInfo = clientInfo
}

// SetUsageRecorder 注入逐请求用量记录器（与 WorkBuddy 面板「用量」页共用同一份）。
// nil = OmniGate 请求不计入用量。构造后、开始服务前调用一次即可。
func (s *Server) SetUsageRecorder(rec *usage.Recorder) {
	s.usage = rec
}

// SetPoolStatus 注入出站代理池状态提供者（供 /healthz 观测）。nil = 不展示。
func (s *Server) SetPoolStatus(fn func() any) {
	s.poolStatus = fn
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.requests.Add(1)
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && (path == "/v1/models" || path == "/models"):
		if !s.authorized(w, r) {
			return
		}
		s.handleModels(w, r)
	case r.Method == http.MethodPost && (path == "/v1/chat/completions" || path == "/chat/completions"):
		if !s.authorized(w, r) {
			return
		}
		if !s.rateLimitOK(w, r) {
			return
		}
		s.serveLogged(w, r, s.handleChat)
	case r.Method == http.MethodPost && (path == "/v1/responses" || path == "/responses"):
		if !s.authorized(w, r) {
			return
		}
		s.serveLogged(w, r, s.handleResponses)
	case r.Method == http.MethodPost && (path == "/v1/messages/count_tokens" || path == "/messages/count_tokens"):
		if !s.authorizedAnthropic(w, r) {
			return
		}
		s.handleCountTokens(w, r)
	case r.Method == http.MethodPost && (path == "/v1/messages" || path == "/messages"):
		if !s.authorizedAnthropic(w, r) {
			return
		}
		if !s.rateLimitOK(w, r) {
			return
		}
		s.serveLogged(w, r, s.handleMessages)
	case r.Method == http.MethodGet && path == "/v1beta/models":
		if !s.authorizedGemini(w, r) {
			return
		}
		s.handleGeminiModels(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/v1beta/models/"):
		if !s.authorizedGemini(w, r) {
			return
		}
		if !s.rateLimitOK(w, r) {
			return
		}
		s.serveLogged(w, r, s.handleGeminiGenerate)
	case r.Method == http.MethodPost && (path == "/gemini/v1beta/deepresearch" || path == "/v1beta/deepresearch"):
		if !s.authorizedGemini(w, r) {
			return
		}
		s.handleDeepResearch(w, s.withImageBase(r))
	case r.Method == http.MethodPost && (path == "/gemini/v1beta/deepresearch/stream" || path == "/v1beta/deepresearch/stream"):
		if !s.authorizedGemini(w, r) {
			return
		}
		s.handleDeepResearchStream(w, s.withImageBase(r))
	case r.Method == http.MethodPost && (path == "/gemini/v1beta/deepresearch/interact" || path == "/v1beta/deepresearch/interact"):
		if !s.authorizedGemini(w, r) {
			return
		}
		s.handleDeepResearchInteract(w, s.withImageBase(r))
	case (r.Method == http.MethodGet || r.Method == http.MethodPost || r.Method == http.MethodDelete) &&
		(path == "/admin/model-mapping" || strings.HasPrefix(path, "/admin/model-mapping/")):
		if !s.authorizedAdmin(w, r) {
			return
		}
		s.handleModelMapping(w, r)
	case r.Method == http.MethodGet && path == "/healthz":
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/images/"):
		// Generated images are fetched by whatever renders the reply (a browser,
		// an image proxy), which cannot present an API key. The id is the only
		// secret: 128 random bits, unguessable, and swept after a week.
		s.handleImage(w, r)
	case r.Method == http.MethodGet && path == "/":
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && path == "/admin/raccoon/redirect":
		// Browser callback for the web-redirect (auto-capture) flow: no API key
		// here — the browser navigates to it directly. Safety comes from the
		// one-time, state-validated authorization session.
		s.handleRaccoonRedirect(w, r)
	case r.Method == http.MethodGet && path == "/admin/trae/redirect":
		// Browser callback for the TRAE login redirect flow: TRAE sends the user
		// here after login (no API key — the browser navigates directly). Safety
		// comes from the one-time pending session keyed by the echoed trace id.
		s.handleTraeRedirect(w, r)
	case strings.HasPrefix(path, "/admin/raccoon/"):
		if !s.authorizedAdmin(w, r) {
			return
		}
		s.handleRaccoonAdmin(w, r)
	case strings.HasPrefix(path, "/admin/trae/"):
		if !s.authorizedAdmin(w, r) {
			return
		}
		s.handleTraeAdmin(w, r)
	case strings.HasPrefix(path, "/admin/geminiweb/"):
		if !s.authorizedAdmin(w, r) {
			return
		}
		s.handleGeminiWebAdmin(w, r)
	default:
		writeJSON(w, 404, openai.ErrorJSON("not_found", "not found"))
	}
}

// omniMountPrefix 是网关在宿主服务里的挂载前缀。单引擎版本直接挂在根路径，
// 因此为空；保留这个常量让图片 URL 与请求记录路径共用同一处定义。
const omniMountPrefix = ""

// serveLogged 包裹一次 chat/responses 调用：注入路由记账、捕获状态码与首字节
// 时间，出口把结果写进请求记录。未注入记录器时退化为直接调用（零开销）。
func (s *Server) serveLogged(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	// 把调用方用的对外地址注入上下文：生图类 provider（geminiweb）要拼出客户端
	// 真能取到的图片 URL。放在这里是因为所有 AI 端点都经过它，而只有这一层知道
	// 自己被怎么寻址。前缀要补回来——本服务收到的是 StripPrefix 之后的路径。
	r = r.WithContext(provider.WithImageBase(r.Context(), util.PublicBase(r)+omniMountPrefix))
	if s.reqlog == nil && s.usage == nil {
		fn(w, r)
		return
	}
	start := time.Now()
	id := reqlog.NewRequestID()
	meta := &gateway.ReqMeta{}
	// 把入站密钥的收窄范围带进网关：入口的 allowModel 只校验主目标，而引流链
	// 的后续目标 / 全局 cfg.Fallback 的候选不走那道校验，只能靠网关侧按这个
	// 范围过滤兜底候选（见 gateway.tryFallback）。keyScope 返回的零值 = 全量。
	if k, _ := s.keyScope(r); len(k.Providers) > 0 {
		meta.KeyProviders = k.Providers
	}
	r = r.WithContext(gateway.WithReqMeta(r.Context(), meta))
	lw := &logWriter{ResponseWriter: w, start: start}
	if s.reqlog != nil {
		s.reqlog.Begin()
	}
	fn(lw, r)

	status := lw.status
	if status == 0 {
		status = http.StatusOK
	}
	// gateway 可在流式失败时覆盖观测状态码（HTTP 头已发 200 改不了，用 5xx 观测）
	// 与 outcome（stream_error / interrupted），见 gateway.markStreamFailure。
	if meta.Status != 0 {
		status = meta.Status
	}
	outcome := outcomeOf(status)
	if meta.Outcome != "" {
		outcome = meta.Outcome
	}
	ev := reqlog.Event{
		Time:       start,
		RequestID:  id,
		Path:       omniMountPrefix + r.URL.Path,
		Provider:   meta.Provider,
		Model:      meta.Model,
		Account:    meta.Account,
		Status:     status,
		OK:         status >= 200 && status < 300 && outcome == reqlog.OutcomeSuccess,
		Outcome:    outcome,
		DurationMs: time.Since(start).Milliseconds(),
		TTFBMs:     lw.ttfbMs(),
	}
	if s.clientInfo != nil && s.clientInfo() {
		ev.ClientIP = clientIP(r)
		ev.UserAgent = r.UserAgent()
	}
	// 上游回报的 OpenAI 格式 usage（raccoon 等）：填 Token / 思考列。
	if u := meta.Usage; u != nil {
		ev.PromptTokens = int64(u.PromptTokens)
		ev.CompletionTokens = int64(u.CompletionTokens)
		ev.TotalTokens = int64(u.TotalTokens)
		ev.ReasoningTokens = int64(u.ReasoningTokens)
		if u.CachedTokens > 0 {
			ev.CacheHitTokens = int64(u.CachedTokens)
			if miss := u.PromptTokens - u.CachedTokens; miss > 0 {
				ev.CacheMissTokens = int64(miss)
			}
		}
	}
	// 实际透传给上游的思考档位（reasoning_effort）。
	ev.ReasoningEffort = meta.Effort
	// 上游产出过思考内容但未回报 token 数时（runable），至少记录「有思考」。
	ev.Reasoning = meta.Reasoning
	if s.reqlog != nil {
		s.reqlog.Record(ev)
	}
	// 用量页：把 OmniGate 请求也计入共享的用量记录器（realm=provider、uid=账号），
	// 让「所有 AI 请求」在面板「用量」页也可见，与「请求记录」页口径一致。
	if s.usage != nil {
		s.recordUsage(meta, ev)
	}
}

// recordUsage 把一次 OmniGate 请求写进共享的用量记录器（面板「用量」页）。
//
// 维度映射与「请求记录」页对齐，便于两页对照：
//   - realm  = provider 名（raccoon / runable / …）；缺失回退 "omni"
//   - uid    = 实际服务账号 label；缺失回退 "omni"
//   - model  = 展示模型名（带 provider 前缀，如 runable/zai/glm-5.3-flash）
//
// token/cache/延迟取自 serveLogged 已算好的 ev；ok 用 ev.OK（2xx && outcome=success），
// 失败尝试同样计入请求数（与 WorkBuddy 侧口径一致）。
func (s *Server) recordUsage(meta *gateway.ReqMeta, ev reqlog.Event) {
	realm := meta.Provider
	if realm == "" {
		realm = "omni"
	}
	uid := meta.Account
	if uid == "" {
		uid = "omni"
	}
	d := usage.Delta{
		PromptTokens:     ev.PromptTokens,
		HasPromptTokens:  ev.PromptTokens > 0,
		CompletionTokens: ev.CompletionTokens,
		HasCompletion:    ev.CompletionTokens > 0,
		TotalTokens:      ev.TotalTokens,
		HasTotal:         ev.TotalTokens > 0,
		CacheHitTokens:   ev.CacheHitTokens,
		CacheMissTokens:  ev.CacheMissTokens,
		HasCacheTokens:   ev.CacheHitTokens > 0 || ev.CacheMissTokens > 0,
		LatencyMs:        ev.DurationMs,
		HasLatency:       true,
		// 客户端中断：上游 token 已消耗但 usage 拿不到，用量页单独提示。
		Interrupted: ev.Outcome == reqlog.OutcomeInterrupted,
	}
	// 生成速率：与 WorkBuddy 侧 tokensPerSecond 同口径——扣除 TTFB 后剩余窗口
	// 不足 200ms 视为「生成时长不可测」，退回端到端耗时（见 internal/server）。
	if ev.CompletionTokens > 0 && ev.DurationMs > 0 {
		gen := time.Duration(ev.DurationMs) * time.Millisecond
		if ttfb := time.Duration(ev.TTFBMs) * time.Millisecond; ttfb > 0 {
			if g := gen - ttfb; g >= 200*time.Millisecond {
				gen = g
			}
		}
		d.TokensPerSecond = float64(ev.CompletionTokens) / gen.Seconds()
		d.HasTPS = true
	}
	s.usage.Add(ev.Time, realm, uid, meta.Model, d, ev.OK)
}

// outcomeOf 把 HTTP 状态码映射成请求记录口径（与 WorkBuddy 侧一致：2xx 记
// success，其余记 http_error）。
func outcomeOf(status int) string {
	if status >= 200 && status < 300 {
		return reqlog.OutcomeSuccess
	}
	return reqlog.OutcomeHTTPError
}

// clientIP 取调用来源 IP：优先反向代理写入的 X-Forwarded-For 首段 / X-Real-IP，
// 退回 RemoteAddr 主机部分。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// logWriter 记录响应状态码与首字节时间，供请求记录使用；透传 Flush 以便 SSE
// 流式输出照常逐帧下发。
type logWriter struct {
	http.ResponseWriter
	start     time.Time
	status    int
	firstByte time.Time
}

func (lw *logWriter) WriteHeader(code int) {
	if lw.status == 0 {
		lw.status = code
	}
	lw.ResponseWriter.WriteHeader(code)
}

func (lw *logWriter) Write(b []byte) (int, error) {
	if lw.status == 0 {
		lw.status = http.StatusOK
	}
	if lw.firstByte.IsZero() {
		lw.firstByte = time.Now()
	}
	return lw.ResponseWriter.Write(b)
}

func (lw *logWriter) Flush() {
	if f, ok := lw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (lw *logWriter) ttfbMs() int64 {
	if lw.firstByte.IsZero() {
		return 0
	}
	return lw.firstByte.Sub(lw.start).Milliseconds()
}

// apiKeyFrom 取请求携带的网关密钥（Authorization: Bearer / x-api-key 两种都认）。
func apiKeyFrom(r *http.Request) string {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

// keyScope 解析请求携带的网关密钥，返回该密钥的授权范围。未配置任何密钥时网关
// 本就开放，返回零值 APIKey（= 全量）。
//
// 面板自己那把密钥（cfg.PanelKey，= config.json 的 api_key）恒为全量：面板前端要用
// 它调 /omni/*，不能被任何收窄规则误伤。
func (s *Server) keyScope(r *http.Request) (config.APIKey, bool) {
	// cfg 可能为 nil（单测/探活用最小构造的 Server）：没有密钥配置就等于全量开放，
	// 不能因为取 keyScope 就把请求打成 panic。
	if s.cfg == nil {
		return config.APIKey{}, true
	}
	key := apiKeyFrom(r)
	if s.cfg.PanelKey != "" && key == s.cfg.PanelKey {
		return config.APIKey{}, true
	}
	if len(s.cfg.APIKeys) == 0 && s.cfg.PanelKey == "" {
		return config.APIKey{}, true
	}
	return s.cfg.APIKeys.Find(key)
}

// keyOK 报告请求是否携带了合法的网关密钥，**不写响应**——用于「同一端点按鉴权
// 程度给不同详细度」的场景（见 handleHealth）。未配置 api_key 时与 authorized
// 同口径：网关本就开放，直接视为通过。
func (s *Server) keyOK(r *http.Request) bool {
	_, ok := s.keyScope(r)
	return ok
}

// authorized enforces the optional API-key gate.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if s.keyOK(r) {
		return true
	}
	writeJSON(w, 401, openai.ErrorJSON("invalid_api_key", "invalid or missing API key"))
	return false
}

// authorizedAdmin 是管理端点专用鉴权：除了密钥有效，还必须是全量密钥。
// 发给客户端的收窄密钥（api_keys[].providers 非空）只能调用模型接口，
// 不能改全局映射、增删账号或操作上游登录态。
func (s *Server) authorizedAdmin(w http.ResponseWriter, r *http.Request) bool {
	k, ok := s.keyScope(r)
	if !ok {
		writeJSON(w, 401, openai.ErrorJSON("invalid_api_key", "invalid or missing API key"))
		return false
	}
	if k.Scoped() {
		writeJSON(w, 403, openai.ErrorJSON("admin_key_required",
			"this API key is scoped to providers ["+strings.Join(k.Providers, ", ")+"] and cannot access admin endpoints"))
		return false
	}
	return true
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	k, _ := s.keyScope(r) // 已过 authorized
	models := s.gw.ListModelsFor(ctx, k.Providers)
	body, _ := json.Marshal(openai.ModelList{Object: "list", Data: models})
	writeJSON(w, 200, body)
}

// allowModel 校验当前密钥是否有权使用该模型会路由到的供应商；越权写 403 并返回
// false。刻意不做静默改道（改打到有权的供应商）——那等于绕过收窄，客户端会以为
// 自己调的是 A 实际打到 B。也刻意不假装 404：排障时「不存在」比「无权限」更难查。
func (s *Server) allowModel(w http.ResponseWriter, r *http.Request, model string) bool {
	k, _ := s.keyScope(r) // 已过 authorized
	prov := s.gw.ProviderOf(model)
	if k.Allows(prov) {
		return true
	}
	writeJSON(w, http.StatusForbidden, openai.ErrorJSON("model_not_allowed",
		"api key is scoped to providers ["+strings.Join(k.Providers, ", ")+"]; requested model routes to provider "+prov))
	return false
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, 400, openai.ErrorJSON("bad_request", "failed to read body: "+err.Error()))
		return
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, openai.ErrorJSON("invalid_json", "invalid JSON body: "+err.Error()))
		return
	}
	if !s.allowModel(w, r, req.Model) {
		return
	}
	s.gw.HandleChat(r.Context(), &req, w)
}

// handleRaccoonAdmin serves /admin/raccoon/* — the browser-login flow and the
// account/balance/check-in management endpoints.
func (s *Server) handleRaccoonAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/raccoon/")
	switch {
	case r.Method == http.MethodGet && path == "authorize":
		provider := s.raccoonProviderOrDefault(r.URL.Query().Get("provider"))
		res, err := s.gw.RaccoonAuthorize(provider, r.URL.Query().Get("label"), r.URL.Query().Get("redirect_base"))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, res)
	case r.Method == http.MethodPost && path == "callback":
		var req struct {
			Provider    string `json:"provider"`
			Label       string `json:"label"`
			CallbackURL string `json:"callback_url"`
			Callback    string `json:"callback"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		cb := req.CallbackURL
		if cb == "" {
			cb = req.Callback
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := s.gw.RaccoonLoginCallback(ctx, s.raccoonProviderOrDefault(req.Provider), req.Label, cb)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "account": res})
	case r.Method == http.MethodPost && path == "token":
		var req struct {
			Provider     string `json:"provider"`
			Label        string `json:"label"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		res, err := s.gw.RaccoonSetToken(r.Context(), s.raccoonProviderOrDefault(req.Provider), req.Label, req.AccessToken, req.RefreshToken)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "account": res})
	case r.Method == http.MethodGet && path == "accounts":
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := s.gw.RaccoonAccounts(ctx, s.raccoonProviderOrDefault(r.URL.Query().Get("provider")))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"accounts": res})
	case r.Method == http.MethodPost && path == "checkin":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		res, err := s.gw.RaccoonCheckin(ctx, s.raccoonProviderOrDefault(req.Provider), req.Label)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"results": res})
	case r.Method == http.MethodPost && path == "remove":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		if req.Label == "" {
			s.adminError(w, fmt.Errorf("label 不能为空"))
			return
		}
		removed := s.gw.RaccoonRemoveAccount(s.raccoonProviderOrDefault(req.Provider), req.Label)
		s.writeValue(w, 200, map[string]any{"ok": true, "removed": removed})
	default:
		writeJSON(w, 404, openai.ErrorJSON("not_found", "unknown admin route"))
	}
}

// handleGeminiWebAdmin serves /admin/geminiweb/* — Gemini Web accounts are added
// by pasting the account's Google cookies (there is no OAuth redirect to
// capture), so this is a token-style flow.
func (s *Server) handleGeminiWebAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/geminiweb/")
	switch {
	case r.Method == http.MethodPost && path == "token":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
			Cookie   string `json:"cookie"`
			Token    string `json:"token"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		cookie := req.Cookie
		if cookie == "" {
			cookie = req.Token
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		res, err := s.gw.GeminiWebSetToken(ctx, s.geminiWebProviderOrDefault(req.Provider), req.Label, cookie)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "account": res})
	case r.Method == http.MethodGet && path == "accounts":
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		res, err := s.gw.GeminiWebAccounts(ctx, s.geminiWebProviderOrDefault(r.URL.Query().Get("provider")))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"accounts": res})
	case r.Method == http.MethodPost && path == "checkin":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		res, err := s.gw.GeminiWebCheckin(ctx, s.geminiWebProviderOrDefault(req.Provider), req.Label)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"results": res})
	case r.Method == http.MethodGet && path == "check-account":
		ctx, cancel := context.WithTimeout(r.Context(), 180*time.Second)
		defer cancel()
		res, err := s.gw.GeminiWebCheckAccounts(ctx, s.geminiWebProviderOrDefault(r.URL.Query().Get("provider")))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "accounts": res})
	case r.Method == http.MethodGet && path == "health-history":
		name := s.geminiWebProviderOrDefault(r.URL.Query().Get("provider"))
		total, records, err := s.gw.GeminiWebHealthHistory(name)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "total": total, "records": records})
	case r.Method == http.MethodPost && path == "remove":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		if req.Label == "" {
			s.adminError(w, fmt.Errorf("label 不能为空"))
			return
		}
		removed := s.gw.GeminiWebRemoveAccount(s.geminiWebProviderOrDefault(req.Provider), req.Label)
		s.writeValue(w, 200, map[string]any{"ok": true, "removed": removed})
	case (r.Method == http.MethodGet || r.Method == http.MethodPost) && path == "fingerprint":
		var req struct {
			Provider string `json:"provider"`
			Target   string `json:"target"`
			Sync     bool   `json:"sync"`
		}
		if r.Method == http.MethodPost {
			if err := decodeBody(r, &req); err != nil {
				s.adminError(w, err)
				return
			}
		}
		name := s.geminiWebProviderOrDefault(req.Provider)
		if r.Method == http.MethodPost {
			if req.Target != "" {
				if err := s.gw.GeminiWebSetFingerprintTarget(name, req.Target); err != nil {
					s.adminError(w, err)
					return
				}
			}
			if req.Sync {
				ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
				defer cancel()
				if err := s.gw.GeminiWebSyncChromeVersion(ctx, name); err != nil {
					s.adminError(w, err)
					return
				}
			}
		}
		st, err := s.gw.GeminiWebFingerprintStatus(name)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "fingerprint": st})
	case r.Method == http.MethodGet && path == "gems":
		name := s.geminiWebProviderOrDefault(r.URL.Query().Get("provider"))
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		gems, err := s.gw.GeminiWebListGems(ctx, name, r.URL.Query().Get("label"))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "gems": gems})
	case r.Method == http.MethodPost && path == "gems":
		var req struct {
			Provider    string `json:"provider"`
			Label       string `json:"label"`
			Action      string `json:"action"`
			ID          string `json:"id"`
			Name        string `json:"name"`
			Prompt      string `json:"prompt"`
			Description string `json:"description"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		name := s.geminiWebProviderOrDefault(req.Provider)
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		switch strings.ToLower(strings.TrimSpace(req.Action)) {
		case "create":
			id, err := s.gw.GeminiWebCreateGem(ctx, name, req.Label, req.Name, req.Prompt, req.Description)
			if err != nil {
				s.adminError(w, err)
				return
			}
			s.writeValue(w, 200, map[string]any{"ok": true, "id": id})
		case "update":
			ok, err := s.gw.GeminiWebUpdateGem(ctx, name, req.Label, req.ID, req.Name, req.Prompt, req.Description)
			if err != nil {
				s.adminError(w, err)
				return
			}
			s.writeValue(w, 200, map[string]any{"ok": true, "updated": ok})
		case "delete":
			ok, err := s.gw.GeminiWebDeleteGem(ctx, name, req.Label, req.ID)
			if err != nil {
				s.adminError(w, err)
				return
			}
			s.writeValue(w, 200, map[string]any{"ok": true, "deleted": ok})
		default:
			s.adminError(w, fmt.Errorf("未知的 action %q（create/update/delete）", req.Action))
		}
	case r.Method == http.MethodGet && path == "gemmaps":
		name := s.geminiWebProviderOrDefault(r.URL.Query().Get("provider"))
		mappings, err := s.gw.GeminiWebListGemMappings(name)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "mappings": mappings})
	case r.Method == http.MethodPost && path == "gemmaps":
		var req struct {
			Provider  string `json:"provider"`
			Action    string `json:"action"`
			Model     string `json:"model"`
			GemID     string `json:"gem_id"`
			BaseModel string `json:"base_model"`
			AccountID string `json:"account_id"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		name := s.geminiWebProviderOrDefault(req.Provider)
		switch strings.ToLower(strings.TrimSpace(req.Action)) {
		case "set":
			if err := s.gw.GeminiWebSetGemMapping(name, req.Model, req.GemID, req.BaseModel, req.AccountID); err != nil {
				s.adminError(w, err)
				return
			}
			s.writeValue(w, 200, map[string]any{"ok": true})
		case "delete":
			ok, err := s.gw.GeminiWebDeleteGemMapping(name, req.Model)
			if err != nil {
				s.adminError(w, err)
				return
			}
			s.writeValue(w, 200, map[string]any{"ok": true, "deleted": ok})
		default:
			s.adminError(w, fmt.Errorf("未知的 action %q（set/delete）", req.Action))
		}
	case r.Method == http.MethodGet && path == "chats":
		name := s.geminiWebProviderOrDefault(r.URL.Query().Get("provider"))
		recent, _ := strconv.Atoi(r.URL.Query().Get("recent"))
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		chats, err := s.gw.GeminiWebListChats(ctx, name, r.URL.Query().Get("label"), recent)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "chats": chats})
	case r.Method == http.MethodPost && path == "chats/delete":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
			ID       string `json:"id"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		ok, err := s.gw.GeminiWebDeleteChat(ctx, s.geminiWebProviderOrDefault(req.Provider), req.Label, req.ID)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "deleted": ok})
	case r.Method == http.MethodPost && path == "chats/cleanup":
		var req struct {
			Provider   string  `json:"provider"`
			Label      string  `json:"label"`
			KeepHours  float64 `json:"keep_hours"`
			SkipPinned *bool   `json:"skip_pinned"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		if req.KeepHours <= 0 {
			req.KeepHours = 24
		}
		skipPinned := true
		if req.SkipPinned != nil {
			skipPinned = *req.SkipPinned
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		res, err := s.gw.GeminiWebCleanupChats(ctx, s.geminiWebProviderOrDefault(req.Provider), req.Label, req.KeepHours, skipPinned)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "result": res})
	case r.Method == http.MethodPost && path == "keepalive":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
		defer cancel()
		res, err := s.gw.GeminiWebKeepAlive(ctx, s.geminiWebProviderOrDefault(req.Provider), req.Label)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"results": res})
	default:
		writeJSON(w, 404, openai.ErrorJSON("not_found", "unknown admin route"))
	}
}

// handleModelMapping serves /admin/model-mapping — the global alias → target
// model table (ported from gemini2api's /admin/model-mapping router). GET lists,
// POST sets ({alias,target}), DELETE {alias} removes. Changes take effect on the
// next request (route() consults the table live).
//
// POST 还接受 enabled=false 停用一条规则（别名 + 目标链都留着，只是路由时当它
// 不存在）。引流链是硬规则——别名优先于 routing.prefer（见 gateway.route），一旦
// 配上就处处盖过 prefers；停用开关是运维唯一的「先摘掉、别删配置」退路。
func (s *Server) handleModelMapping(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeValue(w, 200, map[string]any{
			"mappings": s.gw.ModelMappings().All(),
			"disabled": s.gw.ModelMappings().DisabledAliases(),
		})
	case http.MethodPost:
		var req struct {
			Alias   string   `json:"alias"`
			Target  string   `json:"target"`
			Targets []string `json:"targets"`
			Enabled *bool    `json:"enabled"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		// 只改开关、不动目标链：enabled 单独出现时按停用/启用处理。
		if req.Enabled != nil && len(req.Targets) == 0 && strings.TrimSpace(req.Target) == "" {
			if !s.gw.SetModelMappingEnabled(req.Alias, *req.Enabled) {
				s.adminError(w, fmt.Errorf("映射「%s」不存在", strings.TrimSpace(req.Alias)))
				return
			}
			s.writeValue(w, 200, map[string]any{
				"ok": true,
				"mappings": s.gw.ModelMappings().All(),
				"disabled": s.gw.ModelMappings().DisabledAliases(),
			})
			return
		}
		targets := req.Targets
		if len(targets) == 0 && strings.TrimSpace(req.Target) != "" {
			targets = []string{req.Target}
		}
		if err := s.gw.SetModelMappingTargets(req.Alias, targets); err != nil {
			s.adminError(w, err)
			return
		}
		// 保存目标链时若同时带了 enabled，一并应用（新建规则默认启用，带 false
		// 进来 = 存一份先不生效的配置）。
		if req.Enabled != nil {
			s.gw.SetModelMappingEnabled(req.Alias, *req.Enabled)
		}
		s.writeValue(w, 200, map[string]any{
			"ok": true,
			"mappings": s.gw.ModelMappings().All(),
			"disabled": s.gw.ModelMappings().DisabledAliases(),
		})
	case http.MethodDelete:
		alias := strings.TrimPrefix(r.URL.Path, "/admin/model-mapping/")
		if alias == r.URL.Path || alias == "" {
			alias = r.URL.Query().Get("alias")
		}
		if strings.TrimSpace(alias) == "" {
			s.adminError(w, fmt.Errorf("alias 不能为空"))
			return
		}
		if !s.gw.DeleteModelMapping(alias) {
			s.adminError(w, fmt.Errorf("映射「%s」不存在", alias))
			return
		}
		s.writeValue(w, 200, map[string]any{
			"ok": true,
			"mappings": s.gw.ModelMappings().All(),
			"disabled": s.gw.ModelMappings().DisabledAliases(),
		})
	default:
		s.adminError(w, fmt.Errorf("不支持的请求方法 %s", r.Method))
	}
}

// providerOrDefault resolves the provider for a per-type admin route: an
// explicit name wins, otherwise prefer the default provider when it has the
// requested type, otherwise the first configured provider of that type,
// otherwise the first configured provider.
func (s *Server) providerOrDefault(name, typ string) string {
	if name != "" {
		return name
	}
	if s.cfg.DefaultProvider != "" {
		if p := s.cfg.Provider(s.cfg.DefaultProvider); p != nil && p.Type == typ {
			return s.cfg.DefaultProvider
		}
	}
	for _, p := range s.cfg.Providers {
		if p.Type == typ {
			return p.Name
		}
	}
	if len(s.cfg.Providers) > 0 {
		return s.cfg.Providers[0].Name
	}
	return ""
}

// geminiWebProviderOrDefault resolves the provider for /admin/geminiweb/*.
func (s *Server) geminiWebProviderOrDefault(name string) string {
	return s.providerOrDefault(name, "geminiweb")
}

// handleRaccoonRedirect is the unauthenticated browser callback for the
// web-redirect (auto-capture) authorization flow. The 浣熊 authorize page opens
// "<redirect>?state=..&authorization_code=.."; we validate the pending session,
// exchange the code and render a small result page.
func (s *Server) handleRaccoonRedirect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	code := strings.TrimSpace(q.Get("authorization_code"))
	if code == "" {
		code = strings.TrimSpace(q.Get("code"))
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.gw.RaccoonCompleteRedirect(ctx, state, code)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, authRedirectPage(false, err.Error()))
		return
	}
	name := res.Label
	if res.Name != "" {
		name = res.Name + "（" + res.Label + "）"
	}
	_, _ = io.WriteString(w, authRedirectPage(true, "账号已添加："+name))
}

// handleTraeRedirect is the unauthenticated browser callback for the TRAE login
// redirect flow. TRAE sends the browser here after login with refreshToken /
// userInfo / loginTraceID in the query; we validate the pending session (matched
// by the echoed trace id), exchange the token and render a result page.
func (s *Server) handleTraeRedirect(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := strings.TrimSpace(q.Get("state"))
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.gw.TraeCompleteRedirect(ctx, s.traeProviderOrDefault(q.Get("provider")), state, r.URL.RawQuery)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, authRedirectPage(false, err.Error()))
		return
	}
	name := res.Label
	if res.Name != "" {
		name = res.Name + "（" + res.Label + "）"
	}
	_, _ = io.WriteString(w, authRedirectPage(true, "账号已添加："+name))
}

// authRedirectPage renders a tiny self-contained result page (inline styles,
// no external resources) shown in the tab the authorize page opened.
func authRedirectPage(ok bool, text string) string {
	title, color, icon := "授权成功", "#3fb950", "✅"
	if !ok {
		title, color, icon = "授权失败", "#f85149", "⚠️"
	}
	return "<!doctype html><html lang=\"zh-CN\"><head><meta charset=\"utf-8\">" +
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">" +
		"<title>" + title + "</title></head>" +
		"<body style=\"margin:0;background:#0d1117;color:#e6edf3;font:15px/1.6 -apple-system,'Segoe UI','Microsoft YaHei',sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh\">" +
		"<div style=\"max-width:460px;padding:28px 30px;background:#161b22;border:1px solid #2a3240;border-radius:14px;text-align:center\">" +
		"<div style=\"font-size:40px\">" + icon + "</div>" +
		"<h1 style=\"margin:10px 0 6px;font-size:19px;color:" + color + "\">" + title + "</h1>" +
		"<p style=\"margin:0;color:#b6c2cf;word-break:break-all\">" + htmlEscape(text) + "</p>" +
		"<p style=\"margin:16px 0 0;color:#7d8896;font-size:12.5px\">可以关闭本页，返回管理面板。</p>" +
		"</div></body></html>"
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;").Replace(s)
}

// raccoonProviderOrDefault resolves the provider for /admin/raccoon/*.
func (s *Server) raccoonProviderOrDefault(name string) string {
	return s.providerOrDefault(name, "raccoon")
}

// handleTraeAdmin serves /admin/trae/* — the browser-login flow (open the URL,
// then paste the callback link back) and account/balance/check-in management.
func (s *Server) handleTraeAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/trae/")
	switch {
	case r.Method == http.MethodGet && path == "authorize":
		res, err := s.gw.TraeAuthorize(s.traeProviderOrDefault(r.URL.Query().Get("provider")), r.URL.Query().Get("label"))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, res)
	case r.Method == http.MethodPost && path == "callback":
		var req struct {
			Provider    string `json:"provider"`
			State       string `json:"state"`
			CallbackURL string `json:"callback_url"`
			Callback    string `json:"callback"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		cb := req.CallbackURL
		if cb == "" {
			cb = req.Callback
		}
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := s.gw.TraeCompleteCallback(ctx, s.traeProviderOrDefault(req.Provider), req.State, cb)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"ok": true, "account": res})
	case r.Method == http.MethodGet && path == "accounts":
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		res, err := s.gw.TraeAccounts(ctx, s.traeProviderOrDefault(r.URL.Query().Get("provider")))
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"accounts": res})
	case r.Method == http.MethodPost && path == "checkin":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
		defer cancel()
		res, err := s.gw.TraeCheckin(ctx, s.traeProviderOrDefault(req.Provider), req.Label)
		if err != nil {
			s.adminError(w, err)
			return
		}
		s.writeValue(w, 200, map[string]any{"results": res})
	case r.Method == http.MethodPost && path == "remove":
		var req struct {
			Provider string `json:"provider"`
			Label    string `json:"label"`
		}
		if err := decodeBody(r, &req); err != nil {
			s.adminError(w, err)
			return
		}
		if req.Label == "" {
			s.adminError(w, fmt.Errorf("label 不能为空"))
			return
		}
		removed := s.gw.TraeRemoveAccount(s.traeProviderOrDefault(req.Provider), req.Label)
		s.writeValue(w, 200, map[string]any{"ok": true, "removed": removed})
	default:
		writeJSON(w, 404, openai.ErrorJSON("not_found", "unknown admin route"))
	}
}

// traeProviderOrDefault resolves the provider for /admin/trae/*.
func (s *Server) traeProviderOrDefault(name string) string {
	return s.providerOrDefault(name, "trae")
}

func (s *Server) writeValue(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, 500, openai.ErrorJSON("internal_error", err.Error()))
		return
	}
	writeJSON(w, status, body)
}

func (s *Server) adminError(w http.ResponseWriter, err error) {
	writeJSON(w, 400, openai.ErrorJSON("admin_error", err.Error()))
}

func decodeBody(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// handleHealth serves GET /healthz (and GET /). 探活恒无鉴权（编排/负载均衡只
// 需要 2xx 语义），但**明细**只给持 api_key 的调用方：providers 里带全部账号
// 标签、cooldowns/sessions 是调度状态、recent 是运行日志、addresses 是内网地址、
// proxy_pools 是出站代理池——匿名放出去等于把账号清单和调度全貌挂在公网上
// （WorkBuddy 侧同口径：/metrics 要求 api_key，/healthz 只回计数）。
//
// 收窄密钥（按供应商授权、发给客户端的那些）同样只拿最小身份标识：它是客户端凭据，
// 只该能调自己那批模型，不该顺带读到账号清单与调度全貌。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Service", "omnigate")
	if k, ok := s.keyScope(r); !ok || k.Scoped() {
		// 最小身份标识：够探活与「是不是本服务」判别，不泄露任何运营数据。
		writeJSON(w, 200, []byte(`{"status":"ok","service":"omnigate"}`))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info := s.gw.Info(ctx)
	info["port"] = s.cfg.Server.Port
	info["requests"] = s.requests.Load()
	info["uptime_seconds"] = int(time.Since(s.started).Seconds())
	info["recent"] = s.gw.Logger().Recent()
	info["addresses"] = localIPs()
	if s.poolStatus != nil {
		if st := s.poolStatus(); st != nil {
			info["proxy_pools"] = st
		}
	}
	body, _ := json.Marshal(info)
	writeJSON(w, 200, body)
}

// handleImage serves a stored AI-generated image by id.
//
// Deliberately unauthenticated: the URL ends up inside a chat reply, and the
// fetch comes from a browser or an image proxy that has no API key. The id is the
// only secret — 128 random bits, unguessable — and the file is swept after
// imagegen.Retention. The id is validated by the store, so no crafted value can
// reach outside the image directory.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/images/")
	f, mime, err := s.images.Open(id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, openai.ErrorJSON("not_found", "image not found"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	// ServeContent adds Content-Length, honours Range, and closes the loop on
	// conditional requests. No modtime is passed: the id is unique per image, so
	// revalidation buys nothing.
	http.ServeContent(w, r, id, time.Time{}, f)
}

// ListenAndServe blocks serving requests.
func (s *Server) ListenAndServe() error { return s.http.ListenAndServe() }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func localIPs() []string {
	ips := []string{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return []string{"127.0.0.1"}
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.To4() == nil {
				continue
			}
			ips = append(ips, ip.String())
		}
	}
	if len(ips) == 0 {
		ips = append(ips, "127.0.0.1")
	}
	return ips
}
