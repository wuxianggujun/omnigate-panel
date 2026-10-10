package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// scriptStream replays a fixed event list then EOF.
type scriptStream struct {
	events []provider.Event
	i      int
}

func (s *scriptStream) Recv() (provider.Event, error) {
	if s.i >= len(s.events) {
		return provider.Event{}, io.EOF
	}
	e := s.events[s.i]
	s.i++
	return e, nil
}

func (s *scriptStream) Close() error { return nil }

// scriptProvider is a stateless ("openai"-type) provider that returns a scripted
// stream. No accounts, so openStream takes the api-key branch and never touches
// the account pool / state.
type scriptProvider struct{ events []provider.Event }

func (p *scriptProvider) Name() string                                        { return "fake" }
func (p *scriptProvider) Type() string                                        { return "openai" }
func (p *scriptProvider) ListModels(context.Context) ([]openai.Model, error)  { return nil, nil }
func (p *scriptProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return &scriptStream{events: p.events}, nil
}

func testGateway(p provider.Provider) *Gateway {
	return &Gateway{
		cfg:        &config.Config{DefaultProvider: "fake", DefaultModel: "m"},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{p.Name(): p},
		order:      []string{p.Name()},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
		convs:      map[string]*conv{},
		pending:    map[string]raccoonPending{},
	}
}

func streamReq() *openai.ChatRequest {
	return &openai.ChatRequest{
		Model:    "fake/m",
		Stream:   true,
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
}

// 上游 error 帧（HTTP 头已发 200）必须收敛为 stream_error + 5xx 观测，否则请求
// 记录里是假成功。
func TestStreamOutcomeErrorFrame(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventError, Text: "rate limited"},
	}})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), w)

	if meta.Outcome != reqlog.OutcomeStreamError {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, reqlog.OutcomeStreamError)
	}
	if meta.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", meta.Status)
	}
	if !strings.Contains(w.Body.String(), "upstream_error") {
		t.Fatalf("body missing error frame: %s", w.Body.String())
	}
}

// 上游 200 但 0 有效帧 → 空流，同样是失败（502 观测）。
func TestStreamOutcomeEmpty(t *testing.T) {
	g := testGateway(&scriptProvider{events: nil})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), w)

	if meta.Outcome != reqlog.OutcomeStreamError || meta.Status != http.StatusBadGateway {
		t.Fatalf("outcome=%q status=%d, want stream_error/502", meta.Outcome, meta.Status)
	}
}

// 正常流：不覆盖 outcome/status，正文照常下发。
func TestStreamOutcomeSuccess(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hello"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), w)

	if meta.Outcome != "" || meta.Status != 0 {
		t.Fatalf("outcome=%q status=%d, want empty (success)", meta.Outcome, meta.Status)
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// 客户端断连（写失败）：上游帧无恙，归为 interrupted，且不改观测状态码。
func TestStreamOutcomeInterrupted(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hello"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	meta := &ReqMeta{}
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), &failWriter{})

	if meta.Outcome != reqlog.OutcomeInterrupted {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, reqlog.OutcomeInterrupted)
	}
	if meta.Status != 0 {
		t.Fatalf("status = %d, want 0 (keep actual 200)", meta.Status)
	}
}

// 客户端断连的另一种形态：连接关闭使 ctx 取消，上游读随之中断。必须归为
// interrupted（人已走），而不是上游故障 stream_error。
func TestStreamOutcomeClientGone(t *testing.T) {
	g := testGateway(&errProvider{err: errors.New("read: connection reset by peer")})
	meta := &ReqMeta{}
	ctx, cancel := context.WithCancel(WithReqMeta(context.Background(), meta))
	cancel()
	g.HandleChat(ctx, streamReq(), httptest.NewRecorder())

	if meta.Outcome != reqlog.OutcomeInterrupted {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, reqlog.OutcomeInterrupted)
	}
	if meta.Status != 0 {
		t.Fatalf("status = %d, want 0 (keep actual 200)", meta.Status)
	}
}

// errStream always fails its Recv, simulating an upstream read that dies.
type errStream struct{ err error }

func (s *errStream) Recv() (provider.Event, error) { return provider.Event{}, s.err }
func (s *errStream) Close() error                  { return nil }

type errProvider struct{ err error }

func (p *errProvider) Name() string                                       { return "fake" }
func (p *errProvider) Type() string                                       { return "openai" }
func (p *errProvider) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }
func (p *errProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return &errStream{err: p.err}, nil
}

// failWriter fails every write, simulating a disconnected client mid-stream.
type failWriter struct{ h http.Header }

func (w *failWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}
func (w *failWriter) WriteHeader(int)          {}
func (w *failWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }
func (w *failWriter) Flush()                    {}
