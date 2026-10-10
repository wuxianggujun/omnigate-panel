// panel_auth.go 面板登录鉴权：与网关 api_key 完全独立的账号密码登录、服务端会话
// Cookie、角色（admin/viewer）、登录失败限流与 CSRF 防护。
//
// 认证（resolveAuth）：唯一入口是面板会话——浏览器用会话 Cookie，自动化用
// Authorization: Bearer <会话令牌>。面板**不再**接受网关 api_key（哪怕一个面板
// 账号都没配置）：未配置账号时所有 /panel/api/* 一律 401，须先在服务器执行
// `omnigate-panel -set-admin-password` 引导一个账号后才能进入面板。
//
// CSRF：会话 Cookie 走 SameSite=Strict + 非安全方法校验 Origin/Sec-Fetch-Site
// 同源；Bearer（会话令牌）不受 CSRF 影响（浏览器不会自动带上）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// sessionCookieName 面板会话 Cookie 名（HttpOnly，仅 /panel 路径可见）。
const sessionCookieName = "wb2api_session"

// PanelAuthSection 面板鉴权配置（config.json 的 panel_auth 段），面板读写用。
type PanelAuthSection struct {
	SessionHours int              `json:"session_hours"`
	MaxFailures  int              `json:"max_failures"`
	LockMinutes  int              `json:"lock_minutes"`
	Users        []panelauth.User `json:"users"`
}

// authMode 认证来源。面板只有一种：登录会话（Cookie 或 Bearer 令牌）。
type authMode string

const modeSession authMode = "session"

type authResult struct {
	session panelauth.Session
	mode    authMode
	ok      bool
}

type authCtxKeyType struct{}

var authCtxKey authCtxKeyType

func authFromContext(ctx context.Context) authResult {
	if v, ok := ctx.Value(authCtxKey).(authResult); ok {
		return v
	}
	return authResult{}
}

// resolveAuth 解析请求身份：只认面板会话（Cookie 或 Bearer 令牌）。未注入 store
// 或令牌无效时返回未认证（上层 401）——网关 api_key 不再能打开面板。
func (p *Panel) resolveAuth(r *http.Request) authResult {
	if p.cfg.PanelAuth == nil {
		return authResult{}
	}
	if tok := sessionToken(r); tok != "" {
		if s, ok := p.cfg.PanelAuth.Authenticate(tok); ok {
			return authResult{session: s, mode: modeSession, ok: true}
		}
	}
	return authResult{}
}

// auth 中间件：要求任意已认证用户；会话 Cookie 的非安全方法做同源校验。
func (p *Panel) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := p.resolveAuth(r)
		if !ar.ok {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if ar.mode == modeSession && unsafeMethod(r.Method) && !sameOrigin(r) {
			writeErr(w, http.StatusForbidden, "csrf_origin_mismatch")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authCtxKey, ar)))
	}
}

// admin 中间件：在 auth 基础上要求 admin 角色。
func (p *Panel) admin(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(func(w http.ResponseWriter, r *http.Request) {
		if !authFromContext(r.Context()).session.IsAdmin() {
			writeErr(w, http.StatusForbidden, "admin_required")
			return
		}
		next(w, r)
	})
}

// ---------------------------------------------------------------------------
// 会话端点
// ---------------------------------------------------------------------------

