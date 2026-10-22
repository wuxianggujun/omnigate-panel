package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// reEncodingProxy 模拟 new-api 那类 SSE 再编码层，行为与
// relay/channel/openai/relay-openai.go 的 StreamScannerHandler 一致：
//
//  1. 只认 `data:` / `[DONE]` 行，SSE 注释行（以 ':' 开头）**整行丢弃**；
//  2. **保留上一帧**（lastStreamData），等下一帧到达时才把上一帧写下去——
//     这是它为了「把 usage 从最后一帧里捞出来」而做的延迟转发。
//
// 两条叠在一起才是线上故障的完整成因：网关用 ": ping" 注释保活，注释行在
// 再编码层被丢弃，于是扫描回调根本不会被触发，被扣住的那一帧永远发不出去，
// 客户端在整个静默期收不到任何字节，最后按自己的读超时断连（499）。
//
// 所以保活帧不仅要「是 data: 行」，还要能**持续供给**，让延迟转发有下一帧可等。
type reEncodingProxy struct {
	status  int
	body    strings.Builder
	hdr     http.Header
	pending string // lastStreamData：被扣住、尚未写出的上一帧
	done    bool
}

func (p *reEncodingProxy) Header() http.Header {
	if p.hdr == nil {
		p.hdr = http.Header{}
	}
	return p.hdr
}

func (p *reEncodingProxy) WriteHeader(code int) { p.status = code }

func (p *reEncodingProxy) Write(b []byte) (int, error) {
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, ":"):
			continue // 再编码层丢弃注释行：扫描回调不会触发
		case strings.HasPrefix(trimmed, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				p.flushPending()
				p.body.WriteString("data: [DONE]\n\n")
				p.done = true
				continue
			}
			// 延迟转发：先写出被扣住的上一帧，再扣住当前帧。
			p.flushPending()
			p.pending = trimmed
		}
	}
	return len(b), nil
}

func (p *reEncodingProxy) flushPending() {
	if p.pending == "" {
		return
	}
	p.body.WriteString(p.pending + "\n\n")
	p.pending = ""
}

func (p *reEncodingProxy) Flush() {}

// TestKeepAliveSurvivesSSEReEncoder 是本次修复的核心回归：上游长静默时，
// 穿过一个「丢注释 + 延迟一帧转发」的再编码层后，客户端仍必须收到保活字节。
func TestKeepAliveSurvivesSSEReEncoder(t *testing.T) {
	old := sseKeepAliveInterval
	sseKeepAliveInterval = 20 * time.Millisecond
	defer func() { sseKeepAliveInterval = old }()

	g := testGateway(&delayingProvider{name: "fake", delay: 100 * time.Millisecond, events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	proxy := &reEncodingProxy{}
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), streamReq(), proxy)

	out := proxy.body.String()
	if strings.Contains(out, ": ping") {
		t.Fatalf("only data: frames may cross the re-encoder, got comment: %s", out)
	}
	if !strings.Contains(out, `"`+openaikeepaliveField+`":true`) {
		t.Fatalf("keepalive frame was dropped by the SSE re-encoder: %s", out)
	}
	if !strings.Contains(out, "hi") {
		t.Fatalf("content missing: %s", out)
	}
	if !strings.Contains(out, "[DONE]") {
		t.Fatalf("terminal frame missing: %s", out)
	}
}

// TestKeepAliveIsNotSwallowedByDelayForward 锁死「延迟转发」这个致命细节：
// 再编码层会扣住最后一帧等下一帧。如果整条流里只有一个保活帧，它会被扣到流结束
// 才写出——静默期照样零字节。保活必须**周期性持续供给**，客户端才能在静默期
// 不断收到字节。
func TestKeepAliveIsNotSwallowedByDelayForward(t *testing.T) {
	old := sseKeepAliveInterval
	sseKeepAliveInterval = 10 * time.Millisecond
	defer func() { sseKeepAliveInterval = old }()

	// 静默 120ms ≈ 12 个保活周期：足够多帧供给，任何单帧被扣都不会导致空窗。
	g := testGateway(&delayingProvider{name: "fake", delay: 120 * time.Millisecond, events: []provider.Event{
		{Type: provider.EventText, Text: "hi"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	proxy := &reEncodingProxy{}
	g.HandleChat(WithReqMeta(context.Background(), &ReqMeta{}), streamReq(), proxy)

	out := proxy.body.String()
	n := strings.Count(out, `"`+openaikeepaliveField+`":true`)
	// 延迟转发会扣住最后一帧，所以收到的帧数约等于发出帧数 - 1；但必须远多于 1，
	// 否则静默期就是「一帧被扣住 → 零字节」。
	if n < 3 {
		t.Fatalf("keepalive must be continuously supplied while silent, got %d frames: %s", n, out)
	}
}

// TestKeepAliveFrameIsValidOpenAIShape 确认保活帧在 OpenAI 语义下是「无害空 delta」：
// 有 choices、delta 为空对象、无 content、finish_reason 为 null。真实客户端看到它
// 不会追加任何内容，但能感知到连接仍活着。
func TestKeepAliveFrameIsValidOpenAIShape(t *testing.T) {
	var sb strings.Builder
	if err := openai.EmitKeepAlive(&sb, "chatcmpl-test", "m", 1); err != nil {
		t.Fatalf("EmitKeepAlive: %v", err)
	}
	out := sb.String()
	if !strings.HasPrefix(out, "data: ") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("keepalive must be a data: SSE frame, got %q", out)
	}
	for _, want := range []string{
		`"choices":[`, `"delta":{}`, `"finish_reason":null`, `"` + openai.KeepAliveField + `":true`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("keepalive frame missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, `"content"`) {
		t.Fatalf("keepalive must not carry content: %s", out)
	}
}

// TestInterruptedRecordsWhyItWasInterrupted：interrupted 必须带原因，否则「客户端
// 主动取消」与「中间层按读超时掐断」在记录里无法区分（线上排查时正是卡在这里）。
func TestInterruptedRecordsWhyItWasInterrupted(t *testing.T) {
	g := testGateway(&scriptProvider{events: []provider.Event{
		{Type: provider.EventText, Text: "hello"},
		{Type: provider.EventFinish, Finish: "stop"},
	}})
	meta := &ReqMeta{}
	g.HandleChat(WithReqMeta(context.Background(), meta), streamReq(), &failWriter{})

	if meta.Outcome != reqlog.OutcomeInterrupted {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, reqlog.OutcomeInterrupted)
	}
	if strings.TrimSpace(meta.Failure) == "" {
		t.Fatalf("interrupted without a reason is undiagnosable; Failure is empty")
	}
	if !strings.Contains(meta.Failure, "client gone") {
		t.Fatalf("Failure should carry the write error, got %q", meta.Failure)
	}
}

// TestCanceledRequestRecordsCancellation：客户端主动取消（ctx canceled）要记成
// 「客户端取消」而不是含糊的 interrupted。
func TestCanceledRequestRecordsCancellation(t *testing.T) {
	g := testGateway(&errProvider{err: context.Canceled})
	meta := &ReqMeta{}
	ctx, cancel := context.WithCancel(WithReqMeta(context.Background(), meta))
	cancel()
	g.HandleChat(ctx, streamReq(), httptest.NewRecorder())

	if meta.Outcome != reqlog.OutcomeInterrupted {
		t.Fatalf("outcome = %q, want %q", meta.Outcome, reqlog.OutcomeInterrupted)
	}
	if !strings.Contains(meta.Failure, "canceled") {
		t.Fatalf("Failure should mention cancellation, got %q", meta.Failure)
	}
}
