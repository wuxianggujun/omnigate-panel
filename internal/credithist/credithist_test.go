package credithist

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func at(base time.Time, d time.Duration) time.Time { return base.Add(d) }

// 基本口径：窗口内的净变化取「窗口起点样本 → 最新样本」，负值表示掉了。
func TestNetWindow(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	for i := 0; i <= 30; i++ { // 每 30 分钟一个样本，共 15h
		r.RecordAt("u1", 1000-int64(i*10), 0, at(base, time.Duration(i)*30*time.Minute))
	}
	now := at(base, 15*time.Hour)

	// 近 6h：起点是 i=18（15h-6h=9h → 9h/30m=18），余额 1000-180=820，最新 1000-300=700。
	net, from, to, n, ok := r.Net("u1", 6*time.Hour, now)
	if !ok || net != -120 {
		t.Fatalf("近6h net=%d ok=%v, want -120 true", net, ok)
	}
	if !from.Equal(at(base, 9*time.Hour)) || !to.Equal(now) {
		t.Fatalf("覆盖区间 %v → %v, want %v → %v", from, to, at(base, 9*time.Hour), now)
	}
	if n != 13 { // i=18..30
		t.Fatalf("样本数 %d, want 13", n)
	}

	// 窗口大于保留期时退回全部可用样本（起点 = 最早的样本）。
	net, from, _, _, ok = r.Net("u1", 100*time.Hour, now)
	if !ok || net != -300 || !from.Equal(base) {
		t.Fatalf("超长窗口 net=%d from=%v ok=%v, want -300 %v true", net, from, ok, base)
	}
}

// 未知账号 / 样本太少 / 覆盖太短都不出数字。
func TestNetInsufficient(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	if _, _, _, _, ok := r.Net("nope", time.Hour, base); ok {
		t.Fatal("未知账号不应有净变化")
	}
	r.RecordAt("u1", 100, 0, base)
	if _, _, _, _, ok := r.Net("u1", time.Hour, base); ok {
		t.Fatal("单样本不应有净变化")
	}
	// 两个样本但只隔 1 分钟 < MinSpan。
	r.RecordAt("u1", 90, 0, at(base, time.Minute))
	if _, _, _, _, ok := r.Net("u1", time.Hour, at(base, time.Minute)); ok {
		t.Fatal("覆盖时长 < MinSpan 不应出数字")
	}
}

// 同值节流：余额没变且不足 minInterval 就跳过；余额变了必记。
func TestRecordThrottle(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	r.RecordAt("u1", 100, 0, base)
	r.RecordAt("u1", 100, 0, at(base, 10*time.Second)) // 同值 + 10s → 跳过
	r.RecordAt("u1", 100, 0, at(base, 2*time.Minute))  // 同值但超 minInterval → 记
	r.RecordAt("u1", 90, 0, at(base, 3*time.Minute))   // 变值 → 记
	if got := len(r.series["u1"]); got != 3 {
		t.Fatalf("样本数 %d, want 3", got)
	}
	// 快过期量变化也算「变了」。
	r.RecordAt("u1", 90, 50, at(base, 4*time.Minute))
	if got := len(r.series["u1"]); got != 4 {
		t.Fatalf("快过期量变化应记样本，样本数 %d, want 4", got)
	}
}

// 同一秒内重复采样原地覆盖，不产生重复点。
func TestRecordSameSecondOverwrite(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	r.RecordAt("u1", 100, 0, base)
	r.RecordAt("u1", 77, 0, base.Add(500*time.Millisecond)) // 同一 unix 秒
	if got := len(r.series["u1"]); got != 1 {
		t.Fatalf("样本数 %d, want 1（同秒覆盖）", got)
	}
	if r.series["u1"][0].C != 77 {
		t.Fatalf("同秒覆盖后应保留最新值 77，得到 %d", r.series["u1"][0].C)
	}
}