// sessionState GET /panel/api/session：前端启动时判定登录态。
func (p *Panel) sessionState(w http.ResponseWriter, r *http.Request) {
	ar := p.resolveAuth(r)
	configured := p.cfg.PanelAuth != nil && p.cfg.PanelAuth.HasUsers()
	resp := map[string]any{
		"ok":            true,
		"authenticated": ar.ok,
		"configured":    configured,
		"mode":          string(ar.mode),
	}
	if ar.ok {
		resp["username"] = ar.session.Username
		resp["role"] = ar.session.Role
	}
	// 会话模式下滑动续期 Cookie 的浏览器端有效期。
	if ar.ok && ar.mode == modeSession {
		if tok := sessionToken(r); tok != "" {
			p.setSessionCookie(w, r, tok)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// login POST /panel/api/session：用户名 + 密码登录，成功下发会话 Cookie。
func (p *Panel) login(w http.ResponseWriter, r *http.Request) {
	if p.cfg.PanelAuth == nil || !p.cfg.PanelAuth.HasUsers() {
		writeErr(w, http.StatusBadRequest, "panel_auth_not_configured")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	sess, token, err := p.cfg.PanelAuth.Login(strings.TrimSpace(body.Username), body.Password, authClientIP(r))
	if err != nil {
		var le *panelauth.LockedError
		if errors.As(err, &le) {
			secs := int(le.RetryAfter.Seconds()) + 1
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	p.setSessionCookie(w, r, token)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"username": sess.Username,
		"role":     sess.Role,
	})
}

// logout DELETE /panel/api/session：注销当前会话并清 Cookie。
func (p *Panel) logout(w http.ResponseWriter, r *http.Request) {
	if p.cfg.PanelAuth != nil {
		if tok := sessionToken(r); tok != "" {
			p.cfg.PanelAuth.Logout(tok)
		}
	}
	p.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// changeOwnPassword POST /panel/api/password：已登录用户修改自己的密码。
func (p *Panel) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	ar := authFromContext(r.Context())
	if ar.mode != modeSession {
		// 非会话身份（面板只有会话一种身份，此分支为防御）：改密只对登录账号开放。
		writeErr(w, http.StatusBadRequest, "no_panel_account")
		return
	}
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	// 校验当前密码：走与登录一致的限流口径（Verify），避免"改密码"成为绕过
	// 限流的校验口，也不在公司会话表里留下多余令牌。
	if _, err := p.cfg.PanelAuth.Verify(ar.session.Username, body.CurrentPassword, authClientIP(r)); err != nil {
		var le *panelauth.LockedError
		if errors.As(err, &le) {
			w.Header().Set("Retry-After", strconv.Itoa(int(le.RetryAfter.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid_credentials")
		return
	}
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	users, err := panelauth.SetPassword(section.Users, ar.session.Username, body.NewPassword)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	section.Users = users
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 账号管理（admin）
// ---------------------------------------------------------------------------

// listPanelUsers GET /panel/api/admin/users：账号列表（不含密码哈希）。
func (p *Panel) listPanelUsers(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadPanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	users := make([]panelauth.UserView, 0, len(section.Users))
	for _, u := range section.Users {
		users = append(users, panelauth.UserView{Username: u.Username, Role: u.Role})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"users":         users,
		"session_hours": section.SessionHours,
		"max_failures":  section.MaxFailures,
		"lock_minutes":  section.LockMinutes,
	})
}

// savePanelUser POST /panel/api/admin/users：新增/更新账号（{username,role,password}）。
// password 为空 = 仅改角色（保留原密码）。
func (p *Panel) savePanelUser(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	var body struct {
		Username string `json:"username"`
		Role     string `json:"role"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	if !panelauth.ValidRole(panelauth.Role(body.Role)) {
		writeErr(w, http.StatusBadRequest, panelauth.ErrBadRole.Error())
		return
	}
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	// 保护：不允许把当前登录管理员自己降级为 viewer（否则可能把唯一管理员降权锁死）。
	if body.Username == authFromContext(r.Context()).session.Username &&
		panelauth.Role(body.Role) != panelauth.RoleAdmin {
		writeErr(w, http.StatusBadRequest, "不能修改当前登录账号自己的角色")
		return
	}
	users, err := panelauth.UpsertUser(section.Users, strings.TrimSpace(body.Username), panelauth.Role(body.Role), body.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	section.Users = users
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// resetPanelUserPassword POST /panel/api/admin/users/password：重置他人密码。
func (p *Panel) resetPanelUserPassword(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	users, err := panelauth.SetPassword(section.Users, strings.TrimSpace(body.Username), body.Password)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	section.Users = users
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// deletePanelUser POST /panel/api/admin/users/delete：删除账号（{username}）。
func (p *Panel) deletePanelUser(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	var body struct {
		Username string `json:"username"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	username := strings.TrimSpace(body.Username)
	if username == authFromContext(r.Context()).session.Username {
		writeErr(w, http.StatusBadRequest, "不能删除当前登录账号自己")
		return
	}
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	users, err := panelauth.DeleteUser(section.Users, username)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	section.Users = users
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// Cookie / CSRF / 工具
// ---------------------------------------------------------------------------

func (p *Panel) setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	maxAge := 72 * 3600
	if p.cfg.PanelAuth != nil {
		maxAge = int(p.cfg.PanelAuth.SessionTTL().Seconds())
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/panel",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
}

func (p *Panel) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/panel",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secureRequest(r),
		SameSite: http.SameSiteStrictMode,
	})
}

// sessionToken 取浏览器 Cookie 或 Bearer 里的会话令牌。
func sessionToken(r *http.Request) string {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		return strings.TrimSpace(authz[len("Bearer "):])
	}
	return ""
}

// httpauthBearerOK 已并入 httpauth.VerifyBearer（网关/面板同口径常量时间比较）。

func unsafeMethod(m string) bool {
	switch m {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// sameOrigin 校验非安全方法来自同源（防 CSRF）。
//   - 有 Origin：主机名需匹配请求 Host / X-Forwarded-Host；
//   - 无 Origin：看 Sec-Fetch-Site（现代浏览器跨站会带 same-site/cross-site）。
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		switch r.Header.Get("Sec-Fetch-Site") {
		case "", "same-origin", "none":
			return true
		default:
			return false
		}
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := u.Hostname()
	if originHost == "" {
		originHost = u.Host
	}
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if h == "" {
			continue
		}
		// 先比完整 host（含端口），再比主机名（去端口）——反代可能省略默认端口，
		// 浏览器 Origin 有时带端口、有时不带，两种形态都接受。
		if strings.EqualFold(u.Host, h) {
			return true
		}
		hostOnly := h
		if hn, _, err := net.SplitHostPort(h); err == nil {
			hostOnly = hn
		}
		if strings.EqualFold(originHost, hostOnly) {
			return true
		}
	}
	return false
}

// secureRequest 判断是否 HTTPS（直连 TLS 或反代声明 X-Forwarded-Proto: https）。
func secureRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// authClientIP 取客户端 IP（优先 X-Forwarded-For 首段），仅供失败限流分桶。
func authClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	return dec.Decode(v)
}
