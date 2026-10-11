package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// captureProvider records the ChatInput handed to it so tests can assert the
// gateway forwards the client's request fields instead of dropping them.
type captureProvider struct {
	events []provider.Event
	got    provider.ChatInput
}

func (p *captureProvider) Name() string                                       { return "fake" }
func (p *captureProvider) Type() string                                       { return "openai" }
func (p *captureProvider) ListModels(context.Context) ([]openai.Model, error) { return nil, nil }
func (p *captureProvider) StreamChat(_ context.Context, _ *provider.Account, in provider.ChatInput) (provider.Stream, error) {
	p.got = in
	return &scriptStream{events: p.events}, nil
}

// TestHandleChatForwardsSamplingControls 客户端带的 temperature / top_p /
// max_tokens / stop / parallel_tool_calls / tool_choice / response_format / user
// 必须到达 provider。此前 ChatRequest 没有这些字段，解析即丢：用户设了温度、限了
// 输出长度、强制了 tool_choice，上游全都没收到，而且没有任何提示。
func TestHandleChatForwardsSamplingControls(t *testing.T) {
	p := &captureProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}
	g := testGateway(p)

	temp, topP := 0.3, 0.9
	maxTok := 128
	parallel := false
	req := &openai.ChatRequest{
		Model:             "fake/m",
		Messages:          []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
		Temperature:       &temp,
		TopP:              &topP,
		MaxTokens:         &maxTok,
		ParallelToolCalls: &parallel,
		Stop:              json.RawMessage(`["END"]`),
		ToolChoice:        json.RawMessage(`{"type":"function","function":{"name":"f"}}`),
		ResponseFormat:    json.RawMessage(`{"type":"json_object"}`),
		User:              "u-1",
		Tools: []openai.Tool{{
			Type:     "function",
			Function: openai.ToolDef{Name: "f", Parameters: json.RawMessage(`{"type":"object"}`)},
		}},
	}
	g.HandleChat(context.Background(), req, httptest.NewRecorder())

	if p.got.Temperature == nil || *p.got.Temperature != temp {
		t.Errorf("temperature=%v (want %v)", p.got.Temperature, temp)
	}
	if p.got.TopP == nil || *p.got.TopP != topP {
		t.Errorf("top_p=%v (want %v)", p.got.TopP, topP)
	}
	if p.got.MaxTokens == nil || *p.got.MaxTokens != maxTok {
		t.Errorf("max_tokens=%v (want %d)", p.got.MaxTokens, maxTok)
	}
	if p.got.ParallelToolCalls == nil || *p.got.ParallelToolCalls {
		t.Errorf("parallel_tool_calls=%v (want false)", p.got.ParallelToolCalls)
	}
	if string(p.got.Stop) != `["END"]` {
		t.Errorf("stop=%s", p.got.Stop)
	}
	if string(p.got.ToolChoice) != `{"type":"function","function":{"name":"f"}}` {
		t.Errorf("tool_choice=%s", p.got.ToolChoice)
	}
	if string(p.got.ResponseFormat) != `{"type":"json_object"}` {
		t.Errorf("response_format=%s", p.got.ResponseFormat)
	}
	if p.got.User != "u-1" {
		t.Errorf("user=%q", p.got.User)
	}
	if len(p.got.Tools) != 1 {
		t.Errorf("tools=%v", p.got.Tools)
	}
}

// TestHandleChatForwardsMaxCompletionTokens OpenAI 的新别名 max_completion_tokens
// 也要落到 max_tokens（客户端越来越常用它）。
func TestHandleChatForwardsMaxCompletionTokens(t *testing.T) {
	p := &captureProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}}
	g := testGateway(p)

	n := 64
	req := &openai.ChatRequest{
		Model:               "fake/m",
		Messages:            []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
		MaxCompletionTokens: &n,
	}
	g.HandleChat(context.Background(), req, httptest.NewRecorder())

	if p.got.MaxTokens == nil || *p.got.MaxTokens != n {
		t.Errorf("max_tokens=%v (want %d, from max_completion_tokens)", p.got.MaxTokens, n)
	}
}
