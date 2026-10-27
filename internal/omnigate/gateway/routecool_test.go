package gateway

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/openaibackend"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/raccoon"
)

func TestRouteFailureLedgerBackoffAndClear(t *testing.T) {
	l := newRouteFailureLedger()
	now := time.Now()
	want := []time.Duration{
		30 * time.Second,
		time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		5 * time.Minute,
		5 * time.Minute,
	}
	for i, d := range want {
		until, strikes := l.Fail("cline-pass", "m", "quota", now)
		if strikes != i+1 {
			t.Fatalf("第 %d 次 strikes = %d, want %d", i+1, strikes, i+1)
		}
		if got := until.Sub(now); got != d {
			t.Fatalf("第 %d 次冷却 = %v, want %v", i+1, got, d)
		}
		if !l.Cooling("cline-pass", "m", now) {
			t.Fatalf("第 %d 次失败后应处于绕行窗口", i+1)
		}
		if got := len(l.Snapshot(now)); got != 1 {
			t.Fatalf("第 %d 次快照条数 = %d, want 1", i+1, got)
		}
		now = until // 到期的下一瞬间再失败，验证退避继续增长
	}

	if _, had := l.Clear("cline-pass", "m"); !had {
		t.Fatal("Clear 应报告此前存在记录")
	}
	if l.Cooling("cline-pass", "m", now) {
		t.Fatal("Clear 后不应再处于绕行窗口")
	}
	if _, had := l.Clear("cline-pass", "m"); had {
		t.Fatal("重复 Clear 不应报告存在记录")
	}
	if got := l.Snapshot(now); len(got) != 0 {
		t.Fatalf("Clear 后快照应为空，实际 %+v", got)
	}
}

