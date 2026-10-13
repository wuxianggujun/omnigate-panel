// reqlog.go 请求记录的 PostgreSQL 后端（reqlog.Sink 实现）。
//
// 定位：把原本按天轮转的 requests-*.jsonl 归档搬进库表 request_logs——
//   - 库为权威：配置了本后端后不再写文件，读写 / 清理 / 统计全走 SQL；
//   - 首次启用一次性导入：main 在表为空时调用 ImportRequestLogsFromDir 把
//     既有 JSONL 灌进来，之后文件可以删除。
//
// 只用 pgx（CopyFrom 批量写、参数化查询），不引 ORM。表结构与索引建表幂等。
package pgstore

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// requestLogsSchemaTemplate 请求记录表。所有列 not null + default，写侧总是显式填值，
// 读侧因此不必处理 NULL。time 上的降序索引服务「最近 N 条 + 时间区间」查询。
// 表名用 %s 注入（生产 request_logs；测试派生独立表，绝不碰生产数据）。
const requestLogsSchemaTemplate = `
create table if not exists %s (
    id                 bigserial        primary key,
    time               timestamptz      not null,
    request_id         text             not null default '',
    path               text             not null default '',
    account            text             not null default '',
    model              text             not null default '',
    realm              text             not null default '',
    status             int              not null default 0,
    ok                 boolean          not null default false,
    outcome            text             not null default '',
    duration_ms        bigint           not null default 0,
    ttfb_ms            bigint           not null default 0,
    attempts           int              not null default 0,
    prompt_tokens      bigint           not null default 0,
    completion_tokens  bigint           not null default 0,
    total_tokens       bigint           not null default 0,
    credit             double precision not null default 0,
    credit_known       boolean          not null default false,
    cache_hit_tokens   bigint           not null default 0,
    cache_miss_tokens  bigint           not null default 0,
    reasoning_effort   text             not null default '',
    reasoning_tokens   bigint           not null default 0,
    reasoning          boolean          not null default false,
    client_ip          text             not null default '',
    user_agent         text             not null default '',
    provider           text             not null default ''
);
create index if not exists %s_time_idx on %s (time desc, id desc);
create index if not exists %s_outcome_idx on %s (outcome);
`

// requestLogColumns 与建表顺序一致，供 CopyFrom 与 SELECT 复用。
var requestLogColumns = []string{
	"time", "request_id", "path", "account", "model", "realm",
	"status", "ok", "outcome", "duration_ms", "ttfb_ms", "attempts",
	"prompt_tokens", "completion_tokens", "total_tokens",
	"credit", "credit_known", "cache_hit_tokens", "cache_miss_tokens",
	"reasoning_effort", "reasoning_tokens", "reasoning",
	"client_ip", "user_agent", "provider",
}

// ensureRequestLogs 建请求记录表（幂等）。由 open 调用。
func ensureRequestLogs(ctx context.Context, s *Store) error {
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(requestLogsSchemaTemplate, s.logTable, s.logTable, s.logTable, s.logTable, s.logTable)); err != nil {
		return fmt.Errorf("pgstore: 建请求记录表失败: %w", err)
	}
	return nil
}

// AppendLogs 批量写入请求记录（pgx CopyFrom，单次往返）。
func (s *Store) AppendLogs(events []reqlog.Event) error {
	if s == nil || s.pool == nil {
		return kvErrUnavailable()
	}
	if len(events) == 0 {
		return nil
	}
	rows := make([][]any, len(events))
	for i, e := range events {
		rows[i] = []any{
			e.Time, e.RequestID, e.Path, e.Account, e.Model, e.Realm,
			e.Status, e.OK, e.Outcome, e.DurationMs, e.TTFBMs, e.Attempts,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens,
			e.Credit, e.HasCredit, e.CacheHitTokens, e.CacheMissTokens,
			e.ReasoningEffort, e.ReasoningTokens, e.Reasoning,
			e.ClientIP, e.UserAgent, e.Provider,
		}
	}
	if _, err := s.pool.CopyFrom(context.Background(),
		pgx.Identifier{s.logTable}, requestLogColumns, pgx.CopyFromRows(rows)); err != nil {
		return fmt.Errorf("pgstore: 写请求记录: %w", err)
	}
	return nil
}

