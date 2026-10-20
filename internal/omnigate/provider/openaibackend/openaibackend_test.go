package openaibackend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

// ---------------------------------------------------------------------------
// 额度耗尽：自动禁用（长冷却）+ 连击记账 + 持久化 + 恢复清零
// ---------------------------------------------------------------------------

// quotaServer 假上游：quotaKeys 里的 key 回 atria 形状的 429 额度错误，其余回 SSE。
func quotaServer(t *testing.T, quotaKeys map[string]bool, rec *keyRecorder) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		k := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		rec.add(k)
		if quotaKeys[k] {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"Token quota exhausted.","type":"atria_api_error","code":"insufficient_quota"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// healthOf 测试用读取某 key 的健康状态。
func healthOf(p *Provider, key string) (Health, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	h, ok := p.states[key]
	return h, ok
}

// memHealth 内存版 HealthStore：验证写穿 / 清理与载入路径。
type memHealth struct {
	mu      sync.Mutex
	m       map[string]Health
	saved   int
	dropped int
}

func newMemHealth() *memHealth { return &memHealth{m: map[string]Health{}} }

func (h *memHealth) LoadHealth(provider string) map[string]Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]Health{}
	for id, x := range h.m {
		out[id] = x
	}
	return out
}

func (h *memHealth) SaveHealth(provider, id string, x Health) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.m[id] = x
	h.saved++
}

func (h *memHealth) DropHealth(provider, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.m[id]; ok {
		delete(h.m, id)
		h.dropped++
	}
}

// 额度型 429：自动禁用（不是 1 分钟短冷却）——下一次请求直接跳过它，
// 连「全池冷却退让」轮都不摸；状态里记账 strikes=1。
func TestQuotaErrorDisablesKeyAndIsSkipped(t *testing.T) {
	rec := &keyRecorder{}
	srv := quotaServer(t, map[string]bool{"dead": true}, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"dead", "live"})

	for i := 0; i < 2; i++ {
		st, err := p.StreamChat(context.Background(), nil, testInput())
		if err != nil {
			t.Fatalf("第 %d 次: %v", i+1, err)
		}
		_ = st.Close()
	}

	got := rec.all()
	if len(got) != 3 || got[0] != "dead" || got[1] != "live" || got[2] != "live" {
		t.Fatalf("第二次请求应跳过 dead（不再退让重试），实际 %v", got)
	}
	h, ok := healthOf(p, "dead")
	if !ok || !h.Disabled || h.Strikes != 1 || h.Reason != "quota" {
		t.Fatalf("dead 应被禁用并记账，实际 %+v (ok=%v)", h, ok)
	}
	if h.Until <= time.Now().Unix() {
		t.Fatalf("禁用应是长冷却（未来时间戳），实际 %d", h.Until)
	}
}

// 全池额度耗尽：第一次把上游原文如实上报；此后快速失败，给一句能看懂的
// 话（不再每次重演上游的 quota 文案）。
func TestAllQuotaDisabledFailsFastWithReason(t *testing.T) {
	rec := &keyRecorder{}
	srv := quotaServer(t, map[string]bool{"d1": true, "d2": true}, rec)

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"d1", "d2"})

	if _, err := p.StreamChat(context.Background(), nil, testInput()); err == nil ||
		!strings.Contains(err.Error(), "429") {
		t.Fatalf("第一次应如实上报上游错误，实际 %v", err)
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("第一次应各试一把，实际 %v", got)
	}
	_, err := p.StreamChat(context.Background(), nil, testInput())
	if err == nil || !strings.Contains(err.Error(), "自动禁用") {
		t.Fatalf("第二次应快速失败并说明原因，实际 %v", err)
	}
	if got := rec.all(); len(got) != 2 {
		t.Fatalf("禁用中的 key 不该再打上游，实际 %v", got)
	}
}

