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

	signin := strings.TrimRight(np.Client().BaseURL(), "/") + "/sign-in"
	return map[string]string{
		"provider":   providerName,
		"label":      label,
		"state":      stateID,
		"signin_url": signin,
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
	if access == "" {
		return nil, fmt.Errorf("上游未返回 access_token，请确认已在浏览器登录")
	}
	// 尽力补全昵称 / 用户 ID：失败不影响抓取本身。
	name, userID := strings.TrimSpace(p.Username), strings.TrimSpace(p.UserID)
	if name == "" || userID == "" {
		sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if self, serr := np.Client().Self(sctx, access); serr == nil {
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
	}
	return g.upsertNewAPIAccount(pend.provider, label, access, strings.TrimSpace(p.SessionSID),
		strings.TrimSpace(p.SessionCookie), p.ExpiresAt, name, userID, pend.apiKey), nil
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
	return fmt.Sprintf("账号 %d", len(g.Accounts(providerName))+1)
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
	if userID == "" {
		userID = strings.ReplaceAll(util.UUID(), "-", "")
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
		item := map[string]any{
			"label":       acc.Label,
			"has_token":   acc.Cookie != "",
			"has_api_key": strings.TrimSpace(acc.APIKey) != "",
		}
		if acc.ExpiresAt > 0 {
			item["expires_at"] = time.Unix(acc.ExpiresAt, 0).Format(time.RFC3339)
		}
		authInvalid := np.TokenExpired(acc)
		if authInvalid {
			item["auth_invalid"] = true
			item["auth_error"] = "access_token 已失效且自动刷新失败，请重新登录该账号"
		}
		if withBalance && acc.Cookie != "" && !authInvalid {
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			self, serr := np.Client().Self(bctx, acc.Cookie)
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
		if acc.Cookie == "" {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: "没有可用 access_token，请先登录"})
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, cerr := np.Client().Checkin(cctx, acc.Cookie)
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
