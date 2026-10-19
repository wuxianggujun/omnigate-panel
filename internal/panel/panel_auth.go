// panel_auth.go 面板登录鉴权：与网关 api_key 完全独立的账号密码登录、服务端会话
// Cookie、角色（admin/viewer）、登录失败限流与 CSRF 防护。
//
// 认证（resolveAuth）：唯一入口是面板会话——浏览器用会话 Cookie，自动化用
// Authorization: Bearer <会话令牌>。面板**不接受**网关 api_key。
//
// 两种门（默认公开只读）：
//   - read：**只读**接口（GET）默认公开，无需登录；已登录则注入身份，未登录以匿名
//     身份放行。响应里的凭证由 maskSecrets 统一脱敏，匿名请求还会额外抹掉网关
//     自己的 api_key / api_keys（见 secret.go），所以「能看」不会泄漏可用密钥。
//   - auth/admin：**写**接口一律要求登录（admin 才能改配置/运维账号）。未登录时
//     前端只会看到只读视图，点写操作会被引导去登录。
//
// CSRF：会话 Cookie 走 SameSite=Strict + 非安全方法校验 Origin/Sec-Fetch-Site
// 同源；Bearer（会话令牌）不受 CSRF 影响（浏览器不会自动带上）。
package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// sessionCookieName 面板会话 Cookie 名（HttpOnly，仅 /panel 路径可见）。
const sessionCookieName = "omnigate_session"

// PanelAuthSection 面板鉴权配置（config.json 的 panel_auth 段），面板读写用。
type PanelAuthSection struct {
	SessionHours int              `json:"session_hours"`
	MaxFailures  int              `json:"max_failures"`
	LockMinutes  int              `json:"lock_minutes"`
	Users        []panelauth.User `json:"users"`
	// AllowRegistration 是否开放自助注册（nil = 未配置 = 允许）。与 cmd/server 的
	// PanelAuthConfig 同名字段一一对应，整段回写时不丢配置。
	AllowRegistration *bool `json:"allow_registration,omitempty"`
}

// RegistrationAllowed 自助注册是否开启（nil/未配置 = 允许）。
func (s PanelAuthSection) RegistrationAllowed() bool {
	return s.AllowRegistration == nil || *s.AllowRegistration
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

// read 中间件：只读接口（GET）默认公开。已登录则注入身份（前端据此显示写权限），
// 未登录以匿名身份放行（ar.ok=false）。凭证脱敏由各 handler 负责，匿名请求还会
// 额外抹掉网关 api_key / api_keys。
//
// 只用于安全方法（GET）；写接口仍走 auth/admin。
func (p *Panel) read(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ar := p.resolveAuth(r)
		next(w, r.WithContext(context.WithValue(r.Context(), authCtxKey, ar)))
	}
}

// authed 报告当前请求是否带有效面板会话（read 中间件注入）。
func authed(r *http.Request) bool { return authFromContext(r.Context()).ok }

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
		// 前端资源指纹：已经打开的页面据此发现「服务端已更新」并提示刷新。
		"build": panelBuild,
	}
	// 自助注册是否开放：前端据此决定登录门里是否显示「注册」入口。
	allowReg := false
	if configured && p.cfg.LoadPanelAuth != nil {
		if section, err := p.cfg.LoadPanelAuth(); err == nil {
			allowReg = section.RegistrationAllowed()
		}
	}
	resp["allow_registration"] = allowReg
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
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "csrf_origin_mismatch")
		return
	}
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
		"ok":                 true,
		"users":              users,
		"session_hours":      section.SessionHours,
		"max_failures":       section.MaxFailures,
		"lock_minutes":       section.LockMinutes,
		"allow_registration": section.RegistrationAllowed(),
	})
}

// setRegistration POST /panel/api/admin/registration：开关自助注册（仅管理员）。
func (p *Panel) setRegistration(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	var body struct {
		Allow bool `json:"allow"`
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
	allow := body.Allow
	section.AllowRegistration = &allow
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 自助注册已%s（操作人 %s）", map[bool]string{true: "开启", false: "关闭"}[allow], usernameOf(r))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "allow_registration": allow})
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
	// 归属清理：被删用户的账号变为无主（仅管理员可见），不会跟着账号残留。
	p.ownership.DeleteUser(username)
	log.Printf("panel: 面板账号 %q 已删除（其名下账号归属已清空）", username)
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
