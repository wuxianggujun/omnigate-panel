package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

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
