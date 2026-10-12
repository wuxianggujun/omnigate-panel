package geminiweb

import (
	"context"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// Keep-alive: proactively rotate each account's credential cookie and rebuild
// its session so an idle pool does not pay for (or fail on) a cold handshake.
// This is the body of gclient.py's _auto_refresh_loop (rotate -> obtain token ->
// heartbeat). The gateway already refreshes sessions on demand, so the loop is
// opt-in and off by default; when enabled it mirrors the reference's cadence.

const (
	// keepAliveStartDelay lets the gateway finish starting (and load accounts)
	// before the first pass.
	keepAliveStartDelay = time.Minute
	// keepAlivePerAccount bounds one account's rotate + handshake so a single
	// slow upstream cannot stall the whole round.
	keepAlivePerAccount = 90 * time.Second
	// chatCleanupPerAccount bounds one account's cleanup run (list + delete
	// rounds) so a single heavy account cannot stall the whole round.
	chatCleanupPerAccount = 10 * time.Minute
)

// StartKeepAlive starts the background keep-alive loop (idempotent). interval
// <= 0 disables it. accounts is consulted on every tick so accounts added later
// are picked up without restarting the loop.
func (p *Provider) StartKeepAlive(ctx context.Context, interval time.Duration, accounts func() []*provider.Account) {
	if p == nil || interval <= 0 || accounts == nil {
		return
	}
	p.keepAliveOnce.Do(func() {
		p.keepAliveRun.Store(true)
		go p.keepAliveLoop(ctx, interval, accounts)
	})
}

// KeepAliveRunning reports whether the background keep-alive loop was started
// (for a panel status snapshot).
func (p *Provider) KeepAliveRunning() bool {
	return p != nil && p.keepAliveRun.Load()
}

func (p *Provider) keepAliveLoop(ctx context.Context, interval time.Duration, accounts func() []*provider.Account) {
	if !pauseCtx(ctx, keepAliveStartDelay) {
		return
	}
	for {
		for _, acc := range accounts() {
			cctx, cancel := context.WithTimeout(ctx, keepAlivePerAccount)
			if err := p.KeepAlive(cctx, acc); err != nil {
				p.warn("geminiweb: 保活账号 %s 失败: %v", acc.Label, err)
			}
			cancel()
			if ctx.Err() != nil {
				return
			}
		}
		if !pauseCtx(ctx, interval) {
			return
		}
	}
}

// StartChatCleanup starts the background web-chat cleanup loop (idempotent):
// periodically delete each account's Gemini web chats older than keepHours.
// Ported from gemini2api's main.py web_chat_cleanup_loop. interval <= 0 disables
// it. accounts is consulted on every tick so accounts added later are picked up.
func (p *Provider) StartChatCleanup(ctx context.Context, interval time.Duration, keepHours float64, skipPinned bool, accounts func() []*provider.Account) {
	if p == nil || interval <= 0 || accounts == nil {
		return
	}
	p.chatCleanupOnce.Do(func() {
		p.chatCleanupRun.Store(true)
		go p.chatCleanupLoop(ctx, interval, keepHours, skipPinned, accounts)
	})
}

// ChatCleanupRunning reports whether the background web-chat cleanup loop was
// started (for a panel status snapshot).
func (p *Provider) ChatCleanupRunning() bool {
	return p != nil && p.chatCleanupRun.Load()
}

func (p *Provider) chatCleanupLoop(ctx context.Context, interval time.Duration, keepHours float64, skipPinned bool, accounts func() []*provider.Account) {
	if !pauseCtx(ctx, keepAliveStartDelay) {
		return
	}
	for {
		for _, acc := range accounts() {
			cctx, cancel := context.WithTimeout(ctx, chatCleanupPerAccount)
			res, err := p.CleanupChats(cctx, acc, keepHours, skipPinned)
			cancel()
			if err != nil {
				p.warn("geminiweb: 清理账号 %s 会话失败: %v", acc.Label, err)
			} else if res.Deleted > 0 {
				p.warn("geminiweb: 清理账号 %s 会话：删除 %d 个（跳过 %d，失败 %d）",
					acc.Label, res.Deleted, res.Skipped, res.Failed)
			}
			if ctx.Err() != nil {
				return
			}
		}
		if !pauseCtx(ctx, interval) {
			return
		}
	}
}
