package wire

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

func readAll(t *testing.T, s provider.Stream) []provider.Event {
	t.Helper()
	var out []provider.Event
	for i := 0; i < 200; i++ {
		ev, err := s.Recv()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		out = append(out, ev)
	}
	t.Fatal("流没有终止")
	return nil
}

func textOf(events []provider.Event) string {
	var b strings.Builder
	for _, e := range events {
		if e.Type == provider.EventText {
			b.WriteString(e.Text)
		}
	}
	return b.String()
}

// TestEncodeAnthropicShape 锁定 chat → /v1/messages 的翻译：system 提升为顶层字段、
// tool 结果变成 tool_result 块、工具 schema 改名成 input_schema。
func TestEncodeAnthropicShape(t *testing.T) {
	max := 256
	in := provider.ChatInput{
		Model: "claude-sonnet-4-5",
		Messages: []openai.Message{
			{Role: "system", Content: openai.Content{Text: "你是助手"}},
			{Role: "user", Content: openai.Content{Text: "看下天气"}},
			{Role: "assistant", Content: openai.Content{Text: "我查一下"}, ToolCalls: []openai.ToolCall{{
				ID: "call_1", Type: "function",
				Function: openai.ToolFunction{Name: "weather", Arguments: `{"city":"上海"}`},
			}}},
			{Role: "tool", ToolCallID: "call_1", Content: openai.Content{Text: "晴"}},
		},
		Tools: []openai.Tool{{Type: "function", Function: openai.ToolDef{
			Name: "weather", Description: "查天气",
			Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}}},
		MaxTokens: &max,
	}
	raw, err := EncodeAnthropic(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got["system"] != "你是助手" {
		t.Errorf("system = %#v", got["system"])
	}
	if got["max_tokens"] != float64(256) {
		t.Errorf("max_tokens = %#v", got["max_tokens"])
	}
	if got["stream"] != true {
		t.Errorf("stream = %#v", got["stream"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages 长度 = %d (%#v)", len(msgs), msgs)
	}
	// assistant 的 tool_calls 必须变成 tool_use 块。
	asst, _ := msgs[1].(map[string]any)
	blocks, _ := asst["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %#v", blocks)
	}
	use, _ := blocks[1].(map[string]any)
	if use["type"] != "tool_use" || use["name"] != "weather" || use["id"] != "call_1" {
		t.Errorf("tool_use 块错误：%#v", use)
	}
	// tool 消息必须变成 user + tool_result。
	toolTurn, _ := msgs[2].(map[string]any)
	if toolTurn["role"] != "user" {
		t.Errorf("tool 结果应转成 user 轮次，得到 %#v", toolTurn["role"])
	}
	tb, _ := toolTurn["content"].([]any)
	res, _ := tb[0].(map[string]any)
	if res["type"] != "tool_result" || res["tool_use_id"] != "call_1" {
		t.Errorf("tool_result 块错误：%#v", res)
	}
	// 工具定义用 input_schema。
	tools, _ := got["tools"].([]any)
	td, _ := tools[0].(map[string]any)
	if td["name"] != "weather" || td["input_schema"] == nil {
		t.Errorf("tools 形状错误：%#v", td)
	}
	if _, has := td["function"]; has {
		t.Error("Anthropic 的 tools 不该有 function 包装")
	}
}

// TestEncodeAnthropicMaxTokensDefault：上游强制要求 max_tokens，客户端没给时
// 必须兜一个默认值，否则整条请求 400。
func TestEncodeAnthropicMaxTokensDefault(t *testing.T) {
	raw, err := EncodeAnthropic(provider.ChatInput{Model: "m", Messages: []openai.Message{
		{Role: "user", Content: openai.Content{Text: "hi"}},
	}})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		MaxTokens int `json:"max_tokens"`
	}
	_ = json.Unmarshal(raw, &got)
	if got.MaxTokens <= 0 {
		t.Fatalf("max_tokens 必须有默认值，得到 %d", got.MaxTokens)
	}
}

// TestAnthropicStreamText：文本增量、usage、stop_reason 都要归一化出来。
func TestAnthropicStreamText(t *testing.T) {
	sse := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":12}}}`,
		``,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你"}}`,
		``,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"好"}}`,
		``,
		`data: {"type":"content_block_stop","index":0}`,
		``,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
		``,
		`data: {"type":"message_stop"}`,
		``,
	}, "\n")

	s := NewAnthropicStream(io.NopCloser(strings.NewReader(sse)))
	events := readAll(t, s)
	if got := textOf(events); got != "你好" {
		t.Fatalf("文本 = %q", got)
	}
	var usage *provider.Usage
	var finish string
	for _, e := range events {
		if e.Type == provider.EventUsage && e.Usage != nil {
			usage = e.Usage
		}
		if e.Type == provider.EventFinish {
			finish = e.Finish
		}
	}
	if usage == nil || usage.PromptTokens != 12 || usage.CompletionTokens != 2 {
		t.Errorf("usage = %+v", usage)
	}
	if finish != "stop" {
		t.Errorf("finish = %q", finish)
	}
}

// TestAnthropicStreamToolUse：input_json_delta 必须拼成一个完整参数的 tool call。
func TestAnthropicStreamToolUse(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"上海\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
		`data: {"type":"message_stop"}`,
	}, "\n")

	s := NewAnthropicStream(io.NopCloser(strings.NewReader(sse)))
	events := readAll(t, s)
	var calls []openai.ToolCall
	var finish string
	for _, e := range events {
		if e.Type == provider.EventToolCalls {
			calls = append(calls, e.ToolCalls...)
		}
		if e.Type == provider.EventFinish {
			finish = e.Finish
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	if calls[0].ID != "toolu_1" || calls[0].Function.Name != "weather" {
		t.Errorf("tool call = %+v", calls[0])
	}
	var args map[string]string
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("参数不是合法 JSON：%q", calls[0].Function.Arguments)
	}
	if args["city"] != "上海" {
		t.Errorf("参数 = %v", args)
	}
	if finish != "tool_calls" {
		t.Errorf("finish = %q", finish)
	}
}

// TestAnthropicStreamError：上游 error 事件必须变成 EventError，而不是被静默
// 当成空流（那会让客户端无限重试）。
func TestAnthropicStreamError(t *testing.T) {
	sse := `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n"
	s := NewAnthropicStream(io.NopCloser(strings.NewReader(sse)))
	events := readAll(t, s)
	if len(events) == 0 || events[0].Type != provider.EventError {
		t.Fatalf("events = %#v", events)
	}
	if !strings.Contains(events[0].Text, "Overloaded") {
		t.Errorf("错误文本 = %q", events[0].Text)
	}
}

