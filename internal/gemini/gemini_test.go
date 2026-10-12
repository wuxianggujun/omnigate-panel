package gemini

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// chatBody is the decoded chat request the translator produced.
type chatBody struct {
	Model       string           `json:"model"`
	Stream      bool             `json:"stream"`
	Messages    []map[string]any `json:"messages"`
	Tools       []map[string]any `json:"tools"`
	Temperature *float64         `json:"temperature"`
	MaxTokens   *int             `json:"max_tokens"`
}

func mustChat(t *testing.T, raw string, stream bool) (*ChatRequest, chatBody) {
	t.Helper()
	cr, err := RequestToChat("models/gemini-pro", []byte(raw), stream)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var body chatBody
	if err := json.Unmarshal(cr.Body, &body); err != nil {
		t.Fatalf("chat body not json: %v (%s)", err, cr.Body)
	}
	return cr, body
}

// TestRequestToChatBasic：systemInstruction + contents 拍成 system/user 消息，模型名
// 的 "models/" 前缀被剥掉。
func TestRequestToChatBasic(t *testing.T) {
	cr, body := mustChat(t, `{
		"systemInstruction": {"parts": [{"text": "be brief"}]},
		"contents": [{"role": "user", "parts": [{"text": "hi"}]}]
	}`, false)

	if body.Model != "gemini-pro" {
		t.Fatalf("model = %q, want gemini-pro", body.Model)
	}
	if cr.Model != "gemini-pro" {
		t.Fatalf("cr.Model = %q", cr.Model)
	}
	if len(body.Messages) != 2 {
		t.Fatalf("messages = %v, want 2", body.Messages)
	}
	if body.Messages[0]["role"] != "system" || body.Messages[0]["content"] != "be brief" {
		t.Fatalf("system message = %v", body.Messages[0])
	}
	if body.Messages[1]["role"] != "user" || body.Messages[1]["content"] != "hi" {
		t.Fatalf("user message = %v", body.Messages[1])
	}
}

// TestRequestToChatSnakeCase：官方 SDK 发 camelCase，手写调用方常发 snake_case；
// 两种拼写都必须被接受（上游靠 pydantic alias 做到这点）。
func TestRequestToChatSnakeCase(t *testing.T) {
	_, body := mustChat(t, `{
		"system_instruction": "be brief",
		"generation_config": {"max_output_tokens": 128, "top_p": 0.5},
		"contents": [{"role": "model", "parts": [{"text": "prior"}]}, {"role": "user", "parts": [{"text": "hi"}]}]
	}`, true)

	if len(body.Messages) != 3 {
		t.Fatalf("messages = %v, want 3", body.Messages)
	}
	if body.Messages[0]["role"] != "system" {
		t.Fatalf("system = %v", body.Messages[0])
	}
	// role "model" is the assistant in Gemini's vocabulary.
	if body.Messages[1]["role"] != "assistant" {
		t.Fatalf("assistant = %v", body.Messages[1])
	}
	if body.MaxTokens == nil || *body.MaxTokens != 128 {
		t.Fatalf("max_tokens = %v, want 128", body.MaxTokens)
	}
	if !body.Stream {
		t.Fatal("stream = false, want true")
	}
}

