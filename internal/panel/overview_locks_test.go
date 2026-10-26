package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// keyHealthStub 构造一个 key-health 回调：providers 为上报名单。
func keyHealthStub(providers ...map[string]any) func() (any, error) {
	return func() (any, error) {
		return map[string]any{"providers": providers}, nil
	}
}

func overviewBody(t *testing.T, p *Panel, cookie *http.Cookie) map[string]any {
	t.Helper()
	rec := do(p, http.MethodGet, "/panel/api/overview", "", cookie, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("overview = %d body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("overview 响应非法 JSON: %v", err)
	}
	return body
}

// TestOverviewReportsLockedUpstreamKeys 钉住「模型锁与冷却」必须说真话。
//
// 之前总览从不返回 model_locks，卡片永远渲染「没有模型锁」；而线上 atria 两把
// 密钥正因额度耗尽被自动禁用。这属于典型的假阴性：越需要它报警越说没事。
func TestOverviewReportsLockedUpstreamKeys(t *testing.T) {
	future := time.Now().Add(20 * time.Hour).Unix()
	p, _ := mustPanel(t, Config{
		OmniKeyHealth: keyHealthStub(
			map[string]any{"name": "atria", "keys": []map[string]any{
				{"key": "***a", "disabled": true, "reason": "quota", "until": future},
				{"key": "***b", "disabled": true, "reason": "quota", "until": future},
			}},
			map[string]any{"name": "yijia", "keys": []map[string]any{
				{"key": "***c", "disabled": false},
			}},
		),
	})
	body := overviewBody(t, p, login(t, p, "admin", "pw123456"))
	locks, ok := body["model_locks"].([]any)
	if !ok {
		t.Fatalf("model_locks 缺失或类型错误: %#v", body["model_locks"])
	}
	if len(locks) != 1 {
		t.Fatalf("model_locks = %#v，期望只含 atria 一条（健康密钥不该出现）", locks)
	}
	row := locks[0].(map[string]any)
	if row["provider"] != "atria" || row["keys"] != float64(2) || row["kind"] != "quota" {
		t.Fatalf("model_locks[0] = %#v，期望 atria/quota/2", row)
	}
}

// TestOverviewLocksSkipExpiredDisable 已过禁用截止的密钥不该继续挂在卡片上：
// 网关下次使用时会自动复活，卡片仍报警只会让人以为没恢复。
func TestOverviewLocksSkipExpiredDisable(t *testing.T) {
	past := time.Now().Add(-time.Hour).Unix()
	p, _ := mustPanel(t, Config{
		OmniKeyHealth: keyHealthStub(map[string]any{"name": "atria", "keys": []map[string]any{
			{"key": "***a", "disabled": true, "reason": "quota", "until": past},
		}}),
	})
	body := overviewBody(t, p, login(t, p, "admin", "pw123456"))
	if locks, _ := body["model_locks"].([]any); len(locks) != 0 {
		t.Fatalf("已过期禁用仍上报: %#v", locks)
	}
}

// TestOverviewLocksHiddenFromUnprivileged 「读不到」与「没有锁」必须能区分：
// 匿名与普通用户的响应里根本不该有 model_locks 字段，前端据此显示不可见，
// 而不是把缺字段当成「当前没有锁定」。
func TestOverviewLocksHiddenFromUnprivileged(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	p, _ := mustPanel(t, Config{
		OmniKeyHealth: keyHealthStub(map[string]any{"name": "atria", "keys": []map[string]any{
			{"key": "***a", "disabled": true, "reason": "quota", "until": future},
		}}),
	})
	p.cfg.PanelAuth = panelauth.New([]panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "bob", panelauth.RoleUser, "pw123456"),
	}, time.Hour, 5, 15*time.Minute)

	if body := overviewBody(t, p, nil); body["model_locks"] != nil {
		t.Errorf("匿名响应不该带 model_locks: %#v", body["model_locks"])
	}
	if body := overviewBody(t, p, login(t, p, "bob", "pw123456")); body["model_locks"] != nil {
		t.Errorf("普通用户响应不该带 model_locks: %#v", body["model_locks"])
	}
	// viewer 与 admin 同权限：能看平台级状态。
	if body := overviewBody(t, p, login(t, p, "admin", "pw123456")); body["model_locks"] == nil {
		t.Error("admin 响应缺少 model_locks")
	}
}

// TestOverviewLocksAbsentWhenKeyHealthUnreadable 回调未装配或报错时不下发该字段：
// 「读不到」不能序列化成空数组，否则前端无法区分「没有锁」。
func TestOverviewLocksAbsentWhenKeyHealthUnreadable(t *testing.T) {
	p, _ := mustPanel(t, Config{})
	body := overviewBody(t, p, login(t, p, "admin", "pw123456"))
	if _, ok := body["model_locks"]; ok {
		t.Errorf("未装配 key-health 时不该有 model_locks: %#v", body["model_locks"])
	}

	p2, _ := mustPanel(t, Config{
		OmniKeyHealth: func() (any, error) { return nil, errors.New("key health unavailable") },
	})
	if body := overviewBody(t, p2, login(t, p2, "admin", "pw123456")); body["model_locks"] != nil {
		t.Errorf("key-health 报错时不该下发 model_locks: %#v", body["model_locks"])
	}
}
