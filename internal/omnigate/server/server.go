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
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
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
}

// New builds a Server.
func New(cfg *config.Config, gw *gateway.Gateway, log *logx.Logger) *Server {
	s := &Server{cfg: cfg, gw: gw, log: log, started: time.Now()}
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
		s.serveLogged(w, r, s.handleChat)
	case r.Method == http.MethodPost && (path == "/v1/responses" || path == "/responses"):
		if !s.authorized(w, r) {
			return
		}
		s.serveLogged(w, r, s.handleResponses)
	case r.Method == http.MethodGet && path == "/healthz":
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && path == "/":
		s.handleHealth(w, r)
	case r.Method == http.MethodGet && path == "/admin/raccoon/redirect":
		// Browser callback for the web-redirect (auto-capture) flow: no API key
		// here — the browser navigates to it directly. Safety comes from the
		// one-time, state-validated authorization session.
		s.handleRaccoonRedirect(w, r)
	case strings.HasPrefix(path, "/admin/raccoon/"):
		if !s.authorized(w, r) {
			return
		}
		s.handleRaccoonAdmin(w, r)
	default:
		writeJSON(w, 404, openai.ErrorJSON("not_found", "not found"))
	}
}

// omniMountPrefix 是 OmniGate 在本服务里的挂载前缀（见 cmd/server 的
// http.StripPrefix("/omni", …)）。请求记录里补回前缀，让 /omni/* 路径可读、
// 且与 WorkBuddy 的 /v1/* 区分开。
const omniMountPrefix = "/omni"

// serveLogged 包裹一次 chat/responses 调用：注入路由记账、捕获状态码与首字节
// 时间，出口把结果写进请求记录。未注入记录器时退化为直接调用（零开销）。
func (s *Server) serveLogged(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	if s.reqlog == nil {
		fn(w, r)
		return
	}
	start := time.Now()
	id := reqlog.NewRequestID()
	meta := &gateway.ReqMeta{}
	r = r.WithContext(gateway.WithReqMeta(r.Context(), meta))
	lw := &logWriter{ResponseWriter: w, start: start}
	s.reqlog.Begin()
	fn(lw, r)

	status := lw.status
	if status == 0 {
		status = http.StatusOK
	}
	ok := status >= 200 && status < 300
	ev := reqlog.Event{
		Time:       start,
		RequestID:  id,
		Path:       omniMountPrefix + r.URL.Path,
		Provider:   meta.Provider,
		Model:      meta.Model,
		Account:    meta.Account,
		Status:     status,
		OK:         ok,
		Outcome:    outcomeOf(status),
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
	s.reqlog.Record(ev)
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

// authorized enforces the optional API-key gate.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	if len(s.cfg.APIKeys) == 0 {
		return true
	}
	key := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		key = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	if key == "" {
		key = r.Header.Get("x-api-key")
	}
	for _, k := range s.cfg.APIKeys {
		if k != "" && k == key {
			return true
		}
	}
	writeJSON(w, 401, openai.ErrorJSON("invalid_api_key", "invalid or missing API key"))
	return false
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	models := s.gw.ListModels(ctx)
	body, _ := json.Marshal(openai.ModelList{Object: "list", Data: models})
	writeJSON(w, 200, body)
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
		_, _ = io.WriteString(w, raccoonRedirectPage(false, err.Error()))
		return
	}
	name := res.Label
	if res.Name != "" {
		name = res.Name + "（" + res.Label + "）"
	}
	_, _ = io.WriteString(w, raccoonRedirectPage(true, "账号已添加："+name))
}

// raccoonRedirectPage renders a tiny self-contained result page (inline styles,
// no external resources) shown in the tab the authorize page opened.
func raccoonRedirectPage(ok bool, text string) string {
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

// raccoonProviderOrDefault resolves the provider for /admin/raccoon/*: an
// explicit name wins, otherwise prefer the default provider when it is a
// raccoon one, otherwise the first raccoon provider configured.
func (s *Server) raccoonProviderOrDefault(name string) string {
	if name != "" {
		return name
	}
	if s.cfg.DefaultProvider != "" {
		if p := s.cfg.Provider(s.cfg.DefaultProvider); p != nil && p.Type == "raccoon" {
			return s.cfg.DefaultProvider
		}
	}
	for _, p := range s.cfg.Providers {
		if p.Type == "raccoon" {
			return p.Name
		}
	}
	if len(s.cfg.Providers) > 0 {
		return s.cfg.Providers[0].Name
	}
	return ""
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

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	info := s.gw.Info(ctx)
	info["port"] = s.cfg.Server.Port
	info["requests"] = s.requests.Load()
	info["uptime_seconds"] = int(time.Since(s.started).Seconds())
	info["recent"] = s.gw.Logger().Recent()
	info["addresses"] = localIPs()
	body, _ := json.Marshal(info)
	writeJSON(w, 200, body)
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
