package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// delayingProvider blocks for delay before handing back its stream, simulating a
// slow first token (e.g. geminiweb extended thinking) so the keepalive can fire.
type delayingProvider struct {
	name   string
	delay  time.Duration
	events []provider.Event
}

func (p *delayingProvider) Name() string { return p.name }
func (p *delayingProvider) Type() string { return "openai" }
func (p *delayingProvider) ListModels(context.Context) ([]openai.Model, error) {
	return nil, nil
}
func (p *delayingProvider) StreamChat(ctx context.Context, _ *provider.Account, _ provider.ChatInput) (provider.Stream, error) {
	select {
	case <-time.After(p.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &scriptStream{events: p.events}, nil
}

// TestStreamKeepAlivePing：上游长时间静默时，网关要发保活注释 ping，且必须在
// 终止帧 [DONE] 之前（移植自参考实现 app/core/stream.py）。
func TestStreamKeepAlivePing(t *testing.T) {
	old := sseKeepAliveInterval
	sseKeepAliveInterval = 20 * time.Millisecond
	defer func() { sseKeepAliveInterval = old }()

	g := testGateway(&delayingProvider{name: "fake", delay: 90 * time.Millisecond, events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), streamReq(), w)

	out := w.Body.String()
	i := strings.Index(out, sseKeepAliveFrame)
	j := strings.Index(out, "[DONE]")
	if i < 0 {
		t.Fatalf("stream missing keepalive ping: %s", out)
	}
	if j < 0 || i > j {
		t.Fatalf("ping must precede [DONE] (ping=%d done=%d): %s", i, j, out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content missing: %s", out)
	}
}

// TestStreamNoKeepAliveWhenFast：上游立即应答时不得发 ping（ticker 从未触发）。
func TestStreamNoKeepAliveWhenFast(t *testing.T) {
	old := sseKeepAliveInterval
	sseKeepAliveInterval = time.Hour
	defer func() { sseKeepAliveInterval = old }()

	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), streamReq(), w)
	if strings.Contains(w.Body.String(), sseKeepAliveFrame) {
		t.Fatalf("unexpected keepalive ping: %s", w.Body.String())
	}
}
