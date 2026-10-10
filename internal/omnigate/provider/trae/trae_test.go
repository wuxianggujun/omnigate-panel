package trae

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestPrepareBodyRewrites(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":false,"messages":[` +
		`{"role":"user","content":"hi"},` +
		`{"role":"assistant","tool_calls":[{"id":"1","function":{"name":"f","arguments":"{}"}},{"id":"2","function":{}}]}` +
		`],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"none"}`)
	out := PrepareBody(in)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["stream"] != true {
		t.Errorf("stream not forced true: %v", obj["stream"])
	}
	if obj["function"] != Function {
		t.Errorf("function=%v want %v", obj["function"], Function)
	}
	if obj["config_name"] != "glm-5.2" || obj["model"] != "glm-5.2" {
		t.Errorf("model/config_name=%v/%v", obj["model"], obj["config_name"])
	}
	if _, ok := obj["tools"]; ok {
		t.Errorf("tool_choice=none must drop tools")
	}
	if _, ok := obj["tool_choice"]; ok {
		t.Errorf("tool_choice=none must be dropped")
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d", len(msgs))
	}
	m0, _ := msgs[0].(map[string]any)
	c, ok := m0["content"].([]any)
	if !ok || len(c) != 1 {
		t.Fatalf("string content not rewritten to array: %v", m0["content"])
	}
	part, _ := c[0].(map[string]any)
	if part["type"] != "text" || part["text"] != "hi" {
		t.Errorf("content part=%v", part)
	}
	// assistant tool_calls: function -> function_call; the name-less one is dropped.
	m1, _ := msgs[1].(map[string]any)
	tcs, _ := m1["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("want 1 kept tool_call, got %d", len(tcs))
	}
	tc0, _ := tcs[0].(map[string]any)
	if _, ok := tc0["function"]; ok {
		t.Errorf("function key should be renamed")
	}
	if _, ok := tc0["function_call"]; !ok {
		t.Errorf("assistant tool_call not converted to function_call: %v", tc0)
	}
}

func TestPrepareBodyDefaultModel(t *testing.T) {
	out := PrepareBody([]byte(`{"messages":[]}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["config_name"] != DefaultConfigName {
		t.Errorf("config_name=%v want %v", obj["config_name"], DefaultConfigName)
	}
}

func TestPrepareBodyToolChoiceFunctionName(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"m","tool_choice":{"type":"function","function":{"name":"do_x"}}}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["tool_choice"] != "do_x" {
		t.Errorf("tool_choice=%v want do_x", obj["tool_choice"])
	}
}

func TestNormalizeToolsStringifiesParameters(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	tools, _ := obj["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len=%d", len(tools))
	}
	fn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if _, isStr := fn["parameters"].(string); !isStr {
		t.Errorf("parameters not serialized to string: %T", fn["parameters"])
	}
}

func TestParseCallbackRefreshToken(t *testing.T) {
	raw := "http://127.0.0.1:18080/authorize?refreshToken=RT&userInfo=" +
		url.QueryEscape(`{"UserID":"u1","ScreenName":"Nick","TenantID":"ent1"}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.RefreshToken != "RT" || info.UID != "u1" || info.Nickname != "Nick" || info.EnterpriseID != "ent1" {
		t.Errorf("%+v", info)
	}
}

func TestParseCallbackJwtFallback(t *testing.T) {
	raw := "http://x/authorize?userJwt=" + url.QueryEscape(`{"Token":"AT","TokenExpireAt":1786847930141}`)
	info, err := ParseCallback(raw)
	if err != nil {
		t.Fatal(err)
	}
	if info.AccessToken != "AT" {
		t.Errorf("access=%q", info.AccessToken)
	}
	if info.ExpiresAt != 1786847930 {
		t.Errorf("exp=%d want 1786847930 (ms normalized to s)", info.ExpiresAt)
	}
}

func TestParseCallbackEmpty(t *testing.T) {
	if _, err := ParseCallback("  "); err == nil {
		t.Error("empty callback should error")
	}
}

func TestBuildLoginURL(t *testing.T) {
	u := BuildLoginURL("m1", "d1", "http://127.0.0.1:18080/authorize")
	for _, want := range []string{
		"client_id=" + ClientID, "auth_from=solo", "auth_type=local",
		"auth_callback_url=", "machine_id=m1", "device_id=d1", "x_device_id=d1",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("login url missing %q: %s", want, u)
		}
	}
}

func TestParseSOLOLineOutput(t *testing.T) {
	ev, err := ParseSOLOLine("output", `{"response":"hi","reasoning_content":"why"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Response != "hi" || ev.Reasoning != "why" {
		t.Errorf("%+v", ev)
	}
}

func TestParseUsage(t *testing.T) {
	ev, err := ParseSOLOLine("token_usage", `{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3,"reasoning_tokens":1}`)
	if err != nil {
		t.Fatal(err)
	}
	u := parseUsage(ev.Usage)
	if u == nil || u.TotalTokens != 3 || u.ReasoningTokens != 1 || u.PromptTokens != 1 {
		t.Errorf("%+v", u)
	}
}

func TestScanLineFramesEvent(t *testing.T) {
	st := &sseState{}
	if ev := scanLine(st, "event: output"); ev != nil {
		t.Fatal("event line must not emit")
	}
	if ev := scanLine(st, `data: {"response":"x"}`); ev != nil {
		t.Fatal("data line must not emit")
	}
	ev := scanLine(st, "")
	if ev == nil || ev.Response != "x" {
		t.Fatalf("boundary must emit parsed event: %+v", ev)
	}
}

func TestNewCheckinDeviceID(t *testing.T) {
	d, err := NewCheckinDeviceID()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 16 {
		t.Fatalf("len=%d want 16", len(d))
	}
	if d[0] == '0' {
		t.Fatalf("leading zero not allowed: %s", d)
	}
	for _, c := range d {
		if c < '0' || c > '9' {
			t.Fatalf("non-digit in %s", d)
		}
	}
}

func TestSoloErrorMessageClassification(t *testing.T) {
	if got := soloErrorMessage(1005, "x"); !strings.Contains(got, "积分不足") {
		t.Errorf("1005 should map to out-of-credits: %q", got)
	}
	if got := soloErrorMessage(4008, "x"); !strings.Contains(got, "积分不足") {
		t.Errorf("4008 should map to out-of-credits: %q", got)
	}
	if got := soloErrorMessage(4011, "x"); !strings.Contains(got, "限流") {
		t.Errorf("4011 should map to rate limit: %q", got)
	}
}

func TestUnauthorizedDetection(t *testing.T) {
	if !Unauthorized(&Error{Kind: ErrSessionDead, Status: 401}) {
		t.Error("session-dead must be unauthorized")
	}
	if Unauthorized(&Error{Kind: ErrServer, Status: 500}) {
		t.Error("server error must not be unauthorized")
	}
}