// TestEncodeResponsesShape：Codex 的 Responses 形态把 assistant 的工具调用拍平成
// 顶层 function_call，工具结果拍平成 function_call_output。
func TestEncodeResponsesShape(t *testing.T) {
	in := provider.ChatInput{
		Model: "gpt-5-codex",
		Messages: []openai.Message{
			{Role: "system", Content: openai.Content{Text: "be brief"}},
			{Role: "user", Content: openai.Content{Text: "ls"}},
			{Role: "assistant", ToolCalls: []openai.ToolCall{{
				ID: "c1", Type: "function",
				Function: openai.ToolFunction{Name: "shell", Arguments: `{"cmd":"ls"}`},
			}}},
			{Role: "tool", ToolCallID: "c1", Content: openai.Content{Text: "a.txt"}},
		},
	}
	raw, err := EncodeResponses(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var got struct {
		Model        string `json:"model"`
		Instructions string `json:"instructions"`
		Stream       bool   `json:"stream"`
		Store        bool   `json:"store"`
		Input        []map[string]any
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	if got.Model != "gpt-5-codex" || got.Instructions != "be brief" || !got.Stream {
		t.Errorf("头部字段错误：%+v", got)
	}
	if got.Store {
		t.Error("store 应为 false：网关无状态，不该让上游持久化")
	}
	var kinds []string
	for _, item := range got.Input {
		if s, ok := item["type"].(string); ok {
			kinds = append(kinds, s)
		}
	}
	want := []string{"message", "function_call", "function_call_output"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("input 类型序列 = %v，期望 %v", kinds, want)
	}
}

// TestResponsesStreamToolCall：output_item.added/delta/done 三段要合成一个
// 完整参数的 tool call。
func TestResponsesStreamToolCall(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_9","name":"shell","arguments":""}}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"cmd\":"}`,
		`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"\"ls\"}"}`,
		`data: {"type":"response.output_item.done","item":{"id":"item_1","type":"function_call","call_id":"call_9","name":"shell"}}`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}}`,
	}, "\n")

	s := NewResponsesStream(io.NopCloser(strings.NewReader(sse)))
	events := readAll(t, s)
	var calls []openai.ToolCall
	var usage *provider.Usage
	for _, e := range events {
		if e.Type == provider.EventToolCalls {
			calls = append(calls, e.ToolCalls...)
		}
		if e.Type == provider.EventUsage {
			usage = e.Usage
		}
	}
	if len(calls) != 1 {
		t.Fatalf("tool calls = %#v", calls)
	}
	if calls[0].ID != "call_9" || calls[0].Function.Name != "shell" {
		t.Errorf("tool call = %+v", calls[0])
	}
	if calls[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Errorf("参数 = %q", calls[0].Function.Arguments)
	}
	if usage == nil || usage.PromptTokens != 5 || usage.CompletionTokens != 3 {
		t.Errorf("usage = %+v", usage)
	}
}

// TestFormatPath：方言决定端点后缀，AnyRouter 只接受 /messages。
func TestFormatPath(t *testing.T) {
	cases := map[Format]string{
		FormatOpenAI:    "/chat/completions",
		FormatAnthropic: "/messages",
		FormatResponses: "/responses",
	}
	for f, want := range cases {
		if got := f.Path(); got != want {
			t.Errorf("%s.Path() = %q want %q", f, got, want)
		}
	}
	if ParseFormat("") != FormatOpenAI {
		t.Error("空协议应默认为 openai")
	}
	if ParseFormat("ANTHROPIC") != FormatAnthropic {
		t.Error("协议名应大小写不敏感")
	}
	if ParseFormat("garbage") != FormatOpenAI {
		t.Error("未知协议应退回 openai，而不是让供应商不可路由")
	}
}
