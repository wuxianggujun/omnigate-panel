package scheduler

import (
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
)

func TestJitterOffsetDeterministicAndBounded(t *testing.T) {
	jitter := 30 * time.Minute
	a := jitterOffset("justwoker-checkin", 1000, jitter)
	b := jitterOffset("justwoker-checkin", 1000, jitter)
	if a != b {
		t.Fatalf("同一 (name,last) 应稳定：%v != %v", a, b)
	}
	if a < -jitter || a >= jitter {
		t.Fatalf("抖动应落在 [-jitter, +jitter)：%v", a)
	}
	// 跨周期（last 变化）应给出不同偏移，避免每天固定时刻签到。
	same := 0
	for i := int64(0); i < 50; i++ {
		if jitterOffset("justwoker-checkin", 1000+i, jitter) == a {
			same++
		}
	}
	if same > 5 {
		t.Fatalf("跨周期抖动过于集中：%d/50 与首值相同", same)
	}
}

func TestJitterOffsetZeroDisabled(t *testing.T) {
	if got := jitterOffset("x", 1, 0); got != 0 {
		t.Fatalf("jitter=0 应返回 0，得到 %v", got)
	}
}

func TestFirstRunOffsetBounded(t *testing.T) {
	for _, interval := range []time.Duration{10 * time.Minute, 360 * time.Minute, 24 * time.Hour} {
		off := firstRunOffset("justwoker-checkin", interval)
		cap := interval / 4
		if cap > 30*time.Minute {
			cap = 30 * time.Minute
		}
		if off < 0 || off >= cap {
			t.Fatalf("首次错峰 %v 超出 [0, %v)", off, cap)
		}
	}
	// 同名稳定（同一进程生命周期内不跳）。
	if firstRunOffset("a", time.Hour) != firstRunOffset("a", time.Hour) {
		t.Fatal("首次错峰应对同一任务名稳定")
	}
}

func TestCheckinTaskJitterResolution(t *testing.T) {
	zero := 0
	forty := 40
	ten := 10

	// 任务级覆盖全局。
	task := config.CheckinTask{JitterMinutes: &ten}
	if got := task.Jitter(&forty); got != 10*time.Minute {
		t.Fatalf("任务级应覆盖全局，得到 %v", got)
	}
	// 任务级显式 0 = 关闭（不继承全局）。
	task = config.CheckinTask{JitterMinutes: &zero}
	if got := task.Jitter(&forty); got != 0 {
		t.Fatalf("任务级 0 应关闭抖动，得到 %v", got)
	}
	// 未设置任务级 → 用全局。
	task = config.CheckinTask{}
	if got := task.Jitter(&forty); got != 40*time.Minute {
		t.Fatalf("应继承全局，得到 %v", got)
	}
	// 全局显式 0 = 关闭。
	if got := (config.CheckinTask{}).Jitter(&zero); got != 0 {
		t.Fatalf("全局 0 应关闭抖动，得到 %v", got)
	}
	// 都没设置 → 默认 ±30m。
	if got := (config.CheckinTask{}).Jitter(nil); got != config.DefaultJitter {
		t.Fatalf("缺省应为 %v，得到 %v", config.DefaultJitter, got)
	}
}