// TestRequestToChatTools：functionDeclarations → OpenAI tools；历史里的
// functionCall/functionResponse → assistant tool_calls + role=tool 消息。
func TestRequestToChatTools(t *testing.T) {
	_, body := mustChat(t, `{
		"contents": [
			{"role": "user", "parts": [{"text": "weather?"}]},
			{"role": "model", "parts": [{"functionCall": {"name": "get_weather", "args": {"city": "SF"}}}]},
			{"role": "user", "parts": [{"functionResponse": {"name": "get_weather", "response": {"temp": 20}}}]}
		],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "description": "d", "parameters": {"type": "object"}}]}]
	}`, false)

	if len(body.Tools) != 1 {
		t.Fatalf("tools = %v, want 1", body.Tools)
	}
	fn, _ := body.Tools[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	if len(body.Messages) != 3 {
		t.Fatalf("messages = %v, want 3", body.Messages)
	}
	calls, _ := body.Messages[1]["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", body.Messages[1]["tool_calls"])
	}
	if body.Messages[2]["role"] != "tool" {
		t.Fatalf("tool result message = %v", body.Messages[2])
	}
	// The tool result must carry the id minted for the matching call, otherwise a
	// native-tool provider rejects the conversation.
	if body.Messages[2]["tool_call_id"] == "" || body.Messages[2]["tool_call_id"] == nil {
		t.Fatalf("tool_call_id missing: %v", body.Messages[2])
	}
}

// TestRequestToChatDropsToolsOnImageIntent：生图意图下必须丢掉客户端声明的 tools
// （工具 prompt 会压制生图）。
func TestRequestToChatDropsToolsOnImageIntent(t *testing.T) {
	cr, body := mustChat(t, `{
		"contents": [{"role": "user", "parts": [{"text": "帮我画一张猫"}]}],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
	}`, false)

	if !cr.ImageIntent {
		t.Fatal("ImageIntent = false, want true")
	}
	if len(body.Tools) != 0 {
		t.Fatalf("tools = %v, want none", body.Tools)
	}
	if cr.DroppedTools != 1 {
		t.Fatalf("DroppedTools = %d, want 1", cr.DroppedTools)
	}
}

// TestRequestToChatKeepsToolsOnIdiom：英文习语（draw a conclusion）不是生图意图，
// tools 必须保留——假阳性会静默切断调用方的工具循环。
func TestRequestToChatKeepsToolsOnIdiom(t *testing.T) {
	cr, body := mustChat(t, `{
		"contents": [{"role": "user", "parts": [{"text": "draw a conclusion from the data"}]}],
		"tools": [{"functionDeclarations": [{"name": "get_weather", "parameters": {"type": "object"}}]}]
	}`, false)

	if cr.ImageIntent {
		t.Fatal("ImageIntent = true, want false")
	}
	if len(body.Tools) != 1 {
		t.Fatalf("tools = %v, want 1", body.Tools)
	}
}

