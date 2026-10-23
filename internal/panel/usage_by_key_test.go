package panel

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// fakeKeyUsageSink 同时实现 Sink 与 KeyUsageSink，用来在不起 PostgreSQL 的情况下
// 验证「用户」页的数据组装（配置合并、合计、保留期）。
type fakeKeyUsageSink struct {
	rows []reqlog.KeyUsage
}

func (f *fakeKeyUsageSink) AppendLogs([]reqlog.Event) error { return nil }
func (f *fakeKeyUsageSink) ReadLogs(int, reqlog.Filter) ([]reqlog.Event, error) {
	return nil, nil
}
func (f *fakeKeyUsageSink) PruneLogs(int, int64) error { return nil }
func (f *fakeKeyUsageSink) LogStats() (int64, int64, string) {
	return 0, 0, ""
}
func (f *fakeKeyUsageSink) AggregateKeyUsage(from, to time.Time) ([]reqlog.KeyUsage, error) {
	return f.rows, nil
}

// plainSink 只实现 Sink（没有 SQL 聚合能力），对应 JSONL 文件后端。
type plainSink struct{}

func (plainSink) AppendLogs([]reqlog.Event) error { return nil }
func (plainSink) ReadLogs(int, reqlog.Filter) ([]reqlog.Event, error) {
	return nil, nil
}
func (plainSink) PruneLogs(int, int64) error { return nil }
func (plainSink) LogStats() (int64, int64, string) {
	return 0, 0, ""
}

// newUsagePanel 装配一个带请求记录与引擎配置的面板，返回每种角色已登录的会话。
func newUsagePanel(t *testing.T, sink reqlog.Sink) (*Panel, map[panelauth.Role]*http.Cookie) {
	t.Helper()
	rec := reqlog.New(reqlog.Config{Enabled: true, Sink: sink, RetentionDays: 7})
	t.Cleanup(rec.Close)

	users := []panelauth.User{
		pwUser(t, "admin", panelauth.RoleAdmin, "pw123456"),
		pwUser(t, "viewer", panelauth.RoleViewer, "pw123456"),
		pwUser(t, "alice", panelauth.RoleUser, "pw123456"),
	}
	store := panelauth.New(users, time.Hour, 5, 15*time.Minute)
	section := PanelAuthSection{SessionHours: 1, MaxFailures: 5, LockMinutes: 15, Users: users}
	omni := map[string]any{
		"api_keys": []any{
			map[string]any{"key": "sk-a", "name": "生产客户端", "providers": []any{"p"}},
			map[string]any{"key": "sk-b", "name": "从未使用", "providers": []any{"p"}},
			"sk-unnamed",
		},
	}
	p := New(Config{
		Version:        "test",
		RequestLog:     rec,
		LoadOmniConfig: func() (any, error) { return omni, nil },
		PanelAuth:      store,
		LoadPanelAuth:  func() (PanelAuthSection, error) { return section, nil },
		SavePanelAuth: func(next PanelAuthSection) ([]string, error) {
			section = next
			store.Reconfigure(next.Users, time.Hour, 5, 15*time.Minute)
			return nil, nil
		},
	})
	cookies := make(map[panelauth.Role]*http.Cookie, 3)
	for _, u := range users {
		cookies[u.Role] = login(t, p, u.Username, "pw123456")
	}
	return p, cookies
}

func TestUsageByKeyMergesConfiguredAndTotals(t *testing.T) {
	sink := &fakeKeyUsageSink{rows: []reqlog.KeyUsage{
		{
			Key: "生产客户端", Requests: 9, Errors: 2, PromptTokens: 100, CompletionTokens: 50,
			TotalTokens: 150, AvgDurationMs: 1200, LastSeen: time.Now().Add(-time.Minute),
			Models: []reqlog.KeyUsageModel{{Model: "m1", Requests: 9, Errors: 2, TotalTokens: 150}},
		},
		{
			Key: "历史密钥", Requests: 4, TotalTokens: 40,
			LastSeen: time.Now().Add(-48 * time.Hour),
			Models:   []reqlog.KeyUsageModel{{Model: "m2", Requests: 4, TotalTokens: 40}},
		},
	}}
	p, cookies := newUsagePanel(t, sink)

	rec := do(p, http.MethodGet, "/panel/api/usage/by-key", "", cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Rows []reqlog.KeyUsage `json:"rows"`
		Total struct {
			Requests    int64 `json:"requests"`
			Errors      int64 `json:"errors"`
			TotalTokens int64 `json:"total_tokens"`
		} `json:"total"`
		RetentionDays int      `json:"retention_days"`
		Configured    []string `json:"configured"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RetentionDays != 7 {
		t.Errorf("retention_days = %d, want 7", got.RetentionDays)
	}
	// 合计只算观测行：零流量的「从未使用」不能把请求数抬高。
	if got.Total.Requests != 13 || got.Total.Errors != 2 || got.Total.TotalTokens != 190 {
		t.Errorf("total = %+v", got.Total)
	}
	// 已配置但没流量的密钥必须出现在列表末尾，供运营对账。
	byKey := make(map[string]reqlog.KeyUsage, len(got.Rows))
	order := make([]string, 0, len(got.Rows))
	for _, row := range got.Rows {
		byKey[row.Key] = row
		order = append(order, row.Key)
	}
	if _, ok := byKey["从未使用"]; !ok {
		t.Fatalf("缺少零流量密钥行: %v", order)
	}
	if byKey["从未使用"].Requests != 0 || len(byKey["从未使用"].Models) != 0 {
		t.Errorf("零流量行不该有数据: %+v", byKey["从未使用"])
	}
	if len(order) != 3 || order[2] != "从未使用" {
		t.Errorf("零流量行应排在最后: %v", order)
	}
	// 裸串密钥没有标签，不该出现在 configured 里（否则会对不上 unnamed- 摘要）。
	for _, name := range got.Configured {
		if name == "" {
			t.Errorf("configured 混入空标签: %v", got.Configured)
		}
	}
}

func TestUsageByKeyRequiresPrivileged(t *testing.T) {
	p, cookies := newUsagePanel(t, &fakeKeyUsageSink{})

	if rec := do(p, http.MethodGet, "/panel/api/usage/by-key", "", cookies[panelauth.RoleViewer], nil); rec.Code != http.StatusOK {
		t.Errorf("viewer = %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(p, http.MethodGet, "/panel/api/usage/by-key", "", cookies[panelauth.RoleUser], nil); rec.Code != http.StatusForbidden {
		t.Errorf("tenant = %d, want 403", rec.Code)
	}
	if rec := do(p, http.MethodGet, "/panel/api/usage/by-key", "", nil, nil); rec.Code == http.StatusOK {
		t.Errorf("未登录居然拿到 200")
	}
}

// 文件归档后端（没有 KeyUsageSink）必须明确回「不支持」，不能返回空表让
// 运营误以为「这个窗口真的没人用」。
func TestUsageByKeyUnsupportedBackend(t *testing.T) {
	p, cookies := newUsagePanel(t, plainSink{})
	rec := do(p, http.MethodGet, "/panel/api/usage/by-key", "", cookies[panelauth.RoleAdmin], nil)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConfiguredKeyNamesSkipsUnnamed(t *testing.T) {
	p, _ := newUsagePanel(t, &fakeKeyUsageSink{})
	names := p.configuredKeyNames()
	if len(names) != 2 {
		t.Fatalf("names = %v", names)
	}
	seen := map[string]bool{names[0]: true, names[1]: true}
	if !seen["从未使用"] || !seen["生产客户端"] {
		t.Fatalf("names = %v", names)
	}
}
