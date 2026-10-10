package provider

import (
	"io"
	"strings"
	"testing"
)

// sseScript joins SSE data frames into a stream body.
func sseScript(frames ...string) io.ReadCloser {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: ")
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

// TestOpenAIStreamUsage verifies that the trailing usage chunk (which upstreams
// send after the finish_reason chunk) is surfaced as an EventUsage *before* the
// finish event, so callers that stop on EventFinish still record it.
func TestOpenAIStreamUsage(t *testing.T) {
	s := NewOpenAIStream(sseScript(
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"你好"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"completion_tokens_details":{"reasoning_tokens":3},"prompt_tokens_details":{"cached_tokens":6}}}`,
		`[DONE]`,
	))

	var (
		text      string
		finish    string
		gotUsage  *Usage
		usageSeen bool
		order     []string
	)
	for {
		e, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		switch e.Type {
		case EventText:
			text += e.Text
		case EventUsage:
			gotUsage = e.Usage
			usageSeen = true
			order = append(order, "usage")
		case EventFinish:
			finish = e.Finish
			order = append(order, "finish")
		}
	}

	if text != "你好" {
		t.Fatalf("text = %q", text)
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
	if !usageSeen || gotUsage == nil {
		t.Fatal("usage not captured")
	}
	if gotUsage.PromptTokens != 10 || gotUsage.CompletionTokens != 4 || gotUsage.TotalTokens != 14 {
		t.Fatalf("usage tokens = %+v", *gotUsage)
	}
	if gotUsage.CachedTokens != 6 || gotUsage.ReasoningTokens != 3 {
		t.Fatalf("usage details = %+v", *gotUsage)
	}
	// usage must precede finish so a caller that returns on EventFinish sees it.
	if len(order) != 2 || order[0] != "usage" || order[1] != "finish" {
		t.Fatalf("event order = %v, want [usage finish]", order)
	}
}

// TestOpenAIStreamNoUsage ensures streams without a usage chunk still finish
// cleanly and expose no usage.
func TestOpenAIStreamNoUsage(t *testing.T) {
	s := NewOpenAIStream(sseScript(
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`[DONE]`,
	))
	var finish string
	usageSeen := false
	for {
		e, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if e.Type == EventFinish {
			finish = e.Finish
		}
		if e.Type == EventUsage {
			usageSeen = true
		}
	}
	if finish != "stop" {
		t.Fatalf("finish = %q", finish)
	}
	if usageSeen {
		t.Fatal("unexpected usage event")
	}
}
