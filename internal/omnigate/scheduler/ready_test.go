package scheduler

import (
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// newReadyScheduler 造一个带最小网关的调度器。cfg 里供应商的账号会被 gateway.New
// 装载进账号池，而 taskReady 判定的正是这个池子。
func newReadyScheduler(t *testing.T, cfg *config.Config) *Scheduler {
	t.Helper()
	st, err := state.Load(t.TempDir())
	if err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	gw, err := gateway.New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatalf("gateway.New: %v", err)
	}
	return New(cfg, gw, logx.New(10))
}

// 没有账号的供应商不该被排进签到：内置任务（runable/raccoon/trae/newapi/
// geminiweb）的语义都是「维护某个供应商已有账号的会话或额度」，账号为 0 时任务既
// 没有可续的 cookie 也没有可签的账号，跑起来只会在每次到点留下一条必然失败的日志。
// 历史上 4 个空壳供应商各自挂着签到任务，就是这么每天空转的。
func TestTaskReadyRejectsProviderWithoutAccounts(t *testing.T) {
	cfg := &config.Config{
		DefaultProvider: "workbuddy",
		Providers: []config.ProviderConfig{
			// key 型供应商：有 api_key 但没有账号，签到同样无处下手。
			{Name: "workbuddy", Type: "openai", APIKey: "sk-x"},
			// 空壳：账号型，0 账号。
			{Name: "runable", Type: "runable"},
			// 有账号：可执行。
			{Name: "raccoon", Type: "raccoon", Accounts: []config.AccountConfig{{Label: "a1", Cookie: "c"}}},
		},
	}
	s := newReadyScheduler(t, cfg)

	cases := []struct {
		desc    string
		task    config.CheckinTask
		want    bool
		wantSub string
	}{
		{"账号型任务指向空壳供应商 → 挂起",
			config.CheckinTask{Name: "r", Type: "runable", Provider: "runable"}, false, "尚未添加账号"},
		{"账号型任务指向有账号的供应商 → 可执行",
			config.CheckinTask{Name: "rc", Type: "raccoon", Provider: "raccoon"}, true, ""},
		{"指向不存在的供应商 → 挂起",
			config.CheckinTask{Name: "ghost", Type: "trae", Provider: "nope"}, false, "不存在"},
		{"key 型供应商也没有账号可签 → 挂起",
			config.CheckinTask{Name: "wb", Type: "newapi", Provider: "workbuddy"}, false, "尚未添加账号"},
		{"省略 provider 时回落到 default_provider 再判定",
			config.CheckinTask{Name: "d", Type: "runable"}, false, "尚未添加账号"},
		{"通用 HTTP 任务不依赖供应商 → 可执行",
			config.CheckinTask{Name: "h", Type: "http", URL: "https://example.com"}, true, ""},
		{"type 省略同样按通用 HTTP 处理 → 可执行",
			config.CheckinTask{Name: "h2", URL: "https://example.com"}, true, ""},
	}
	for _, c := range cases {
		got, why := s.taskReady(c.task)
		if got != c.want {
			t.Errorf("%s: ready=%v want %v (why=%q)", c.desc, got, c.want, why)
			continue
		}
		if c.wantSub != "" && !strings.Contains(why, c.wantSub) {
			t.Errorf("%s: 挂起原因 %q 应包含 %q", c.desc, why, c.wantSub)
		}
		if got && why != "" {
			t.Errorf("%s: 可执行时原因应为空，得到 %q", c.desc, why)
		}
	}
}

// 既没写 provider、配置里也没有 default_provider：应挂起并说清原因，
// 而不是拿空名字去查池子。
func TestTaskReadyWithoutProviderOrDefault(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "raccoon", Type: "raccoon"}},
	}
	s := newReadyScheduler(t, cfg)
	ok, why := s.taskReady(config.CheckinTask{Type: "raccoon"})
	if ok {
		t.Fatal("无供应商且无默认供应商时应挂起")
	}
	if !strings.Contains(why, "默认供应商") {
		t.Fatalf("挂起原因应点明缺默认供应商，得到 %q", why)
	}
}
