package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

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

// 超限：不再拒绝，而是截断到上限以内后照常打到上游（用户拿得到回答）。
func TestPromptLimitTruncatesBeforeUpstream(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq(strings.Repeat("a", runable.MaxPromptChars+500)), w)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	got := prov.inputs[0].Prompt
	if n := utf8.RuneCountInString(got); n > runable.MaxPromptChars {
		t.Fatalf("截断后仍超限: %d 字符", n)
	}
	if !strings.Contains(got, "省略") {
		t.Fatalf("截断应留下省略标记: %q", got[:60])
	}
}

// 流式请求同样在写响应头之前就截断：客户端拿到的是一个正常的 SSE 流，而不是
// 400，也不是「stream disconnected before completion」。
func TestPromptLimitStreamingTruncates(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	req := chatReq(strings.Repeat("a", runable.MaxPromptChars+500))
	req.Stream = true
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("流式请求应回 SSE: %s", ct)
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	if n := utf8.RuneCountInString(prov.inputs[0].Prompt); n > runable.MaxPromptChars {
		t.Fatalf("截断后仍超限: %d 字符", n)
	}
}

// 边界：恰好等于上限要原样转发（实测上游 8000 通过、8001 拒绝）。
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
		t.Fatalf("未超限的 prompt 不该被改动: %+v", prov.inputs)
	}
}

// 截断必须保留开头（system / 提问）与结尾，且首轮照常把 system 带上。
func TestPromptLimitTruncateKeepsSystem(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	long := strings.Repeat("a", runable.MaxPromptChars+500)

	req := &openai.ChatRequest{Model: "fake/m", Messages: []openai.Message{
		{Role: "system", Content: openai.Content{Text: "SYS"}},
		{Role: "user", Content: openai.Content{Text: long}},
	}}
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	got := prov.inputs[0].Prompt
	if !strings.HasPrefix(got, "SYS") {
		t.Fatalf("截断后丢了 system: %q", got[:40])
	}
	if n := utf8.RuneCountInString(got); n > runable.MaxPromptChars {
		t.Fatalf("截断后仍超限: %d 字符", n)
	}
}

// 工具路径（把整段对话压成一条 prompt）同样会被截断到上限以内。
func TestClampPromptToolsPath(t *testing.T) {
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
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	if n := utf8.RuneCountInString(prov.inputs[0].Prompt); n > runable.MaxPromptChars {
		t.Fatalf("工具 prompt 截断后仍超限: %d 字符", n)
	}
}

// clampPrompt 的直接单测：不超限原样返回；超限截到 n 以内且保留头尾。
func TestClampPrompt(t *testing.T) {
	short := "hello"
	if got, raw := clampPrompt(short, 8000); got != short || raw != 5 {
		t.Fatalf("短文本不该被改: %q raw=%d", got, raw)
	}
	// limit<=0 = 关闭限制：原样返回。
	long := strings.Repeat("x", 100)
	if got, raw := clampPrompt(long, 0); got != long || raw != 100 {
		t.Fatalf("limit<=0 应原样返回: len=%d raw=%d", utf8.RuneCountInString(got), raw)
	}
	// 头尾都要保留。
	body := "HEAD" + strings.Repeat("m", 500) + "TAIL"
	got, raw := clampPrompt(body, 100)
	if raw != utf8.RuneCountInString(body) {
		t.Fatalf("raw 应为原始字符数: %d", raw)
	}
	if n := utf8.RuneCountInString(got); n > 100 {
		t.Fatalf("超过上限: %d", n)
	}
	if !strings.HasPrefix(got, "HEAD") || !strings.HasSuffix(got, "TAIL") {
		t.Fatalf("应保留头尾: %q", got)
	}
	// 预算极小：硬截断也要保证不超限、且不切坏多字节字符。
	tiny, _ := clampPrompt(strings.Repeat("字", 50), 5)
	if n := utf8.RuneCountInString(tiny); n > 5 {
		t.Fatalf("极小预算超限: %d", n)
	}
	if !utf8.ValidString(tiny) {
		t.Fatal("硬截断切坏了多字节字符")
	}
}

// 供应商配置可以覆盖阈值，负数表示显式关闭限制（超长原样转发）。
func TestPromptLimitConfigOverride(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{{Name: "fake", Type: "runable", MaxPromptChars: 5}}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq("1234567890"), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if n := utf8.RuneCountInString(prov.inputs[0].Prompt); n > 5 {
		t.Fatalf("配置的 5 字符上限没生效: %d", n)
	}

	// 负数 = 关闭：超长原样转发。
	prov2 := &runableFake{}
	g2 := runableGateway(t, prov2)
	g2.cfg.Providers = []config.ProviderConfig{{Name: "fake", Type: "runable", MaxPromptChars: -1}}
	raw := strings.Repeat("a", 20000)
	w2 := httptest.NewRecorder()
	g2.HandleChat(context.Background(), chatReq(raw), w2)
	if w2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w2.Code, w2.Body.String())
	}
	if prov2.inputs[0].Prompt != raw {
		t.Fatal("关闭限制时不该截断")
	}
}

// 上游自己回绝时（限制被关掉 / 上游收紧了阈值），错误文案要换成人话。
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
