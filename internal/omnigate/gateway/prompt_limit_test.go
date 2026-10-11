package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/runable"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// runableFake 是 runable 形态的假上游：记录收到的 ChatInput，回一段固定正文。
type runableFake struct {
	inputs []provider.ChatInput
}

func (p *runableFake) Name() string                                       { return "fake" }
func (p *runableFake) Type() string                                       { return "runable" }
func (p *runableFake) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }

func (p *runableFake) StreamChat(_ context.Context, _ *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	p.inputs = append(p.inputs, in)
	return &scriptStream{events: []provider.Event{
		{Type: provider.EventText, Text: "ok"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}, nil
}

// runableGateway 搭一个带账号池的 runable 形态网关（openStream 需要 st/cool/sess）。
func runableGateway(t *testing.T, p provider.Provider) *Gateway {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := testGateway(p)
	g.st = st
	g.cool = newCooldownLedger()
	g.sess = newSessionHealth()
	g.accounts["fake"] = []*provider.Account{{Label: "a", Cookie: "c"}}
	return g
}

func chatReq(text string) *openai.ChatRequest {
	return &openai.ChatRequest{
		Model:    "fake/m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: text}}},
	}
}

// 超限：必须在调用上游之前就回 400（省掉一次注定失败的上游请求）。
func TestPromptLimitRejectsBeforeUpstream(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq(strings.Repeat("a", runable.MaxPromptChars+1)), w)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "prompt_too_long") {
		t.Fatalf("错误码不对: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "8000") {
		t.Fatalf("错误文案要说清上限: %s", w.Body.String())
	}
	if len(prov.inputs) != 0 {
		t.Fatalf("超限请求不该打到上游，实际调用 %d 次", len(prov.inputs))
	}
}

// 流式请求同样在写响应头之前拒：否则客户端只会看到 200 流里夹一帧 error，
// 报成「stream disconnected before completion」。
func TestPromptLimitStreamingRejectsWithHTTP400(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	req := chatReq(strings.Repeat("a", runable.MaxPromptChars+1))
	req.Stream = true
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "event-stream") {
		t.Fatalf("不该回 SSE: %s", ct)
	}
	if len(prov.inputs) != 0 {
		t.Fatal("超限请求不该打到上游")
	}
}

// 边界：恰好等于上限要通过（实测上游 8000 通过、8001 拒绝）。
func TestPromptLimitAllowsExactLimit(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	text := strings.Repeat("a", runable.MaxPromptChars)
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq(text), w)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 || prov.inputs[0].Prompt != text {
		t.Fatalf("上游收到的 prompt 不对: %+v", prov.inputs)
	}
}

// 被拒的首轮不能把「system 已发送」置位，否则下一轮会漏发 system。
// 会话 key 取首条用户消息，所以第二轮要沿用同一条首消息。
func TestPromptLimitRejectKeepsSystemPending(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	long := strings.Repeat("a", runable.MaxPromptChars)

	turn := func(extra ...openai.Message) *openai.ChatRequest {
		msgs := []openai.Message{
			{Role: "system", Content: openai.Content{Text: "SYS"}},
			{Role: "user", Content: openai.Content{Text: long}},
		}
		return &openai.ChatRequest{Model: "fake/m", Messages: append(msgs, extra...)}
	}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), turn(), w)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("首轮 status = %d, want 400", w.Code)
	}
	if len(prov.inputs) != 0 {
		t.Fatal("被拒的首轮不该打到上游")
	}

	// 第二轮（同一条首消息 = 同一个会话）：短消息必须仍然带上 system。
	w2 := httptest.NewRecorder()
	g.HandleChat(context.Background(), turn(
		openai.Message{Role: "assistant", Content: openai.Content{Text: "ok"}},
		openai.Message{Role: "user", Content: openai.Content{Text: "hi"}},
	), w2)
	if w2.Code != http.StatusOK {
		t.Fatalf("第二轮 status = %d, want 200 (body=%s)", w2.Code, w2.Body.String())
	}
	if len(prov.inputs) != 1 || prov.inputs[0].Prompt != "SYS\n\nhi" {
		t.Fatalf("第二轮丢了 system: %+v", prov.inputs)
	}
}

// 工具路径（把整段对话压成一条 prompt）同样受闸。
func TestCheckPromptLimitToolsPath(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	req := &openai.ChatRequest{
		Model: "fake/m",
		Tools: []openai.Tool{{Type: "function", Function: openai.ToolDef{Name: "f", Description: "d"}}},
	}
	for i := 0; i < 5; i++ {
		req.Messages = append(req.Messages, openai.Message{
			Role: "user", Content: openai.Content{Text: strings.Repeat("b", 3000)},
		})
	}
	if err := g.checkPromptLimit(prov, "m", req, true); err == nil {
		t.Fatal("超长工具 prompt 应该被拒")
	}
}

// 供应商配置可以覆盖阈值，负数表示显式关闭检查。
func TestPromptLimitConfigOverride(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{{Name: "fake", Type: "runable", MaxPromptChars: 5}}
	if err := g.checkPromptLimit(prov, "m", chatReq("123456"), false); err == nil {
		t.Fatal("配置的 5 字符上限应该生效")
	}
	g.cfg.Providers[0].MaxPromptChars = -1
	if err := g.checkPromptLimit(prov, "m", chatReq(strings.Repeat("a", 20000)), false); err != nil {
		t.Fatalf("负数应关闭检查: %v", err)
	}
}

// 上游自己回绝时（预检被关掉 / 上游收紧了阈值），错误文案要换成人话。
func TestFriendlyUpstreamErrMapsPromptLimit(t *testing.T) {
	raw := errors.New("upstream HTTP 400: PROMPT_LIMIT_EXCEEDED")
	got := friendlyUpstreamErr("runable", 8000, raw)
	if strings.Contains(got.Error(), "PROMPT_LIMIT_EXCEEDED") {
		t.Fatalf("还是上游原话: %v", got)
	}
	if !strings.Contains(got.Error(), "8000") {
		t.Fatalf("要说清上限: %v", got)
	}
	other := errors.New("boom")
	if friendlyUpstreamErr("runable", 8000, other) != other {
		t.Fatal("非 PROMPT_LIMIT 错误不该被改写")
	}
	if friendlyUpstreamErr("runable", 8000, nil) != nil {
		t.Fatal("nil 应原样返回")
	}
}
