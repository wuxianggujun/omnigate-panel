package provider

import (
	"encoding/json"
	"testing"
)

// TestApplySampling 采样/结构控制字段必须落进上游请求体；tool_choice /
// response_format / stop 保持原 JSON 形态（字符串与对象都合法）。
func TestApplySampling(t *testing.T) {
	temp, topP := 0.2, 0.8
	maxTok := 256
	parallel := false
	in := ChatInput{
		Temperature:       &temp,
		TopP:              &topP,
		MaxTokens:         &maxTok,
		ParallelToolCalls: &parallel,
		Stop:              json.RawMessage(`"END"`),
		ToolChoice:        json.RawMessage(`{"type":"function","function":{"name":"f"}}`),
		ResponseFormat:    json.RawMessage(`{"type":"json_object"}`),
		User:              "u-1",
	}
	body := map[string]any{"model": "m", "messages": []any{}, "stream": true}
	ApplySampling(body, in)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["temperature"] != 0.2 || got["top_p"] != 0.8 || got["max_tokens"] != float64(256) {
		t.Errorf("sampling=%v", got)
	}
	if got["parallel_tool_calls"] != false {
		t.Errorf("parallel_tool_calls=%v", got["parallel_tool_calls"])
	}
	if tc, _ := got["tool_choice"].(map[string]any); tc["type"] != "function" {
		t.Errorf("tool_choice=%v", got["tool_choice"])
	}
	if rf, _ := got["response_format"].(map[string]any); rf["type"] != "json_object" {
		t.Errorf("response_format=%v", got["response_format"])
	}
	if got["stop"] != "END" {
		t.Errorf("stop=%v", got["stop"])
	}
	if got["user"] != "u-1" {
		t.Errorf("user=%v", got["user"])
	}
}

// TestApplySamplingEmptyIsNoop 客户端没传的字段不能凭空出现在上游请求体里。
func TestApplySamplingEmptyIsNoop(t *testing.T) {
	body := map[string]any{"model": "m"}
	ApplySampling(body, ChatInput{})
	if len(body) != 1 {
		t.Fatalf("body=%v (want untouched)", body)
	}
}
