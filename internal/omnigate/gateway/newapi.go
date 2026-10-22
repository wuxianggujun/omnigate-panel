// newapi.go OmniGate 的 New API（QuantumNous/new-api）供应商接入：浏览器登录抓取、
// 每日签到、会话统一刷新、账号列表。首个实例是 https://api.justwoker.icu。
//
// 凭据模型（一个账号三把钥匙，见 provider.Account）：
//
//	AccessToken（Cookie 槽）    /api/user/* 的 Bearer（签到、查额度）
//	SessionSID（RefreshToken 槽）X-Auth-Session，用于轮换 access_token
//	APIKey                      sk-...，用于 /v1/chat/completions
//
// 该站登录开了 Cloudflare Turnstile，服务端直连密码登录会被拦，所以账号只能靠
// 「浏览器登录 + 控制台脚本回传」建立（见 NewAPIAuthorize / NewAPICapture）。
package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/newapi"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// newapiPendingTTL 是浏览器抓取会话的有效期。
const newapiPendingTTL = 15 * time.Minute

// NewAPILoginResult describes a New API account added/updated by capture.
type NewAPILoginResult struct {
	Label     string `json:"label"`
	Name      string `json:"name,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	HasAPIKey bool   `json:"has_api_key"`
}

// NewAPICapturePayload 是浏览器控制台脚本回传的鉴权包。
type NewAPICapturePayload struct {
	AccessToken   string `json:"access_token"`
	TokenType     string `json:"token_type"`
	ExpiresAt     int64  `json:"access_expires_at"`
	SessionSID    string `json:"session_sid"`
	SessionCookie string `json:"cookie"`
	Username      string `json:"username"`
	UserID        string `json:"user_id"`
	// APIKey is the relay's sk- chat key. The snippet cannot read it (it lives in
	// a separate "令牌" page), so it is normally passed by the panel instead;
	// accepting it here lets a manual paste add it in the same step.
	APIKey string `json:"api_key"`
}

// newapiProvider resolves a provider name to a *newapi.Provider.
func (g *Gateway) newapiProvider(name string) (*newapi.Provider, error) {
	if pcfg := g.cfg.Provider(name); pcfg == nil || pcfg.Type != "newapi" {
		return nil, fmt.Errorf("provider %q 不是 newapi 类型", name)
	}
	np, ok := g.providers[name].(*newapi.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 newapi 类型", name)
	}
	return np, nil
}

// NewAPIAuthorize 登记一次浏览器抓取会话，返回 state / 登录页 / 标签。
// 真正的回传由 NewAPICapture 完成（脚本 POST 回来）。
func (g *Gateway) NewAPIAuthorize(providerName, label, apiKey string) (map[string]string, error) {
	np, err := g.newapiProvider(providerName)
	if err != nil {
		return nil, err
	}
	// label 留空 = 自动命名：账号名在回传时用站点身份生成（用户不用自己起名）。
	label = strings.TrimSpace(label)
	stateID := util.UUID()
	g.newapiPendMu.Lock()
	for k, v := range g.newapiPend {
		if time.Since(v.at) > newapiPendingTTL {
			delete(g.newapiPend, k)
		}
	}
	g.newapiPend[stateID] = newapiPending{
		provider:  providerName,
		label:     label,
		autoLabel: label == "",
		apiKey:    strings.TrimSpace(apiKey),
		at:        time.Now(),
	}
	g.newapiPendMu.Unlock()

	signin := strings.TrimRight(np.Client().BaseURL(), "/") + np.Client().Endpoints().SigninPath
	authMode := "bearer"
	if np.Client().Endpoints().UsesCookieAuth() {
		authMode = "cookie"
	}
	return map[string]string{
		"provider":   providerName,
		"label":      label,
		"state":      stateID,
		"signin_url": signin,
		"auth_mode":  authMode,
		"self_path":  np.Client().Endpoints().SelfPath,
	}, nil
}

// NewAPIPending reports whether a capture state is still waiting for its bundle.
func (g *Gateway) NewAPIPending(stateID string) bool {
	g.newapiPendMu.Lock()
	defer g.newapiPendMu.Unlock()
	p, ok := g.newapiPend[stateID]
	return ok && time.Since(p.at) <= newapiPendingTTL
}

// NewAPICapture 完成一次浏览器抓取：校验一次性 state，落库账号。
func (g *Gateway) NewAPICapture(ctx context.Context, stateID string, p NewAPICapturePayload) (*NewAPILoginResult, error) {
	stateID = strings.TrimSpace(stateID)
	if stateID == "" {
		return nil, fmt.Errorf("缺少 state")
	}
	g.newapiPendMu.Lock()
	pend, ok := g.newapiPend[stateID]
	delete(g.newapiPend, stateID)
	g.newapiPendMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("state 校验失败或已过期，请重新发起")
	}
	if time.Since(pend.at) > newapiPendingTTL {
		return nil, fmt.Errorf("授权会话已过期，请重新发起")
	}
	np, err := g.newapiProvider(pend.provider)
	if err != nil {
		return nil, err
	}
	access := strings.TrimSpace(p.AccessToken)
	sid := strings.TrimSpace(p.SessionSID)
	cookie := strings.TrimSpace(p.SessionCookie)
	if access == "" && cookie == "" && sid == "" {
		return nil, fmt.Errorf("抓取包是空的：需要在站点登录后回传 access_token 或会话 Cookie")
	}
	// Cookie 型站点（AnyRouter / AgentRouter）只有浏览器会话，没有可轮换的
	// access_token；此时会话 Cookie 就是用户接口凭据，签到/查额度都用它。
	cookieAuth := np.Client().Endpoints().UsesCookieAuth()
	if access == "" && !cookieAuth {
		return nil, fmt.Errorf("上游未返回 access_token，请确认已在浏览器登录")
	}
	if access == "" && cookie == "" {
		return nil, fmt.Errorf("该站点用会话 Cookie 鉴权，但抓取包里没有 Cookie")
	}
	// 尽力补全昵称 / 用户 ID：失败不影响抓取本身。
	name, userID := strings.TrimSpace(p.Username), strings.TrimSpace(p.UserID)
	if name == "" || userID == "" {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		tmp := &provider.Account{
			Cookie:        access,
			RefreshToken:  sid,
			SessionCookie: cookie,
			UID:           userID,
		}
		if self, serr := np.Client().Self(sctx, tmp); serr == nil {
			if name == "" {
				name = self.DisplayName
			}
			if userID == "" && self.UserID > 0 {
				userID = fmt.Sprintf("%d", self.UserID)
			}
		}
		cancel()
	}
	// 自动命名：用站点身份当账号名（用户名优先，其次数字 ID，最后退回「账号 N」）。
	label := pend.label
	if pend.autoLabel || label == "" {
		label = g.newAPIAutoLabel(pend.provider, name, userID)
		// 同一个浏览器会话就是同一个账号：如果这次抓回来的 Cookie 与某个既有
		// 账号一致，就复用它的名字。否则「第一次没补到用户 ID」会落一个「账号 1」
		// 占位名，第二次补抓又生成新名字，同一账号在面板里裂成两条——而前一条
		// 恰好是缺 New-Api-User 的坏账号，用户根本不知道该删哪个。
		if existing := g.newAPIAccountBySession(pend.provider, cookie); existing != "" {
			label = existing
		}
	}
	apiKey := pend.apiKey
	if apiKey == "" {
		apiKey = strings.TrimSpace(p.APIKey)
	}
	return g.upsertNewAPIAccount(pend.provider, label, access, sid,
		cookie, p.ExpiresAt, name, userID, apiKey), nil
}

// newAPIAccountBySession 找出使用同一份浏览器会话（Cookie）的既有账号名。
// Cookie 型站点没有 access_token 可以区分账号，会话 Cookie 就是唯一稳定的身份；
// 用它做「同会话 = 同账号」的判定，避免补抓时裂成两条。
func (g *Gateway) newAPIAccountBySession(providerName, cookie string) string {
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return ""
	}
	for _, a := range g.Accounts(providerName) {
		if strings.TrimSpace(a.SessionCookie) == cookie {
			return a.Label
		}
	}
	return ""
}

// newAPIAutoLabel 在用户没填账号名时，用站点身份生成一个：优先用户名（站点内唯一），
// 其次数字用户 ID，最后才退回「账号 N」占位。
func (g *Gateway) newAPIAutoLabel(providerName, name, userID string) string {
	if s := sanitizeLabel(name); s != "" {
		return s
	}
	if s := sanitizeLabel(userID); s != "" {
		return "u" + s
	}
	// 占位名要避开已存在的，否则「删掉中间一条再加一条」会撞上同名账号，
	// 把两条不同的账号写进同一个 label 槽位。
	taken := map[string]bool{}
	for _, a := range g.Accounts(providerName) {
		taken[a.Label] = true
	}
	for n := 1; ; n++ {
		candidate := fmt.Sprintf("账号 %d", n)
		if !taken[candidate] {
			return candidate
		}
	}
}

// sanitizeLabel 把站点用户名收成可安全当账号名 / 归属键的短字符串：丢掉控制字符、
// 折叠空白、限长 48 个 rune（账号名会进 state.json 与归属表，不该带换行或超长串）。
func sanitizeLabel(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	prevSpace := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if prevSpace || b.Len() == 0 {
				continue
			}
			prevSpace = true
			b.WriteRune(' ')
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		prevSpace = false
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if rs := []rune(out); len(rs) > 48 {
		out = string(rs[:48])
	}
	return out
}

// newAPIMissingUIDHint 是「Cookie 型账号缺数字用户 ID」时的统一提示语。
// 这是唯一一条必须靠重新抓取修复的错误，提示里要写清楚怎么做。
const newAPIMissingUIDHint = "缺少数字用户 ID（New-Api-User）：请在面板重新抓取一次该账号的会话 Cookie"

// missingNewAPIUserID reports whether a cookie-auth account lacks the numeric
// user id that its user-API requires as the New-Api-User header.
//
// Only cookie-auth relays are checked: bearer relays treat UID as optional
// metadata, and flagging them would invent a failure that does not exist.
func missingNewAPIUserID(np *newapi.Provider, acc *provider.Account) bool {
	if np == nil || acc == nil || !np.Client().Endpoints().UsesCookieAuth() {
		return false
	}
	if np.Client().Endpoints().UserHeader == "" {
		return false
	}
	return !isNumericID(acc.UID)
}

// isNumericID reports whether s is a plain positive decimal integer, i.e. the
// shape of a New API user primary key. Used to reject garbage captured by a
// login snippet before it becomes an always-401 New-Api-User header.
func isNumericID(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != "0"
}

// NewAPISetAPIKey updates an existing account's chat API key (sk-...). The key is
// what /v1/chat/completions authenticates with, so an account added without one
// (or with a wrong one) can be fixed here without a full browser re-capture.
func (g *Gateway) NewAPISetAPIKey(providerName, label, apiKey string) error {
	if _, err := g.newapiProvider(providerName); err != nil {
		return err
	}
	label = strings.TrimSpace(label)
	apiKey = strings.TrimSpace(apiKey)
	if label == "" {
		return fmt.Errorf("label 不能为空")
	}
	if apiKey == "" {
		return fmt.Errorf("API Key 不能为空")
	}
	g.accountsMu.Lock()
	found := false
	for _, a := range g.accounts[providerName] {
		if a.Label == label {
			a.APIKey = apiKey
			found = true
			break
		}
	}
	g.accountsMu.Unlock()
	if !found {
		return fmt.Errorf("账号「%s」不存在", label)
	}
	// 持久化：保留 DynAccount 其余字段，只换 APIKey。
	for _, d := range g.st.DynAccounts(providerName) {
		if d.Label == label {
			d.APIKey = apiKey
			g.st.UpsertDynAccount(providerName, d)
			break
		}
	}
	return nil
}

func (g *Gateway) upsertNewAPIAccount(providerName, label, access, sid, cookie string, expiresAt int64, name, userID, apiKey string) *NewAPILoginResult {
	// 用户 ID 缺失时**不能**编一个随机值：Cookie 型站点把它原样当 New-Api-User
	// 发出去，假 ID 只会换来一个必然的 401（签到与查额度全废），而且这个假 ID
	// 会随账号一起落库，之后每次刷新都在延续错误。留空反而更诚实——用户接口调用
	// 会明确报「缺少 New-Api-User」，用户补一次抓取即可。
	//
	// 只在 cookie 型站点上做数字校验：Bearer 型老站点的用户 ID 语义没有变，
	// 不能因为这次加固改动既有账号的行为。
	if userID != "" {
		if np, err := g.newapiProvider(providerName); err == nil &&
			np.Client().Endpoints().UsesCookieAuth() && !isNumericID(userID) {
			// 真站点只接受数字主键；非数字说明抓取脚本读错了字段，按缺失处理。
			userID = ""
		}
	}
	if name == "" {
		name = "New API 账号"
	}
	// 重抓时若某字段为空，保留既有值，避免把之前填好的 API Key / session 抹掉。
	for _, d := range g.st.DynAccounts(providerName) {
		if d.Label == label {
			if apiKey == "" {
				apiKey = d.APIKey
			}
			if cookie == "" {
				cookie = d.SessionCookie
			}
			if sid == "" {
				sid = d.RefreshToken
			}
			if name == "New API 账号" && d.Name != "" {
				name = d.Name
			}
			break
		}
	}

	g.accountsMu.Lock()
	accts := g.accounts[providerName]
	found := false
	for _, a := range accts {
		if a.Label == label {
			a.Cookie = access
			a.RefreshToken = sid
			a.SessionCookie = cookie
			a.ExpiresAt = expiresAt
			// UID 是 Cookie 型站点（AnyRouter / AgentRouter）的身份头
			// New-Api-User 的来源；不写进运行时账号，签到与查额度都会 401。
			if userID != "" {
				a.UID = userID
			}
			if apiKey != "" {
				a.APIKey = apiKey
			}
			found = true
			break
		}
	}
	if !found {
		g.accounts[providerName] = append(accts, &provider.Account{
			Label:         label,
			Cookie:        access,
			RefreshToken:  sid,
			SessionCookie: cookie,
			ExpiresAt:     expiresAt,
			UID:           userID,
			APIKey:        apiKey,
		})
	}
	g.accountsMu.Unlock()

	g.st.UpsertDynAccount(providerName, state.DynAccount{
		Label:         label,
		Name:          name,
		UserID:        userID,
		AccessToken:   access,
		RefreshToken:  sid,
		SessionCookie: cookie,
		ExpiresAt:     expiresAt,
		APIKey:        apiKey,
		AddedAt:       time.Now().Unix(),
	})

	out := &NewAPILoginResult{Label: label, Name: name, UserID: userID, HasAPIKey: apiKey != ""}
	if expiresAt > 0 {
		out.ExpiresAt = time.Unix(expiresAt, 0).Format(time.RFC3339)
	}
	return out
}

// NewAPIRemoveAccount drops a runtime account and its credentials.
func (g *Gateway) NewAPIRemoveAccount(providerName, label string) bool {
	g.accountsMu.Lock()
	accts := g.accounts[providerName]
	out := accts[:0]
	for _, a := range accts {
		if a.Label == label {
			continue
		}
		out = append(out, a)
	}
	g.accounts[providerName] = out
	g.accountsMu.Unlock()
	return g.st.RemoveDynAccount(providerName, label)
}

// NewAPIRefreshAccount 强制轮换一个账号的 access_token（「统一刷新」用）。
func (g *Gateway) NewAPIRefreshAccount(ctx context.Context, providerName, label string) error {
	np, err := g.newapiProvider(providerName)
	if err != nil {
		return err
	}
	matched := false
	for _, acc := range g.Accounts(providerName) {
		if label != "" && acc.Label != label {
			continue
		}
		matched = true
		if err := g.refreshToken(ctx, providerName, np, acc); err != nil {
			return fmt.Errorf("账号「%s」刷新失败: %w", acc.Label, err)
		}
	}
	if label != "" && !matched {
		return fmt.Errorf("账号「%s」不存在", label)
	}
	return nil
}

// NewAPIRefreshAll 统一刷新一个 provider 下所有账号的 access_token，返回逐账号结果。
func (g *Gateway) NewAPIRefreshAll(ctx context.Context, providerName string) ([]RaccoonCheckinOutcome, error) {
	np, err := g.newapiProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []RaccoonCheckinOutcome{}
	for _, acc := range g.Accounts(providerName) {
		rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		rerr := g.refreshToken(rctx, providerName, np, acc)
		cancel()
		item := RaccoonCheckinOutcome{Label: acc.Label, Success: rerr == nil}
		if rerr != nil {
			item.Error = rerr.Error()
		} else {
			item.Message = "已刷新"
		}
		out = append(out, item)
	}
	return out, nil
}

// ensureNewAPIToken 在 access_token 缺失/临近过期时轮换一次（静默失败）。
func (g *Gateway) ensureNewAPIToken(ctx context.Context, provName string, np *newapi.Provider, acc *provider.Account) {
	if !np.TokenExpired(acc) {
		return
	}
	if strings.TrimSpace(acc.RefreshToken) == "" && strings.TrimSpace(acc.SessionCookie) == "" {
		return
	}
	if err := g.refreshToken(ctx, provName, np, acc); err != nil {
		g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, err)
	}
}

// NewAPIAccounts lists a provider's accounts with a best-effort quota snapshot.
func (g *Gateway) NewAPIAccounts(ctx context.Context, providerName string, withBalance bool) ([]map[string]any, error) {
	np, err := g.newapiProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		if withBalance {
			g.ensureNewAPIToken(ctx, providerName, np, acc)
		}
		// has_token 表达的是「用户接口有没有可用凭据」，不是「有没有 access_token」：
		// Cookie 型站点（AnyRouter / AgentRouter）根本没有 token，只看 Cookie 会让
		// 面板把一个可用账号标成「未配置凭证」。
		hasUserCred := acc.Cookie != "" || acc.SessionCookie != ""
		item := map[string]any{
			"label":       acc.Label,
			"has_token":   hasUserCred,
			"has_api_key": strings.TrimSpace(acc.APIKey) != "",
		}
		// 数字用户 ID 对 Cookie 型站点是「能不能用」的一部分，露出来用户才能
		// 一眼看出账号是否完整（面板会显示 UID 徽标）。
		if acc.UID != "" {
			item["user_id"] = acc.UID
		}
		if acc.ExpiresAt > 0 {
			item["expires_at"] = time.Unix(acc.ExpiresAt, 0).Format(time.RFC3339)
		}
		authInvalid := np.TokenExpired(acc)
		if authInvalid {
			item["auth_invalid"] = true
			if np.Client().Endpoints().UsesCookieAuth() {
				item["auth_error"] = "会话 Cookie 缺失或已失效，请重新登录该账号"
			} else {
				item["auth_error"] = "access_token 已失效且自动刷新失败，请重新登录该账号"
			}
		}
		// Cookie 型站点还要一个数字用户 ID 作 New-Api-User 头，缺了它签到与额度
		// 一律 401。但它**不影响聊天**（聊天只用 sk- 密钥），所以不能标成
		// auth_invalid —— 那会让路由和用户都以为这条账号废了。额度查询也直接跳过：
		// 明知会 401 的请求没必要发，把原因写进错误位即可。
		uidMissing := missingNewAPIUserID(np, acc)
		if uidMissing {
			item["uid_missing"] = true
			item["uid_error"] = newAPIMissingUIDHint
			if withBalance {
				item["balance_error"] = newAPIMissingUIDHint
			}
		}
		if withBalance && hasUserCred && !authInvalid && !uidMissing {
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			self, serr := np.Client().Self(bctx, acc)
			cancel()
			if serr != nil {
				item["balance_error"] = serr.Error()
			} else {
				item["available"] = self.Quota
				item["used"] = self.UsedQuota
				item["name"] = self.DisplayName
				if self.Group != "" {
					item["group"] = self.Group
				}
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// NewAPICheckin runs the daily check-in for the provider's accounts (all, or just
// one when label is set).
func (g *Gateway) NewAPICheckin(ctx context.Context, providerName, label string) ([]RaccoonCheckinOutcome, error) {
	np, err := g.newapiProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []RaccoonCheckinOutcome{}
	matched := false
	for _, acc := range g.Accounts(providerName) {
		if label != "" && acc.Label != label {
			continue
		}
		matched = true
		g.ensureNewAPIToken(ctx, providerName, np, acc)
		if missingNewAPIUserID(np, acc) {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: newAPIMissingUIDHint})
			continue
		}
		if acc.Cookie == "" && acc.SessionCookie == "" {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: "没有可用凭据（access_token 或会话 Cookie），请先登录"})
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, cerr := np.Client().Checkin(cctx, acc)
		cancel()
		if cerr != nil {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: cerr.Error()})
			continue
		}
		out = append(out, RaccoonCheckinOutcome{
			Label:          acc.Label,
			Success:        res.Success,
			Message:        res.Message,
			AlreadyClaimed: res.AlreadyClaimed,
		})
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}
