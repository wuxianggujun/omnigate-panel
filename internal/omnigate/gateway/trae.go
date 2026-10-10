package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/trae"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// traeProvider returns the TRAE provider named name, or an error.
func (g *Gateway) traeProvider(name string) (*trae.Provider, error) {
	tp, ok := g.providers[name].(*trae.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 trae 类型", name)
	}
	return tp, nil
}

// TraeAuthorize starts a browser login: it mints a per-account machine/device
// fingerprint, remembers it under a state id, and returns the login URL for the
// user to open. The tokens come back via TraeCompleteCallback (paste-the-URL).
func (g *Gateway) TraeAuthorize(providerName, label string) (map[string]string, error) {
	if _, err := g.traeProvider(providerName); err != nil {
		return nil, err
	}
	if strings.TrimSpace(label) == "" {
		label = fmt.Sprintf("TRAE 账号 %d", len(g.Accounts(providerName))+1)
	}
	machineID, err := trae.NewMachineID()
	if err != nil {
		return nil, err
	}
	deviceID, err := trae.NewCheckinDeviceID()
	if err != nil {
		return nil, err
	}
	stateID := util.UUID()

	g.traePendMu.Lock()
	for k, v := range g.traePend {
		if time.Since(v.at) > 15*time.Minute {
			delete(g.traePend, k)
		}
	}
	g.traePend[stateID] = traePending{
		provider:  providerName,
		label:     label,
		machineID: machineID,
		deviceID:  deviceID,
		at:        time.Now(),
	}
	g.traePendMu.Unlock()

	return map[string]string{
		"provider":      providerName,
		"label":         label,
		"state":         stateID,
		"mode":          "manual",
		"authorize_url": trae.BuildLoginURL(machineID, deviceID, trae.DefaultCallbackURL),
		"callback_hint": "http://127.0.0.1:18080/authorize?refreshToken=..&userInfo=..",
	}, nil
}

