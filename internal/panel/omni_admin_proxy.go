// omni_admin_proxy.go 面板 → 进程内 OmniGate 的「登录类管理端点」代理。
//
// 背景：raccoon / TRAE 的账号添加走的是 OmniGate 自己的 /omni/admin/<kind>/* 端点
// （授权链接、回调兑换、粘贴 Token、账号列表、签到、移除），原先由前端用网关
// api_key 直连。多租户下普通 user 不该、也没有这把 api_key，于是把这条路改由面板
// 代理：面板注入 api_key 转发到进程内 OmniGate，并在转发前后做归属处理——
//
//	authorize → 记下「谁在给哪个 label 授权」，先打归属标记（自动捕获流程里账号是
//	            浏览器回调直接落库的，面板只能在这里抢先认领）；
//	callback  → 兑换成功后按响应里的 account.label 再打一次（覆盖手动粘贴流程）；
//	token     → 同上；
//	accounts  → 按归属过滤，普通 user 只看得到自己的账号；
//	checkin   → 校验归属；
//	remove    → 校验归属，成功后清理归属记录。
//
// 归属守卫：label 已被**别人**占用时拒绝（409），避免 user 用别人的账号名
// 「认领」到已有账号。管理员添加的账号不打标（保持无主/共享）。
//
// 安全边界：只放行上述白名单子路径，其余一律 404；请求体限 1MiB。
package panel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// omniAdminKinds 允许代理的 OmniGate 管理子域（raccoon / trae / geminiweb）。
var omniAdminKinds = map[string]bool{"raccoon": true, "trae": true, "geminiweb": true}

// omniAdminSubs 允许代理的子路径白名单。
var omniAdminSubs = map[string]bool{
	"authorize": true,
	"callback":  true,
	"token":     true,
	"accounts":  true,
	"checkin":   true,
	"remove":    true,
}

// captureWriter 缓冲下游 handler 的响应（状态码 + 体），供代理解析后再写出。
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newCaptureWriter() *captureWriter { return &captureWriter{header: http.Header{}} }

func (c *captureWriter) Header() http.Header { return c.header }
func (c *captureWriter) WriteHeader(s int) {
	if c.status == 0 {
		c.status = s
	}
}
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// omniAdminProxy 见文件头注释。
func (p *Panel) omniAdminProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniAdmin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/panel/api/omni/admin/")
	kind, sub, ok := strings.Cut(rest, "/")
	if !ok || !omniAdminKinds[kind] || !omniAdminSubs[sub] {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}

	// 读全请求体（用于解析 provider/label，并原样转发）。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	provider, label := omniProxyTarget(kind, sub, r.URL.Query(), raw)

	// 归属守卫：authorize/token/callback 会新增账号 → 先确认 label 未被别人占用；
	// checkin/remove 是既有账号操作 → 必须已经属于自己。
	switch sub {
	case "authorize", "token", "callback":
		if label != "" && !p.claimOmniLabel(r, provider, label) {
			writeErr(w, http.StatusConflict, "账号名「"+label+"」已被其他用户占用，请换一个")
			return
		}
	case "checkin", "remove":
		if label != "" && !p.requireOmni(w, r, provider, label) {
			return
		}
	}

	// 转发：路径改写为 OmniGate 侧的真实路径，注入网关 api_key。
	fwd := r.Clone(r.Context())
	fwd.URL.Path = "/admin/" + kind + "/" + sub
	fwd.Body = io.NopCloser(bytes.NewReader(raw))
	fwd.ContentLength = int64(len(raw))
	fwd.Header.Set("Authorization", "Bearer "+p.apiKey())
	fwd.Header.Del("Cookie") // 面板会话 Cookie 不外传

	cw := newCaptureWriter()
	p.cfg.OmniAdmin.ServeHTTP(cw, fwd)
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	body := cw.body.Bytes()

	// 上游 401 = 网关 api_key 不匹配，**不是**「面板会话失效」。原样回传会被前端
	// 当成掉线并弹登录门（授权轮询每 3 秒一次，会反复弹），这里改写成 502 +
	// 明确错误码，前端只当普通失败处理。
	if status == http.StatusUnauthorized {
		status = http.StatusBadGateway
		body = []byte(`{"ok":false,"error":"omnigate_unauthorized"}`)
	}

	// 成功后的归属维护。
	if status >= 200 && status < 300 {
		switch sub {
		case "authorize":
			// 授权链接已生成：归属已在上面认领（自动捕获流程靠这一步）。
		case "callback", "token":
			if lbl := omniAccountLabel(body); lbl != "" {
				p.assignOmni(r, provider, lbl)
			}
		case "remove":
			if label != "" {
				p.ownership.Remove(omniOwnerKey(provider, label))
			}
		case "accounts":
			body = p.filterOmniAccounts(r, provider, body)
		}
	}

	if ct := cw.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// omniProxyTarget 从查询串与请求体里解析 (provider, label)。GET 走查询串；
