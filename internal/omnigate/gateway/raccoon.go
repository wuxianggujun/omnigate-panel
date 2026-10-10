package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// RaccoonLoginResult describes an account added/updated through the web login.
type RaccoonLoginResult struct {
	Label     string `json:"label"`
	Name      string `json:"name"`
	UserID    string `json:"user_id"`
	OrgName   string `json:"org_name,omitempty"`
	OrgRole   string `json:"org_role,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// raccoonProvider resolves a provider name to a *raccoon.Provider.
func (g *Gateway) raccoonProvider(name string) (*raccoon.Provider, error) {
	if pcfg := g.cfg.Provider(name); pcfg == nil || pcfg.Type != "raccoon" {
		return nil, fmt.Errorf("provider %q 不是 raccoon 类型", name)
	}
	rp, ok := g.providers[name].(*raccoon.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 raccoon 类型", name)
	}
	return rp, nil
}

// refreshRaccoonToken mints a fresh access token and persists it.
func (g *Gateway) refreshRaccoonToken(ctx context.Context, provName string, prov provider.Provider, acc *provider.Account) error {
	rp, ok := prov.(*raccoon.Provider)
	if !ok {
		return fmt.Errorf("provider %q 不是 raccoon 类型", provName)
	}
	if err := rp.RefreshAccount(ctx, acc); err != nil {
		return err
	}
	g.st.SetTokens(provName, acc.Label, acc.Cookie, acc.RefreshToken)
	return nil
}

// ensureRaccoonToken refreshes acc when its access token is expired or missing.
func (g *Gateway) ensureRaccoonToken(ctx context.Context, provName string, rp *raccoon.Provider, acc *provider.Account) {
	if acc.RefreshToken == "" || !rp.TokenExpired(acc) {
		return
	}
	if err := g.refreshRaccoonToken(ctx, provName, rp, acc); err != nil {
		g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, err)
	}
}

// RaccoonAuthorize starts a browser authorization and returns the URL + state.
// When redirectBase is non-empty the web-redirect flow is used: after login the
// authorize page sends the code to redirectBase (auto-capture). Otherwise the
// desktop flow is used and the user must paste the custom-scheme callback.
func (g *Gateway) RaccoonAuthorize(providerName, label, redirectBase string) (map[string]string, error) {
	rp, err := g.raccoonProvider(providerName)
	if err != nil {
		return nil, err
	}
	if label == "" {
		label = fmt.Sprintf("账号 %d", len(g.Accounts(providerName))+1)
	}
	state := util.UUID()
	g.pendingMu.Lock()
	// Opportunistically drop stale pending authorizations.
	for k, v := range g.pending {
		if time.Since(v.at) > 15*time.Minute {
			delete(g.pending, k)
		}
	}
	g.pending[state] = raccoonPending{provider: providerName, label: label, at: time.Now()}
	g.pendingMu.Unlock()

	out := map[string]string{
		"provider":      providerName,
		"label":         label,
		"state":         state,
		"callback_hint": raccoon.CallbackPrefix + "?code=..&state=..",
	}
	if redirectBase != "" {
		// Embed state so the redirect handler can validate the pending session
		// (the authorize page only appends authorization_code, not state).
		sep := "?"
		if strings.Contains(redirectBase, "?") {
			sep = "&"
		}
		redirectURL := redirectBase + sep + "state=" + state
		out["mode"] = "auto"
		out["redirect_url"] = redirectURL
		out["authorize_url"] = rp.Client().BuildAuthorizeURLWithRedirect(redirectURL)
	} else {
		out["mode"] = "manual"
		out["authorize_url"] = rp.Client().BuildAuthorizeURL(state)
	}
	return out, nil
}

// RaccoonCompleteRedirect finishes a web-redirect authorization: it validates the
// state against a pending session, exchanges the authorization code and adds the
// account. Used by the panel's auto-capture callback page.
func (g *Gateway) RaccoonCompleteRedirect(ctx context.Context, state, code string) (*RaccoonLoginResult, error) {
	if state == "" || code == "" {
		return nil, fmt.Errorf("缺少 state 或 authorization_code")
	}
	g.pendingMu.Lock()
	p, ok := g.pending[state]
	delete(g.pending, state)
	g.pendingMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("state 校验失败或已过期，请重新发起授权")
	}
	if time.Since(p.at) > 15*time.Minute {
		return nil, fmt.Errorf("授权会话已过期，请重新发起")
	}
	rp, err := g.raccoonProvider(p.provider)
	if err != nil {
		return nil, err
	}
	res, err := rp.Client().ExchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return g.upsertRaccoonAccount(p.provider, p.label, res.AccessToken, res.RefreshToken, res.DisplayName, res.OrgName, res.OrgRole), nil
}

// RaccoonLoginCallback validates a pasted callback URL and adds the account.
func (g *Gateway) RaccoonLoginCallback(ctx context.Context, providerName, label, callbackURL string) (*RaccoonLoginResult, error) {
	code, state, ok := raccoon.ParseCallback(callbackURL)
	if !ok {
		return nil, fmt.Errorf("回调地址无效，应形如 %s?code=..&state=..", raccoon.CallbackPrefix)
	}
	g.pendingMu.Lock()
	p, ok := g.pending[state]
	delete(g.pending, state)
	g.pendingMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("state 校验失败或已过期，请重新发起授权")
	}
	if time.Since(p.at) > 15*time.Minute {
		return nil, fmt.Errorf("授权会话已过期，请重新发起")
	}
	if providerName == "" {
		providerName = p.provider
	}
	if label == "" {
		label = p.label
	}
	rp, err := g.raccoonProvider(providerName)
	if err != nil {
		return nil, err
	}
	res, err := rp.Client().ExchangeCode(ctx, code)
	if err != nil {
		return nil, err
	}
	return g.upsertRaccoonAccount(providerName, label, res.AccessToken, res.RefreshToken, res.DisplayName, res.OrgName, res.OrgRole), nil
}

// RaccoonSetToken adds/updates an account from manually supplied tokens.
func (g *Gateway) RaccoonSetToken(ctx context.Context, providerName, label, accessToken, refreshToken string) (*RaccoonLoginResult, error) {
	if _, err := g.raccoonProvider(providerName); err != nil {
		return nil, err
	}
	access := raccoon.StripBearer(accessToken)
	if access == "" {
		return nil, fmt.Errorf("access_token 不能为空")
	}
	refresh := raccoon.StripBearer(refreshToken)
	if refresh == "" {
		refresh = g.st.RefreshToken(providerName, label, "")
	}
	if label == "" {
		label = fmt.Sprintf("账号 %d", len(g.Accounts(providerName))+1)
	}
	return g.upsertRaccoonAccount(providerName, label, access, refresh, "", "", ""), nil
}

func (g *Gateway) upsertRaccoonAccount(providerName, label, access, refresh, name, org, role string) *RaccoonLoginResult {
	claims := raccoon.DecodeClaims(access)
	userID := raccoon.ExtractUserID(claims)
	if userID == "" {
		userID = strings.ReplaceAll(util.UUID(), "-", "")
	}
	if name == "" {
		name = raccoon.DisplayNameOf(claims)
	}
	if name == "" {
		name = "小浣熊账号"
	}

	g.accountsMu.Lock()
	accts := g.accounts[providerName]
	found := false
	for _, a := range accts {
		if a.Label == label {
			a.Cookie = access
			a.RefreshToken = refresh
			found = true
			break
		}
	}
	if !found {
		g.accounts[providerName] = append(accts, &provider.Account{Label: label, Cookie: access, RefreshToken: refresh})
	}
	g.accountsMu.Unlock()

	g.st.UpsertDynAccount(providerName, state.DynAccount{
		Label:        label,
		Name:         name,
		UserID:       userID,
		OrgName:      org,
		OrgRole:      role,
		AccessToken:  access,
		RefreshToken: refresh,
		AddedAt:      time.Now().Unix(),
	})

	out := &RaccoonLoginResult{Label: label, Name: name, UserID: userID, OrgName: org, OrgRole: role}
	if t := raccoon.TokenExpiry(access); !t.IsZero() {
		out.ExpiresAt = t.Format(time.RFC3339)
	}
	return out
}

// RaccoonRemoveAccount drops a runtime account and its tokens.
func (g *Gateway) RaccoonRemoveAccount(providerName, label string) bool {
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

// RaccoonAccounts lists a provider's accounts with a best-effort balance.
func (g *Gateway) RaccoonAccounts(ctx context.Context, providerName string) ([]map[string]any, error) {
	rp, err := g.raccoonProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		item := map[string]any{
			"label":      acc.Label,
			"has_token":  acc.Cookie != "",
			"expires_at": raccoon.TokenExpiry(acc.Cookie).Format(time.RFC3339),
		}
		if t := raccoon.TokenExpiry(acc.Cookie); t.IsZero() {
			delete(item, "expires_at")
		}
		if acc.Cookie != "" {
			g.ensureRaccoonToken(ctx, providerName, rp, acc)
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			bal, berr := rp.Client().QueryBalance(bctx, acc.Cookie)
			cancel()
			if berr != nil {
				item["balance_error"] = berr.Error()
			} else {
				item["available"] = bal.Available
				item["wallets"] = bal.Wallets
				item["subscription"] = bal.Subscription
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// RaccoonCheckinOutcome is one account's check-in result.
type RaccoonCheckinOutcome struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Message string `json:"msg"`
	Error   string `json:"error,omitempty"`
	// AlreadyClaimed 见 raccoon.CheckinResult：今天已领过，不是失败。
	AlreadyClaimed bool `json:"already_claimed,omitempty"`
}

// RaccoonCheckin runs the desktop login-reward grant for the provider's
// accounts (all of them, or just one when label is set).
func (g *Gateway) RaccoonCheckin(ctx context.Context, providerName, label string) ([]RaccoonCheckinOutcome, error) {
	rp, err := g.raccoonProvider(providerName)
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
		g.ensureRaccoonToken(ctx, providerName, rp, acc)
		if acc.Cookie == "" {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: "没有可用 access_token，请先登录"})
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		res, err := rp.Client().Checkin(cctx, acc.Cookie)
		cancel()
		if err != nil {
			out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Error: err.Error()})
			continue
		}
		out = append(out, RaccoonCheckinOutcome{Label: acc.Label, Success: res.Success, Message: res.Message, AlreadyClaimed: res.AlreadyClaimed})
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}

// RunRaccoonCheckin is the scheduler entrypoint for type "raccoon" tasks.
func (g *Gateway) RunRaccoonCheckin(providerName string) {
	rp, err := g.raccoonProvider(providerName)
	if err != nil {
		g.log.Warn("签到：%v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, acc := range g.Accounts(providerName) {
		g.ensureRaccoonToken(ctx, providerName, rp, acc)
		if acc.Cookie == "" {
			g.log.Warn("签到：账号「%s」没有可用 access_token", acc.Label)
			continue
		}
		res, err := rp.Client().Checkin(ctx, acc.Cookie)
		if err != nil {
			g.log.Warn("签到：账号「%s」失败: %v", acc.Label, err)
			continue
		}
		if res.Success {
			g.log.Info("签到 %s「%s」成功：%s", providerName, acc.Label, res.Message)
		} else {
			g.log.Info("签到 %s「%s」：%s", providerName, acc.Label, res.Message)
		}
	}
}
