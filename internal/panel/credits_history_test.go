package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
	"github.com/wuxianggujun/omnigate-panel/internal/credithist"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pool"
)

// reconFixture 造一个两账号的池 + 一份余额历史：
// u1（alice 的）1000 → 900，u2（bob 的）500 → 400。
// 起点取 23h 前（而不是 24h 整）：净变化只认**窗口内**的样本，正好落在 24h 边界上的
// 样本会被排除——这是刻意的（窗口外的不算），但用它当测试基线会得到「样本不足」。
func reconFixture(t *testing.T) *Panel {
	t.Helper()
	pl := pool.New("")
	for _, a := range []*auth.Auth{
		{UID: "u1", Nickname: "wb-1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000},
		{UID: "u2", Nickname: "wb-2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000},
	} {
		pl.Add(a)
	}
	hist := credithist.New("")
	base := time.Now().Add(-23 * time.Hour)
	now := time.Now()
	hist.RecordAt("u1", 1000, 0, base)
	hist.RecordAt("u2", 500, 0, base)
	hist.RecordAt("u1", 900, 0, now)
	hist.RecordAt("u2", 400, 0, now)

	p := New(Config{Version: "test", APIKey: "k", Pool: pl, History: hist})
	p.ownership.Set(wbOwnerKey("u1"), "alice")
	p.ownership.Set(wbOwnerKey("u2"), "bob")
	return p
}

func getHistory(t *testing.T, p *Panel, r *http.Request, query string) map[string]any {
	t.Helper()
	r.URL.Path = "/panel/api/credits/history"
	r.URL.RawQuery = query
	w := httptest.NewRecorder()
	p.creditsHistory(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 历史端点的窗口口径与用量页一致（hours=24 → 86400 秒），逐账号给出净变化。
func TestCreditsHistoryWindowAndNet(t *testing.T) {
	p := reconFixture(t)
	out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin), "hours=24")
	if got := out["window_sec"]; got != float64(86400) {
		t.Fatalf("window_sec=%v want 86400", got)
	}
	accts, _ := out["accounts"].([]any)
	if len(accts) != 2 {
		t.Fatalf("admin 应看到 2 个账号，得到 %d", len(accts))
	}
	byUID := map[string]map[string]any{}
	for _, a := range accts {
		m, _ := a.(map[string]any)
		byUID[m["uid"].(string)] = m
	}
	for uid, want := range map[string]float64{"u1": -100, "u2": -100} {
		m := byUID[uid]
		if m == nil {
			t.Fatalf("缺少账号 %s", uid)
		}
		if m["net"] != want || m["ok"] != true {
			t.Fatalf("%s net=%v ok=%v want %v true", uid, m["net"], m["ok"], want)
		}
		if m["span_sec"] != float64(23*3600) {
			t.Fatalf("%s span_sec=%v want %d", uid, m["span_sec"], 23*3600)
		}
		if s, _ := m["series"].([]any); len(s) != 2 {
			t.Fatalf("%s series 点数=%d want 2", uid, len(s))
		}
	}
}

// 「全部历史」（hours=0）只能给保留期那一段：window_sec 回落到 Retention，
// 前端据此如实标注，不假装成无限窗口。
func TestCreditsHistoryClampsToRetention(t *testing.T) {
	p := reconFixture(t)
	out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin), "hours=0")
	want := float64(int64(credithist.Retention.Seconds()))
	if out["window_sec"] != want {
		t.Fatalf("window_sec=%v want %v（保留期）", out["window_sec"], want)
	}
}

// 归属过滤：普通用户只看自己账号；admin 看全部；未登录一个都看不到。
func TestCreditsHistoryOwnershipFilter(t *testing.T) {
	p := reconFixture(t)

	out := getHistory(t, p, reqAs("alice", panelauth.RoleUser), "hours=24")
	accts, _ := out["accounts"].([]any)
	if len(accts) != 1 {
		t.Fatalf("alice 应只看到自己的 1 个账号，得到 %d", len(accts))
	}
	if m, _ := accts[0].(map[string]any); m["uid"] != "u1" {
		t.Fatalf("alice 看到的是 %v，应为 u1", m["uid"])
	}

	out = getHistory(t, p, reqAs("", ""), "hours=24")
	if accts, _ := out["accounts"].([]any); len(accts) != 0 {
		t.Fatalf("未登录应看不到账号，得到 %d", len(accts))
	}
}

// 未注入 History → 501（前端据此隐藏「积分对账」整块）。
func TestCreditsHistoryDisabled(t *testing.T) {
	p := New(Config{Version: "test", APIKey: "k"})
	r := reqAs("alice", panelauth.RoleAdmin)
	r.URL.Path = "/panel/api/credits/history"
	w := httptest.NewRecorder()
	p.creditsHistory(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status=%d want 501", w.Code)
	}
}

// overview 的 credits_net 只含本次可见账号，且是近 24h 净变化。
func TestOverviewCreditsNet(t *testing.T) {
	p := reconFixture(t)
	r := reqAs("alice", panelauth.RoleUser)
	w := httptest.NewRecorder()
	p.overview(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		Accounts []struct {
			UID string `json:"uid"`
		} `json:"accounts"`
		CreditsNet map[string]struct {
			Net     int64  `json:"net"`
			OK      bool   `json:"ok"`
			SpanSec int64  `json:"span_sec"`
			From    string `json:"from"`
		} `json:"credits_net"`
		WindowSec int64 `json:"credits_net_window_sec"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Accounts) != 1 || out.Accounts[0].UID != "u1" {
		t.Fatalf("alice 的账号列表=%+v，应只有 u1", out.Accounts)
	}
	if len(out.CreditsNet) != 1 {
		t.Fatalf("credits_net 应只含可见账号，得到 %d 项", len(out.CreditsNet))
	}
	rec, ok := out.CreditsNet["u1"]
	if !ok || rec.Net != -100 || !rec.OK || rec.From == "" {
		t.Fatalf("u1 credits_net=%+v want net=-100 ok", rec)
	}
	if out.WindowSec != int64(credithist.DefaultWindow.Seconds()) {
		t.Fatalf("credits_net_window_sec=%d want %d", out.WindowSec, int64(credithist.DefaultWindow.Seconds()))
	}
}

// 样本不足的账号（刚重启）不出数字：ok=false，前端显示「—」而不是伪造一个 0。
func TestOverviewCreditsNetInsufficient(t *testing.T) {
	pl := pool.New("")
	pl.Add(&auth.Auth{UID: "u1", Nickname: "wb-1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1893456000})
	hist := credithist.New("")
	hist.RecordAt("u1", 1000, 0, time.Now()) // 只有一个采样点
	p := New(Config{Version: "test", APIKey: "k", Pool: pl, History: hist})
	p.ownership.Set(wbOwnerKey("u1"), "alice")
	w := httptest.NewRecorder()
	p.overview(w, reqAs("alice", panelauth.RoleUser))
	var out struct {
		CreditsNet map[string]any `json:"credits_net"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if rec, ok := out.CreditsNet["u1"]; ok {
		t.Fatalf("样本不足不应出数字，得到 %+v", rec)
	}
}