// 禁用状态持久化：热重载重建 provider 后，死 key 仍然被跳过（不复活）。
func TestQuotaDisabledStateSurvivesReload(t *testing.T) {
	rec := &keyRecorder{}
	srv := quotaServer(t, map[string]bool{"dead": true}, rec)
	mem := newMemHealth()

	p1 := New("fake", srv.URL, "", nil)
	p1.SetAPIKeys([]string{"dead", "live"})
	p1.SetHealthStore(mem)
	if st, err := p1.StreamChat(context.Background(), nil, testInput()); err != nil {
		t.Fatalf("首轮: %v", err)
	} else {
		_ = st.Close()
	}
	if _, ok := mem.m[KeyID("dead")]; !ok {
		t.Fatal("禁用状态应写穿到 store")
	}

	// 模拟面板保存配置后的热重载：新 provider 从同一个 store 载入。
	p2 := New("fake", srv.URL, "", nil)
	p2.SetAPIKeys([]string{"dead", "live"})
	p2.SetHealthStore(mem)
	before := len(rec.all())
	st, err := p2.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("重载后: %v", err)
	}
	_ = st.Close()
	if got := rec.all()[before:]; len(got) != 1 || got[0] != "live" {
		t.Fatalf("重载后应直接跳过 dead，实际 %v", got)
	}
}

// 试炼成功：禁用到期后重试一把成了（上游恢复），状态清零、store 记录删除。
func TestExpiredDisableTrialsAndRecovers(t *testing.T) {
	rec := &keyRecorder{}
	srv := quotaServer(t, nil, rec) // 全好：试炼会成功
	mem := newMemHealth()
	// 预置一条「禁用已到期」的记录：连败 2 次、首见 8 天前。
	mem.m[KeyID("k")] = Health{
		Disabled: true, Until: time.Now().Add(-time.Hour).Unix(), Reason: "quota",
		Strikes: 2, FirstSeen: time.Now().Add(-8 * 24 * time.Hour).Unix(),
	}

	p := New("fake", srv.URL, "", nil)
	p.SetAPIKeys([]string{"k", "other"})
	p.SetHealthStore(mem)

	st, err := p.StreamChat(context.Background(), nil, testInput())
	if err != nil {
		t.Fatalf("试炼: %v", err)
	}
	_ = st.Close()
	got := rec.all()
	if len(got) != 1 || got[0] != "k" {
		t.Fatalf("到期后应优先试炼被禁用的 k，实际 %v", got)
	}
	if _, ok := healthOf(p, "k"); ok {
		t.Fatal("试炼成功应清除内存状态")
	}
	if _, ok := mem.m[KeyID("k")]; ok || mem.dropped == 0 {
		t.Fatalf("试炼成功应删除 store 记录，dropped=%d", mem.dropped)
	}
	if mem.saved == 0 {
		t.Fatal("载入时把「禁用到期」归一为试炼态应写回 store")
	}
}

// isQuotaFailure 分类口径：402 直接算；429/403 看特征串；分不清按短限流。
func TestIsQuotaFailureClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{429, `{"error":{"message":"Token quota exhausted.","code":"insufficient_quota"}}`, true},
		{429, `{"error":{"message":"Rate limit exceeded, retry later"}}`, false},
		{429, `{"error":{"message":"余额不足，请充值"}}`, true},
		{402, `{"error":{"message":"payment required"}}`, true},
		{403, `insufficient credits`, true},
		{401, `insufficient_quota`, false}, // 401 是鉴权类，不按额度处理
		{500, `insufficient_quota`, false}, // 5xx 与 key 无关
		{429, ``, false},
	}
	for _, c := range cases {
		if got := isQuotaFailure(c.status, []byte(c.body)); got != c.want {
			t.Errorf("isQuotaFailure(%d, %q) = %v want %v", c.status, c.body, got, c.want)
		}
	}
}

// 显式配置 models 时，目录就是这份白名单：不访问上游 /models，也不把上游
// 的全部模型混进来（手动只加一个就只显示一个）。
func TestListModelsUsesConfiguredListOnly(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"upstream-only"},{"id":"another"}]}`))
	}))
	defer srv.Close()

	p := New("fake", srv.URL, "", []string{"deepseek-v4.1-flash"})
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 1 || models[0].ID != "deepseek-v4.1-flash" || models[0].OwnedBy != "fake" {
		t.Fatalf("应只返回配置的模型，实际 %+v", models)
	}
	if hits != 0 {
		t.Fatalf("显式模型清单不应访问上游 /models，实际 %d 次", hits)
	}
}

// models 留空时才自动发现上游目录。
func TestListModelsAutoDiscoversWhenUnset(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"upstream-a"},{"id":"upstream-b"}]}`))
	}))
	defer srv.Close()

	p := New("fake", srv.URL, "", nil)
	models, err := p.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(models) != 2 || models[0].ID != "upstream-a" || models[1].ID != "upstream-b" {
		t.Fatalf("models 留空应自动发现上游目录，实际 %+v", models)
	}
}
