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
	"time"

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
    api_key            text             not null default '',
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
    provider           text             not null default '',
    upstream_model     text             not null default '',
    failure            text             not null default ''
);
create index if not exists %s_time_idx on %s (time desc, id desc);
create index if not exists %s_outcome_idx on %s (outcome);
-- 按入站密钥聚合（面板「用户」页）：api_key + time 复合索引让「某用户的最近请求」
-- 和「区间内按 key 分组」都走索引，不必扫全表。请求记录表是热表，这个索引是
-- 面板唯一会做聚合查询的维度，值得单独建。
create index if not exists %s_key_idx on %s (api_key, time desc);
alter table %s add column if not exists upstream_model text not null default '';
alter table %s add column if not exists failure text not null default '';
alter table %s add column if not exists api_key text not null default '';
`

// requestLogColumns 与建表顺序一致，供 CopyFrom 与 SELECT 复用。
var requestLogColumns = []string{
	"time", "request_id", "path", "account", "api_key", "model", "realm",
	"status", "ok", "outcome", "duration_ms", "ttfb_ms", "attempts",
	"prompt_tokens", "completion_tokens", "total_tokens",
	"credit", "credit_known", "cache_hit_tokens", "cache_miss_tokens",
	"reasoning_effort", "reasoning_tokens", "reasoning",
	"client_ip", "user_agent", "provider", "upstream_model", "failure",
}

// ensureRequestLogs 建请求记录表（幂等）。由 open 调用。
//
// 建表语句里的 alter table 负责给**已存在的旧表**补列（线上表在加列前就已建好，
// create table if not exists 不会动它）。同一个 %s 参数被复用的次数必须与占位符
// 数量一致，否则 fmt.Sprintf 会多出 %!s(MISSING)。
func ensureRequestLogs(ctx context.Context, s *Store) error {
	if _, err := s.pool.Exec(ctx, fmt.Sprintf(requestLogsSchemaTemplate,
		s.logTable, s.logTable, s.logTable, s.logTable, s.logTable, s.logTable, s.logTable,
		s.logTable, s.logTable, s.logTable)); err != nil {
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
			e.Time, e.RequestID, e.Path, e.Account, e.APIKey, e.Model, e.Realm,
			e.Status, e.OK, e.Outcome, e.DurationMs, e.TTFBMs, e.Attempts,
			e.PromptTokens, e.CompletionTokens, e.TotalTokens,
			e.Credit, e.HasCredit, e.CacheHitTokens, e.CacheMissTokens,
			e.ReasoningEffort, e.ReasoningTokens, e.Reasoning,
			e.ClientIP, e.UserAgent, e.Provider, e.UpstreamModel, e.Failure,
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
	like("api_key", filter.APIKey)
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
			&e.Time, &e.RequestID, &e.Path, &e.Account, &e.APIKey, &e.Model, &e.Realm,
			&e.Status, &e.OK, &e.Outcome, &e.DurationMs, &e.TTFBMs, &e.Attempts,
			&e.PromptTokens, &e.CompletionTokens, &e.TotalTokens,
			&e.Credit, &e.HasCredit, &e.CacheHitTokens, &e.CacheMissTokens,
			&e.ReasoningEffort, &e.ReasoningTokens, &e.Reasoning,
			&e.ClientIP, &e.UserAgent, &e.Provider, &e.UpstreamModel, &e.Failure,
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

// AggregateKeyUsage 按入站密钥聚合 [from, to] 区间内的请求。
//
// 两条查询拼一份结果：主查询出每把密钥的汇总，第二条出逐模型拆分。不写成
// 一条 join / json_agg 是为了让主查询永远只返回「用户数」那么多行——面板一次
// 渲染几十行，拆分表再按 key 挂上去，读起来和调试都简单。
//
// 口径说明：
//   - 只统计 api_key <> ''：开放模式与旧归档没有身份，混进来会变成一行巨大
//     的「空」，把真正能对账的数据压掉；
//   - errors 取 not ok（与请求记录页同一口径），客户端中断也算失败；
//   - avg(duration_ms) 含失败请求：用户体感就是「我这次等了多久」，不该只统计
//     成功的那些。
func (s *Store) AggregateKeyUsage(from, to time.Time) ([]reqlog.KeyUsage, error) {
	if s == nil || s.pool == nil {
		return nil, kvErrUnavailable()
	}
	ctx := context.Background()
	args := []any{}
	conds := []string{"api_key <> ''"}
	if !from.IsZero() {
		args = append(args, from)
		conds = append(conds, fmt.Sprintf("time >= $%d", len(args)))
	}
	if !to.IsZero() {
		args = append(args, to)
		conds = append(conds, fmt.Sprintf("time <= $%d", len(args)))
	}
	where := " where " + strings.Join(conds, " and ")

	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		select api_key,
		       count(*),
		       count(*) filter (where not ok),
		       coalesce(sum(prompt_tokens), 0),
		       coalesce(sum(completion_tokens), 0),
		       coalesce(sum(total_tokens), 0),
		       coalesce(avg(duration_ms), 0)::bigint,
		       max(time)
		from %s%s
		group by api_key
		order by count(*) desc, api_key asc`, s.logTable, where), args...)
	if err != nil {
		return nil, fmt.Errorf("pgstore: 按密钥聚合请求记录: %w", err)
	}
	defer rows.Close()

	var out []reqlog.KeyUsage
	index := make(map[string]int)
	for rows.Next() {
		var u reqlog.KeyUsage
		if err := rows.Scan(&u.Key, &u.Requests, &u.Errors, &u.PromptTokens,
			&u.CompletionTokens, &u.TotalTokens, &u.AvgDurationMs, &u.LastSeen); err != nil {
			return out, fmt.Errorf("pgstore: 解析密钥聚合: %w", err)
		}
		index[u.Key] = len(out)
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// 逐模型拆分。区间条件与主查询完全一致，避免两半数据对不上。
	rows2, err := s.pool.Query(ctx, fmt.Sprintf(`
		select api_key, model,
		       count(*),
		       count(*) filter (where not ok),
		       coalesce(sum(prompt_tokens), 0),
		       coalesce(sum(completion_tokens), 0),
		       coalesce(sum(total_tokens), 0)
		from %s%s
		group by api_key, model
		order by count(*) desc`, s.logTable, where), args...)
	if err != nil {
		return out, fmt.Errorf("pgstore: 按密钥聚合模型拆分: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		var (
			key string
			m   reqlog.KeyUsageModel
		)
		if err := rows2.Scan(&key, &m.Model, &m.Requests, &m.Errors,
			&m.PromptTokens, &m.CompletionTokens, &m.TotalTokens); err != nil {
			return out, fmt.Errorf("pgstore: 解析密钥模型拆分: %w", err)
		}
		if i, ok := index[key]; ok {
			out[i].Models = append(out[i].Models, m)
		}
	}
	return out, rows2.Err()
}

// 编译期断言：*Store 实现按密钥聚合（面板「用户」页依赖的能力）。
var _ reqlog.KeyUsageSink = (*Store)(nil)
