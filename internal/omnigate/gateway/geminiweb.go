package gateway

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/util"
)

// GeminiWebAccountResult describes an account added/updated from pasted cookies.
type GeminiWebAccountResult struct {
	Label  string `json:"label"`
	Name   string `json:"name,omitempty"`
	Valid  bool   `json:"valid"`
	Models int    `json:"models,omitempty"`
	Error  string `json:"error,omitempty"`
}

// geminiWebProvider resolves a provider name to a *geminiweb.Provider.
func (g *Gateway) geminiWebProvider(name string) (*geminiweb.Provider, error) {
	if pcfg := g.cfg.Provider(name); pcfg == nil || pcfg.Type != "geminiweb" {
		return nil, fmt.Errorf("provider %q 不是 geminiweb 类型", name)
	}
	gp, ok := g.providers[name].(*geminiweb.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 geminiweb 类型", name)
	}
	return gp, nil
}

// GeminiWebSetToken adds/updates an account from a pasted Google cookie string.
// The cookie is validated against the upstream before it is stored, so a typo
// fails here rather than at the first chat request.
func (g *Gateway) GeminiWebSetToken(ctx context.Context, providerName, label, cookie string) (*GeminiWebAccountResult, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	cookie = strings.TrimSpace(cookie)
	if cookie == "" {
		return nil, fmt.Errorf("Cookie 不能为空")
	}
	if label == "" {
		label = g.freeGeminiWebLabel(providerName)
	}
	acc := &provider.Account{Label: label, Cookie: cookie}
	vctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	check := gp.CheckAccount(vctx, acc)
	if !check.Valid {
		return nil, fmt.Errorf("Cookie 校验失败：%s", check.Error)
	}
	// CheckAccount may have rotated __Secure-1PSIDTS; store what came back.
	g.upsertGeminiWebAccount(providerName, label, acc.Cookie)
	return &GeminiWebAccountResult{Label: label, Name: label, Valid: true, Models: len(check.Models)}, nil
}

// freeGeminiWebLabel picks the smallest "账号 N" not already in the pool, so an
// empty label can never silently overwrite another account.
func (g *Gateway) freeGeminiWebLabel(providerName string) string {
	used := map[string]bool{}
	for _, a := range g.Accounts(providerName) {
		used[a.Label] = true
	}
	for i := 1; ; i++ {
		l := fmt.Sprintf("账号 %d", i)
		if !used[l] {
			return l
		}
	}
}

// upsertGeminiWebAccount writes the account into the live pool and state.
func (g *Gateway) upsertGeminiWebAccount(providerName, label, cookie string) {
	g.accountsMu.Lock()
	accts := g.accounts[providerName]
	found := false
	for _, a := range accts {
		if a.Label == label {
			a.Cookie = cookie
			found = true
			break
		}
	}
	if !found {
		g.accounts[providerName] = append(accts, &provider.Account{Label: label, Cookie: cookie})
	}
	g.accountsMu.Unlock()

	// state.Cookies 优先于 DynAccount.AccessToken（见 New 的账号合并），写两处保证
	// 「重新粘贴覆盖旧 Cookie」立即生效。
	g.st.SetCookie(providerName, label, cookie)
	g.st.UpsertDynAccount(providerName, state.DynAccount{
		Label:       label,
		Name:        label,
		UserID:      strings.ReplaceAll(util.UUID(), "-", ""),
		AccessToken: cookie,
		AddedAt:     time.Now().Unix(),
	})
}

// GeminiWebFingerprintStatus returns the anti-detection / protocol-masquerade
// state for a provider (current Chrome version, TLS target, header order,
// jitter / version-sync / TLS switches, cookie dir, available TLS targets).
func (g *Gateway) GeminiWebFingerprintStatus(providerName string) (map[string]any, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	return gp.FingerprintStatus(), nil
}

