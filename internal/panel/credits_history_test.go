package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
	"github.com/wuxianggujun/omnigate-panel/internal/credithist"
	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/pool"
)

// reconFixture 造一个两账号的池 + 一份「像真的一样」的余额历史：每 10 分钟一个采样点、
// 覆盖 30 小时（保留期 26h 会裁掉更早的），u1 在 1 小时前从 1000 掉到 900、u2 从
// 500 掉到 400。
//
// 为什么必须密采样：对账窗口要按**整点桶**对齐（用量按小时桶记账），服务端会把窗口
// 收窄到采样真正覆盖的桶数。只放两个相距 23 小时的采样点，任何窗口都取不到左端点，
// 净变化会退化成「样本不足」——那样测出来的就不是真实行为。
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
	now := time.Now()
	drop := now.Add(-time.Hour)
	for ts := now.Add(-30 * time.Hour); !ts.After(now); ts = ts.Add(10 * time.Minute) {
		c1, c2 := int64(1000), int64(500)
		if !ts.Before(drop) {
			c1, c2 = 900, 400
		}
		hist.RecordAt("u1", c1, 0, ts)
		hist.RecordAt("u2", c2, 0, ts)
	}
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

// 窗口口径必须与 /panel/api/usage 的 hours **逐位一致**：[当前整点-(N-1)h, now]。
// 用量按「桶起点落在窗口内」筛桶，余额按采样点筛；左边界不对齐整点，两侧就差小半桶，
// 而热模型一小时约 90 分——足以把对账结论从「对得上」翻成「积分过期」。
func TestCreditsHistoryWindowAlignedToUsage(t *testing.T) {
	p := reconFixture(t)
	out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin), "hours=24")
	if out["hours"] != float64(24) {
		t.Fatalf("hours=%v want 24（历史覆盖 26h，不该被收窄）", out["hours"])
	}
	ws, _ := out["window_sec"].(float64)
	if ws <= float64(23*3600) || ws > float64(24*3600) {
		t.Fatalf("window_sec=%v 应落在 (23h, 24h]（= (N-1)h + 当前小时已过的部分）", ws)
	}
	winFrom, err := time.Parse(time.RFC3339, out["from"].(string))
	if err != nil {
		t.Fatalf("from=%v 不是 RFC3339", out["from"])
	}
	if winFrom.Minute() != 0 || winFrom.Second() != 0 {
		t.Fatalf("from=%v 未对齐整点（用量按小时桶筛选，不对齐就会差半桶）", winFrom)
	}
}

// 逐账号净变化与曲线：u1/u2 都在 1 小时前掉 100 分，24h 窗口内必须量出来。
func TestCreditsHistoryNetAndSeries(t *testing.T) {
	p := reconFixture(t)
	out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin), "hours=24")
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
		// 实际覆盖区间 ≈ 窗口长度（采样点 10 分钟一个，误差不超过一个间隔）。
		span, _ := m["span_sec"].(float64)
		if span <= float64(22*3600) || span > float64(24*3600) {
			t.Fatalf("%s span_sec=%v 应落在 (22h, 24h]", uid, span)
		}
		s, _ := m["series"].([]any)
		if len(s) == 0 || len(s) > creditHistoryPoints {
			t.Fatalf("%s series 点数=%d want 1..%d（降采样上限）", uid, len(s), creditHistoryPoints)
		}
	}
}

// 「全部历史」与超过保留期的窗口一律收敛到保留期小时数（26），且 hours 必须是
// **实际生效**值——前端拿它去拉同口径的用量，回请求值就会让两侧又错开。
func TestCreditsHistoryClampsToRetention(t *testing.T) {
	p := reconFixture(t)
	for _, q := range []string{"hours=0", "hours=26", "hours=48"} {
		out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin), q)
		if out["hours"] != float64(26) {
			t.Fatalf("%s → hours=%v want 26", q, out["hours"])
		}
		ws, _ := out["window_sec"].(float64)
		if ws <= float64(25*3600) || ws > float64(26*3600) {
			t.Fatalf("%s → window_sec=%v 应落在 (25h, 26h]", q, ws)
		}
	}
}

