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
