package openaibackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// keyRecorder 记录上游实际收到的 Bearer key（按调用顺序）。
type keyRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *keyRecorder) add(k string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = append(r.keys, k)
}

func (r *keyRecorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.keys))
	copy(out, r.keys)
	return out
}

// keyServer 起一个假上游：badStatus[key] 非 0 时对该 key 回那个状态码，否则回一段 SSE。
func keyServer(t *testing.T, badStatus map[string]int, rec *keyRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		rec.add(k)
		if code := badStatus[k]; code != 0 {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testInput() provider.ChatInput {
	return provider.ChatInput{
		Model:    "m",
		Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
	}
}

// 第一把 key 401：应自动冷却并换到第二把，整体仍成功。
func TestKeyPoolRotatesOnUnauthorized(t *testing.T) {
	rec := &keyRecorder{}
	srv := keyServer(t, map[string]int{"bad": http.StatusUnauthorized}, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"bad", "good"})

	st, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	_ = st.Close()

	got := rec.all()
	if len(got) != 2 || got[0] != "bad" || got[1] != "good" {
		t.Fatalf("应先用 bad 再用 good，实际 %v", got)
	}
}

// 429（限流）同样触发换 key。
func TestKeyPoolRotatesOnRateLimit(t *testing.T) {
	rec := &keyRecorder{}
	srv := keyServer(t, map[string]int{"slow": http.StatusTooManyRequests}, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"slow", "fast"})

	st, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	_ = st.Close()

	if got := rec.all(); len(got) != 2 || got[0] != "slow" || got[1] != "fast" {
		t.Fatalf("应先用 slow 再用 fast，实际 %v", got)
	}
}

// 单把 key 不做轮询：401 原样上报（行为与从前一致）。
func TestSingleKeyDoesNotRetry(t *testing.T) {
	rec := &keyRecorder{}
	srv := keyServer(t, map[string]int{"only": http.StatusUnauthorized}, rec)

	p := New("fake", srv.URL, "only", nil)

	_, err := p.StreamChat(context.Background(), nil, testInput())
	if err == nil {
		t.Fatal("单 key 401 应上报错误")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误应带上游状态码: %v", err)
	}
	if got := rec.all(); len(got) != 1 {
		t.Fatalf("单 key 不该重试，实际调用 %v", got)
	}
}

// 池里全是坏 key：把最后一次上游错误上报（不静默、也不死循环）。
func TestKeyPoolAllBadReturnsUpstreamError(t *testing.T) {
	rec := &keyRecorder{}
	srv := keyServer(t, map[string]int{"k1": http.StatusForbidden, "k2": http.StatusUnauthorized}, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"k1", "k2"})

	if _, err := p.StreamChat(context.Background(), nil, testInput()); err == nil {
		t.Fatal("全坏应报错")
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("每把 key 最多试一次，实际 %v", got)
	}
}

// 两把都好 key：跨请求轮询（不会永远只打第一把）。
func TestKeyPoolRoundRobinsAcrossRequests(t *testing.T) {
	rec := &keyRecorder{}
	srv := keyServer(t, nil, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"a", "b"})

	for i := 0; i < 2; i++ {
		st, err := p.StreamChat(context.Background(), nil, testInput())
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		_ = st.Close()
	}

	got := rec.all()
	if len(got) != 2 || got[0] == got[1] {
		t.Fatalf("两次请求应用不同的 key，实际 %v", got)
	}
}

// 空池回落单个 api_key；SetAPIKeys 会丢掉空白项。
func TestKeyPoolFallsBackToSingleKey(t *testing.T) {
	p := New("fake", "http://127.0.0.1:1", "sk-single", nil)
	if got := p.candidates(); len(got) != 1 || got[0] != "sk-single" {
		t.Fatalf("应回落到单个 api_key，实际 %v", got)
	}
	p.SetAPIKeys([]string{"  ", ""})
	if got := p.candidates(); len(got) != 1 || got[0] != "sk-single" {
		t.Fatalf("空池应仍回落单个 api_key，实际 %v", got)
	}
	p.SetAPIKeys([]string{" sk-pool-a ", "sk-pool-b"})
	if got := p.candidates(); len(got) != 2 || got[0] != "sk-pool-a" || got[1] != "sk-pool-b" {
		t.Fatalf("池子应去空白，实际 %v", got)
	}
}
