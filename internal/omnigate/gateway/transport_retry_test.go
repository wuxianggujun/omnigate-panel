package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// flakyStreamProvider 在最初的几次建流中返回指定错误，之后返回正常脚本流。
// 用来验证「响应头到达前断连」只会被原地重试一次，而不会无限重试。
type flakyStreamProvider struct {
	name     string
	failures int
	err      error
	events   []provider.Event
	calls    int
}

func (p *flakyStreamProvider) Name() string { return p.name }
func (p *flakyStreamProvider) Type() string { return "openai" }
func (p *flakyStreamProvider) ListModels(context.Context) ([]openai.Model, error) {
	return nil, nil
}
func (p *flakyStreamProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	p.calls++
	if p.calls <= p.failures {
		return nil, p.err
	}
	return &scriptStream{events: p.events}, nil
}

func retryReq(stream bool) *openai.ChatRequest {
	return &openai.ChatRequest{
		Model:    "fake/m",
		Stream:   stream,
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
}

// 无账号池的密钥型上游在拿到响应头前断开时，同一个上游应重试一次；
// 线上 fengshao1227 的 `unexpected EOF` 正属于这一类。
func TestOpenStreamRetriesTransientFailure(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			p := &flakyStreamProvider{
				name:     "fake",
				failures: 1,
				err:      fmt.Errorf("Post \"https://example.test/v1/chat/completions\": %w", io.ErrUnexpectedEOF),
				events: []provider.Event{
					{Type: provider.EventText, Text: "recovered"},
					{Type: provider.EventFinish, Finish: "stop"},
				},
			}
			g := testGateway(p)
			w := httptest.NewRecorder()
			g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), retryReq(stream), w)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if p.calls != 2 {
				t.Fatalf("StreamChat calls = %d, want 2 (one transient retry)", p.calls)
			}
			if !strings.Contains(w.Body.String(), "recovered") {
				t.Fatalf("body = %s", w.Body.String())
			}
		})
	}
}

// 明确的上游拒绝（429）不是瞬时断连：不能原地重试，应立即走路由冷却/兜底。
func TestOpenStreamDoesNotRetryExplicitFailure(t *testing.T) {
	p := &flakyStreamProvider{
		name:     "fake",
		failures: 1,
		err:      errors.New("upstream HTTP 429: weekly usage limit"),
		events: []provider.Event{
			{Type: provider.EventText, Text: "should not be reached"},
			{Type: provider.EventFinish, Finish: "stop"},
		},
	}
	g := testGateway(p)
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), retryReq(false), w)

	if p.calls != 1 {
		t.Fatalf("StreamChat calls = %d, want 1 (explicit 429 must not retry)", p.calls)
	}
	if w.Code == http.StatusOK {
		t.Fatalf("status = %d, want failure", w.Code)
	}
}
