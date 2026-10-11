// tenant.go 多租户：面板角色 → 账号可见范围 / 写权限 的判定，以及自助注册。
//
// 三种视角（由面板会话角色决定）：
//   - admin   全权：看得到也运维得了全部账号，并能改平台配置；
//   - viewer  只读全部：看得到全部账号（含别人的标签），但不能写；
//   - user    普通用户：只看得到/运维得了**自己拥有**的账号（归属见 ownership.go），
//     平台配置页一律 403；
//   - 未登录  访客：只看到匿名统计（账号数/健康度）与模型目录，看不到任何账号明细。
//
// 归属过滤统一走 ownFilter：admin/viewer 返回 all=true（不过滤），其余返回
// 「可见账号键集合」（user = 自己拥有的；访客 = 空集）。
package panel

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/logfmt"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pool"
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
//   - 否则 owned 是「可见账号键」集合（wb:<uid> / omni:<provider>/<label>）；
//     普通 user = 自己拥有的；未登录访客 = 空集（只看匿名统计）。
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

// canSeeWB 当前请求是否有权查看/运维该 WorkBuddy 账号。
func (p *Panel) canSeeWB(r *http.Request, uid string) bool {
	owned, all := p.ownFilter(r)
	return all || owned[wbOwnerKey(uid)]
}

// canSeeOmni 当前请求是否有权查看/运维该 OmniGate 账号。
func (p *Panel) canSeeOmni(r *http.Request, provider, label string) bool {
	owned, all := p.ownFilter(r)
	return all || owned[omniOwnerKey(provider, label)]
}

// requireWB 无权时写 403 并返回 false（各 WorkBuddy 单号运维入口共用）。
func (p *Panel) requireWB(w http.ResponseWriter, r *http.Request, uid string) bool {
	if p.canSeeWB(r, uid) {
		return true
	}
	writeErr(w, http.StatusForbidden, "not_your_account")
	return false
}

// requireOmni 无权时写 403 并返回 false（各 OmniGate 单号运维入口共用）。
func (p *Panel) requireOmni(w http.ResponseWriter, r *http.Request, provider, label string) bool {
	if p.canSeeOmni(r, provider, label) {
		return true
	}
	writeErr(w, http.StatusForbidden, "not_your_account")
	return false
}

// tenant 中间件：要求「可写账号」的登录身份——admin（全权）或 user（仅自己）。
// viewer 与未登录分别 403 / 401；具体归属校验由各 handler 用 requireWB/requireOmni。
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

// privileged 中间件：平台配置类只读接口——要求 admin 或 viewer（普通 user 与
// 未登录访客一律 403）。这些接口即便脱敏也会暴露全部供应商/账号标签，不属于
// 「自己的账号」范畴。
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

// wbAllow 返回 WorkBuddy 批量操作的 uid 过滤函数：admin/viewer → nil（全量），
// 普通 user → 只看自己名下的账号。
func (p *Panel) wbAllow(r *http.Request) func(uid string) bool {
	owned, all := p.ownFilter(r)
	if all {
		return nil
	}
	return func(uid string) bool { return owned[wbOwnerKey(uid)] }
}

// ownedOmniAccounts 返回当前请求拥有的 OmniGate 账号 (provider,label) 列表；
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

// tagWBOwner 给新加的 WorkBuddy 账号打归属（仅普通 user；管理员加的号保持无主）。
// uid 已被别人拥有时不覆盖（uid 唯一，理论上不会发生）。
func (p *Panel) tagWBOwner(r *http.Request, uid string) {
	if uid == "" || roleOf(r) != panelauth.RoleUser {
		return
	}
	key := wbOwnerKey(uid)
	if cur := p.ownership.Owner(key); cur != "" && cur != usernameOf(r) {
		return
	}
	p.ownership.Set(key, usernameOf(r))
}

