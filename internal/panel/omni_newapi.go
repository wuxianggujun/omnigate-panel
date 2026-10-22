// omni_newapi.go 面板 → New API（QuantumNous/new-api）供应商的接入编排。
//
// New API 站点的登录开了 Cloudflare Turnstile，服务端直连密码登录会被拦，所以
// 账号只能靠「浏览器登录 + 控制台脚本回传」建立：
//
//	POST /panel/api/omni/newapi/authorize   （登录用户）登记一次性 state，回登录页 + 脚本
//	POST /panel/api/omni/newapi/capture     （公开、state 校验）脚本把鉴权包 POST 回来
//	GET  /panel/api/omni/newapi/status      （登录用户）轮询抓取是否完成
//
// capture 走 cors 模式的 text/plain POST：text/plain 让它保持「简单请求」（无预检），
// 放开 CORS 头则让脚本能读到服务端的裁定（成功 / 具体拒绝原因），而不是只能靠面板轮询。
//
// 分工与 omni_accounts.go 一致：面板只做 HTTP 编排，真正落库在 main 注入的闭包里。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// omniNewAPIAuthorizeReq 面板「New API 浏览器登录」发起请求体。
type omniNewAPIAuthorizeReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
}

// omniNewAPIAuthorize 登记一次浏览器抓取会话，返回登录页地址与可复制的控制台脚本。
func (p *Panel) omniNewAPIAuthorize(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniNewAPIAuthorize == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req omniNewAPIAuthorizeReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Label = strings.TrimSpace(req.Label)
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.Provider == "" {
		writeErr(w, http.StatusBadRequest, "provider 不能为空")
		return
	}
	// label 可以留空 = 自动命名（回传时用站点用户名当账号名）。留空时发起阶段还
	// 不知道叫什么，认领不了，改在回传成功后用真实账号名落归属（见 takeNewAPIClaim）。
	if req.Label != "" && !p.claimOmniLabel(r, req.Provider, req.Label) {
		writeErr(w, http.StatusConflict, "账号名「"+req.Label+"」已被其他用户占用，请换一个")
		return
	}
	d, err := p.cfg.OmniNewAPIAuthorize(req.Provider, req.Label, req.APIKey)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	out, _ := toGeneric(d)
	m, _ := out.(map[string]any)
	stateID, _ := m["state"].(string)
	signin, _ := m["signin_url"].(string)
	if stateID == "" || signin == "" {
		writeErr(w, http.StatusBadGateway, "上游未返回登录信息")
		return
	}
	if req.Label == "" {
		p.rememberNewAPIClaim(stateID, req.Provider, r)
	}
	who := req.Label
	if who == "" {
		who = "（自动命名）"
	}
	authMode, _ := m["auth_mode"].(string)
	selfPath, _ := m["self_path"].(string)
	log.Printf("panel: New API 浏览器登录发起 %s/%s (auth=%s)", req.Provider, who, authMode)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"state":      stateID,
		"signin_url": signin,
		"label":      req.Label,
		"auth_mode":  authMode,
		"snippet":    newAPICaptureSnippet(panelPublicBase(r), stateID, selfPath, authMode == "cookie"),
	})
}

// omniNewAPICapturePreflight 回应浏览器（cors 模式）的 OPTIONS 预检。
func (p *Panel) omniNewAPICapturePreflight(w http.ResponseWriter, _ *http.Request) {
	setCaptureCORS(w)
	w.WriteHeader(http.StatusNoContent)
}

