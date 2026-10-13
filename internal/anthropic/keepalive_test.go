package anthropic

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslatorForwardsKeepAlive：上游的 SSE 注释（保活 ping）在 Anthropic SSE 里
// 也是合法注释，必须原样转发——否则一段较长的上游静默会触发中间代理的读超时。
func TestTranslatorForwardsKeepAlive(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x"})
	_, _ = tr.Write([]byte(": ping\n\n"))
	_, _ = tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	if !strings.Contains(rec.Body.String(), ": ping") {
		t.Fatalf("keepalive not forwarded: %s", rec.Body.String())
	}
}