// TraeLoginResult is the outcome of adding a TRAE account.
type TraeLoginResult struct {
	Label        string `json:"label"`
	Name         string `json:"name"`
	UserID       string `json:"user_id"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

// TraeCompleteCallback parses a pasted login callback URL, exchanges the refresh
// token for a usable access token, resolves the user and stores the account.
func (g *Gateway) TraeCompleteCallback(ctx context.Context, providerName, stateID, callbackURL string) (*TraeLoginResult, error) {
	info, err := trae.ParseCallback(callbackURL)
	if err != nil {
		return nil, err
	}

	var pend traePending
	ok := false
	if stateID != "" {
		g.traePendMu.Lock()
		pend, ok = g.traePend[stateID]
		delete(g.traePend, stateID)
		g.traePendMu.Unlock()
	}
	if ok && time.Since(pend.at) > 15*time.Minute {
		ok = false
	}
	if !ok {
		// 无 pending（或已过期）：新生成指纹。deviceId 只用于签到（须互异且
		// 稳定），此处新生成即可，不影响 refreshToken 的可用性。
		machineID, _ := trae.NewMachineID()
		deviceID, _ := trae.NewCheckinDeviceID()
		pend = traePending{provider: providerName, machineID: machineID, deviceID: deviceID}
	}
	if providerName == "" {
		providerName = pend.provider
	}
	tp, err := g.traeProvider(providerName)
	if err != nil {
		return nil, err
	}

	a := &trae.Auth{
		AccessToken:  info.AccessToken,
		RefreshToken: info.RefreshToken,
		ExpiresAt:    info.ExpiresAt,
		UID:          info.UID,
		MachineID:    pend.machineID,
		DeviceID:     pend.deviceID,
		ApiHost:      trae.DefaultOAuthHost,
	}
	// 有 refreshToken → 换一个可用 access token（同时轮换 refreshToken）。
	if a.RefreshToken != "" {
		if err := tp.Client().RefreshAccount(ctx, a); err != nil {
			return nil, fmt.Errorf("换取 access token 失败：%w", err)
		}
	}
	if a.AccessToken == "" {
		return nil, fmt.Errorf("回调未提供可用 token，请重新登录")
	}
	// 补全 uid / 昵称（失败不阻断，UID 允许为空）。
	nickname := info.Nickname
	if uid, nick, ent, gerr := tp.Client().GetUserInfo(ctx, a); gerr == nil {
		if uid != "" {
			a.UID = uid
		}
		if nick != "" {
			nickname = nick
		}
		if ent != "" {
			info.EnterpriseID = ent
		}
	}

	return g.upsertTraeAccount(providerName, pend.label, a, nickname, info.EnterpriseID), nil
}

// upsertTraeAccount adds or updates a TRAE account and persists it.
func (g *Gateway) upsertTraeAccount(providerName, label string, a *trae.Auth, name, enterpriseID string) *TraeLoginResult {
	if strings.TrimSpace(label) == "" {
		label = fmt.Sprintf("TRAE 账号 %d", len(g.Accounts(providerName))+1)
	}
	if strings.TrimSpace(name) == "" {
		name = "TRAE 账号"
	}

	g.accountsMu.Lock()
	accts := g.accounts[providerName]
	found := false
	for _, x := range accts {
		if x.Label == label {
			x.Cookie = a.AccessToken
			x.RefreshToken = a.RefreshToken
			x.ExpiresAt = a.ExpiresAt
			x.UID = a.UID
			x.MachineID = a.MachineID
			x.DeviceID = a.DeviceID
			x.ApiHost = a.ApiHost
			found = true
			break
		}
	}
	if !found {
		g.accounts[providerName] = append(accts, &provider.Account{
			Label:        label,
			Cookie:       a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt,
			UID:          a.UID,
			MachineID:    a.MachineID,
			DeviceID:     a.DeviceID,
			ApiHost:      a.ApiHost,
		})
	}
	g.accountsMu.Unlock()

	g.st.UpsertDynAccount(providerName, state.DynAccount{
		Label:        label,
		Name:         name,
		UserID:       a.UID,
		OrgName:      enterpriseID,
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		MachineID:    a.MachineID,
		DeviceID:     a.DeviceID,
		ApiHost:      a.ApiHost,
		ExpiresAt:    a.ExpiresAt,
		AddedAt:      time.Now().Unix(),
	})

	out := &TraeLoginResult{Label: label, Name: name, UserID: a.UID, EnterpriseID: enterpriseID}
	if a.ExpiresAt > 0 {
		out.ExpiresAt = time.Unix(a.ExpiresAt, 0).Format(time.RFC3339)
	}
	return out
}

// TraeRemoveAccount drops a runtime account and its tokens. TRAE accounts live in
// the same runtime list as raccoon's, so the plumbing is shared.
func (g *Gateway) TraeRemoveAccount(providerName, label string) bool {
	return g.RaccoonRemoveAccount(providerName, label)
}

// ensureTraeToken refreshes acc when its access token is expired or missing.
func (g *Gateway) ensureTraeToken(ctx context.Context, providerName string, tp *trae.Provider, acc *provider.Account) {
	if acc.RefreshToken == "" || !tp.TokenExpired(acc) {
		return
	}
	if err := g.refreshToken(ctx, providerName, tp, acc); err != nil {
		g.log.Warn("账号「%s」刷新 token 失败: %v", acc.Label, err)
	}
}

// TraeAccounts lists a provider's accounts with a best-effort credit balance.
func (g *Gateway) TraeAccounts(ctx context.Context, providerName string) ([]map[string]any, error) {
	tp, err := g.traeProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		item := map[string]any{
			"label":     acc.Label,
			"has_token": acc.Cookie != "",
		}
		if acc.ExpiresAt > 0 {
			item["expires_at"] = time.Unix(acc.ExpiresAt, 0).Format(time.RFC3339)
		}
		if acc.Cookie != "" {
			g.ensureTraeToken(ctx, providerName, tp, acc)
			bctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			remain, limit, used, packs, berr := tp.Client().EntUsage(bctx, trae.AuthOf(acc))
			cancel()
			if berr != nil {
				item["balance_error"] = berr.Error()
			} else {
				item["available"] = remain
				item["credits_limit"] = limit
				item["credits_used"] = used
				item["packs"] = packs
			}
		}
		out = append(out, item)
	}
	return out, nil
}

// TraeCheckinOutcome is one account's check-in result.
type TraeCheckinOutcome struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Message string `json:"msg"`
	Error   string `json:"error,omitempty"`
	// AlreadyClaimed 表示今天已领过（中性，不是失败）。
	AlreadyClaimed bool `json:"already_claimed,omitempty"`
}

// TraeCheckin runs the daily check-in for the provider's accounts (all of them,
// or just one when label is set).
func (g *Gateway) TraeCheckin(ctx context.Context, providerName, label string) ([]TraeCheckinOutcome, error) {
	tp, err := g.traeProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []TraeCheckinOutcome{}
	matched := false
	for _, acc := range g.Accounts(providerName) {
		if label != "" && acc.Label != label {
			continue
		}
		matched = true
		g.ensureTraeToken(ctx, providerName, tp, acc)
		if acc.Cookie == "" {
			out = append(out, TraeCheckinOutcome{Label: acc.Label, Error: "没有可用 access_token，请先登录"})
			continue
		}
		a := trae.AuthOf(acc)
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		checkedIn, credits, enable, serr := tp.Client().CheckinStatus(cctx, a)
		if serr != nil {
			cancel()
			out = append(out, TraeCheckinOutcome{Label: acc.Label, Error: serr.Error()})
			continue
		}
		if !enable {
			cancel()
			out = append(out, TraeCheckinOutcome{Label: acc.Label, Error: "该账号未开启签到"})
			continue
		}
		if checkedIn {
			cancel()
			out = append(out, TraeCheckinOutcome{
				Label:          acc.Label,
				Success:        true,
				AlreadyClaimed: true,
				Message:        fmt.Sprintf("今日已签到（当前积分 %d）", credits),
			})
			continue
		}
		cerr := tp.Client().CheckinClaim(cctx, a)
		cancel()
		if cerr != nil {
			out = append(out, TraeCheckinOutcome{Label: acc.Label, Error: cerr.Error()})
			continue
		}
		out = append(out, TraeCheckinOutcome{
			Label:   acc.Label,
			Success: true,
			Message: fmt.Sprintf("签到成功（签到前积分 %d）", credits),
		})
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}

// RunTraeCheckin is the scheduler entry point: check in every account.
func (g *Gateway) RunTraeCheckin(providerName string) {
	if _, err := g.traeProvider(providerName); err != nil {
		g.log.Warn("签到：%v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	outcomes, err := g.TraeCheckin(ctx, providerName, "")
	if err != nil {
		g.log.Warn("签到：%v", err)
		return
	}
	for _, o := range outcomes {
		switch {
		case o.Error != "":
			g.log.Warn("签到 %s「%s」失败: %s", providerName, o.Label, o.Error)
		default:
			g.log.Info("签到 %s「%s」：%s", providerName, o.Label, o.Message)
		}
	}
}
