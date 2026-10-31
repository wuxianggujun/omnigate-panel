package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

// TestServeLoggedRecordsAPIKey 验证入站密钥标签会随请求写进归档：面板「用户」页
// 完全依赖这一列，漏写就会让所有请求都算成「无身份」。
func TestServeLoggedRecordsAPIKey(t *testing.T) {
	rec := reqlog.New(reqlog.Config{})
	s := &Server{
		reqlog: rec,
		cfg: &config.Config{
			APIKeys: config.APIKeyList{{Key: "sk-user", Name: "南瓜"}},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-user")
	w := httptest.NewRecorder()

	s.serveLogged(w, r, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	recent := rec.Snapshot().Recent
	if len(recent) == 0 {
		t.Fatal("no request recorded")
	}
	if got := recent[0].APIKey; got != "南瓜" {
		t.Errorf("api_key = %q, want 南瓜", got)
	}
}

// TestServeLoggedInjectsScopeWithoutRecorders 验证没有请求记录/用量记录器时，
// serveLogged 仍会把入站密钥的供应商范围挂到 ctx 上。否则 HandleChat 的
// routeScoped 拿到 nil，收窄密钥会退化成全量路由。
func TestServeLoggedInjectsScopeWithoutRecorders(t *testing.T) {
	s := &Server{
		cfg: &config.Config{
			APIKeys: config.APIKeyList{{
				Key: "sk-user", Name: "南瓜", Providers: []string{"atria"},
			}},
		},
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer sk-user")
	w := httptest.NewRecorder()

	var got []string
	s.serveLogged(w, r, func(w http.ResponseWriter, r *http.Request) {
		m := gateway.ReqMetaFrom(r.Context())
		if m != nil {
			got = m.KeyProviders
		}
		w.WriteHeader(http.StatusOK)
	})

	if len(got) != 1 || got[0] != "atria" {
		t.Fatalf("KeyProviders = %v, want [atria]", got)
	}
}

// TestServeLoggedRecordsReasoning 验证 serveLogged 会把 gateway 回填的「上游产出过
// 思考内容」标记落进请求记录（runable 等不回报 usage 时，思考列靠它兜底）。
func TestServeLoggedRecordsReasoning(t *testing.T) {
	rec := reqlog.New(reqlog.Config{})
	s := &Server{reqlog: rec}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	s.serveLogged(w, r, func(w http.ResponseWriter, r *http.Request) {
		if m := gateway.ReqMetaFrom(r.Context()); m != nil {
			m.Provider = "runable"
			m.Reasoning = true
			m.Attempts = 3
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	recent := rec.Snapshot().Recent
	if len(recent) == 0 {
		t.Fatal("no request recorded")
	}
	e := recent[0]
	if !e.Reasoning {
		t.Errorf("reasoning = false, want true")
	}
	if e.Provider != "runable" {
		t.Errorf("provider = %q, want runable", e.Provider)
	}
	if e.Attempts != 3 {
		t.Errorf("attempts = %d, want 3", e.Attempts)
	}
}

// TestServeLoggedRecordsUsage 验证 serveLogged 会把 OmniGate 请求计入共享用量
// 记录器（realm=provider、uid=账号、model=展示名），让「用量」页也覆盖 OmniGate。
func TestServeLoggedRecordsUsage(t *testing.T) {
	rec := reqlog.New(reqlog.Config{})
	urec := usage.New("")
	s := &Server{reqlog: rec, usage: urec}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	w := httptest.NewRecorder()

	s.serveLogged(w, r, func(w http.ResponseWriter, r *http.Request) {
		if m := gateway.ReqMetaFrom(r.Context()); m != nil {
			m.Provider = "raccoon"
			m.Model = "raccoon/raccoon-8c4485"
			m.Account = "浣熊账号1"
			m.Usage = &provider.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	snap := urec.SnapshotWindow(usage.Window{}, nil, nil)
	if snap.Totals.Requests != 1 || snap.Totals.TotalTokens != 15 {
		t.Fatalf("usage totals = %+v, want requests=1 total=15", snap.Totals)
	}
	if len(snap.ByRealm) != 1 || snap.ByRealm[0].Key != "raccoon" {
		t.Fatalf("by_realm = %+v, want one raccoon bucket", snap.ByRealm)
	}
	if len(snap.ByAccount) != 1 || snap.ByAccount[0].Key != "浣熊账号1" {
		t.Fatalf("by_account = %+v, want one account bucket", snap.ByAccount)
	}
}

// TestServeLoggedAppliesStreamOutcome 验证 serveLogged 会把 gateway 回填的
// outcome/status 覆盖进请求记录。流式失败时 HTTP 头（200）在首字节前已发出、无法
// 再改状态码，只有靠这层覆盖才能把「假成功」纠回 stream_error / interrupted。
func TestServeLoggedAppliesStreamOutcome(t *testing.T) {
	cases := []struct {
		name       string
		outcome    string
		status     int
		wantStatus int
		wantOK     bool
		wantOut    string
	}{
		{"success", "", 0, http.StatusOK, true, reqlog.OutcomeSuccess},
		{"error_frame", reqlog.OutcomeStreamError, http.StatusServiceUnavailable, http.StatusServiceUnavailable, false, reqlog.OutcomeStreamError},
		{"empty_stream", reqlog.OutcomeStreamError, http.StatusBadGateway, http.StatusBadGateway, false, reqlog.OutcomeStreamError},
		{"interrupted", reqlog.OutcomeInterrupted, 0, http.StatusOK, false, reqlog.OutcomeInterrupted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := reqlog.New(reqlog.Config{})
			s := &Server{reqlog: rec}
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()

			s.serveLogged(w, r, func(w http.ResponseWriter, r *http.Request) {
				if m := gateway.ReqMetaFrom(r.Context()); m != nil {
					m.Outcome = tc.outcome
					m.Status = tc.status
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("ok"))
			})

			recent := rec.Snapshot().Recent
			if len(recent) == 0 {
				t.Fatal("no request recorded")
			}
			e := recent[0]
			if e.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", e.Status, tc.wantStatus)
			}
			if e.OK != tc.wantOK {
				t.Errorf("ok = %v, want %v", e.OK, tc.wantOK)
			}
			if e.Outcome != tc.wantOut {
				t.Errorf("outcome = %q, want %q", e.Outcome, tc.wantOut)
			}
		})
	}
}

// TestHealthAnonymousHidesOpsDetail：/omni/healthz 恒无鉴权（编排探活只需状态码
// 语义），但匿名 / 错 key / **收窄密钥** 只能拿到最小身份标识。providers（含全部
// 账号标签）、cooldowns、sessions、recent（运行日志）、addresses（内网地址）、
// proxy_pools 都是运营数据，匿名放出去等于把账号清单与调度全貌挂在公网；收窄密钥
// 是发给客户端的凭据，只该能调自己那批模型，同样不该顺带读到这些。
//
// 这里故意把 gw 留成 nil：一旦走了明细分支就会 panic，等于反向证明没走。
func TestHealthAnonymousHidesOpsDetail(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{
		{Key: "secret-key"},
		{Key: "sk-rc", Name: "浣熊专用", Providers: []string{"raccoon"}},
	}}}
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no_key", nil},
		{"wrong_key", map[string]string{"Authorization": "Bearer nope"}},
		{"empty_bearer", map[string]string{"Authorization": "Bearer "}},
		{"scoped_key", map[string]string{"Authorization": "Bearer sk-rc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			s.handleHealth(w, r)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", w.Code)
			}
			if got := w.Header().Get("X-Service"); got != "omnigate" {
				t.Errorf("X-Service = %q, want omnigate", got)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not json: %v (%s)", err, w.Body.String())
			}
			if body["service"] != "omnigate" || body["status"] != "ok" {
				t.Errorf("identity fields = %v, want service=omnigate status=ok", body)
			}
			if len(body) != 2 {
				t.Errorf("anonymous healthz body = %v, want only {status,service}", body)
			}
			for _, leak := range []string{
				"providers", "cooldowns", "sessions", "recent", "addresses",
				"proxy_pools", "requests", "uptime_seconds", "port",
			} {
				if _, ok := body[leak]; ok {
					t.Errorf("anonymous healthz leaked %q: %v", leak, body)
				}
			}
		})
	}
}

// TestKeyOK：healthz 的「明细档位」与 authorized 共用同一判定。
func TestKeyOK(t *testing.T) {
	keyless := &Server{cfg: &config.Config{}}
	if !keyless.keyOK(httptest.NewRequest(http.MethodGet, "/healthz", nil)) {
		t.Error("未配置 api_key 时应视为通过（网关本就开放，与 authorized 同口径）")
	}

	s := &Server{cfg: &config.Config{APIKeys: config.APIKeyList{{Key: "secret-key"}}}}
	cases := []struct {
		name string
		set  func(*http.Request)
		want bool
	}{
		{"none", func(*http.Request) {}, false},
		{"wrong", func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, false},
		{"bearer", func(r *http.Request) { r.Header.Set("Authorization", "Bearer secret-key") }, true},
		{"bearer_spaces", func(r *http.Request) { r.Header.Set("Authorization", "Bearer   secret-key ") }, true},
		{"x_api_key", func(r *http.Request) { r.Header.Set("x-api-key", "secret-key") }, true},
		{"x_goog_api_key", func(r *http.Request) { r.Header.Set("x-goog-api-key", "secret-key") }, true},
		{"bearer_wins_over_goog", func(r *http.Request) {
			r.Header.Set("Authorization", "Bearer nope")
			r.Header.Set("x-goog-api-key", "secret-key")
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			tc.set(r)
			if got := s.keyOK(r); got != tc.want {
				t.Errorf("keyOK = %v, want %v", got, tc.want)
			}
		})
	}
}

// scopeReq 造一个带密钥的请求（Bearer）。
func scopeReq(key string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	return r
}

// TestKeyScopeNarrowsProviders 客户端密钥的授权范围必须如实解析：全量密钥放行所有
// 供应商，收窄密钥只放行白名单内的。
func TestKeyScopeNarrowsProviders(t *testing.T) {
	s := &Server{cfg: &config.Config{
		APIKeys: config.APIKeyList{
			{Key: "sk-full"},
			{Key: "sk-rc", Name: "浣熊专用", Providers: []string{"raccoon"}},
		},
		PanelKey: "sk-panel",
	}}

	k, ok := s.keyScope(scopeReq("sk-full"))
	if !ok || !k.Allows("raccoon") || !k.Allows("runable") {
		t.Errorf("全量密钥应放行所有供应商: ok=%v scope=%+v", ok, k)
	}

	k, ok = s.keyScope(scopeReq("sk-rc"))
	if !ok || !k.Allows("raccoon") || k.Allows("runable") {
		t.Errorf("收窄密钥只该放行 raccoon: ok=%v scope=%+v", ok, k)
	}

	if _, ok := s.keyScope(scopeReq("nope")); ok {
		t.Error("未登记的密钥必须判为不通过")
	}
	if _, ok := s.keyScope(scopeReq("")); ok {
		t.Error("未携带密钥必须判为不通过")
	}
}

// TestKeyScopePanelKeyAlwaysFull 面板自己那把密钥（= config.json 的 api_key）必须恒为
// 全量：面板前端要用它调 /omni/*，任何收窄规则都不能误伤它。
func TestKeyScopePanelKeyAlwaysFull(t *testing.T) {
	s := &Server{cfg: &config.Config{
		APIKeys:  config.APIKeyList{{Key: "sk-rc", Providers: []string{"raccoon"}}},
		PanelKey: "sk-panel",
	}}
	k, ok := s.keyScope(scopeReq("sk-panel"))
	if !ok {
		t.Fatal("面板密钥必须始终有效")
	}
	if !k.Allows("raccoon") || !k.Allows("runable") || !k.Allows("deepseek") {
		t.Errorf("面板密钥必须恒为全量，得到 %+v", k)
	}
}

// TestKeyScopePanelKeyLocksGateway api_keys 为空但注入了面板密钥时，网关**不能**被当成
// 「未配置密钥、本就开放」——否则任何匿名请求都能直接打上游。
func TestKeyScopePanelKeyLocksGateway(t *testing.T) {
	s := &Server{cfg: &config.Config{PanelKey: "sk-panel"}}
	if _, ok := s.keyScope(scopeReq("")); ok {
		t.Error("只注入面板密钥时，匿名请求必须判为不通过")
	}
	if _, ok := s.keyScope(scopeReq("anything")); ok {
		t.Error("只注入面板密钥时，错误密钥必须判为不通过")
	}
	if _, ok := s.keyScope(scopeReq("sk-panel")); !ok {
		t.Error("面板密钥本身必须通过")
	}
}

// TestKeyLabel 入站密钥标签是「用户统计」的唯一身份来源，必须满足三条：
//   - 写进归档的只能是不可逆的标签，不能是密钥本身；
//   - 面板密钥不能被当成一个用户；
//   - 未命名密钥之间不能合并成同一行（否则不同人算成一个人）。
func TestKeyLabel(t *testing.T) {
	s := &Server{cfg: &config.Config{
		APIKeys: config.APIKeyList{
			{Key: "sk-named", Name: "  南瓜  "},
			{Key: "sk-anon-a"},
			{Key: "sk-anon-b"},
		},
		PanelKey: "sk-panel",
	}}

	if got := s.keyLabel(scopeReq("sk-named")); got != "南瓜" {
		t.Errorf("命名密钥标签 = %q，期望去掉首尾空白的 name", got)
	}
	if got := s.keyLabel(scopeReq("sk-panel")); got != panelKeyLabel {
		t.Errorf("面板密钥标签 = %q，期望 %q", got, panelKeyLabel)
	}

	a := s.keyLabel(scopeReq("sk-anon-a"))
	b := s.keyLabel(scopeReq("sk-anon-b"))
	if a == "" || b == "" {
		t.Fatal("未命名但已配置的密钥也必须有标签，否则会被统计成「无身份」")
	}
	if a == b {
		t.Errorf("两把不同的未命名密钥得到相同标签 %q，统计会合并成一个人", a)
	}
	if strings.Contains(a, "sk-anon-a") || strings.Contains(b, "sk-anon-b") {
		t.Errorf("标签里不能出现密钥明文: %q / %q", a, b)
	}
	if again := s.keyLabel(scopeReq("sk-anon-a")); again != a {
		t.Errorf("同一把密钥的标签必须稳定: %q != %q", again, a)
	}

	if got := s.keyLabel(scopeReq("sk-unknown")); got != "" {
		t.Errorf("未登记密钥不应产生标签，得到 %q", got)
	}
	if got := s.keyLabel(scopeReq("")); got != "" {
		t.Errorf("匿名请求不应产生标签，得到 %q", got)
	}
}
