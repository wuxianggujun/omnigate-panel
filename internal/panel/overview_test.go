package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// overviewRecent 拉取总览并解出 recent 数组。
func overviewRecent(t *testing.T, p *Panel, cookie *http.Cookie) []map[string]any {
	t.Helper()
	rec := do(p, http.MethodGet, "/panel/api/overview", "", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Recent []map[string]any `json:"recent"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview 响应非法 JSON: %v (%s)", err, rec.Body.String())
	}
	return body.Recent
}

// newOverviewReqlog 构造一个把事件写进内存归档的请求记录器（Dir 走临时目录）。
func newOverviewReqlog(t *testing.T, events ...reqlog.Event) *reqlog.Recorder {
	t.Helper()
	r := reqlog.New(reqlog.Config{Enabled: true, Dir: t.TempDir(), RetentionDays: 7, MaxBytes: 1 << 20})
	for _, e := range events {
		r.Record(e)
	}
	// 归档是异步刷盘的；Close 会等待队列写完。
	r.Close()
	return r
}

// TestOverviewRecentReturnsArchive 总览 recent 必须来自请求归档，而不是恒为空数组。
// 回归：单引擎重构曾把它写死成 `[]any{}`，导致「近期请求」卡片永远显示空态。
func TestOverviewRecentReturnsArchive(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	log := newOverviewReqlog(t,
		reqlog.Event{
			Time: base.Add(time.Second), RequestID: "newest", Provider: "cline-pass",
			Account: "账号(uid8)", Model: "deepseek-v4.1-flash",
			Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess, DurationMs: 1234, TotalTokens: 42,
		},
		reqlog.Event{
			Time: base, RequestID: "older", Provider: "atria",
			Account: "账号(uid8)", Model: "glm-5.3",
			Status: 429, OK: false, Outcome: reqlog.OutcomeHTTPError, DurationMs: 30,
		},
	)
	p, token := mustPanel(t, Config{RequestLog: log})
	recent := overviewRecent(t, p, login(t, p, "admin", "pw123456"))
	_ = token
	if len(recent) != 2 {
		t.Fatalf("recent 长度 = %d，期望 2（%+v）", len(recent), recent)
	}
	if recent[0]["request_id"] != "newest" {
		t.Fatalf("recent[0] = %+v，期望按时间倒序（newest 在前）", recent[0])
	}
	if recent[0]["total_tokens"] != float64(42) {
		t.Errorf("recent[0].total_tokens = %v，期望 42", recent[0]["total_tokens"])
	}
}

// TestOverviewRecentEmptyWhenNoRequestLog 没有记录器时不返回 recent 字段内容，
// 前端回落空态即可；关键是不能因为 nil 记录器而 500。
func TestOverviewRecentEmptyWhenNoRequestLog(t *testing.T) {
	p, _ := mustPanel(t, Config{})
	recent := overviewRecent(t, p, login(t, p, "admin", "pw123456"))
	if len(recent) != 0 {
		t.Fatalf("无记录器时 recent = %+v，期望空", recent)
	}
}

// TestOverviewRecentFiltersByOwnership 普通用户只看归属自己的账号请求；
// 无归属的账号记录必须被剔除（与 /panel/api/request_logs 同一口径）。
func TestOverviewRecentFiltersByOwnership(t *testing.T) {
	base := time.Now().Add(-time.Minute)
	log := newOverviewReqlog(t,
		reqlog.Event{
			Time: base.Add(2 * time.Second), RequestID: "mine", Provider: "cline-pass",
			Account: "mine", Model: "m", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		},
		reqlog.Event{
			Time: base.Add(time.Second), RequestID: "theirs", Provider: "atria",
			Account: "theirs", Model: "m", Status: 200, OK: true, Outcome: reqlog.OutcomeSuccess,
		},
	)
	p, _ := mustPanel(t, Config{
		RequestLog: log,
	})
	p.cfg.PanelAuth = panelauth.New([]panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "bob", panelauth.RoleUser, "pw123456"),
	}, time.Hour, 5, 15*time.Minute)
	p.ownership.Set("omni:cline-pass/mine", "bob")
	recent := overviewRecent(t, p, login(t, p, "bob", "pw123456"))
	if len(recent) != 1 || recent[0]["request_id"] != "mine" {
		t.Fatalf("普通用户 recent = %+v，期望仅含归属自己的 mine", recent)
	}
}
