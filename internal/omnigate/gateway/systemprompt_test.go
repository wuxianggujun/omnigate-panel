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

// captureInputProvider 记录它收到的 ChatInput（openai 形态，走 messages 分支）。
type captureInputProvider struct{ got provider.ChatInput }

func (p *captureInputProvider) Name() string { return "fake" }
func (p *captureInputProvider) Type() string { return "openai" }
func (p *captureInputProvider) ListModels(context.Context) ([]openai.Model, error) {
	return nil, nil
}
func (p *captureInputProvider) StreamChat(_ context.Context, _ *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	p.got = in
	return &scriptStream{events: []provider.Event{
		{Type: provider.EventText, Text: "ok"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}, nil
}

// 消息型上游：供应商级 system_prompt 应在 messages 头部插一条 system，原消息保留在后。
func TestProviderSystemPromptPrependsSystemMessage(t *testing.T) {
	prov := &captureInputProvider{}
	g := testGateway(prov)
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "fake", Type: "openai", SystemPrompt: "你是夜猫子。"},
	}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq("你好"), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.got.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (%+v)", len(prov.got.Messages), prov.got.Messages)
	}
	if head := prov.got.Messages[0]; head.Role != "system" || head.Content.Text != "你是夜猫子。" {
		t.Fatalf("首条应是注入的 system: %+v", head)
	}
	if prov.got.Messages[1].Role != "user" {
		t.Fatalf("客户端消息应保留在后: %+v", prov.got.Messages[1])
	}
}

// 单条 prompt 型上游（runable）：system_prompt 拼到 prompt 开头。
func TestProviderSystemPromptPrependsToPrompt(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "fake", Type: "runable", SystemPrompt: "SYS"},
	}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq("hi"), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	if got := prov.inputs[0].Prompt; !strings.HasPrefix(got, "SYS\n\n") {
		t.Fatalf("prompt 应以注入的 system 开头: %q", got)
	}
}

// 留空 = 完全不注入（上游原样）。
func TestProviderSystemPromptEmptyIsNoop(t *testing.T) {
	prov := &captureInputProvider{}
	g := testGateway(prov)
	g.cfg.Providers = []config.ProviderConfig{{Name: "fake", Type: "openai"}}

	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), chatReq("hi"), w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.got.Messages) != 1 || prov.got.Messages[0].Role != "user" {
		t.Fatalf("空 system_prompt 不该改动 messages: %+v", prov.got.Messages)
	}
}

// 工具路径（把整段对话压成一条 prompt）同样要吃到供应商级 system_prompt。
func TestProviderSystemPromptOnToolPath(t *testing.T) {
	prov := &runableFake{}
	g := runableGateway(t, prov)
	g.cfg.Providers = []config.ProviderConfig{
		{Name: "fake", Type: "runable", SystemPrompt: "SYS"},
	}

	req := &openai.ChatRequest{
		Model:    "fake/m",
		Tools:    []openai.Tool{{Type: "function", Function: openai.ToolDef{Name: "f", Description: "d"}}},
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
	w := httptest.NewRecorder()
	g.HandleChat(context.Background(), req, w)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if len(prov.inputs) != 1 {
		t.Fatalf("上游应被调用 1 次，实际 %d 次", len(prov.inputs))
	}
	if got := prov.inputs[0].Prompt; !strings.HasPrefix(got, "SYS\n\n") {
		t.Fatalf("工具 prompt 应以注入的 system 开头: %q", got[:min(40, len(got))])
	}
}