// TestImageIntent：关键词表的关键分界（收窄匹配 + draw-a 习语闭集）。
func TestImageIntent(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"画一张猫的图片", true},
		{"来张图", true},
		{"draw a cat", true},
		{"generate an image of a dog", true},
		{"draw a conclusion", false},
		{"draw a map of the kingdom", false}, // 习语闭集优先，安全的一侧
		{"生成一份报告", false},
		{"create a plan", false},
		{"", false},
	} {
		if got := isImageGenerationIntent(tc.text); got != tc.want {
			t.Errorf("isImageGenerationIntent(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// TestChatResponseToGemini：非流式 chat 响应 → Gemini 形态（parts + usageMetadata）。
func TestChatResponseToGemini(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
	out, err := ChatResponseToGemini(body, 99)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
				Role  string           `json:"role"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		Usage map[string]int `json:"usageMetadata"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Candidates) != 1 || got.Candidates[0].Content.Role != "model" {
		t.Fatalf("candidates = %+v", got.Candidates)
	}
	if got.Candidates[0].Content.Parts[0]["text"] != "hello" {
		t.Fatalf("parts = %v", got.Candidates[0].Content.Parts)
	}
	if got.Candidates[0].FinishReason != "STOP" {
		t.Fatalf("finishReason = %q", got.Candidates[0].FinishReason)
	}
	if got.Usage["promptTokenCount"] != 7 || got.Usage["candidatesTokenCount"] != 3 {
		t.Fatalf("usage = %v", got.Usage)
	}
}

// TestChatResponseToGeminiToolCall：tool_calls → functionCall parts（args 是对象，
// 不是字符串）。
func TestChatResponseToGeminiToolCall(t *testing.T) {
	body := []byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
		{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"SF\"}"}}]},
		"finish_reason":"tool_calls"}]}`)
	out, err := ChatResponseToGemini(body, 1)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	part := got.Candidates[0].Content.Parts[0]
	fc, _ := part["functionCall"].(map[string]any)
	if fc == nil || fc["name"] != "get_weather" {
		t.Fatalf("functionCall = %v", part)
	}
	args, _ := fc["args"].(map[string]any)
	if args["city"] != "SF" {
		t.Fatalf("args = %v (must be an object)", fc["args"])
	}
}

// TestTranslatorStreamsNDJSON：流式路径必须回**换行分隔 JSON**（不是 SSE），末行带
// finishReason + usageMetadata。
func TestTranslatorStreamsNDJSON(t *testing.T) {
	w := httptest.NewRecorder()
	tr := NewTranslator(w, true, "gemini-pro", 10)

	_, _ = tr.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"He\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"llo\"},\"finish_reason\":null}]}\n\n"))
	_, _ = tr.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2,\"total_tokens\":7}}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json (NDJSON, not SSE)", ct)
	}
	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), w.Body.String())
	}
	for i, line := range lines {
		var v map[string]any
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line %d is not JSON: %v (%s)", i, err, line)
		}
		if strings.HasPrefix(line, "data:") {
			t.Fatalf("line %d carries an SSE prefix: %s", i, line)
		}
	}
	if !strings.Contains(lines[0], `"text":"He"`) {
		t.Fatalf("line 0 = %s", lines[0])
	}
	if !strings.Contains(lines[2], `"finishReason":"STOP"`) || !strings.Contains(lines[2], `"promptTokenCount":5`) {
		t.Fatalf("terminal line = %s", lines[2])
	}
}

// TestTranslatorStreamErrorFrame：上游错误帧 → NDJSON 错误对象（不是空流）。
func TestTranslatorStreamErrorFrame(t *testing.T) {
	w := httptest.NewRecorder()
	tr := NewTranslator(w, true, "gemini-pro", 10)
	_, _ = tr.Write([]byte("data: {\"error\":{\"message\":\"rate limited\"}}\n\n"))
	tr.Finish()

	body := w.Body.String()
	if !strings.Contains(body, "rate limited") || !strings.Contains(body, `"api_error"`) {
		t.Fatalf("body = %s", body)
	}
}

// TestTranslatorNonStreamErrorShape：非流式 4xx → Gemini 错误形态（不是 OpenAI 形态）。
func TestTranslatorNonStreamErrorShape(t *testing.T) {
	w := httptest.NewRecorder()
	tr := NewTranslator(w, false, "gemini-pro", 10)
	tr.WriteHeader(429)
	_, _ = tr.Write([]byte(`{"error":{"message":"too many"}}`))
	tr.Finish()

	if w.Code != 429 {
		t.Fatalf("status = %d, want 429", w.Code)
	}
	var got struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Error.Message != "too many" || got.Error.Status != "RESOURCE_EXHAUSTED" {
		t.Fatalf("error = %+v", got.Error)
	}
}

// TestTranslatorReplaysNonStreamAsStream：客户端要流式、上游回非流式 JSON 时，按
// 真流式的块序列重放，客户端不会挂住。
func TestTranslatorReplaysNonStreamAsStream(t *testing.T) {
	w := httptest.NewRecorder()
	tr := NewTranslator(w, true, "gemini-pro", 10)
	tr.WriteHeader(200)
	_, _ = tr.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`))
	tr.Finish()

	lines := strings.Split(strings.TrimRight(w.Body.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), w.Body.String())
	}
	if !strings.Contains(lines[0], `"text":"hi"`) {
		t.Fatalf("line 0 = %s", lines[0])
	}
	if !strings.Contains(lines[1], `"finishReason":"STOP"`) {
		t.Fatalf("line 1 = %s", lines[1])
	}
}
