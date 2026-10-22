package responses

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslatorMapsKeepAliveChunkToInProgress：网关的保活帧（data: 空 delta
// chunk + omnigate_keepalive 标记）在 Responses 方言里要翻成 response.in_progress
// 心跳。Responses 没有 ping 事件，但 in_progress 是合法且无内容副作用的心跳。
func TestTranslatorMapsKeepAliveChunkToInProgress(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, "resp-model", nil, Echo{})
	_, _ = tr.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":null}],"omnigate_keepalive":true}` + "\n\n"))
	_, _ = tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	out := rec.Body.String()
	if !strings.Contains(out, "response.in_progress") {
		t.Fatalf("keepalive chunk must become response.in_progress: %s", out)
	}
	if strings.Contains(out, "omnigate_keepalive") {
		t.Fatalf("keepalive marker leaked to the client: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content lost: %s", out)
	}
}
