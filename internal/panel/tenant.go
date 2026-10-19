// tenant.go 多租户：面板角色 → 账号可见范围 / 写权限 的判定，以及自助注册。
//
// 归属键统一为 omni:<provider>/<label>。admin/viewer 看全部；user 只看自己
// 拥有的账号；未登录访客只看匿名统计与模型目录。
package panel

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// roleOf 当前请求的面板角色；未登录返回 ""。
func roleOf(r *http.Request) panelauth.Role {
	ar := authFromContext(r.Context())
	if !ar.ok {
		return ""
	}
	return ar.session.Role
}

// usernameOf 当前请求的登录用户名；未登录返回 ""。
func usernameOf(r *http.Request) string {
	ar := authFromContext(r.Context())
	if !ar.ok {
		return ""
	}
	return ar.session.Username
}

// ownFilter 当前请求的归属过滤。
//   - filterAll=true：不限制（admin / viewer）；
//   - 否则 owned 是「可见账号键」集合（omni:<provider>/<label>）。
func (p *Panel) ownFilter(r *http.Request) (owned map[string]bool, filterAll bool) {
	role := roleOf(r)
	if role == "" {
		return map[string]bool{}, false
	}
	if role.IsAdmin() || role == panelauth.RoleViewer {
		return nil, true
	}
	owned = map[string]bool{}
	for _, k := range p.ownership.KeysOf(usernameOf(r)) {
		owned[k] = true
	}
	return owned, false
}

// canSeeOmni 当前请求是否有权查看/运维该账号。
func (p *Panel) canSeeOmni(r *http.Request, provider, label string) bool {
	owned, all := p.ownFilter(r)
	return all || owned[omniOwnerKey(provider, label)]
}

// requireOmni 无权时写 403 并返回 false。
func (p *Panel) requireOmni(w http.ResponseWriter, r *http.Request, provider, label string) bool {
	if p.canSeeOmni(r, provider, label) {
		return true
	}
	writeErr(w, http.StatusForbidden, "not_your_account")
	return false
}

// tenant 中间件：要求「可写账号」的登录身份——admin（全权）或 user（仅自己）。
// viewer 与未登录分别 403 / 401；具体归属校验由各 handler 用 requireOmni。
func (p *Panel) tenant(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(func(w http.ResponseWriter, r *http.Request) {
		role := authFromContext(r.Context()).session.Role
		if !role.IsAdmin() && !role.IsUser() {
			writeErr(w, http.StatusForbidden, "tenant_required")
			return
		}
		next(w, r)
	})
}

// privileged 中间件：平台配置类只读接口——要求 admin 或 viewer。
func (p *Panel) privileged(next http.HandlerFunc) http.HandlerFunc {
	return p.auth(func(w http.ResponseWriter, r *http.Request) {
		role := authFromContext(r.Context()).session.Role
		if !role.IsAdmin() && role != panelauth.RoleViewer {
			writeErr(w, http.StatusForbidden, "forbidden")
			return
		}
		next(w, r)
	})
}

// ownedOmniAccounts 返回当前请求拥有的账号 (provider,label) 列表；
// admin/viewer 返回 nil（调用方走全量入口）。
func (p *Panel) ownedOmniAccounts(r *http.Request) [][2]string {
	owned, all := p.ownFilter(r)
	if all {
		return nil
	}
	var out [][2]string
	for k := range owned {
		s, ok := strings.CutPrefix(k, "omni:")
		if !ok {
			continue
		}
		prov, label, ok := strings.Cut(s, "/")
		if ok && prov != "" && label != "" {
			out = append(out, [2]string{prov, label})
		}
	}
	return out
}

// scopeDesc 批量操作日志里的范围描述（全量 / 指定用户）。
func scopeDesc(r *http.Request) string {
	role := roleOf(r)
	if role.IsAdmin() || role == panelauth.RoleViewer {
		return "全量"
	}
	if u := usernameOf(r); u != "" {
		return "user:" + u
	}
	return "匿名（无账号）"
}

