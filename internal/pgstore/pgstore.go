// Package pgstore 把面板状态从「本地 JSON 文件」搬到用户自己的 PostgreSQL。
//
// 定位与边界（面板是生产入口，数据库抖一下不能让谁都登不进来）：
//   - 数据库是权威存储：能连上就以库里的数据为准，写操作先写库；
//   - 本地配置/文件是镜像 + 兜底：写库成功后同步写一份，库连不上时回落镜像，
//     保证故障期间老用户仍能登录、面板仍可用；
//   - 首次启用一次性迁移：库里还没有数据时从现有配置导入。
//
// 只用 pgx 的连接池，不引 ORM；表结构在本包内维护，建表幂等。
package pgstore

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// schemaTemplate 是面板账号表。密码只存哈希（与 config.json 时代同一套 bcrypt 哈希，
// 迁移不需要重新设密码）。表名用 %s 注入，便于集成测试用一张临时表，不碰生产行。
const schemaTemplate = `
create table if not exists %s (
    username      text        primary key,
    role          text        not null,
    password_hash text        not null,
    updated_at    timestamptz not null default now()
);
`

// defaultTable 生产表名。
const defaultTable = "panel_users"

// Store 是 PostgreSQL 后端。
type Store struct {
	pool  *pgxpool.Pool
	label string
	table string
}

// Open 解析 dsn、建立连接池、Ping 并建表（幂等）。任何一步失败都返回错误，
// 由调用方决定「回落本地文件」还是「直接启动失败」。
func Open(ctx context.Context, dsn string) (*Store, error) {
	return openWithTable(ctx, dsn, defaultTable)
}

// openWithTable 与 Open 相同，但可指定表名（集成测试用临时表，避免写生产数据）。
func openWithTable(ctx context.Context, dsn, table string) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("pgstore: dsn 为空")
	}
	if table == "" {
		return nil, fmt.Errorf("pgstore: 表名为空")
	}
	for _, r := range table {
		if !(r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return nil, fmt.Errorf("pgstore: 非法表名 %q（只允许小写字母/数字/下划线）", table)
		}
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 解析 dsn: %w", err)
	}
	// 面板请求量很小：池子收窄，别把用户的 PostgreSQL 连接数占满
	// （同一个实例上还住着 NewAPI 等业务）。
	cfg.MaxConns = 6
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.MaxConnLifetime = time.Hour

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 建立连接池: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: ping 失败: %w", err)
	}
	if _, err := pool.Exec(ctx, fmt.Sprintf(schemaTemplate, table)); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgstore: 建表失败: %w", err)
	}
	cc := cfg.ConnConfig
	return &Store{
		pool:  pool,
		label: fmt.Sprintf("%s@%s:%d/%s", cc.User, cc.Host, cc.Port, cc.Database),
		table: table,
	}, nil
}

// Close 关闭连接池（Store 为 nil 时安全）。
func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

// Label 返回不含密码的连接描述，供日志 / 面板展示。
func (s *Store) Label() string {
	if s == nil {
		return ""
	}
	return s.label
}

// ---------------------------------------------------------------------------
// 面板账号
// ---------------------------------------------------------------------------

// LoadPanelUsers 读取全部面板账号（按用户名排序）。
// 第二个返回值表示「库里确实有数据」：false = 表为空，调用方应执行首次导入。
func (s *Store) LoadPanelUsers(ctx context.Context) ([]panelauth.User, bool, error) {
	rows, err := s.pool.Query(ctx, fmt.Sprintf(
		`select username, role, password_hash from %s order by username`, s.table))
	if err != nil {
		return nil, false, fmt.Errorf("pgstore: 查询 panel_users: %w", err)
	}
	defer rows.Close()

	var users []panelauth.User
	for rows.Next() {
		var (
			u    panelauth.User
			role string
		)
		if err := rows.Scan(&u.Username, &role, &u.PasswordHash); err != nil {
			return nil, false, fmt.Errorf("pgstore: 解析 panel_users: %w", err)
		}
		u.Role = panelauth.Role(role)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("pgstore: 遍历 panel_users: %w", err)
	}
	return users, len(users) > 0, nil
}

// SavePanelUsers 用给定集合覆盖面板账号（单个事务内 upsert + 删除多余行）。
func (s *Store) SavePanelUsers(ctx context.Context, users []panelauth.User) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("pgstore: 开启事务: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	keep := make([]string, 0, len(users))
	for _, u := range users {
		if u.Username == "" {
			return fmt.Errorf("pgstore: 账号用户名为空")
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`
			insert into %s (username, role, password_hash, updated_at)
			values ($1, $2, $3, now())
			on conflict (username) do update set
				role          = excluded.role,
				password_hash = excluded.password_hash,
				updated_at    = now()`, s.table),
			u.Username, string(u.Role), u.PasswordHash); err != nil {
			return fmt.Errorf("pgstore: 写入账号 %q: %w", u.Username, err)
		}
		keep = append(keep, u.Username)
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`delete from %s where not (username = any($1))`, s.table), keep); err != nil {
		return fmt.Errorf("pgstore: 清理已删除账号: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pgstore: 提交: %w", err)
	}
	return nil
}
