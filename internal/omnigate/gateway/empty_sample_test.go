package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
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

// 上游正常收尾（发了 finish_reason）但一个正文/思考字节都没有：这是合法空回复
// （实测成因：max_tokens 很小时模型的输出全在上游不上报的推理里），必须按 OpenAI
// 语义回 200 + 空正文 + finish_reason，而不是误判成上游故障。
func TestEmptyResponseWithFinishIsNotAFailure(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventFinish, Finish: "length"},
	}})
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), &openai.ChatRequest{
		Model:    "fake/m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not json: %v", err)
	}
	ch := got["choices"].([]any)[0].(map[string]any)
	if ch["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want length", ch["finish_reason"])
	}
	if msg := ch["message"].(map[string]any); msg["content"] != "" {
		t.Errorf("content = %v, want empty string", msg["content"])
	}
}

// 流式同理：收到过 finish_reason 的空回复不能写 error 帧，也不能记成 stream_error。
func TestEmptyStreamWithFinishIsNotAFailure(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventFinish, Finish: "length"},
	}})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), w)

	body := w.Body.String()
	if strings.Contains(body, "upstream_error") {
		t.Fatalf("empty-but-finished stream reported a failure: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Fatalf("body missing finish_reason: %s", body)
	}
	if meta.Outcome != "" {
		t.Fatalf("outcome = %q, want success (empty)", meta.Outcome)
	}
}

// 连 finish_reason 都没有的空流仍然是失败（上游真断了）：这条边界不能被放宽吞掉。
func TestEmptyStreamWithoutFinishIsStillAFailure(t *testing.T) {
	g := testGateway(&scriptProvider{events: nil})
	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), w)

	if meta.Outcome != reqlog.OutcomeStreamError || meta.Status != http.StatusBadGateway {
		t.Fatalf("outcome=%q status=%d, want stream_error/502", meta.Outcome, meta.Status)
	}
	if !strings.Contains(w.Body.String(), "upstream_error") {
		t.Fatalf("body = %s", w.Body.String())
	}
}
