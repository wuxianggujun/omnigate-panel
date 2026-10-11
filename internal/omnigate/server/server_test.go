package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
	"github.com/wuxianggujun/omnigate-panel/internal/usage"
)

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
// 语义），但匿名 / 错 key 只能拿到最小身份标识。providers（含全部账号标签）、
// cooldowns、sessions、recent（运行日志）、addresses（内网地址）、proxy_pools
// 都是运营数据，匿名放出去等于把账号清单与调度全貌挂在公网。
//
// 这里故意把 gw 留成 nil：一旦走了明细分支就会 panic，等于反向证明没走。
func TestHealthAnonymousHidesOpsDetail(t *testing.T) {
	s := &Server{cfg: &config.Config{APIKeys: []string{"secret-key"}}}
	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"no_key", nil},
		{"wrong_key", map[string]string{"Authorization": "Bearer nope"}},
		{"empty_bearer", map[string]string{"Authorization": "Bearer "}},
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

	s := &Server{cfg: &config.Config{APIKeys: []string{"secret-key"}}}
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