// ownedTokens 当前请求可见账号的「日志/记录匹配词」：provider/label。
// 日志行只有标签，没有完整归属键。
func (p *Panel) ownedTokens(r *http.Request) map[string]bool {
	owned, all := p.ownFilter(r)
	omni := map[string]bool{}
	if all {
		return omni
	}
	for k := range owned {
		if s, ok := strings.CutPrefix(k, "omni:"); ok {
			omni[s] = true
		}
	}
	return omni
}

// reqlogVisible 返回请求记录的可见判定；all=true（admin/viewer）时返回 nil（不过滤）。
func (p *Panel) reqlogVisible(r *http.Request) (fn func(reqlog.Event) bool, all bool) {
	_, all = p.ownFilter(r)
	if all {
		return nil, true
	}
	omni := p.ownedTokens(r)
	return func(e reqlog.Event) bool {
		if e.Provider == "" || e.Account == "" {
			return false
		}
		return omni[e.Provider+"/"+e.Account]
	}, false
}

// logLineVisible 返回运行日志行的可见判定；all=true 时不过滤。
func (p *Panel) logLineVisible(r *http.Request) (fn func(string) bool, all bool) {
	_, all = p.ownFilter(r)
	if all {
		return nil, true
	}
	omni := p.ownedTokens(r)
	return func(text string) bool {
		for k := range omni {
			if strings.Contains(text, k) {
				return true
			}
		}
		return false
	}, false
}

// ---------------------------------------------------------------------------
// 自助注册
// ---------------------------------------------------------------------------

// regLimiter 注册限流：同一 IP 固定窗口内最多 max 次（防脚本批量注册）。
type regLimiter struct {
	mu     sync.Mutex
	m      map[string]*regWindow
	max    int
	window time.Duration
}

type regWindow struct {
	count int
	start time.Time
}

func newRegLimiter(max int, window time.Duration) *regLimiter {
	return &regLimiter{m: map[string]*regWindow{}, max: max, window: window}
}

func (l *regLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	for k, w := range l.m {
		if now.Sub(w.start) > l.window {
			delete(l.m, k)
		}
	}
	w := l.m[key]
	if w == nil {
		l.m[key] = &regWindow{count: 1, start: now}
		return true, 0
	}
	if w.count >= l.max {
		return false, l.window - now.Sub(w.start)
	}
	w.count++
	return true, 0
}

// register POST /panel/api/register：自助注册一个 user 角色账号并直接登录。
func (p *Panel) register(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		writeErr(w, http.StatusForbidden, "csrf_origin_mismatch")
		return
	}
	if p.cfg.LoadPanelAuth == nil || p.cfg.SavePanelAuth == nil || p.cfg.PanelAuth == nil {
		writeErr(w, http.StatusNotImplemented, "panel auth not available")
		return
	}
	if !p.cfg.PanelAuth.HasUsers() {
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
	section, err := p.cfg.LoadPanelAuth()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load panel auth: "+err.Error())
		return
	}
	if !section.RegistrationAllowed() {
		writeErr(w, http.StatusForbidden, "registration_disabled")
		return
	}
	ip := authClientIP(r)
	if ok, retry := p.regLimit.allow(ip); !ok {
		secs := int(retry.Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		writeErr(w, http.StatusTooManyRequests, "too_many_attempts")
		return
	}
	username := strings.TrimSpace(body.Username)
	users, err := panelauth.Register(section.Users, username, body.Password)
	if err != nil {
		status := http.StatusBadRequest
		if err == panelauth.ErrUserExists {
			status = http.StatusConflict
		}
		writeErr(w, status, err.Error())
		return
	}
	section.Users = users
	if _, err := p.cfg.SavePanelAuth(section); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	sess, token, err := p.cfg.PanelAuth.Login(username, body.Password, ip)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "username": username, "role": string(panelauth.RoleUser)})
		return
	}
	p.setSessionCookie(w, r, token)
	log.Printf("panel: 自助注册新用户 %q（来源 %s）", username, ip)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"username": sess.Username,
		"role":     string(sess.Role),
	})
}