// 裁剪：丢掉超出保留期的样本，但至少留下最后一个（否则窗口滑出后净变化消失）。
func TestPruneKeepsLast(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	r.RecordAt("u1", 500, 0, base)                       // 40h 前
	r.RecordAt("u1", 400, 0, at(base, 39*time.Hour))     // 1h 前
	r.RecordAt("u1", 390, 0, at(base, 40*time.Hour))     // 现在
	ss := r.series["u1"]
	if len(ss) != 2 {
		t.Fatalf("样本数 %d, want 2（40h 前那条被裁掉）", len(ss))
	}
	if ss[0].C != 400 {
		t.Fatalf("裁剪后首样本 %d, want 400", ss[0].C)
	}
	// 全部样本都超出保留期时仍保留最后一条。
	r2 := New("")
	r2.RecordAt("u2", 10, 0, base)
	r2.RecordAt("u2", 20, 0, at(base, time.Hour))
	r2.RecordAt("u2", 30, 0, at(base, 200*time.Hour))
	if ss2 := r2.series["u2"]; len(ss2) != 1 || ss2[0].C != 30 {
		t.Fatalf("全超期时应只剩最后一条 30，得到 %+v", ss2)
	}
}

// 降采样：点数受限、首尾保留、升序。
func TestSeriesDownsample(t *testing.T) {
	base := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	r := New("")
	for i := 0; i < 100; i++ {
		r.RecordAt("u1", int64(1000-i), 0, at(base, time.Duration(i)*time.Minute))
	}
	now := at(base, 99*time.Minute)
	ss := r.Series("u1", 24*time.Hour, 10, now)
	if len(ss) != 10 {
		t.Fatalf("降采样点数 %d, want 10", len(ss))
	}
	if ss[0].C != 1000 || ss[9].C != 901 {
		t.Fatalf("首尾应保留，得到 %d..%d", ss[0].C, ss[9].C)
	}
	for i := 1; i < len(ss); i++ {
		if ss[i].T < ss[i-1].T {
			t.Fatalf("降采样结果必须升序，第 %d 点乱序", i)
		}
	}
	// 样本数少于上限时原样返回。
	if got := r.Series("u1", 24*time.Hour, 500, now); len(got) != 100 {
		t.Fatalf("未超上限应原样返回 100 点，得到 %d", len(got))
	}
	// 窗口外无样本 → nil。
	if got := r.Series("u1", time.Minute, 10, at(base, 48*time.Hour)); got != nil {
		t.Fatalf("窗口外应为 nil，得到 %d 点", len(got))
	}
}

// 落盘 → 重新加载后净变化仍可算（跨重启/跨部署不失忆）。
//
// 基准时间必须相对 time.Now()：loadFromBytes 会用「当前时间 - 26h」裁剪历史样本，
// 而这里曾写死 2026-10-11 —— 部署一段时间后那个时间点就落到保留窗口外，所有样本在
// 重新加载时被裁光，"重新加载后应有净变化" 必然失败（10-13 起必现）。
// 用 now()-2h 当基准，样本永远落在窗口内，测试也不再随时钟漂移。
func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "data", "credits_history.json")
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	r := New(fp)
	r.RecordAt("u1", 1000, 0, base)
	r.RecordAt("u1", 900, 0, at(base, time.Hour))
	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(fp); err != nil {
		t.Fatalf("落盘文件不存在: %v", err)
	}
	r2 := New(fp)
	if _, _, _, _, ok := r2.Net("u1", 24*time.Hour, at(base, time.Hour)); !ok {
		t.Fatal("重新加载后应有净变化")
	}
	net, _, _, _, _ := r2.Net("u1", 24*time.Hour, at(base, time.Hour))
	if net != -100 {
		t.Fatalf("重新加载 net=%d, want -100", net)
	}
	// 损坏文件按空历史启动，不 panic。
	if err := os.WriteFile(fp, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := len(New(fp).UIDs()); got != 0 {
		t.Fatalf("损坏文件应按空历史启动，得到 %d 个账号", got)
	}
}

// 无脏数据时 Save 不写盘（避免每 2 分钟空转 IO）。
func TestSaveSkipsWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "h.json")
	r := New(fp)
	if err := r.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatal("无脏数据不应落盘")
	}
}
