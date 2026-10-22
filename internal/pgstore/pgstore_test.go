package pgstore

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// dropTestTables 清掉 open() 建的三张表（主表 / _kv / _request_logs）。
// 集成测试跑在用户自己的 PostgreSQL 上，漏一张就是把垃圾留在生产库里 —— 每个
// 用 openWithTable 的测试都必须通过它收尾，不能各写各的 drop 语句。
func dropTestTables(t *testing.T, st *Store, table string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, name := range []string{table + "_request_logs", table + "_kv", table} {
			if _, err := st.pool.Exec(ctx, "drop table if exists "+name); err != nil {
				t.Logf("清理测试表 %s 失败: %v", name, err)
			}
		}
	})
}

// 集成测试：需要一个真实 PostgreSQL。设置 PGSTORE_TEST_DSN 后运行。
// 全程只碰一张独立的临时表，绝不写生产 panel_users。
func TestPanelUsersRoundTrip(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PGSTORE_TEST_DSN，跳过 PostgreSQL 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const table = "panel_users_pgstore_test"
	st, err := openWithTable(ctx, dsn, table)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	dropTestTables(t, st, table)
	if st.Label() == "" {
		t.Fatal("Label 不该为空")
	}

	// 1) 空表：have=false 是「首次启用」的信号，调用方据此做一次性导入。
	users, have, err := st.LoadPanelUsers(ctx)
	if err != nil || have || len(users) != 0 {
		t.Fatalf("空表应返回 (空,false,nil)，得到 (%+v,%v,%v)", users, have, err)
	}

	// 2) 写入（故意乱序）→ 读回应按用户名排序（面板列表展示稳定）。
	zoe := panelauth.User{Username: "zoe", Role: panelauth.RoleAdmin, PasswordHash: "h-z"}
	amy := panelauth.User{Username: "amy", Role: panelauth.RoleUser, PasswordHash: "h-a"}
	if err := st.SavePanelUsers(ctx, []panelauth.User{zoe, amy}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, have, err := st.LoadPanelUsers(ctx)
	if err != nil || !have {
		t.Fatalf("读回失败 have=%v err=%v", have, err)
	}
	if len(got) != 2 || got[0].Username != "amy" || got[1].Username != "zoe" {
		t.Fatalf("读回应按用户名排序: %+v", got)
	}
	if got[1].Role != panelauth.RoleAdmin || got[1].PasswordHash != "h-z" {
		t.Fatalf("字段丢失: %+v", got[1])
	}

	// 3) 改 zoe 的密码 = upsert，不产生重复行，也不碰 amy。
	zoe.PasswordHash = "h-z2"
	if err := st.SavePanelUsers(ctx, []panelauth.User{zoe, amy}); err != nil {
		t.Fatalf("save2: %v", err)
	}
	got, _, err = st.LoadPanelUsers(ctx)
	if err != nil || len(got) != 2 {
		t.Fatalf("upsert 后应仍是 2 行: %+v err=%v", got, err)
	}
	byName := map[string]panelauth.User{}
	for _, u := range got {
		byName[u.Username] = u
	}
	if byName["zoe"].PasswordHash != "h-z2" {
		t.Fatalf("zoe 的哈希应已更新: %+v", byName["zoe"])
	}
	if byName["amy"].PasswordHash != "h-a" {
		t.Fatalf("amy 不该被牵连: %+v", byName["amy"])
	}

	// 4) 少传 zoe = 删除 zoe（面板「删除账号」走同一条路）。
	if err := st.SavePanelUsers(ctx, []panelauth.User{amy}); err != nil {
		t.Fatalf("save3: %v", err)
	}
	got, _, err = st.LoadPanelUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Username != "amy" {
		t.Fatalf("删除后应只剩 amy: %+v", got)
	}

	// 5) 空集合 = 全清（用 any('{}') 的边界，别写成「什么都不做」）。
	if err := st.SavePanelUsers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	got, have, err = st.LoadPanelUsers(ctx)
	if err != nil || have || len(got) != 0 {
		t.Fatalf("清空失败: %+v have=%v err=%v", got, have, err)
	}
}

// 通用 KV（面板其余状态）往返：未命中 → 写 → 命中 → 覆盖 → 删除。
func TestBlobRoundTrip(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PGSTORE_TEST_DSN，跳过 PostgreSQL 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const table = "panel_kv_pgstore_test"
	st, err := openWithTable(ctx, dsn, table)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	dropTestTables(t, st, table)

	// 1) 未命中。
	if _, found, err := st.Load("usage"); err != nil || found {
		t.Fatalf("空表应未命中，得到 found=%v err=%v", found, err)
	}
	// 2) 写入 → 读回。
	if err := st.Save("usage", []byte("v1")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, found, err := st.Load("usage")
	if err != nil || !found || string(got) != "v1" {
		t.Fatalf("读回应为 v1，得到 %q found=%v err=%v", got, found, err)
	}
	// 3) 覆盖写。
	if err := st.Save("usage", []byte("v2")); err != nil {
		t.Fatalf("覆盖: %v", err)
	}
	if got, _, _ = st.Load("usage"); string(got) != "v2" {
		t.Fatalf("覆盖后应为 v2，得到 %q", got)
	}
	// 4) 键之间互不影响。
	if err := st.Save("ownership", []byte("{}")); err != nil {
		t.Fatalf("save2: %v", err)
	}
	if got, _, _ = st.Load("usage"); string(got) != "v2" {
		t.Fatalf("另一键写入不该影响 usage，得到 %q", got)
	}
	// 5) 删除（幂等）。
	if err := st.Delete("usage"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, err := st.Load("usage"); err != nil || found {
		t.Fatalf("删除后应未命中，得到 found=%v err=%v", found, err)
	}
	if err := st.Delete("usage"); err != nil {
		t.Fatalf("重复删除应幂等: %v", err)
	}
}

// 请求记录落库：批量写 / 筛选读 / 统计 / 按保留期清理。
func TestRequestLogSink(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PGSTORE_TEST_DSN，跳过 PostgreSQL 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const table = "panel_reqlog_test"
	st, err := openWithTable(ctx, dsn, table)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	dropTestTables(t, st, table)

	now := time.Now()
	events := []reqlog.Event{
		{Time: now.Add(-2 * time.Minute), RequestID: "a", Path: "/v1/chat", Account: "u1", Model: "alias-x", UpstreamModel: "u1-real", Provider: "raccoon", Outcome: "success", Status: 200, OK: true, TotalTokens: 10},
		{Time: now.Add(-1 * time.Minute), RequestID: "b", Path: "/v1/chat", Account: "u2", Model: "m2", Provider: "runable", Outcome: "http_error", Status: 500, Failure: "upstream 429 rate limited"},
		{Time: now.AddDate(0, 0, -30), RequestID: "c", Path: "/v1/chat", Account: "u3", Model: "m3", Provider: "raccoon", Outcome: "success", Status: 200, OK: true},
	}
	if err := st.AppendLogs(events); err != nil {
		t.Fatalf("append: %v", err)
	}
	rows, _, errMsg := st.LogStats()
	if errMsg != "" || rows != 3 {
		t.Fatalf("stats rows=%d err=%q", rows, errMsg)
	}

	got, err := st.ReadLogs(10, reqlog.Filter{})
	if err != nil || len(got) != 3 {
		t.Fatalf("read all: %v len=%d", err, len(got))
	}
	if !got[0].Time.After(got[1].Time) {
		t.Fatalf("应按时间倒序，得到 %v", []time.Time{got[0].Time, got[1].Time})
	}
	if got, _ = st.ReadLogs(10, reqlog.Filter{Account: "U1"}); len(got) != 1 || got[0].RequestID != "a" {
		t.Fatalf("account 子串匹配（大小写不敏感）失败: %+v", got)
	}
	if got, _ = st.ReadLogs(10, reqlog.Filter{Outcome: "success"}); len(got) != 2 {
		t.Fatalf("outcome 筛选应 2 条，得到 %d", len(got))
	}
	if got, _ = st.ReadLogs(10, reqlog.Filter{Provider: "raccoon"}); len(got) != 2 {
		t.Fatalf("provider 筛选应 2 条，得到 %d", len(got))
	}
	// upstream_model 必须原样往返：它是「别名有没有生效」的唯一记录。
	if all, _ := st.ReadLogs(10, reqlog.Filter{Model: "alias-x"}); len(all) != 1 || all[0].UpstreamModel != "u1-real" {
		t.Fatalf("upstream_model 未正确落库/读回: %+v", all)
	}
	// failure 必须原样往返：它是「谁先把连接断掉」的唯一记录，丢了就只能靠猜。
	if all, _ := st.ReadLogs(10, reqlog.Filter{Model: "m2"}); len(all) != 1 || all[0].Failure != "upstream 429 rate limited" {
		t.Fatalf("failure 未正确落库/读回: %+v", all)
	}
	if got, _ = st.ReadLogs(1, reqlog.Filter{}); len(got) != 1 {
		t.Fatalf("limit=1 应 1 条，得到 %d", len(got))
	}

	// 保留 7 天：30 天前那条应被清掉。
	if err := st.PruneLogs(7, 0); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if rows, _, _ = st.LogStats(); rows != 2 {
		t.Fatalf("prune 后应剩 2 行，得到 %d", rows)
	}
}

// 既有 JSONL 归档一次性导入。
func TestImportRequestLogsFromDir(t *testing.T) {
	dsn := os.Getenv("PGSTORE_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 PGSTORE_TEST_DSN，跳过 PostgreSQL 集成测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const table = "panel_reqlog_import_test"
	st, err := openWithTable(ctx, dsn, table)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	dropTestTables(t, st, table)

	dir := t.TempDir()
	line := `{"time":"2026-01-01T00:00:00Z","request_id":"x","path":"/v1","outcome":"success","status":200,"ok":true}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "requests-2026-01-01.jsonl"), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := st.ImportRequestLogsFromDir(dir)
	if err != nil || n != 1 {
		t.Fatalf("import n=%d err=%v", n, err)
	}
	if rows, _, _ := st.LogStats(); rows != 1 {
		t.Fatalf("导入后应 1 行，得到 %d", rows)
	}
}

func TestOpenValidatesInput(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, ""); err == nil {
		t.Fatal("空 dsn 应报错")
	}
	// 非法表名在连库之前就要拒绝（避免把表名拼进 SQL 里做注入面）。
	for _, bad := range []string{"", "Bad-Name", "a b", "1;drop", `x"y`} {
		if _, err := openWithTable(ctx, "postgres://user@host:5432/db", bad); err == nil {
			t.Fatalf("非法表名 %q 应报错", bad)
		}
	}
}
