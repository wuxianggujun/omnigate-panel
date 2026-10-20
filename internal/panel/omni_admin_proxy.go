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
	"log"
	"net/http"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// omniAdminKinds 允许代理的 OmniGate 管理子域，由插件描述符派生（login.transport=admin
// 的类型，如 raccoon / trae / geminiweb）。默认闭包未注入时回落到内置三种，保证测试与
// 单机嵌入场景仍可用；生产由 main 注入 catalog 派生的结果。
var omniAdminKindsFallback = map[string]bool{"raccoon": true, "trae": true, "geminiweb": true}

// omniAdminSubs 允许代理的子路径白名单（多段子路径按首段匹配）。
var omniAdminSubs = map[string]bool{
	"authorize":   true,
	"callback":    true,
	"token":       true,
	"accounts":    true,
	"checkin":     true,
	"remove":      true,
	"fingerprint": true,
	"gems":        true,
	"gemmaps":     true,
	"chats":       true,
	"keepalive":   true,
}

// adminKindAllowed 判断 kind 是否属于走 admin 代理的供应商类型。优先用注入的
// 描述符集合；未注入时回落内置三种（单机/测试）。
func (p *Panel) adminKindAllowed(kind string) bool {
	if p.cfg.OmniAdminKinds != nil {
		for _, k := range p.cfg.OmniAdminKinds() {
			if k == kind {
				return true
			}
		}
		return false
	}
	return omniAdminKindsFallback[kind]
}

// firstSeg 返回子路径的首段（"chats/delete" → "chats"）。
func firstSeg(sub string) string {
	if i := strings.IndexByte(sub, '/'); i >= 0 {
		return sub[:i]
	}
	return sub
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
	// sub 可能是多段（如 chats/delete）：白名单只看首段，转发保留完整子路径。
	if !ok || !p.adminKindAllowed(kind) || !omniAdminSubs[firstSeg(sub)] {
		writeErr(w, http.StatusNotFound, "not_found")
		return
	}

	// fingerprint / gemmaps 改的是全局供应商设置（与 /panel/api/omni/config 同级），
	// 只允许管理员读写；普通 user / viewer 即使猜到路径也不能改全局 TLS 指纹或
	// 把自定义 Gem 暴露成全局模型名。
	if (sub == "fingerprint" || firstSeg(sub) == "gemmaps") && roleOf(r) != panelauth.RoleAdmin {
		writeErr(w, http.StatusForbidden, "admin_required")
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
	switch firstSeg(sub) {
	case "authorize", "token", "callback":
		if label != "" && !p.claimOmniLabel(r, provider, label) {
			writeErr(w, http.StatusConflict, "账号名「"+label+"」已被其他用户占用，请换一个")
			return
		}
	case "checkin", "remove":
		if label != "" && !p.requireOmni(w, r, provider, label) {
			return
		}
	case "gems", "chats", "keepalive":
		// 账号级运维（自定义 Gem / 会话历史 / 保活）：label 必须属于调用者。
		// 这里不跳过空 label —— 空 label 会落到「池里第一个账号」，普通 user
		// 若不带 label 就能操作别人的账号，所以空 label 仅 admin/viewer 放行。
		if !p.requireOmni(w, r, provider, label) {
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
// label 已被别人占用时返回 false；检查与写入在归属表锁内原子完成。
func (p *Panel) claimOmniLabel(r *http.Request, provider, label string) bool {
	if roleOf(r) != panelauth.RoleUser {
		return true
	}
	return p.ownership.Claim(omniOwnerKey(provider, label), usernameOf(r))
}

// assignOmni 把 label 归到当前用户名下（用于 callback/token 成功之后）。
// 普通用户只能认领无主或自己的 label，不能覆盖别人的账号；管理员保持无主/共享。
func (p *Panel) assignOmni(r *http.Request, provider, label string) {
	key := omniOwnerKey(provider, label)
	if roleOf(r) != panelauth.RoleUser {
		if p.ownership.Owner(key) != "" {
			p.ownership.Remove(key)
		}
		return
	}
	if !p.ownership.Claim(key, usernameOf(r)) {
		log.Printf("panel: 归属冲突 %s（已属 %s，跳过 user %s）", key, p.ownership.Owner(key), usernameOf(r))
	}
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
		return []byte(`{"accounts":[]}`)
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
		return []byte(`{"accounts":[]}`)
	}
	return enc
}
