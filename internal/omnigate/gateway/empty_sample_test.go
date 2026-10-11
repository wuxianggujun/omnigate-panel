package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// sampleStream yields no events but reports an upstream sample — exactly like a
// real upstream that answered 200 with a non-SSE JSON error body.
type sampleStream struct{ sample []byte }

func (s *sampleStream) Recv() (provider.Event, error) { return provider.Event{}, io.EOF }
func (s *sampleStream) Close() error                  { return nil }
func (s *sampleStream) UpstreamSample() []byte        { return s.sample }

type sampleProvider struct{ sample []byte }

func (p *sampleProvider) Name() string                                       { return "fake" }
func (p *sampleProvider) Type() string                                       { return "openai" }
func (p *sampleProvider) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }
func (p *sampleProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return &sampleStream{sample: p.sample}, nil
}

const upstreamSampleJSON = `{"code":200003,"message":"authorization_verify_error"}`

// 上游 200 + 0 帧（非流式）：上游原文必须出现在 502 的错误信息里。这类失败最常见
// 的成因是上游用 200 回了一个非 SSE 的 JSON 错误体，只报 "empty upstream response"
// 会把限流/审核/参数错的真实原因彻底吞掉。
func TestEmptyResponseSurfacesUpstreamSample(t *testing.T) {
	g := testGateway(&sampleProvider{sample: []byte(upstreamSampleJSON)})
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), &openai.ChatRequest{
		Model:    "fake/m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if !strings.Contains(w.Body.String(), "authorization_verify_error") {
		t.Fatalf("body missing upstream sample: %s", w.Body.String())
	}
}

// 流式同理：error 帧里要带上游原文。
func TestEmptyStreamSurfacesUpstreamSample(t *testing.T) {
	g := testGateway(&sampleProvider{sample: []byte(upstreamSampleJSON)})
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), streamReq(), w)

	if !strings.Contains(w.Body.String(), "authorization_verify_error") {
		t.Fatalf("stream body missing upstream sample: %s", w.Body.String())
	}
}

// 上游什么都没回（没有样本）时不能凭空编出「上游原文」字样。
func TestEmptyStreamWithoutSampleOmitsDetail(t *testing.T) {
	g := testGateway(&sampleProvider{})
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), streamReq(), w)

	body := w.Body.String()
	if !strings.Contains(body, "empty upstream stream") {
		t.Fatalf("body = %s", body)
	}
	if strings.Contains(body, "上游原文") {
		t.Fatalf("body invented an upstream detail: %s", body)
	}
}
