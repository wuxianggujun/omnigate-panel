package geminiweb

import (
	"context"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// This file ports gclient.py's management surface — the account-scoped RPCs that
// are not part of a chat turn: custom Gems (list/create/update/delete), the web
// chat history (list/delete/cleanup) and the explicit keep-alive. They all share
// the same session (jar + SNlM0e token) a chat turn uses, so a management call
// also refreshes a stale session.

// noteAuth marks the account unhealthy when err is an auth failure, so the next
// call rebuilds the session instead of repeating a doomed request.
func (p *Provider) noteAuth(acc *provider.Account, err error) {
	if Unauthorized(err) {
		p.markUnhealthy(acc, err)
	}
}

// ListGems returns the account's custom Gems.
func (p *Provider) ListGems(ctx context.Context, acc *provider.Account) ([]Gem, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return nil, err
	}
	gems, err := p.client.listGems(ctx, s.jar, s.sessionToken)
	if err != nil {
		p.noteAuth(acc, err)
		return nil, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return gems, nil
}

// CreateGem creates a custom Gem and returns its id ("" when the upstream
// accepted the call but returned no id).
func (p *Provider) CreateGem(ctx context.Context, acc *provider.Account, name, prompt, description string) (string, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return "", err
	}
	id, err := p.client.createGem(ctx, s.jar, s.sessionToken, name, prompt, description)
	if err != nil {
		p.noteAuth(acc, err)
		return "", err
	}
	acc.Cookie = s.jar.CookieHeader()
	return id, nil
}

// UpdateGem edits a custom Gem.
func (p *Provider) UpdateGem(ctx context.Context, acc *provider.Account, gemID, name, prompt, description string) (bool, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return false, err
	}
	ok, err := p.client.updateGem(ctx, s.jar, s.sessionToken, gemID, name, prompt, description)
	if err != nil {
		p.noteAuth(acc, err)
		return false, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return ok, nil
}

// DeleteGem removes a custom Gem.
func (p *Provider) DeleteGem(ctx context.Context, acc *provider.Account, gemID string) (bool, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return false, err
	}
	ok, err := p.client.deleteGem(ctx, s.jar, s.sessionToken, gemID)
	if err != nil {
		p.noteAuth(acc, err)
		return false, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return ok, nil
}

// ListChats lists the account's web chat history (newest first). recent caps one
// page (<=0 uses the default 300).
func (p *Provider) ListChats(ctx context.Context, acc *provider.Account, recent int) ([]WebChat, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return nil, err
	}
	chats, err := p.client.listWebChats(ctx, s.jar, s.sessionToken, recent)
	if err != nil {
		p.noteAuth(acc, err)
		return nil, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return chats, nil
}

// DeleteChat removes one web chat (cid must look like "c_xxx").
func (p *Provider) DeleteChat(ctx context.Context, acc *provider.Account, cid string) (bool, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return false, err
	}
	ok, err := p.client.deleteWebChat(ctx, s.jar, s.sessionToken, cid)
	if err != nil {
		p.noteAuth(acc, err)
		return false, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return ok, nil
}

// CleanupChats deletes web chats older than keepHours (pinned ones optionally
// skipped) and returns the tallies.
func (p *Provider) CleanupChats(ctx context.Context, acc *provider.Account, keepHours float64, skipPinned bool) (CleanupResult, error) {
	s, err := p.ensureSession(ctx, acc)
	if err != nil {
		return CleanupResult{}, err
	}
	res, err := p.client.cleanupWebChats(ctx, s.jar, s.sessionToken, keepHours, skipPinned)
	if err != nil {
		p.noteAuth(acc, err)
		return res, err
	}
	acc.Cookie = s.jar.CookieHeader()
	return res, nil
}

// KeepAlive proactively rotates the credential cookie and rebuilds the session
// (the body of the reference's _auto_refresh_loop, minus its timer). The gateway
// refreshes sessions on demand; this exists so an operator can warm a session
// before it is needed, and so an optional background loop can reuse it.
func (p *Provider) KeepAlive(ctx context.Context, acc *provider.Account) error {
	raw := strings.TrimSpace(acc.Cookie)
	if raw == "" {
		return &UpstreamError{Status: 401, Message: "该账号未配置 Cookie"}
	}
	jar := p.fpRuntime().OpenJar(fingerprint.AccountKey(raw))
	jar.MergeFrom(raw)
	// Rotate __Secure-1PSIDTS: ordinary credential maintenance the browser also
	// performs, which heads off the hard failure when the session ages out.
	if err := p.client.rotateCookies(ctx, jar); err != nil {
		p.warn("geminiweb: 保活轮换 Cookie 失败: %v", err)
	}
	s := &session{jar: jar, sessionUUID: newUUID(), source: jar.CookieHeader()}
	token, push, err := p.client.acquire(ctx, s.jar)
	if err != nil {
		p.markUnhealthy(acc, err)
		return err
	}
	s.sessionToken = token
	s.pushID = push
	if fm, serr := p.client.status(ctx, s.jar, token); serr == nil && len(fm) > 0 {
		s.familyModel = fm
	}
	s.healthy = true
	p.store(p.sessionKey(acc), s)
	acc.Cookie = s.jar.CookieHeader()
	return nil
}
