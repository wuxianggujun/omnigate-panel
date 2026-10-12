// omni_newapi.go 面板 → New API（QuantumNous/new-api）供应商的接入编排。
//
// New API 站点的登录开了 Cloudflare Turnstile，服务端直连密码登录会被拦，所以
// 账号只能靠「浏览器登录 + 控制台脚本回传」建立：
//
//	POST /panel/api/omni/newapi/authorize   （登录用户）登记一次性 state，回登录页 + 脚本
//	POST /panel/api/omni/newapi/capture     （公开、state 校验）脚本把鉴权包 POST 回来
//	GET  /panel/api/omni/newapi/status      （登录用户）轮询抓取是否完成
//
// capture 走 no-cors 的 text/plain POST（浏览器「简单请求」，无需 CORS 预检），
// 因此这里额外放开 CORS 头，让 cors 模式的 fetch 也能读到结果。
//
// 分工与 omni_accounts.go 一致：面板只做 HTTP 编排，真正落库在 main 注入的闭包里。
package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"

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
	if req.Label == "" {
		writeErr(w, http.StatusBadRequest, "label 不能为空")
		return
	}
	// 先认领 label（普通 user 只能用自己的名字），避免脚本回传后才失败。
	if !p.claimOmniLabel(r, req.Provider, req.Label) {
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
	log.Printf("panel: New API 浏览器登录发起 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"state":      stateID,
		"signin_url": signin,
		"label":      req.Label,
		"snippet":    newAPICaptureSnippet(panelPublicBase(r), stateID),
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
	log.Printf("panel: New API 浏览器登录回传完成 state=%s", stateID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": d})
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
	scheme := "https"
	if p := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); p != "" {
		scheme = p
	} else {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		// 裸主机名 / localhost / 字面 IP 都按明文 http 处理。
		if !strings.Contains(host, ".") || strings.EqualFold(host, "localhost") || net.ParseIP(host) != nil {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

// newAPICaptureSnippet 生成让用户在 New API 站点控制台粘贴的抓取脚本。
//
// 脚本在 New API 的 origin 上跑，且走的就是站点前端自己的「bootstrap」路径：
// 新开页面时前端内存态为空，getExpectedSID() 为 undefined，于是用
// credentials:'include' 调 /api/user/auth/refresh（**不带** X-Auth-Session 头，
// 靠 httpOnly 会话 cookie 认身份）拿一份鉴权包。所以脚本也这样做。
//
// session.sid 是稳定的（刷新只换 access_token，sid 不变），因此面板与浏览器可共用
// 同一会话；并发刷新时上游回 409 AUTH_REFRESH_RACE，脚本重试即可。脚本还会抢站点
// 前端同名的 Web Lock（new-api:auth-refresh），避免和它抢刷新。
func newAPICaptureSnippet(base, stateID string) string {
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
    await fetch(%q, {
      method: 'POST', mode: 'no-cors',
      headers: { 'Content-Type': 'text/plain' },
      body: JSON.stringify(payload)
    });
    console.log('%%c已回传登录凭据，请回到面板查看结果。建议随后关闭本站标签页，避免浏览器与面板同时刷新会话。', 'color:#0a0');
  } catch (e) {
    console.error('抓取失败：', e);
  }
})();`
	return fmt.Sprintf(tpl, captureURL)
}
