package anthropic

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTranslatorForwardsKeepAlive：上游的 SSE 注释在 Anthropic SSE 里也是合法注释，
// 仍应原样转发（直连场景下依然有效）。
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

// TestTranslatorMapsKeepAliveChunkToPing：网关的保活帧是 data: 空 delta chunk
// （带 omnigate_keepalive 标记）。Anthropic 方言必须把它翻成原生 ping 事件，
// 否则这段静默在 Anthropic 客户端看来就是「什么都没发生」，读超时照样触发。
func TestTranslatorMapsKeepAliveChunkToPing(t *testing.T) {
	rec := httptest.NewRecorder()
	tr := NewTranslator(rec, true, Options{Model: "claude-x"})
	_, _ = tr.Write([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":null}],"omnigate_keepalive":true}` + "\n\n"))
	_, _ = tr.Write([]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	_, _ = tr.Write([]byte("data: [DONE]\n\n"))
	tr.Finish()

	out := rec.Body.String()
	if !strings.Contains(out, "event: ping") {
		t.Fatalf("keepalive chunk must become an Anthropic ping event: %s", out)
	}
	// 保活帧不得被误当成正文或错误。
	if strings.Contains(out, "omnigate_keepalive") {
		t.Fatalf("keepalive marker leaked to the client: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content lost: %s", out)
	}
}
