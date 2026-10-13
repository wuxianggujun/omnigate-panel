package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// namedScriptProvider is a stateless openai-type provider with a settable name
// and scripted stream, used to stand in for a third-party fallback target.
type namedScriptProvider struct {
	name   string
	events []provider.Event
}

func (p *namedScriptProvider) Name() string { return p.name }
func (p *namedScriptProvider) Type() string { return "openai" }
func (p *namedScriptProvider) ListModels(context.Context) ([]openai.Model, error) {
	return []openai.Model{{ID: "m1", Object: "model"}}, nil
}
func (p *namedScriptProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	return &scriptStream{events: p.events}, nil
}

// fallbackGateway builds a gateway whose primary provider "fake" returns an empty
// stream, with an "alt" provider that answers — the classic "Gemini answered
// nothing, try a third party" setup.
func fallbackGateway(alt provider.Provider, fallback *config.FallbackConfig) *Gateway {
	g := testGateway(&scriptProvider{})
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "fake", Type: "openai"},
		{Name: "alt", Type: "openai", Models: []string{"m1"}},
	}
	g.cfg.Fallback = fallback
	g.providers["alt"] = alt
	g.order = []string{"fake", "alt"}
	return g
}

func textProvider(name, text string) *namedScriptProvider {
	return &namedScriptProvider{name: name, events: []provider.Event{
		{Type: provider.EventText, Text: text},
		{Type: provider.EventFinish, Finish: "stop"},
	}}
}

func fbChatReq(stream bool) *openai.ChatRequest {
	return &openai.ChatRequest{
		Model:    "fake/m",
		Stream:   stream,
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
}

// TestFallbackNonStream: an empty primary result is replaced by the fallback.
func TestFallbackNonStream(t *testing.T) {
	g := fallbackGateway(textProvider("alt", "fallback answer"), &config.FallbackConfig{Enabled: true})
	ctx := WithReqMeta(context.Background(), &ReqMeta{})
	w := httptest.NewRecorder()
	g.HandleChat(ctx, fbChatReq(false), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback answer") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// TestFallbackDisabled: with fallback off, an empty result keeps the original
// behaviour (502 empty upstream response) — zero regression.
func TestFallbackDisabled(t *testing.T) {
	g := fallbackGateway(textProvider("alt", "fallback answer"), &config.FallbackConfig{Enabled: false})
	ctx := WithReqMeta(context.Background(), &ReqMeta{})
	w := httptest.NewRecorder()
	g.HandleChat(ctx, fbChatReq(false), w)

	if w.Code == http.StatusOK {
		t.Fatalf("disabled fallback should not rescue the request, body=%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "fallback answer") {
		t.Fatalf("fallback text leaked while disabled: %s", w.Body.String())
	}
}

// TestFallbackStream: a zero-content stream is replaced by the fallback, emitted
// as normal SSE chunks under the original model name.
func TestFallbackStream(t *testing.T) {
	g := fallbackGateway(textProvider("alt", "streamed fallback"), &config.FallbackConfig{Enabled: true})
	ctx := WithReqMeta(context.Background(), &ReqMeta{})
	w := httptest.NewRecorder()
	g.HandleChat(ctx, fbChatReq(true), w)

	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	out := w.Body.String()
	if !strings.Contains(out, "streamed fallback") {
		t.Fatalf("stream = %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("stream missing [DONE]: %s", out)
	}
}

// TestFallbackCandidateSelection checks modality filtering, pinned order, and
// exclusion of the primary model.
func TestFallbackCandidateSelection(t *testing.T) {
	g := fallbackGateway(textProvider("alt", "x"), &config.FallbackConfig{Enabled: true})
	// Give "alt" a mixed catalog: one chat model, one clearly non-chat model.
	g.providers["alt"] = &catalogProvider{name: "alt", models: []openai.Model{
		{ID: "chat-model", Object: "model"},
		{ID: "imagen-3.0-generate", Object: "model"},
	}}

	// Auto: non-chat filtered out, primary excluded.
	cands := g.fallbackCandidates(context.Background(), "fake", "m")
	if len(cands) != 1 || cands[0].Model != "chat-model" {
		t.Fatalf("auto candidates = %+v", cands)
	}

	// Pinned: order is honoured exactly (no modality filter for pinned names).
	g.cfg.Fallback.Models = []string{"imagen-3.0-generate", "chat-model"}
	cands = g.fallbackCandidates(context.Background(), "fake", "m")
	if len(cands) != 2 || cands[0].Model != "imagen-3.0-generate" || cands[1].Model != "chat-model" {
		t.Fatalf("pinned candidates = %+v", cands)
	}

	// Self-routing guard: pinning the primary model yields no candidate.
	g.cfg.Fallback.Models = []string{"fake/m"}
	if got := g.fallbackCandidates(context.Background(), "fake", "m"); len(got) != 0 {
		t.Fatalf("primary should be excluded, got %+v", got)
	}
}