// GeminiWebSetFingerprintTarget switches the TLS ClientHello impersonation
// target (e.g. "chrome120"). It takes effect on the next direct HTTPS handshake.
func (g *Gateway) GeminiWebSetFingerprintTarget(providerName, target string) error {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return err
	}
	return gp.SetChromeTarget(target)
}

// GeminiWebSyncChromeVersion triggers one immediate Chrome stable-version check
// and, if newer, promotes the fingerprint. Bounded so a slow upstream cannot
// hang the panel request.
func (g *Gateway) GeminiWebSyncChromeVersion(ctx context.Context, providerName string) error {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	gp.SyncChromeVersion(cctx)
	return nil
}

// GeminiWebRemoveAccount drops a runtime account and its cookie.
func (g *Gateway) GeminiWebRemoveAccount(providerName, label string) bool {
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
	if gp, err := g.geminiWebProvider(providerName); err == nil {
		gp.ResetAccount(label)
	}
	return g.st.RemoveDynAccount(providerName, label)
}

// GeminiWebAccounts lists a provider's accounts with a best-effort session probe.
func (g *Gateway) GeminiWebAccounts(ctx context.Context, providerName string) ([]map[string]any, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for _, acc := range g.Accounts(providerName) {
		item := map[string]any{
			"label":     acc.Label,
			"has_token": acc.Cookie != "",
		}
		if acc.Cookie == "" {
			item["auth_invalid"] = true
			item["auth_error"] = "未配置 Cookie"
			out = append(out, item)
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		check := gp.CheckAccount(cctx, acc)
		cancel()
		if !check.Valid {
			item["auth_invalid"] = true
			item["auth_error"] = check.Error
			out = append(out, item)
			continue
		}
		item["models"] = check.Models
		out = append(out, item)
	}
	return out, nil
}

// GeminiWebCheckinOutcome is one account's session-probe result.
type GeminiWebCheckinOutcome struct {
	Label   string `json:"label"`
	Success bool   `json:"success"`
	Message string `json:"msg"`
	Error   string `json:"error,omitempty"`
}

// RunGeminiWebCheckin probes every account's session and persists any cookie
// rotation — the scheduler's periodic health check for geminiweb, ported from
// gclient.py's _health_check_loop. It also flags an account whose session no
// longer works so the panel can surface it.
func (g *Gateway) RunGeminiWebCheckin(providerName string) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		g.log.Warn("签到：%v", err)
		return
	}
	for _, acc := range g.Accounts(providerName) {
		if acc.Cookie == "" {
			g.log.Warn("签到：账号「%s」未配置 Cookie", acc.Label)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		check := gp.CheckAccount(ctx, acc)
		cancel()
		if check.Valid {
			g.st.SetCookie(providerName, acc.Label, acc.Cookie)
			g.log.Info("签到 %s「%s」会话有效（%d 个模型）", providerName, acc.Label, len(check.Models))
			continue
		}
		g.sess.MarkExpired(providerName, acc.Label, check.Error)
		g.log.Warn("签到 %s「%s」失败: %s", providerName, acc.Label, check.Error)
	}
}

// GeminiWebCheckin probes each account's session. Gemini Web has no check-in
// reward, so this only verifies the cookie still works (and refreshes the
// cached session), which is what the panel's 签到 button is useful for here.
func (g *Gateway) GeminiWebCheckin(ctx context.Context, providerName, label string) ([]GeminiWebCheckinOutcome, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	out := []GeminiWebCheckinOutcome{}
	matched := false
	for _, acc := range g.Accounts(providerName) {
		if label != "" && acc.Label != label {
			continue
		}
		matched = true
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		check := gp.CheckAccount(cctx, acc)
		cancel()
		if !check.Valid {
			out = append(out, GeminiWebCheckinOutcome{Label: acc.Label, Error: check.Error})
			continue
		}
		out = append(out, GeminiWebCheckinOutcome{
			Label:   acc.Label,
			Success: true,
			Message: fmt.Sprintf("会话有效（%d 个模型可用）", len(check.Models)),
		})
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}
