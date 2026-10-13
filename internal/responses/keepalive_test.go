package responses

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslatorForwardsKeepAlive：上游的 SSE 注释（保活 ping）在 Responses SSE 里
// 也是合法注释，必须原样转发，避免长静默被中间代理按读超时掐断。
func TestTranslatorForwardsKeepAlive(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "m", nil, Echo{})
	_, _ = tr.Write([]byte(": ping\n\n"))
	_, _ = tr.Write([]byte("data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	if !strings.Contains(rec.Body.String(), ": ping") {
		t.Fatalf("keepalive not forwarded: %s", rec.Body.String())
	}
}
