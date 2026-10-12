package geminiweb

import (
	"encoding/json"
	"strings"
	"testing"
)

// wrbFrame builds one StreamGenerate frame: ["wrb.fr",null,"<payload json>",...].
func wrbFrame(t *testing.T, payload any) string {
	t.Helper()
	inner, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	outer, err := json.Marshal([]any{[]any{"wrb.fr", nil, string(inner), nil, nil, nil, "generic"}})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return string(outer)
}

// textPayload builds a payload with a text candidate and (optionally) a thinking
// chain at candidate[37].
func textPayload(conv, text, thoughts string) []any {
	candidate := make([]any, 1)
	candidate[0] = nil
	if text != "" {
		candidate = append(candidate, []any{text})
	} else {
		candidate = append(candidate, nil)
	}
	for len(candidate) <= 37 {
		candidate = append(candidate, nil)
	}
	if thoughts != "" {
		candidate[37] = []any{[]any{thoughts}}
	}
	return []any{nil, conv, nil, nil, []any{candidate}}
}

func TestEncodePayloadShape(t *testing.T) {
	raw := encodePayload("hi", "gemini-3-flash", "", nil, "")
	var outer []any
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		t.Fatalf("outer not json: %v (%s)", err, raw)
	}
	if len(outer) != 2 || outer[0] != nil {
		t.Fatalf("outer = %v want [null, \"<json>\"]", outer)
	}
	inner, ok := outer[1].(string)
	if !ok {
		t.Fatalf("outer[1] not a string: %T", outer[1])
	}
	var arr []any
	if err := json.Unmarshal([]byte(inner), &arr); err != nil {
		t.Fatalf("inner not json: %v", err)
	}
	if len(arr) < 4 {
		t.Fatalf("inner len = %d want >= 4", len(arr))
	}
	if arr[1] != nil || arr[2] != nil {
		t.Fatalf("inner[1]/inner[2] = %v/%v want nil/nil", arr[1], arr[2])
	}
	if arr[3] != "gemini-3-flash" {
		t.Fatalf("inner[3] = %v want gemini-3-flash", arr[3])
	}
	content, ok := arr[0].([]any)
	if !ok || len(content) != 1 || content[0] != "hi" {
		t.Fatalf("inner[0] = %v want [\"hi\"]", arr[0])
	}
}

func TestEncodePayloadThinkingShape(t *testing.T) {
	raw := encodePayloadThinking("hi", "gemini-3-pro", "", nil, "", "UUID-1")
	var outer []any
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		t.Fatalf("outer not json: %v", err)
	}
	inner, _ := outer[1].(string)
	var arr []any
	if err := json.Unmarshal([]byte(inner), &arr); err != nil {
		t.Fatalf("inner not json: %v", err)
	}
	if len(arr) != 81 {
		t.Fatalf("thinking payload len = %d want 81", len(arr))
	}
	if arr[59] != "UUID-1" {
		t.Fatalf("arr[59] = %v want UUID-1", arr[59])
	}
	if arr[79] != float64(3) { // pro = model number 3
		t.Fatalf("arr[79] = %v want 3", arr[79])
	}
	if arr[80] != float64(2) { // 2 = extended thinking
		t.Fatalf("arr[80] = %v want 2", arr[80])
	}
}

func TestScanCompleteWrbFramesHandlesPartial(t *testing.T) {
	full := wrbFrame(t, textPayload("c1", "hello", ""))
	// A frame split across reads must not be reported until it is complete.
	half := full[:len(full)/2]
	frames, consumed := scanCompleteWrbFrames(half)
	if len(frames) != 0 || consumed != 0 {
		t.Fatalf("partial buffer produced frames=%d consumed=%d want 0/0", len(frames), consumed)
	}
	frames, consumed = scanCompleteWrbFrames(full)
	if len(frames) != 1 {
		t.Fatalf("complete buffer produced %d frames want 1", len(frames))
	}
	// The scanner consumes the frame itself; the wrapping array's brackets stay.
	if consumed != len(full)-1 {
		t.Fatalf("consumed = %d want %d", consumed, len(full)-1)
	}
	// Re-scanning the remainder must not yield a phantom frame.
	if rest, _ := scanCompleteWrbFrames(full[consumed:]); len(rest) != 0 {
		t.Fatalf("remainder produced %d frames want 0", len(rest))
	}
}

func TestScanCompleteWrbFramesMultiple(t *testing.T) {
	buf := "[" + wrbFrame(t, textPayload("c1", "one", "")) + "," +
		wrbFrame(t, textPayload("c1", "one two", "")) + "]"
	frames, consumed := scanCompleteWrbFrames(buf)
	if len(frames) != 2 {
		t.Fatalf("frames = %d want 2 (consumed=%d)", len(frames), consumed)
	}
	// The second frame is the cumulative one, so it supersedes the first.
	text, _, _, _ := extractTextFromWrb(frames[1])
	if text != "one two" {
		t.Fatalf("second frame text = %q want %q", text, "one two")
	}
}