// omniNewAPICapture 接收浏览器脚本回传的鉴权包（公开端点，靠一次性 state 校验）。
func (p *Panel) omniNewAPICapture(w http.ResponseWriter, r *http.Request) {
	setCaptureCORS(w)
	if p.cfg.OmniNewAPICapture == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	stateID := strings.TrimSpace(r.URL.Query().Get("state"))
	if stateID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 state")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if !json.Valid(raw) {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	d, err := p.cfg.OmniNewAPICapture(stateID, raw)
	if err != nil {
		// 抓取失败不泄露内部细节，但把原因回给脚本控制台（cors 模式下可读）。
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 自动命名的账号到这一步才知道叫什么，此时给发起人落归属。
	if lbl := omniResultLabel(d); lbl != "" {
		p.takeNewAPIClaim(stateID, lbl)
	}
	log.Printf("panel: New API 浏览器登录回传完成 state=%s", stateID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": d})
}

// newapiClaim 一次「自动命名」抓取会话的发起人。
type newapiClaim struct {
	provider string
	owner    string
	at       time.Time
}

// newapiClaimTTL 与网关侧 newapiPendingTTL（15 分钟）对齐。
const newapiClaimTTL = 15 * time.Minute

// rememberNewAPIClaim 记下发起人，供回传后按真实账号名落归属。只有普通 user 需要
// （管理员/无主账号保持共享），其他角色直接忽略。
func (p *Panel) rememberNewAPIClaim(stateID, provider string, r *http.Request) {
	if stateID == "" || roleOf(r) != panelauth.RoleUser {
		return
	}
	owner := usernameOf(r)
	if owner == "" {
		return
	}
	p.newapiClaimsMu.Lock()
	defer p.newapiClaimsMu.Unlock()
	if p.newapiClaims == nil {
		p.newapiClaims = map[string]newapiClaim{}
	}
	for k, v := range p.newapiClaims {
		if time.Since(v.at) > newapiClaimTTL {
			delete(p.newapiClaims, k)
		}
	}
	p.newapiClaims[stateID] = newapiClaim{provider: provider, owner: owner, at: time.Now()}
}

// takeNewAPIClaim 用回传到的真实账号名给发起人落归属。label 已被别人占用时不动
// （不抢别人的账号），只记一条日志。
func (p *Panel) takeNewAPIClaim(stateID, label string) {
	p.newapiClaimsMu.Lock()
	c, ok := p.newapiClaims[stateID]
	delete(p.newapiClaims, stateID)
	p.newapiClaimsMu.Unlock()
	if !ok || c.owner == "" || label == "" || p.ownership == nil {
		return
	}
	key := omniOwnerKey(c.provider, label)
	if cur := p.ownership.Owner(key); cur != "" && cur != c.owner {
		log.Printf("panel: New API 自动命名归属冲突 %s（已属 %s，跳过）", key, cur)
		return
	}
	p.ownership.Set(key, c.owner)
}

// omniResultLabel 取抓取结果的最终账号名（自动命名时与发起请求里的空 label 不同）。
func omniResultLabel(d any) string {
	g, err := toGeneric(d)
	if err != nil {
		return ""
	}
	m, _ := g.(map[string]any)
	if m == nil {
		return ""
	}
	s, _ := m["label"].(string)
	return strings.TrimSpace(s)
}

// omniNewAPICaptureStatus 轮询抓取是否完成（前端在发起后轮询）。
func (p *Panel) omniNewAPICaptureStatus(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniNewAPICaptureStatus == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	stateID := strings.TrimSpace(r.URL.Query().Get("state"))
	if stateID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 state")
		return
	}
	pending, err := p.cfg.OmniNewAPICaptureStatus(stateID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": pending})
}

// omniAccountRefresh 统一刷新：轮换某 provider（或单账号）的会话 access_token。
func (p *Panel) omniAccountRefresh(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniRefresh == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	req, ok := decodeOmniAccountReq(w, r)
	if !ok {
		return
	}
	if req.Label == "" {
		// 整 provider 统一刷新会轮换**所有**账号的会话，且结果里带着别人的标签，
		// 只允许管理员发起；普通用户必须指定自己的账号。
		if roleOf(r) != panelauth.RoleAdmin {
			writeErr(w, http.StatusForbidden, "admin_required")
			return
		}
	} else if !p.requireOmni(w, r, req.Provider, req.Label) {
		return
	}
	d, err := p.cfg.OmniRefresh(req.Provider, req.Label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: OmniGate 统一刷新 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, d)
}

// omniNewAPISetKeyReq 面板「补填聊天密钥」请求体。
type omniNewAPISetKeyReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	APIKey   string `json:"api_key"`
}

// omniNewAPISetKey 更新某 New API 账号的聊天 API Key（sk-）。普通用户只能改自己的。
func (p *Panel) omniNewAPISetKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniNewAPISetKey == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req omniNewAPISetKeyReq
	if err := decodeBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Label = strings.TrimSpace(req.Label)
	req.APIKey = strings.TrimSpace(req.APIKey)
	if req.Provider == "" || req.Label == "" {
		writeErr(w, http.StatusBadRequest, "provider/label 不能为空")
		return
	}
	if req.APIKey == "" {
		writeErr(w, http.StatusBadRequest, "API Key 不能为空")
		return
	}
	if !p.requireOmni(w, r, req.Provider, req.Label) {
		return
	}
	if err := p.cfg.OmniNewAPISetKey(req.Provider, req.Label, req.APIKey); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: New API 更新聊天密钥 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setCaptureCORS 放开抓取端点的跨域（脚本跑在 New API 站点 origin 上）。
func setCaptureCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
}

// panelPublicBase 由请求推断面板对外地址（用于拼接脚本里的回传 URL）。
func panelPublicBase(r *http.Request) string {
	return util.PublicBase(r)
}

