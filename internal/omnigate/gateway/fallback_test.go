package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	meta := &ReqMeta{}
	ctx := WithReqMeta(context.Background(), meta)
	w := httptest.NewRecorder()
	g.HandleChat(ctx, fbChatReq(false), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "fallback answer") {
		t.Fatalf("body = %s", w.Body.String())
	}
	if meta.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2（主目标 + 备用目标）", meta.Attempts)
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

// TestFallbackRespectsKeyScope：按供应商收窄的入站密钥，其授权范围必须一路管到
// 兜底候选。入口 allowModel 只校验主目标，而引流链的后续目标和 cfg.Fallback 的
// 候选不走那道校验 —— 网关侧不过滤，一个只授了 raccoon 的 key 就能顺着链打到
// runable，「按供应商收窄」形同虚设。
func TestFallbackRespectsKeyScope(t *testing.T) {
	run := func(keyProviders []string) (int, string) {
		g := fallbackGateway(textProvider("alt", "fallback answer"), &config.FallbackConfig{Enabled: true})
		meta := &ReqMeta{KeyProviders: keyProviders}
		ctx := WithReqMeta(context.Background(), meta)
		w := httptest.NewRecorder()
		g.HandleChat(ctx, fbChatReq(false), w)
		return w.Code, w.Body.String()
	}

	// 1) 全量密钥（KeyProviders 为空）：照旧兜底，不能因为加了过滤就回归。
	if code, body := run(nil); code != http.StatusOK || !strings.Contains(body, "fallback answer") {
		t.Fatalf("全量密钥应照旧兜底，got code=%d body=%s", code, body)
	}

	// 2) 密钥只授 fake：alt 是权限外供应商，兜底必须被拦下。
	code, body := run([]string{"fake"})
	if code == http.StatusOK && strings.Contains(body, "fallback answer") {
		t.Fatalf("只授 fake 的密钥不该让 alt 兜底成功，body=%s", body)
	}
	if strings.Contains(body, "fallback answer") {
		t.Fatalf("alt 的正文泄漏了: %s", body)
	}

	// 3) 密钥同时授 fake + alt：兜底恢复（范围是白名单，不是「只剩第一个」）。
	if code, body := run([]string{"fake", "alt"}); code != http.StatusOK || !strings.Contains(body, "fallback answer") {
		t.Fatalf("授权含 alt 时应能兜底，got code=%d body=%s", code, body)
	}
}

// TestChainFallbackRespectsKeyScope：引流链的回退目标同样受收窄范围约束。
// 主链路返回空流，链的第二个目标有正文 —— 只授 fake 的密钥必须打不通 alt，
// 授权含 alt 时才通。别名不能与链里任何目标同名（dropSelf 会把它删掉，主目标
// 就整个变成权限外供应商，那条路径该由入口 allowModel 拦，不在这儿测）。
func TestChainFallbackRespectsKeyScope(t *testing.T) {
	const alias = "drain-expiring"
	build := func() *Gateway {
		g := fallbackGateway(textProvider("alt", "fallback answer"), &config.FallbackConfig{Enabled: false})
		g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "m.json"))
		if err := g.SetModelMappingTargets(alias, []string{"fake/m", "alt/m1"}); err != nil {
			t.Fatalf("配链: %v", err)
		}
		return g
	}
	chatReq := func() *openai.ChatRequest {
		return &openai.ChatRequest{
			Model:    alias,
			Stream:   false,
			Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
		}
	}

	g := build()
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{KeyProviders: []string{"fake"}}), chatReq(), w)
	if strings.Contains(w.Body.String(), "fallback answer") {
		t.Fatalf("只授 fake 的密钥不该让引流链回退到 alt: %s", w.Body.String())
	}

	g2 := build()
	w2 := httptest.NewRecorder()
	g2.HandleChat(WithReqMeta(context.Background(), &ReqMeta{KeyProviders: []string{"fake", "alt"}}), chatReq(), w2)
	if w2.Code != http.StatusOK || !strings.Contains(w2.Body.String(), "fallback answer") {
		t.Fatalf("授权含 alt 时引流链应能回退，got code=%d body=%s", w2.Code, w2.Body.String())
	}
}

// TestFallbackFailureNamesAttemptedTargets：主目标 429、备用目标也额度耗尽时，
// 客户端拿到的错误必须同时说明「切到了谁、为什么仍失败」。此前只回主目标错误，
// 线上看起来就像网关没有切换。
func TestFallbackFailureNamesAttemptedTargets(t *testing.T) {
	primary := &routeTestProvider{
		name: "cline-pass",
		err:  errors.New(`upstream HTTP 429: {"error":{"code":"INFERENCE_CAP_ERROR","message":"monthly Clinepass limit"}}`),
	}
	backup := &routeTestProvider{
		name: "yijia",
		err:  errors.New(`upstream HTTP 502: {"error":{"message":"weekly usage limit"}}`),
	}
	g := testGateway(primary)
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "cline-pass", Type: "openai"},
		{Name: "yijia", Type: "openai"},
	}
	g.providers["yijia"] = backup
	g.order = []string{"cline-pass", "yijia"}
	g.cfg.DefaultProvider = "cline-pass"
	g.cfg.DefaultModel = "m"
	g.cfg.Fallback = &config.FallbackConfig{}
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err := g.SetModelMappingTargets("drain", []string{"cline-pass/m", "yijia/m"}); err != nil {
		t.Fatalf("配链: %v", err)
	}

	meta := &ReqMeta{}
	w := httptest.NewRecorder()
	g.HandleChat(WithReqMeta(context.Background(), meta), &openai.ChatRequest{
		Model:    "drain",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}, w)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "monthly Clinepass limit") {
		t.Fatalf("错误正文没有保留主目标原因: %s", body)
	}
	if !strings.Contains(body, "yijia/m") || !strings.Contains(body, "quota") {
		t.Fatalf("错误正文没有说明已尝试 yijia: %s", body)
	}
	if !strings.Contains(meta.Failure, "yijia/m") {
		t.Fatalf("请求记录应带上兜底尝试，Failure=%q", meta.Failure)
	}
	if meta.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2（主目标 + 备用目标）", meta.Attempts)
	}
}