func TestExtractTextFromWrb(t *testing.T) {
	frame := wrbFrame(t, textPayload("c_conv_1", "hello", "thinking…"))
	var elem any
	if err := json.Unmarshal([]byte(frame), &elem); err != nil {
		t.Fatal(err)
	}
	// elem is the JSON array of frames; take its first element.
	arr, ok := elem.([]any)
	if !ok || len(arr) != 1 {
		t.Fatalf("frame array = %v", elem)
	}
	text, hasText, conv, thoughts := extractTextFromWrb(arr[0])
	if !hasText || text != "hello" {
		t.Fatalf("text = %q hasText=%v want hello/true", text, hasText)
	}
	if conv != "c_conv_1" {
		t.Fatalf("conv = %q want c_conv_1", conv)
	}
	if thoughts != "thinking…" {
		t.Fatalf("thoughts = %q want thinking…", thoughts)
	}
}

func TestParseModelsFromStatus(t *testing.T) {
	body := make([]any, 16)
	body[15] = []any{[]any{"fbb127bbb056c959"}, []any{"9d8ca3786ebdfbea"}}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal([]any{[]any{"wrb.fr", "otAQ7b", string(bodyJSON), nil, nil, nil, "generic"}})
	if err != nil {
		t.Fatal(err)
	}
	raw := ")]}'\n\n" + string(line)

	got := parseModelsFromStatus(raw)
	if got["flash"] != "gemini-3-flash" {
		t.Fatalf("flash = %q want gemini-3-flash", got["flash"])
	}
	if got["pro"] != "gemini-3-pro" {
		t.Fatalf("pro = %q want gemini-3-pro", got["pro"])
	}
}

func TestParseModelsFromStatusEmpty(t *testing.T) {
	if got := parseModelsFromStatus(")]}'\n\n[]"); got != nil {
		t.Fatalf("got %v want nil", got)
	}
}

func TestStablePlaceholderPrefix(t *testing.T) {
	// A complete image placeholder is stripped.
	if got := stablePlaceholderPrefix("a http://googleusercontent.com/image_generation_content/1 b"); got != "a  b" {
		t.Fatalf("complete placeholder: got %q", got)
	}
	// A trailing fragment that could still grow into a placeholder is held back.
	if got := stablePlaceholderPrefix("a http://googleusercontent.com/"); got != "a " {
		t.Fatalf("partial placeholder: got %q want %q", got, "a ")
	}
	// A bare "http" tail is held back too.
	if got := stablePlaceholderPrefix("see http"); got != "see " {
		t.Fatalf("trailing http: got %q", got)
	}
	if got := stablePlaceholderPrefix("plain text"); got != "plain text" {
		t.Fatalf("plain: got %q", got)
	}
}

func TestResolveModel(t *testing.T) {
	if got := resolveModel("gemini-pro", nil); got != "gemini-3-pro" {
		t.Fatalf("gemini-pro → %q want gemini-3-pro", got)
	}
	if got := resolveModel("gemini-2.5-flash", nil); got != "gemini-3-flash" {
		t.Fatalf("alias → %q want gemini-3-flash", got)
	}
	// The account's learned family model wins over the family default.
	if got := resolveModel("gemini-flash", map[string]string{"flash": "gemini-3-flash-plus"}); got != "gemini-3-flash-plus" {
		t.Fatalf("family override → %q", got)
	}
	// Unknown names pass through untouched.
	if got := resolveModel("some-other-model", nil); got != "some-other-model" {
		t.Fatalf("passthrough → %q", got)
	}
}

func TestBuildModelHeaders(t *testing.T) {
	h := buildModelHeader("gemini-pro")
	if h == nil {
		t.Fatal("standard header nil")
	}
	if !strings.Contains(h[modelHeaderKey], "9d8ca3786ebdfbea") {
		t.Fatalf("standard header = %q", h[modelHeaderKey])
	}
	th := buildModelHeaderThinking("gemini-pro", "UUID-9")
	if th == nil {
		t.Fatal("thinking header nil")
	}
	if !strings.Contains(th[modelHeaderKey], "UUID-9") {
		t.Fatalf("thinking header = %q", th[modelHeaderKey])
	}
	if !strings.Contains(th[modelHeaderKey], `"9d8ca3786ebdfbea"`) {
		t.Fatalf("thinking header missing model id: %q", th[modelHeaderKey])
	}
}

func TestPublicCatalog(t *testing.T) {
	if len(publicModels) != 4 {
		t.Fatalf("publicModels = %v", publicModels)
	}
	for _, m := range publicModels {
		if _, ok := publicFamily[m]; !ok {
			t.Fatalf("public model %q has no family", m)
		}
	}
}
