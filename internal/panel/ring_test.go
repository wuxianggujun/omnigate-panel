package panel

import (
	"strings"
	"testing"
	"time"
)

func TestClassifyLine(t *testing.T) {
	cases := map[string]string{
		"| #001 | glm-5.2 | stream | 200 | uid=c8a3e793 | TTFB=120ms |": ChChat,
		"school c8a3e793: ★ 分享任务完成":                                     ChTask,
		"streak-bonus 5c162cc9: 🎊 新手礼包 +100c":                           ChTask,
		"blackcat c8a3e793: 完成 3 次夜间对话":                                 ChTask,
		"checkin 5c162cc9: 已签到":                                         ChTask,
		"panel: 任务动作 uid=x code=chat_5":                                 ChTask,
		"panel: 队列启动：6 项（并发 2）":                                         ChTask,
		"panel: revive uid=x":                                           ChSys,
		"omnigate-panel listening on :7863":                              ChSys,
		"scheduler: 余额后台刷新每 5m0s":                                       ChSys,
	}
	for line, want := range cases {
		if got := classifyLine(line); got != want {
			t.Errorf("classifyLine(%q)=%q want %q", line, got, want)
		}
	}
}

func TestRingWriteStripsTimestamp(t *testing.T) {
	r := NewRing(4)
	if _, err := r.Write([]byte("2026/09/14 00:12:34 school x: done\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("| #002 | glm | stream | 200 | ok |")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	if len(es) != 2 {
		t.Fatalf("entries=%d want 2", len(es))
	}
	if strings.HasPrefix(es[0].Text, "2026/") {
		t.Errorf("timestamp not stripped: %q", es[0].Text)
	}
	if es[0].Ch != ChTask || es[1].Ch != ChChat {
		t.Errorf("channels: %q %q", es[0].Ch, es[1].Ch)
	}
	if time.Since(es[0].TS) > 5*time.Second {
		t.Errorf("stale ts: %v", es[0].TS)
	}
}

// 对话突发不得把任务 / 系统日志挤出缓冲（chat 子上限）。
//
// 为什么需要：chat 每请求一行，量级远大于签到 / 保活。共用一个 FIFO 时一次突发
// 就能让「运行日志」页再也看不到签到记录。这里钉住 chat 单独受 chatCap 约束、
// 任务条目在洪水后仍然保留。
func TestRingChatFloodKeepsTask(t *testing.T) {
	r := NewRing(10) // chatCap = 5
	for i := 0; i < 100; i++ {
		if _, err := r.Write([]byte("| #001 | glm | stream | 200 | ok |\n")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Write([]byte("checkin uid=x: 已签到\n")); err != nil {
		t.Fatal(err)
	}
	es := r.Snapshot()
	chat, task := 0, 0
	for _, e := range es {
		switch e.Ch {
		case ChChat:
			chat++
		case ChTask:
			task++
		}
	}
	if task != 1 {
		t.Fatalf("任务日志被对话洪水挤出：task=%d chat=%d", task, chat)
	}
	if chat > 5 {
		t.Fatalf("chat 未受子上限约束：chat=%d want<=5", chat)
	}
	if len(es) > 10 {
		t.Fatalf("超出总容量：%d", len(es))
	}
	// 任务条目应保留在缓冲内且是最后一条（时间最新）。
	if last := es[len(es)-1]; last.Ch != ChTask {
		t.Fatalf("最后一条应为 task，得到 %q", last.Ch)
	}
}