// 显式区间（from/to）：窗口超过保留期时会被收敛到 26h，此时响应里的 hours 必须跟着
// 重算——前端就是拿这个 hours 去拉同口径用量的。hours 比 window_sec 大 = 让用量侧
// 多算几个小时，差额会被读成「积分过期」（最坏的那种假警报）。
func TestCreditsHistoryExplicitWindowClampsHours(t *testing.T) {
	p := reconFixture(t)
	now := time.Now()
	unix := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }

	// 超保留期：48h 收敛到 26h，hours 必须同步收敛。
	out := getHistory(t, p, reqAs("alice", panelauth.RoleAdmin),
		"from="+unix(-48*time.Hour)+"&to="+unix(0))
	ws, _ := out["window_sec"].(float64)
	wantSec := float64(int(credithist.Retention / time.Second))
	if ws != wantSec {
		t.Fatalf("window_sec=%v want %v（超保留期应收敛）", ws, wantSec)
	}
	wantH := float64(int(credithist.Retention / time.Hour))
	if out["hours"] != wantH {
		t.Fatalf("hours=%v want %v（必须与 window_sec 同口径，不能回请求的 48h）",
			out["hours"], wantH)
	}

	// 保留期内：原样生效，hours 与 window_sec 依旧一致。
	out = getHistory(t, p, reqAs("alice", panelauth.RoleAdmin),
		"from="+unix(-3*time.Hour)+"&to="+unix(0))
	if out["hours"] != float64(3) {
		t.Fatalf("hours=%v want 3", out["hours"])
	}
	if ws, _ = out["window_sec"].(float64); ws != float64(3*3600) {
		t.Fatalf("window_sec=%v want %v", ws, 3*3600)
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

// alignedHours 是「两侧能不能对账」的闸门：窗口锚在当前整点，桶数上限由最早采样决定。
// 采样晚于锚点（刚上线几分钟）→ 0：此时用量侧任何桶都算满、余额侧却缺半截，宁可不给数字。
func TestAlignedHours(t *testing.T) {
	now := time.Date(2026, 10, 11, 18, 34, 0, 0, time.Local)
	anchor := now.Truncate(time.Hour) // 18:00
	snap := func(from time.Time) []credithist.Account {
		return []credithist.Account{{UID: "u", OK: true, From: from.Format(time.RFC3339)}}
	}
	cases := []struct {
		name string
		from time.Time
		want int
	}{
		{"采样晚于锚点太多（刚上线）", anchor.Add(35 * time.Minute), 0},
		{"刚好压在锚点上", anchor, 1},
		{"覆盖到 5 小时前", anchor.Add(-5 * time.Hour), 6},
		{"覆盖到 25 小时前", anchor.Add(-25 * time.Hour), 26},
	}
	for _, c := range cases {
		if got := alignedHours(snap(c.from), now); got != c.want {
			t.Errorf("%s: alignedHours=%d want %d", c.name, got, c.want)
		}
	}
	// 取所有账号里**最晚**的 From：历史最短的账号决定可对账窗口——它的最早采样点若
	// 晚于窗口左边界，那一小段净变化会被掺进整窗口合计（用量算整桶、余额只算一截）。
	mixed := []credithist.Account{
		{UID: "old", OK: true, From: anchor.Add(-25 * time.Hour).Format(time.RFC3339)},
		{UID: "new", OK: true, From: anchor.Add(-2 * time.Hour).Format(time.RFC3339)},
	}
	if got := alignedHours(mixed, now); got != 3 {
		t.Errorf("应被历史最短的账号约束（3 个桶），得到 %d", got)
	}
	// 余量 20 分钟：采样晚于锚点 16 分钟仍在余量内（对齐到当前桶），超过就放弃对齐。
	if got := alignedHours(snap(anchor.Add(16*time.Minute)), now); got != 1 {
		t.Errorf("锚点后 16 分钟应在余量内得 1，得到 %d", got)
	}
	if got := alignedHours(snap(anchor.Add(25*time.Minute)), now); got != 0 {
		t.Errorf("锚点后 25 分钟超出余量应得 0，得到 %d", got)
	}
	// 没有可解析的采样时间 → 0。
	if got := alignedHours([]credithist.Account{{UID: "u", OK: true}}, now); got != 0 {
		t.Errorf("无采样时间应得 0，得到 %d", got)
	}
	if got := alignedHours(nil, now); got != 0 {
		t.Errorf("空快照应得 0，得到 %d", got)
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
