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

// TestStreamKeepAlivePing：上游长时间静默时，网关要发保活帧，且必须在终止帧
// [DONE] 之前发完。保活帧是**合法 data: 帧**（不是 SSE 注释）：注释帧只能穿透
// 直连场景，遇到 new-api 这类只转发 data: 行的再编码层会被丢弃，客户端于是在静默
// 期收不到任何字节并按读超时断连（线上 499/interrupted 的成因）。
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
	i := strings.Index(out, `"`+openaikeepaliveField+`":true`)
	j := strings.Index(out, "[DONE]")
	if i < 0 {
		t.Fatalf("stream missing keepalive frame: %s", out)
	}
	if j < 0 || i > j {
		t.Fatalf("keepalive must precede [DONE] (ka=%d done=%d): %s", i, j, out)
	}
	if strings.Contains(out, ": ping") {
		t.Fatalf("SSE comment keepalive must be gone (it cannot cross SSE re-encoders): %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content missing: %s", out)
	}
}

// openaikeepaliveField mirrors openai.KeepAliveField for assertions.
const openaikeepaliveField = "omnigate_keepalive"

// TestStreamNoKeepAliveWhenFast：上游立即应答时不得发保活（静默从未达到阈值）。
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
	if strings.Contains(w.Body.String(), openaikeepaliveField) {
		t.Fatalf("unexpected keepalive frame: %s", w.Body.String())
	}
}