// ---------------------------------------------------------------------------
// 账号明细过滤（列表 / 请求记录 / 运行日志）
// ---------------------------------------------------------------------------



// visibleWB 返回当前请求可见的 WorkBuddy 账号快照（admin/viewer 全部；
// 普通 user 仅自己的；未登录访客为空）。
func (p *Panel) visibleWB(r *http.Request) []pool.Status {
	owned, all := p.ownFilter(r)
	all2 := p.cfg.Pool.List()
	if all {
		return all2
	}
	out := make([]pool.Status, 0, len(all2))
	for _, st := range all2 {
		if owned[wbOwnerKey(st.UID)] {
			out = append(out, st)
		}
	}
	return out
}

// ownedTokens 当前请求可见账号的「日志/记录匹配词」：WorkBuddy 的 uid8 与昵称、
// OmniGate 的 provider/label。用于过滤运行日志与请求记录（这些行只有标签，
// 没有完整 uid）。
func (p *Panel) ownedTokens(r *http.Request) (uid8 map[string]bool, omni map[string]bool) {
	owned, all := p.ownFilter(r)
	uid8 = map[string]bool{}
	omni = map[string]bool{}
	if all {
		return uid8, omni
	}
	for k := range owned {
		if s, ok := strings.CutPrefix(k, "wb:"); ok {
			uid8[logfmt.UID8(s)] = true
			if a := p.cfg.Pool.AuthByUID(s); a != nil && strings.TrimSpace(a.Nickname) != "" {
				uid8[strings.TrimSpace(a.Nickname)] = true
			}
		} else if s, ok := strings.CutPrefix(k, "omni:"); ok {
			omni[s] = true // "provider/label"
		}
	}
	return uid8, omni
}

// reqlogVisible 返回请求记录的可见判定；all=true（admin/viewer）时返回 nil（不过滤）。
func (p *Panel) reqlogVisible(r *http.Request) (fn func(reqlog.Event) bool, all bool) {
	_, all = p.ownFilter(r)
	if all {
		return nil, true
	}
	uid8, omni := p.ownedTokens(r)
	return func(e reqlog.Event) bool {
		if e.Provider == "" {
			// WorkBuddy 行：Account = "昵称(uid8)" 或裸 uid8。
			if e.Account == "" {
				return false
			}
			if uid8[e.Account] {
				return true
			}
			for u := range uid8 {
				if strings.HasSuffix(e.Account, "("+u+")") {
					return true
				}
			}
			return false
		}
		return omni[e.Provider+"/"+e.Account]
	}, false
}

// logLineVisible 返回运行日志行的可见判定；all=true 时返回 nil（不过滤）。
// 匹配口径：行文本里出现了自己账号的 uid8 / 昵称 / provider/label 之一。
func (p *Panel) logLineVisible(r *http.Request) (fn func(string) bool, all bool) {
	_, all = p.ownFilter(r)
	if all {
		return nil, true
	}
	uid8, omni := p.ownedTokens(r)
	return func(text string) bool {
		for u := range uid8 {
			if u != "" && strings.Contains(text, u) {
				return true
			}
		}
		for k := range omni {
			if strings.Contains(text, k) {
				return true
			}
		}
		return false
	}, false
}



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

// allow 是否放行该来源的一次注册；不放行时返回需等待的时长。
func (l *regLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// 顺手回收过期窗口，防 map 无限增长（面板常驻进程）。
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
//
// 公开端点（无鉴权中间件）：未配置面板账号（无管理员）时拒绝——自助注册只在
// 已有管理员的实例上有意义；管理员可用 allow_registration=false 关闭。
func (p *Panel) register(w http.ResponseWriter, r *http.Request) {
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
	// 注册即登录：直接下发会话 Cookie（省一次输入）。
	sess, token, err := p.cfg.PanelAuth.Login(username, body.Password, ip)
	if err != nil {
		// 极少见（刚写入即校验失败）：不阻断注册，让前端回登录页。
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