func TestRouteFailurePenalized(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil/空响应", nil, true},
		{"客户端取消", context.Canceled, false},
		{"400", errors.New("upstream HTTP 400: bad request"), false},
		{"400 quota", errors.New("upstream HTTP 400: insufficient_quota"), true},
		{"422", errors.New("status 422: unprocessable"), false},
		{"raccoon 400", &raccoon.UpstreamError{Status: 400, Message: "bad request"}, false},
		{"prompt 超限", &promptTooLongErr{Provider: "p", Limit: 10}, false},
		{"401", errors.New("upstream HTTP 401: invalid key"), true},
		{"429", errors.New("upstream HTTP 429: rate limited"), true},
		{"500", errors.New("upstream HTTP 500: overloaded"), true},
		{"openaibackend 500", &openaibackend.UpstreamError{Status: 500, Body: "overloaded"}, true},
		{"404", errors.New("upstream HTTP 404: model not found"), true},
		{"未知错误", errors.New("connection reset by peer"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeFailurePenalized(tc.err); got != tc.want {
				t.Fatalf("routeFailurePenalized(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestRouteQuotaFloor(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		want time.Duration
		ok   bool
	}{
		{
			"cline weekly cap",
			errors.New(`upstream HTTP 429: {"error":{"code":"INFERENCE_CAP_ERROR","message":"Error 429: You have reached your weekly Clinepass limit. The limit resets in 2h 12m, please try again later."}}`),
			2*time.Hour + 13*time.Minute,
			true,
		},
		{
			"minutes",
			errors.New(`upstream HTTP 429: weekly limit resets in 28m`),
			29 * time.Minute,
			true,
		},
		{
			"quota without reset hint",
			errors.New(`upstream HTTP 402: insufficient_quota`),
			routeCoolQuotaDefault,
			true,
		},
		{
			"long reset is capped",
			errors.New(`upstream HTTP 429: weekly limit resets in 2d`),
			routeCoolQuotaMax,
			true,
		},
		{
			"401 stays short",
			errors.New(`upstream HTTP 401: insufficient_quota`),
			0,
			false,
		},
		{
			"plain rate limit stays short",
			errors.New(`upstream HTTP 429: rate limited`),
			0,
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			until, ok := routeQuotaFloor(tc.err, now)
			if ok != tc.ok {
				t.Fatalf("routeQuotaFloor ok = %v, want %v", ok, tc.ok)
			}
			if ok && until.Sub(now) != tc.want {
				t.Fatalf("routeQuotaFloor 窗口 = %v, want %v", until.Sub(now), tc.want)
			}
		})
	}
}

func TestMarkRouteFailureUsesQuotaReset(t *testing.T) {
	g := &Gateway{routeCool: newRouteFailureLedger()}
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	err := errors.New(`upstream HTTP 429: {"error":{"code":"INFERENCE_CAP_ERROR","message":"You have reached your weekly Clinepass limit. The limit resets in 2h 12m."}}`)
	g.markRouteFailure("cline-pass", "m", err, false, now)
	snap := g.routeCool.Snapshot(now)
	if len(snap) != 1 {
		t.Fatalf("route_cooldowns 条数 = %d, want 1", len(snap))
	}
	if snap[0].Reason != "quota" {
		t.Fatalf("reason = %q, want quota", snap[0].Reason)
	}
	if got, want := snap[0].Until.Sub(now), 2*time.Hour+13*time.Minute; got != want {
		t.Fatalf("周额度窗口 = %v, want %v", got, want)
	}
}

func TestRouteFailureLongQuotaWindow(t *testing.T) {
	l := newRouteFailureLedger()
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	floor := now.Add(2*time.Hour + 13*time.Minute)
	until, strikes := l.FailUntil("cline-pass", "m", "quota", now, floor)
	if strikes != 1 || !until.Equal(floor) {
		t.Fatalf("首次长窗口 = %v strikes=%d, want %v/1", until, strikes, floor)
	}
	if !l.Cooling("cline-pass", "m", now.Add(2*time.Hour)) {
		t.Fatal("2 小时后仍应在周额度绕行窗口内")
	}
	if l.Cooling("cline-pass", "m", floor) {
		t.Fatal("到达截止时间后应允许探针")
	}

	// 21 分钟后又探针失败一次（超过安静重置门槛），不能把长窗口缩短成 30m。
	later := now.Add(21 * time.Minute)
	until, strikes = l.FailUntil("cline-pass", "m", "quota", later, later.Add(routeCoolQuotaDefault))
	if !until.Equal(floor) {
		t.Fatalf("短失败不应缩短长窗口，得到 %v want %v", until, floor)
	}
	if strikes != 1 {
		t.Fatalf("安静 21 分钟后 strikes 应重置为 1，得到 %d", strikes)
	}

	// FailUntil 自身也要把超长 floor 封顶在 24h。
	until, _ = l.FailUntil("cline-pass", "other", "quota", now, now.Add(48*time.Hour))
	if got := until.Sub(now); got != routeCoolQuotaMax {
		t.Fatalf("超长窗口 = %v, want %v", got, routeCoolQuotaMax)
	}
}

func TestRouteFailureReasonQuota(t *testing.T) {
	err := errors.New(`upstream HTTP 429: INFERENCE_CAP_ERROR: weekly Clinepass limit resets in 2h`)
	if got := routeFailureReason(err); got != "quota" {
		t.Fatalf("routeFailureReason = %q, want quota", got)
	}
	if !routeFailurePenalized(err) {
		t.Fatal("周额度上限应记为目标侧失败")
	}
}

func TestPromoteCoolingPrimary(t *testing.T) {
	g := &Gateway{routeCool: newRouteFailureLedger()}
	now := time.Now()
	primary := fallbackCandidate{Provider: "cline-pass", Model: "m"}
	chain := []fallbackCandidate{
		{Provider: "yijia", Model: "m"},
		{Provider: "other", Model: "m"},
	}

	g.routeCool.Fail(primary.Provider, primary.Model, "quota", now)
	next, rest, ok := g.promoteCoolingPrimary(primary, chain, now)
	if !ok || next != chain[0] {
		t.Fatalf("应把 yijia 提为主目标，得到 %+v ok=%v", next, ok)
	}
	if len(rest) != 2 || rest[0] != chain[1] || rest[1] != primary {
		t.Fatalf("回退链应先列未冷却候选、再把原主目标作为探针，得到 %+v", rest)
	}

	// 链上候选也全部冷却：不要乱换主目标，保留原顺序（必要时仍会探针）。
	g.routeCool.Fail("yijia", "m", "quota", now)
	g.routeCool.Fail("other", "m", "quota", now)
	if next, rest, ok := g.promoteCoolingPrimary(primary, chain, now); ok ||
		next != primary || len(rest) != len(chain) {
		t.Fatalf("全冷却时应保持原样，得到 next=%+v rest=%+v ok=%v", next, rest, ok)
	}

	// 没有备用目标：即使主目标冷却也不改变顺序。
	if next, rest, ok := g.promoteCoolingPrimary(primary, nil, now); ok ||
		next != primary || rest != nil {
		t.Fatalf("无备用目标时应保持原样，得到 next=%+v rest=%+v ok=%v", next, rest, ok)
	}
}

// routeTestProvider 是可计数的假上游：err 非空时失败，否则回一段正文。
type routeTestProvider struct {
	name  string
	err   error
	text  string
	calls int
}

func (p *routeTestProvider) Name() string { return p.name }
func (p *routeTestProvider) Type() string { return "openai" }
func (p *routeTestProvider) ListModels(context.Context) ([]openai.Model, error) {
	return []openai.Model{{ID: "m", Object: "model"}}, nil
}
func (p *routeTestProvider) StreamChat(context.Context, *provider.Account, provider.ChatInput) (provider.Stream, error) {
	p.calls++
	if p.err != nil {
		return nil, p.err
	}
	return &scriptStream{events: []provider.Event{
		{Type: provider.EventText, Text: p.text},
		{Type: provider.EventFinish, Finish: "stop"},
	}}, nil
}

// 主目标连续失败时，下一次请求应直接走别名链上的备用目标，而不是每次都先撞
// 一次 cline-pass。恢复后仍会探针回主目标（由冷却到期保证）。
func TestHandleChatBypassesCoolingPrimary(t *testing.T) {
	bad := &routeTestProvider{name: "cline-pass", err: errors.New("upstream HTTP 500: overloaded")}
	good := &routeTestProvider{name: "yijia", text: "ok"}
	g := &Gateway{
		cfg:        &config.Config{DefaultProvider: "cline-pass", DefaultModel: "deepseek"},
		log:        logx.New(10),
		providers:  map[string]provider.Provider{"cline-pass": bad, "yijia": good},
		order:      []string{"cline-pass", "yijia"},
		accounts:   map[string][]*provider.Account{},
		modelCache: map[string]modelEntry{},
		convs:      map[string]*conv{},
		pending:    map[string]raccoonPending{},
		cool:       newCooldownLedger(),
		routeCool:  newRouteFailureLedger(),
		sess:       newSessionHealth(),
	}
	g.modelMap = NewModelMappingStore(filepath.Join(t.TempDir(), "mapping.json"))
	if err := g.SetModelMappingTargets("deepseek", []string{"cline-pass/m", "yijia/m"}); err != nil {
		t.Fatalf("配链: %v", err)
	}

	chat := func() (int, string, *ReqMeta) {
		meta := &ReqMeta{}
		w := httptest.NewRecorder()
		g.HandleChat(WithReqMeta(context.Background(), meta), &openai.ChatRequest{
			Model:    "deepseek",
			Messages: []openai.Message{{Role: "user", Content: openai.Content{Text: "hi"}}},
		}, w)
		return w.Code, w.Body.String(), meta
	}

	code, body, meta := chat()
	if code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("第一次请求应由兜底成功，code=%d body=%s", code, body)
	}
	if bad.calls != 1 || good.calls != 1 {
		t.Fatalf("第一次调用次数 bad=%d good=%d, want 1/1", bad.calls, good.calls)
	}
	if meta.Provider != "yijia" {
		t.Fatalf("兜底后请求记录应归到 yijia，实际 %q", meta.Provider)
	}
	if !g.routeCool.Cooling("cline-pass", "m", time.Now()) {
		t.Fatal("cline-pass 失败后应进入绕行窗口")
	}

	code, body, meta = chat()
	if code != 200 || !strings.Contains(body, "ok") {
		t.Fatalf("第二次请求应直接走备用目标，code=%d body=%s", code, body)
	}
	if bad.calls != 1 {
		t.Fatalf("绕行窗口内不应再打 cline-pass，实际调用 %d 次", bad.calls)
	}
	if good.calls != 2 {
		t.Fatalf("第二次应由 yijia 服务，实际调用 %d 次", good.calls)
	}
	if meta.Provider != "yijia" {
		t.Fatalf("第二次请求记录 provider = %q, want yijia", meta.Provider)
	}
}