// newAPICaptureSnippet 生成让用户在站点控制台粘贴的抓取脚本。
//
// 两种站点形态各有一个脚本：
//
//	bearer（本适配器最初对接的站点）：走站点前端自己的 bootstrap 路径
//	  /api/user/auth/refresh（credentials:'include'，靠 httpOnly 会话 cookie
//	  认身份）拿一份 access_token + sid。session.sid 是稳定的，因此面板与
//	  浏览器可共用同一会话。
//	cookie（AnyRouter / AgentRouter）：站点没有可轮换的 access_token，用户接口
//	  直接认会话 Cookie + New-Api-User。脚本只读 document.cookie 并查一次
//	  /api/user/self 补全用户 ID，然后回传。
func newAPICaptureSnippet(base, stateID, selfPath string, cookieAuth bool) string {
	if cookieAuth {
		if strings.TrimSpace(selfPath) == "" {
			selfPath = "/api/user/self"
		}
		return newAPICookieSnippet(base, stateID, selfPath)
	}
	captureURL := strings.TrimRight(base, "/") + "/panel/api/omni/newapi/capture?state=" + stateID
	// 注意：脚本里含 %c，用 %% 转义避免被 fmt 当成动词。
	tpl := `(async () => {
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const refresh = async () => {
    let last = '';
    for (let i = 0; i < 4; i++) {
      if (i) await sleep(400 * i);
      const r = await fetch('/api/user/auth/refresh', {
        method: 'POST', credentials: 'include', headers: { 'Cache-Control': 'no-store' }
      });
      const b = await r.json().catch(() => ({}));
      if (b && b.success && b.data && b.data.access_token) return b.data;
      last = (b && (b.message || b.code)) || ('HTTP ' + r.status);
      if (r.status === 409 && b && b.code === 'AUTH_REFRESH_RACE') continue;
      break;
    }
    throw new Error('刷新会话失败：' + last);
  };
  try {
    const run = () => refresh();
    const data = (navigator.locks && navigator.locks.request)
      ? await navigator.locks.request('new-api:auth-refresh', { mode: 'exclusive' }, run)
      : await run();
    const s = data.session || {}, u = data.user || {};
    let exp = data.access_expires_at || 0;
    if (typeof exp === 'number' && exp > 1e12) exp = Math.floor(exp / 1000);
    const payload = {
      access_token: data.access_token || '',
      token_type: data.token_type || '',
      access_expires_at: exp,
      session_sid: s.sid || '',
      cookie: document.cookie || '',
      username: u.username || '',
      user_id: u.id ? String(u.id) : ''
    };
    if (!payload.access_token || !payload.session_sid) {
      throw new Error('未拿到 access_token / session，请确认已登录本站');
    }
    const res = await fetch(%q, {
      method: 'POST', mode: 'cors',
      headers: { 'Content-Type': 'text/plain' },
      body: JSON.stringify(payload)
    });
    const out = await res.json().catch(() => ({}));
    if (res.ok && (!out || out.ok !== false)) {
      console.log('%%c已回传登录凭据，请回到面板查看结果。建议随后关闭本站标签页，避免浏览器与面板同时刷新会话。', 'color:#0a0');
    } else {
      console.error('回传被拒（HTTP ' + res.status + '）：' + ((out && out.error) || '未知错误'));
    }
  } catch (e) {
    console.error('抓取失败：', e);
  }
})();`
	return fmt.Sprintf(tpl, captureURL)
}

// newAPICookieSnippet 是 Cookie 型站点（AnyRouter / AgentRouter）的抓取脚本。
//
// 这些站点没有可轮换的 access_token：用户接口直接认浏览器会话 Cookie，并用
// New-Api-User 指明是哪个账号。所以脚本做三件事：
//  1. 读 document.cookie —— 会话 cookie 必须不是 httpOnly 才读得到，读不到就
//     明确报错，而不是回传一个空壳让面板误以为成功。
//  2. 用 Cookie 调一次 /api/user/self，拿数字用户 ID（后面所有请求都要用它）。
//  3. 回传 cookie + user_id。
func newAPICookieSnippet(base, stateID, selfPath string) string {
	captureURL := strings.TrimRight(base, "/") + "/panel/api/omni/newapi/capture?state=" + stateID
	tpl := `(async () => {
  const selfPath = %q;
  try {
    const cookie = document.cookie || '';
    if (!cookie) {
      throw new Error('读不到 Cookie：该会话 Cookie 可能是 httpOnly，请在浏览器开发者工具的 Application → Cookies 里复制整条 Cookie 后回到面板用「粘贴 Cookie」添加账号。');
    }
    const r = await fetch(selfPath, {
      method: 'GET', credentials: 'include',
      headers: { 'Accept': 'application/json', 'Cache-Control': 'no-store' }
    });
    const b = await r.json().catch(() => ({}));
    const u = (b && b.data) || {};
    const uid = u.id ? String(u.id) : '';
    if (!uid) {
      throw new Error('读取用户信息失败（HTTP ' + r.status + '）：' + ((b && b.message) || '未返回用户 ID') + '，请确认已登录本站');
    }
    const payload = {
      cookie: cookie,
      user_id: uid,
      username: u.username || '',
      display_name: u.display_name || ''
    };
    const res = await fetch(%q, {
      method: 'POST', mode: 'cors',
      headers: { 'Content-Type': 'text/plain' },
      body: JSON.stringify(payload)
    });
    const out = await res.json().catch(() => ({}));
    if (res.ok && (!out || out.ok !== false)) {
      console.log('%%c已回传会话 Cookie，请回到面板补齐聊天密钥（sk-）。', 'color:#0a0');
    } else {
      console.error('回传被拒（HTTP ' + res.status + '）：' + ((out && out.error) || '未知错误'));
    }
  } catch (e) {
    console.error('抓取失败：', e);
  }
})();`
	return fmt.Sprintf(tpl, selfPath, captureURL)
}
