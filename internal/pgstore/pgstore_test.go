package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

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
	defer func() {
		_, _ = st.pool.Exec(context.Background(), "drop table if exists "+table)
	}()
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
