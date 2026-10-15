package config

import (
	"strings"
	"testing"
)

// 签到任务的 provider 必须指向真实存在的供应商：写错名字 = 一条永远空转的任务。
// 与 api_keys / routing.prefer 同一口径，在 Normalize 阶段就拒绝，而不是等它每次
// 到点时静默失败。
//
// 注意区分：供应商**存在但账号为 0** 属于运行时状态（账号随时可能补上），这里不
// 拒绝——那种情况由调度器的执行条件守卫挂起，补齐后自动恢复。
func TestNormalizeCheckinTaskProvider(t *testing.T) {
	build := func(tasks ...CheckinTask) *Config {
		return &Config{
			Providers: []ProviderConfig{{Name: "raccoon", Type: "raccoon"}},
			Checkin:   CheckinConfig{Enabled: true, Tasks: tasks},
		}
	}

	// 指向已存在的供应商：放行（哪怕它当前 0 账号）。
	if err := build(CheckinTask{Name: "ok", Type: "raccoon", Provider: "raccoon"}).Normalize(); err != nil {
		t.Fatalf("指向已存在的供应商不该被拒绝: %v", err)
	}
	// 省略 provider：落 default_provider，不做这项校验。
	if err := build(CheckinTask{Name: "legacy", Type: "http", URL: "https://example.com"}).Normalize(); err != nil {
		t.Fatalf("通用 HTTP 任务省略 provider 不该被拒绝: %v", err)
	}
	// 指向不存在的供应商：拒绝，且报错要能一眼定位到是哪条任务。
	err := build(CheckinTask{Name: "ghost", Type: "runable", Provider: "runable"}).Normalize()
	if err == nil {
		t.Fatal("指向不存在的供应商应被拒绝")
	}
	if !strings.Contains(err.Error(), "checkin.tasks[ghost]") {
		t.Fatalf("报错应带任务标签，得到 %v", err)
	}
	// 未命名任务用 type:provider 作标签。
	err = build(CheckinTask{Type: "trae", Provider: "trae"}).Normalize()
	if err == nil || !strings.Contains(err.Error(), "checkin.tasks[trae:trae]") {
		t.Fatalf("未命名任务应以 type:provider 作标签，得到 %v", err)
	}
}
