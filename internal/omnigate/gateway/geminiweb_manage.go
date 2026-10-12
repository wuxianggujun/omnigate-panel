package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb"
)

// This file exposes geminiweb's account-scoped management RPCs (custom Gems, web
// chat history, explicit keep-alive) on the Gateway, mirroring gclient.py's
// list_gems / create_gem / update_gem / delete_gem / list_web_chats /
// delete_web_chat / cleanup_old_web_chats / auto-refresh surface.

// geminiWebAccount resolves a label to a live account in the pool. An empty
// label means "the first account", which is what the panel sends when only one
// account exists.
func (g *Gateway) geminiWebAccount(providerName, label string) (*provider.Account, error) {
	accts := g.Accounts(providerName)
	if label == "" {
		if len(accts) == 0 {
			return nil, fmt.Errorf("供应商 %q 没有可用账号", providerName)
		}
		return accts[0], nil
	}
	for _, a := range accts {
		if a.Label == label {
			return a, nil
		}
	}
	return nil, fmt.Errorf("账号「%s」不存在", label)
}

// GeminiWebListGems lists the account's custom Gems.
func (g *Gateway) GeminiWebListGems(ctx context.Context, providerName, label string) ([]geminiweb.Gem, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.ListGems(cctx, acc)
}

// GeminiWebCreateGem creates a custom Gem and returns its id.
func (g *Gateway) GeminiWebCreateGem(ctx context.Context, providerName, label, name, prompt, description string) (string, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return "", err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return "", err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.CreateGem(cctx, acc, name, prompt, description)
}

// GeminiWebUpdateGem edits a custom Gem.
func (g *Gateway) GeminiWebUpdateGem(ctx context.Context, providerName, label, gemID, name, prompt, description string) (bool, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return false, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.UpdateGem(cctx, acc, gemID, name, prompt, description)
}

// GeminiWebDeleteGem removes a custom Gem.
func (g *Gateway) GeminiWebDeleteGem(ctx context.Context, providerName, label, gemID string) (bool, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return false, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.DeleteGem(cctx, acc, gemID)
}

// GeminiWebListChats lists the account's web chat history (newest first).
func (g *Gateway) GeminiWebListChats(ctx context.Context, providerName, label string, recent int) ([]geminiweb.WebChat, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return nil, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.ListChats(cctx, acc, recent)
}

// GeminiWebDeleteChat removes one web chat.
func (g *Gateway) GeminiWebDeleteChat(ctx context.Context, providerName, label, cid string) (bool, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return false, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return false, err
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return gp.DeleteChat(cctx, acc, cid)
}

// GeminiWebCleanupChats deletes chats older than keepHours. The provider clamps
// keepHours to a 1-hour floor, so a stray 0 cannot wipe the account's history.
func (g *Gateway) GeminiWebCleanupChats(ctx context.Context, providerName, label string, keepHours float64, skipPinned bool) (geminiweb.CleanupResult, error) {
	gp, err := g.geminiWebProvider(providerName)
	if err != nil {
		return geminiweb.CleanupResult{}, err
	}
	acc, err := g.geminiWebAccount(providerName, label)
	if err != nil {
		return geminiweb.CleanupResult{}, err
	}
	// A cleanup run loops list+delete, so give it a generous budget.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	return gp.CleanupChats(cctx, acc, keepHours, skipPinned)
}

// GeminiWebKeepAlive rotates each matching account's cookie and rebuilds its
// session, so the next chat request does not pay for a handshake (or hit an
// aged-out session). An empty label keeps every account alive.
func (g *Gateway) GeminiWebKeepAlive(ctx context.Context, providerName, label string) ([]GeminiWebCheckinOutcome, error) {
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
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		err := gp.KeepAlive(cctx, acc)
		cancel()
		if err != nil {
			out = append(out, GeminiWebCheckinOutcome{Label: acc.Label, Error: err.Error()})
			continue
		}
		out = append(out, GeminiWebCheckinOutcome{Label: acc.Label, Success: true, Message: "保活成功（Cookie 已轮换）"})
	}
	if label != "" && !matched {
		return nil, fmt.Errorf("账号「%s」不存在", label)
	}
	return out, nil
}