// POST 走 JSON 体（callback 的 label 在体内，token 同）。
func omniProxyTarget(kind, sub string, q map[string][]string, raw []byte) (provider, label string) {
	pick := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	provider = pick("provider")
	label = pick("label")
	if len(raw) > 0 {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err == nil {
			if provider == "" {
				if s, ok := m["provider"].(string); ok {
					provider = strings.TrimSpace(s)
				}
			}
			if label == "" {
				if s, ok := m["label"].(string); ok {
					label = strings.TrimSpace(s)
				}
			}
		}
	}
	return provider, label
}

// omniAccountLabel 从响应体里取 account.label（callback / token 成功响应）。
func omniAccountLabel(body []byte) string {
	var m struct {
		Account struct {
			Label string `json:"label"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.Account.Label)
}

// claimOmniLabel 认领 label：仅普通 user 需要打标（管理员加的号保持无主/共享）。
// label 已被别人占用时返回 false。
func (p *Panel) claimOmniLabel(r *http.Request, provider, label string) bool {
	if roleOf(r) != panelauth.RoleUser {
		return true
	}
	owner := usernameOf(r)
	key := omniOwnerKey(provider, label)
	if cur := p.ownership.Owner(key); cur != "" && cur != owner {
		return false
	}
	p.ownership.Set(key, owner)
	return true
}

// assignOmni 无条件把 label 归到当前用户名下（用于 callback/token 成功之后，
// 此时 label 可能来自响应而非请求；管理员则清除可能的误标——保持无主）。
func (p *Panel) assignOmni(r *http.Request, provider, label string) {
	key := omniOwnerKey(provider, label)
	if roleOf(r) != panelauth.RoleUser {
		// 管理员操作：若此前被某个 user 误标（例如占位），清成无主。
		if p.ownership.Owner(key) != "" {
			p.ownership.Remove(key)
		}
		return
	}
	p.ownership.Set(key, usernameOf(r))
}

// filterOmniAccounts 按归属过滤 accounts 响应体（普通 user 只看自己的）。
// provider 已知时按 (provider,label) 精确匹配；未知时退化为「label 属于自己」。
func (p *Panel) filterOmniAccounts(r *http.Request, provider string, body []byte) []byte {
	owned, all := p.ownFilter(r)
	if all {
		return body
	}
	byLabel := map[string]bool{}
	for k := range owned {
		if s, ok := strings.CutPrefix(k, "omni:"); ok {
			if _, lbl, ok := strings.Cut(s, "/"); ok {
				byLabel[lbl] = true
			}
		}
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	list, _ := m["accounts"].([]any)
	out := make([]any, 0, len(list))
	for _, it := range list {
		row, _ := it.(map[string]any)
		if row == nil {
			continue
		}
		label, _ := row["label"].(string)
		if label == "" {
			continue
		}
		if provider != "" {
			if owned[omniOwnerKey(provider, label)] {
				out = append(out, row)
			}
			continue
		}
		if byLabel[label] {
			out = append(out, row)
		}
	}
	m["accounts"] = out
	enc, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return enc
}