// ReadLogs 返回按时间倒序的最近 limit 条（筛选语义与文件归档一致：字符串字段
// 大小写不敏感子串匹配，From/To 闭区间）。
func (s *Store) ReadLogs(limit int, filter reqlog.Filter) ([]reqlog.Event, error) {
	if s == nil || s.pool == nil {
		return nil, kvErrUnavailable()
	}
	if limit <= 0 {
		limit = 200
	}
	var (
		conds []string
		args  []any
	)
	if filter.Outcome != "" {
		args = append(args, filter.Outcome)
		conds = append(conds, fmt.Sprintf("outcome = $%d", len(args)))
	}
	like := func(col, val string) {
		if val == "" {
			return
		}
		args = append(args, "%"+strings.ToLower(val)+"%")
		conds = append(conds, fmt.Sprintf("lower(%s) like $%d", col, len(args)))
	}
	like("account", filter.Account)
	like("model", filter.Model)
	like("provider", filter.Provider)
	like("client_ip", filter.ClientIP)
	like("user_agent", filter.UserAgent)
	if !filter.From.IsZero() {
		args = append(args, filter.From)
		conds = append(conds, fmt.Sprintf("time >= $%d", len(args)))
	}
	if !filter.To.IsZero() {
		args = append(args, filter.To)
		conds = append(conds, fmt.Sprintf("time <= $%d", len(args)))
	}
	where := ""
	if len(conds) > 0 {
		where = " where " + strings.Join(conds, " and ")
	}
	args = append(args, limit)
	q := fmt.Sprintf("select %s from %s%s order by time desc, id desc limit $%d",
		strings.Join(requestLogColumns, ", "), s.logTable, where, len(args))

	rows, err := s.pool.Query(context.Background(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 读请求记录: %w", err)
	}
	defer rows.Close()
	var out []reqlog.Event
	for rows.Next() {
		var e reqlog.Event
		if err := rows.Scan(
			&e.Time, &e.RequestID, &e.Path, &e.Account, &e.Model, &e.Realm,
			&e.Status, &e.OK, &e.Outcome, &e.DurationMs, &e.TTFBMs, &e.Attempts,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens,
			&e.Credit, &e.HasCredit, &e.CacheHitTokens, &e.CacheMissTokens,
			&e.ReasoningEffort, &e.ReasoningTokens, &e.Reasoning,
			&e.ClientIP, &e.UserAgent, &e.Provider,
		); err != nil {
			return out, fmt.Errorf("pgstore: 解析请求记录: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PruneLogs 按保留天数清理；若清理后总字节仍超 maxBytes，再删掉最老的一半行。
func (s *Store) PruneLogs(retentionDays int, maxBytes int64) error {
	if s == nil || s.pool == nil {
		return kvErrUnavailable()
	}
	if retentionDays <= 0 {
		retentionDays = 7
	}
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx,
		fmt.Sprintf(`delete from %s where time < now() - ($1::int * interval '1 day')`, s.logTable), retentionDays); err != nil {
		return fmt.Errorf("pgstore: 清理请求记录: %w", err)
	}
	if maxBytes > 0 {
		var size int64
		if err := s.pool.QueryRow(ctx, `select pg_total_relation_size($1)`, s.logTable).Scan(&size); err != nil {
			return nil // 大小未知就不做二级清理，别把清理失败升级成错误
		}
		if size > maxBytes {
			if _, err := s.pool.Exec(ctx, fmt.Sprintf(`
				delete from %s where id in (
					select id from %s order by time asc limit greatest((select count(*) from %s) / 2, 1)
				)`, s.logTable, s.logTable, s.logTable)); err != nil {
				return fmt.Errorf("pgstore: 请求记录超限清理: %w", err)
			}
		}
	}
	return nil
}

// LogStats 返回行数 / 表总字节 / 最近一次错误（无错误为空串）。
func (s *Store) LogStats() (int64, int64, string) {
	if s == nil || s.pool == nil {
		return 0, 0, "postgres 不可用"
	}
	ctx := context.Background()
	var rows int64
	if err := s.pool.QueryRow(ctx, fmt.Sprintf(`select count(*) from %s`, s.logTable)).Scan(&rows); err != nil {
		return 0, 0, err.Error()
	}
	var size int64
	if err := s.pool.QueryRow(ctx, `select pg_total_relation_size($1)`, s.logTable).Scan(&size); err != nil {
		return rows, 0, err.Error()
	}
	return rows, size, ""
}

// ImportRequestLogsFromDir 一次性导入既有 JSONL 归档（requests-*.jsonl），返回导入条数。
// 调用方负责判断「表为空」以免重复导入。单个文件解析失败只跳过该行，不中断整体导入。
func (s *Store) ImportRequestLogsFromDir(dir string) (int, error) {
	if s == nil || s.pool == nil {
		return 0, kvErrUnavailable()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)

	total := 0
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return total, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		var batch []reqlog.Event
		for sc.Scan() {
			var e reqlog.Event
			if json.Unmarshal(sc.Bytes(), &e) != nil || e.Time.IsZero() {
				continue
			}
			batch = append(batch, e)
			if len(batch) >= 500 {
				if err := s.AppendLogs(batch); err != nil {
					_ = fh.Close()
					return total, err
				}
				total += len(batch)
				batch = batch[:0]
			}
		}
		if len(batch) > 0 {
			if err := s.AppendLogs(batch); err != nil {
				_ = fh.Close()
				return total, err
			}
			total += len(batch)
		}
		scanErr := sc.Err()
		_ = fh.Close()
		if scanErr != nil {
			return total, scanErr
		}
	}
	return total, nil
}

// 编译期断言：*Store 实现 reqlog.Sink。
var _ reqlog.Sink = (*Store)(nil)
