package gemini

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslatorMapsKeepAliveChunkToEmptyCandidates：网关的保活帧（data: 空 delta
// chunk + omnigate_keepalive 标记）在 Gemini 方言里要翻成一帧「空 candidates」的
// 合法 NDJSON 对象——Gemini 的流不是 SSE，没有注释语法，这是唯一不污染正文的心跳。
func TestTranslatorMapsKeepAliveChunkToEmptyCandidates(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "gemini-x", 0)
	_, _ = tr.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":null}],"omnigate_keepalive":true}` + "\n\n"))
	_, _ = tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	out := rec.Body.String()
	if !strings.Contains(out, `"candidates":[]`) {
		t.Fatalf("keepalive chunk must become an empty-candidates object: %s", out)
	}
	if strings.Contains(out, "omnigate_keepalive") {
		t.Fatalf("keepalive marker leaked to the client: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content lost: %s", out)
	}
}
