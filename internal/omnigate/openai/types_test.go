package openai

import (
	"encoding/json"
	"testing"
)

func TestChatRequestReasoningEffort(t *testing.T) {
	var req ChatRequest
	body := []byte(`{"model":"raccoon/x","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"high"}`)
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", req.ReasoningEffort)
	}
}

func TestChatRequestNoReasoningEffort(t *testing.T) {
	var req ChatRequest
	if err := json.Unmarshal([]byte(`{"model":"m","messages":[]}`), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.ReasoningEffort != "" {
		t.Fatalf("ReasoningEffort = %q, want empty", req.ReasoningEffort)
	}
}

// TestChatRequestSamplingControls 采样/结构控制字段必须能解析出来。此前这些字段在
// ChatRequest 里根本不存在，请求体带了也在解析这一步被静默丢掉（/omni/v1/chat/
// completions 与 /omni/v1/responses 都中招——Responses 翻译层明明产出了
// temperature/max_tokens/tool_choice/response_format，一转 struct 就没了）。
func TestChatRequestSamplingControls(t *testing.T) {
	body := []byte(`{
		"model":"m","messages":[{"role":"user","content":"hi"}],
		"temperature":0.25,"top_p":0.75,"max_tokens":512,
		"stop":["END"],"parallel_tool_calls":false,
		"tool_choice":{"type":"function","function":{"name":"f"}},
		"response_format":{"type":"json_object"},
		"user":"u-1",
		"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"},"strict":true}}]
	}`)
	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.Temperature == nil || *req.Temperature != 0.25 {
		t.Errorf("temperature=%v", req.Temperature)
	}
	if req.TopP == nil || *req.TopP != 0.75 {
		t.Errorf("top_p=%v", req.TopP)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 512 {
		t.Errorf("max_tokens=%v", req.MaxTokens)
	}
	if string(req.Stop) != `["END"]` {
		t.Errorf("stop=%s", req.Stop)
	}
	if req.ParallelToolCalls == nil || *req.ParallelToolCalls {
		t.Errorf("parallel_tool_calls=%v", req.ParallelToolCalls)
	}
	if string(req.ToolChoice) != `{"type":"function","function":{"name":"f"}}` {
		t.Errorf("tool_choice=%s", req.ToolChoice)
	}
	if string(req.ResponseFormat) != `{"type":"json_object"}` {
		t.Errorf("response_format=%s", req.ResponseFormat)
	}
	if req.User != "u-1" {
		t.Errorf("user=%q", req.User)
	}
	if len(req.Tools) != 1 || req.Tools[0].Function.Strict == nil || !*req.Tools[0].Function.Strict {
		t.Errorf("tools[0].function.strict not preserved: %+v", req.Tools)
	}
}
