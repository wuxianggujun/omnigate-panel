// Package reqlog 记录脱敏的请求级指标与可选 JSONL 归档。
//
// 内存指标有界保存最近 100 条并维护进程级计数；磁盘归档只写请求元数据，
// 不写提示词、响应正文、Authorization 或其它凭证。可选的调用来源（客户端 IP /
// User-Agent，见 Event.ClientIP/UserAgent）由 server 按配置开关决定是否填充。
// 归档队列满时丢弃并计数，不允许日志写盘阻塞模型请求。
package reqlog

import (
	"bufio"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	recentLimit     = 100
	defaultFileMax  = int64(16 << 20)
	defaultQueue    = 1024
	defaultReadMax  = 1000
	archiveFileGlob = "requests-*.jsonl"
)

// NewRequestID 生成不含用户信息的本地请求 ID。
func NewRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("req-%x", b[:])
	}
	return fmt.Sprintf("req-%d", time.Now().UnixNano())
}

const (
	OutcomeSuccess     = "success"
	OutcomeHTTPError   = "http_error"
	OutcomeStreamError = "stream_error"
	OutcomeInterrupted = "interrupted"
)

// Config 归档参数。Enabled=false 时仍保留内存指标。
type Config struct {
	Dir           string
	Enabled       bool
	RetentionDays int
	MaxBytes      int64
	FileMaxBytes  int64
	QueueSize     int
	// Sink 可选的持久化后端（如 PostgreSQL）。配置后归档不再写 JSONL 文件，
	// 读写 / 清理 / 统计全部经它；nil 时行为与从前完全一致（写本地 JSONL）。
	Sink Sink
}

// Sink 是请求记录的可选持久化后端。实现方须并发安全，且对 append/read/prune 幂等。
// 约定：AppendLogs 批量写；ReadLogs 返回按时间倒序的最近 limit 条（limit 已由调用方
// 收敛到 [1,1000]）；PruneLogs 按保留天数与总字节上限清理；LogStats 返回行数 / 字节
// 数与最近一次错误（无错误为空串）。
type Sink interface {
	AppendLogs(events []Event) error
	ReadLogs(limit int, filter Filter) ([]Event, error)
	PruneLogs(retentionDays int, maxBytes int64) error
	LogStats() (rows int64, bytes int64, errMsg string)
}

// Event 是一条脱敏请求记录。Account 只保存“昵称(uid8)”标签，不保存完整 UID。
//
// ClientIP / UserAgent 是**调用来源**：面板「运行日志」用它回答"这条请求是谁打进来的"。
// 二者由 server 侧按 logging.request_client_info 开关决定是否填充（关掉即保持空串，
// 归档里不会出现来源字段）——来源信息比 token 计数敏感，运营可自行决定是否落盘。
// 仍然不写提示词、响应正文、Authorization 或其它凭证。
type Event struct {
	Time             time.Time `json:"time"`
	RequestID        string    `json:"request_id"`
	Path             string    `json:"path"`
	Account          string    `json:"account,omitempty"`
	// APIKey 是**入站密钥的展示标签**（`api_keys[].name`），用来回答「这条请求是
	// 哪个用户/哪把密钥打进来的」。
	//
	// 只存标签，绝不存密钥本身：请求记录要落库、要进备份、要给非管理员看，
	// 写入 sk- 明文等于把凭证散落到每一份备份里。没写 name 的密钥退化成
	// 不可逆短摘要（见 server.keyLabel），不同密钥不会合并成同一行。
	// 空 = 开放模式（网关未配任何密钥）或旧归档条目。
	APIKey string `json:"api_key,omitempty"`
	Model  string `json:"model,omitempty"`
	Realm            string    `json:"realm,omitempty"` // 实际调度域 cn/global：由**实际服务账号**决定，非请求里的 "[realm:]" 前缀
	Status           int       `json:"status"`
	OK               bool      `json:"ok"`
	Outcome          string    `json:"outcome"`
	DurationMs       int64     `json:"duration_ms"`
	TTFBMs           int64     `json:"ttfb_ms,omitempty"`
	Attempts         int       `json:"attempts,omitempty"`
	PromptTokens     int64     `json:"prompt_tokens,omitempty"`
	CompletionTokens int64     `json:"completion_tokens,omitempty"`
	TotalTokens      int64     `json:"total_tokens,omitempty"`
	// Credit 本次实际消耗积分（仅 WorkBuddy：上游 usage.credit）。**不能 omitempty**：
	// 免费/零倍率模型回报的 0 是真实值，键被省略后前端 Number(undefined)=NaN，会把它
	// 误判成「未知」显示成 —（实测过：deepseek-v4.1-flash 全被显示成空）。
	// 「有没有值」由 HasCredit(credit_known) 表达，两者配对使用。
	Credit    float64 `json:"credit"`
	HasCredit bool    `json:"credit_known"`
	// CacheHitTokens / CacheMissTokens 上游前缀缓存命中/未命中 token（issue #92）。
	// 上游未回该维度时两者皆零值省略；hit=0 + miss>0 即整段未命中。
	CacheHitTokens  int64 `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64 `json:"cache_miss_tokens,omitempty"`
	// ReasoningEffort 本次出站请求体实际生效的思考档位（网关注入 + 降级后的
	// reasoning_effort，如 high/medium/low；显式关闭为 "off"；空 = 未开思考/
	// 模型不支持/旧归档条目）。回答「这次到底按什么思考程度跑的」。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// ReasoningTokens 上游返回的思考 token 数
	// （completion_tokens_details.reasoning_tokens）；上游未回该维度时零值省略。
	// 与 ReasoningEffort 互补：档位是「按什么程度跑」，这里是「实际想了多少」。
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// Reasoning 上游本次是否产出过思考内容（收到过 reasoning 增量）。用于补齐
	// ReasoningTokens 的空档：runable 等自研协议会发 reasoning-delta，却不在流里
	// 回报 usage，于是「有没有想」可观测、「想了多少」不可观测。零值省略。
	Reasoning bool   `json:"reasoning,omitempty"`
	ClientIP  string `json:"client_ip,omitempty"`
	UserAgent string `json:"user_agent,omitempty"`
	// Provider 实际服务的上游供应商名（OmniGate 的 raccoon/runable/deepseek…；
	// WorkBuddy 网关为空，其"上游"由 Realm 表达）。面板「域/供应商」列在无 realm
	// 时用它显示请求落到了哪个供应商——让 OmniGate 请求也能和 WorkBuddy 同表可见。
	Provider string `json:"provider,omitempty"`
	// UpstreamModel 是**真正发给上游**的模型 ID（别名解析 + 目录补全之后）。
	//
	// Model 记录的是客户端请求的名字，别名命中后两者不同：Model 仍是别名，
	// UpstreamModel 才是它最后落到哪个上游模型。没有这一列时，「别名有没有生效」
	// 在请求记录里完全不可见——别名被上游预改写成带前缀的 ID 后，两条路径的
	// 日志一模一样（实测踩过，排查成本极高）。空 = 旧归档条目或未经过别名的
	// WorkBuddy 请求；等于 Model 时表示没有发生别名/目录改写。
	UpstreamModel string `json:"upstream_model,omitempty"`
	// Failure 是失败/中断的结构化原因（由 gateway 的 markStreamFailure 填写）。
	// 只记 outcome 不记原因，会把「客户端主动取消」和「中间层按读超时掐断」记成
	// 同一件事——线上 96 次 interrupted 全是 200/ttfb≈5ms/token 全 0，却分不清
	// 责任方。空 = 成功或旧归档条目。
	Failure string `json:"failure,omitempty"`
}

// Filter 用于从归档中筛选最近记录。字符串字段一律「包含」匹配（大小写不敏感），
// 便于面板用一段 IP 前缀或 UA 片段捞请求；From/To 是闭区间（零值 = 该侧不设界），
// 供「今天 / 近 7 天 / 自定义区间」这类时间查询使用。
type Filter struct {
	Outcome   string
	Account   string
	// APIKey 按入站密钥标签筛选（大小写不敏感子串）。面板「请求」页据此看单个
	// 用户打过什么，与「用户」页的聚合口径一致。
	APIKey    string
	Model     string
	Provider  string
	ClientIP  string
	UserAgent string
	From      time.Time
	To        time.Time
}

// ArchiveStats 归档存储状态。
type ArchiveStats struct {
	Enabled bool   `json:"enabled"`
	Dir     string `json:"dir,omitempty"`
	// Backend 非空表示归档落在这个持久化后端（如 "postgres"），前端据此把「JSONL 归档」
	// 文案换成「数据库归档」。
	Backend       string `json:"backend,omitempty"`
	Files         int    `json:"files"`
	Bytes         int64  `json:"bytes"`
	// RetentionDays 是归档保留天数。面板「用户」页要把它显示在时间范围旁边：
	// 按密钥统计只能覆盖归档窗口，选「近 30 天」但归档只留 7 天时必须说清楚，
	// 否则会把「查不到」误读成「没用过」。
	RetentionDays int  `json:"retention_days,omitempty"`
	DroppedWrites uint64 `json:"dropped_writes"`
	LastError     string `json:"last_error,omitempty"`
}

// Snapshot 一次面板读取的完整指标快照。
type Snapshot struct {
	StartedAt       time.Time    `json:"started_at"`
	Completed       int64        `json:"completed"`
	InFlight        int64        `json:"in_flight"`
	Succeeded       int64        `json:"succeeded"`
	Failed          int64        `json:"failed"`
	SuccessRate     float64      `json:"success_rate"`
	HTTPSuccessRate float64      `json:"http_success_rate"`
	AvgDurationMs   float64      `json:"avg_duration_ms"`
	Recent          []Event      `json:"recent"`
	Archive         ArchiveStats `json:"archive"`
}

// Recorder 并发安全的有界请求指标与归档记录器。
type Recorder struct {
	mu          sync.Mutex
	started     time.Time
	inFlight    int64
	completed   int64
	succeeded   int64
	httpSuccess int64
	durationSum int64
	recent      []Event
	archive     *archiveWriter
}

// New 创建记录器；Dir 为空或 Enabled=false 时只启用内存指标。
func New(cfg Config) *Recorder {
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	r := &Recorder{started: time.Now(), recent: make([]Event, 0, recentLimit)}
	r.archive = newArchiveWriter(cfg)
	return r
}

// Begin 标记一个请求进入处理。
func (r *Recorder) Begin() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.inFlight++
	r.mu.Unlock()
}

// Record 记录一个请求完成事件并写入归档队列。
func (r *Recorder) Record(e Event) {
	if r == nil {
		return
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	if e.Outcome == "" {
		if e.Status == 200 && e.OK {
			e.Outcome = OutcomeSuccess
		} else {
			e.Outcome = OutcomeHTTPError
		}
	}
	r.mu.Lock()
	if r.inFlight > 0 {
		r.inFlight--
	}
	r.completed++
	if e.OK {
		r.succeeded++
	}
	if e.Status >= 200 && e.Status < 300 {
		r.httpSuccess++
	}
	r.durationSum += e.DurationMs
	r.recent = append([]Event{e}, r.recent...)
	if len(r.recent) > recentLimit {
		r.recent = r.recent[:recentLimit]
	}
	r.mu.Unlock()
	if r.archive != nil {
		r.archive.enqueue(e)
	}
}

// Snapshot 返回进程内指标和归档状态。
func (r *Recorder) Snapshot() Snapshot {
	if r == nil {
		return Snapshot{}
	}
	r.mu.Lock()
	s := Snapshot{
		StartedAt: r.started,
		Completed: r.completed,
		InFlight:  r.inFlight,
		Succeeded: r.succeeded,
		Recent:    append([]Event(nil), r.recent...),
	}
	if r.completed > 0 {
		s.Failed = r.completed - r.succeeded
		s.SuccessRate = round1(float64(r.succeeded) / float64(r.completed) * 100)
		s.HTTPSuccessRate = round1(float64(r.httpSuccess) / float64(r.completed) * 100)
		s.AvgDurationMs = float64(r.durationSum) / float64(r.completed)
	}
	r.mu.Unlock()
	if r.archive != nil {
		s.Archive = r.archive.stats()
	}
	return s
}

// ReadArchive 返回最近的归档事件（按时间倒序）。limit<=0 时回落 200，最大 1000。
func (r *Recorder) ReadArchive(limit int, filter Filter) ([]Event, error) {
	if r == nil || r.archive == nil {
		return nil, nil
	}
	return r.archive.read(limit, filter)
}

// RetentionDays 返回归档保留天数；0 表示未启用归档。
//
// 单独一个方法而不是让调用方读 Snapshot().Archive：Snapshot 会顺带统计归档
// 行数（PostgreSQL 上是 count(*)），而面板只想在页面上写一句「最多回溯 N 天」，
// 不值得为此扫一遍热表。
func (r *Recorder) RetentionDays() int {
	if r == nil || r.archive == nil || !r.archive.enabled() {
		return 0
	}
	return r.archive.cfg.RetentionDays
}

// Close 刷盘并停止后台归档。
func (r *Recorder) Close() {
	if r == nil || r.archive == nil {
		return
	}
	r.archive.close()
}

func round1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}

type archiveWriter struct {
	cfg       Config
	ch        chan Event
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	dropped   atomic.Uint64
	lastErrMu sync.Mutex
	lastErr   string

	file *os.File
	buf  *bufio.Writer
	path string
	day  string
	size int64

	// sink 非空时全部记录经它落库（不再写文件）；batch 是攒批缓冲（ticker 每秒或
	// 满 batchMax 条时刷出）。只在 run goroutine 里访问，无需额外锁。
	sink     Sink
	batch    []Event
	batchMax int
}

func newArchiveWriter(cfg Config) *archiveWriter {
	if !cfg.Enabled || (cfg.Sink == nil && strings.TrimSpace(cfg.Dir) == "") {
		return &archiveWriter{cfg: cfg}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = 7
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 100 << 20
	}
	if cfg.FileMaxBytes <= 0 {
		cfg.FileMaxBytes = defaultFileMax
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueue
	}
	w := &archiveWriter{
		cfg:      cfg,
		ch:       make(chan Event, cfg.QueueSize),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		sink:     cfg.Sink,
		batchMax: 200,
	}
	if cfg.Sink == nil {
		if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
			w.setErr(err)
			w.cfg.Enabled = false
			close(w.done)
			return w
		}
	}
	go w.run()
	return w
}

func (w *archiveWriter) enabled() bool {
	if w == nil || w.done == nil {
		return false
	}
	if w.sink != nil {
		return w.cfg.Enabled
	}
	return w.cfg.Enabled && w.cfg.Dir != ""
}

func (w *archiveWriter) enqueue(e Event) {
	if !w.enabled() {
		return
	}
	select {
	case w.ch <- e:
	default:
		w.dropped.Add(1)
	}
}

func (w *archiveWriter) run() {
	defer close(w.done)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case e := <-w.ch:
			w.writeEvent(e)
		case <-ticker.C:
			w.flush()
			w.flushSink()
			w.prune()
		case <-w.stop:
			for {
				select {
				case e := <-w.ch:
					w.writeEvent(e)
				default:
					w.flush()
					w.flushSink()
					w.closeFile()
					return
				}
			}
		}
	}
}

// flushSink 把攒批的记录一次性写进后端；失败则计数丢弃（日志归档不允许阻塞模型请求）。
func (w *archiveWriter) flushSink() {
	if w.sink == nil || len(w.batch) == 0 {
		return
	}
	rows := w.batch
	w.batch = nil
	if err := w.sink.AppendLogs(rows); err != nil {
		w.setErr(err)
		w.dropped.Add(uint64(len(rows)))
	}
}

func (w *archiveWriter) writeEvent(e Event) {
	if w.sink != nil {
		w.batch = append(w.batch, e)
		if len(w.batch) >= w.batchMax {
			w.flushSink()
		}
		return
	}
	raw, err := json.Marshal(e)
	if err != nil {
		w.setErr(err)
		return
	}
	now := e.Time
	if now.IsZero() {
		now = time.Now()
	}
	day := now.Format("2006-01-02")
	if w.file == nil || w.day != day {
		if err := w.openFile(now, false); err != nil {
			w.setErr(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(raw))+1 > w.cfg.FileMaxBytes {
		if err := w.openFile(now, true); err != nil {
			w.setErr(err)
			return
		}
	}
	if _, err := w.buf.Write(raw); err != nil {
		w.setErr(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.setErr(err)
		return
	}
	w.size += int64(len(raw)) + 1
}

func (w *archiveWriter) openFile(now time.Time, rotate bool) error {
	w.closeFile()
	day := now.Format("2006-01-02")
	if err := os.MkdirAll(w.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(w.cfg.Dir, "requests-"+day+".jsonl")
	if rotate {
		path = nextArchivePath(w.cfg.Dir, day)
	} else if latest := latestArchiveForDay(w.cfg.Dir, day); latest != "" {
		path = filepath.Join(w.cfg.Dir, latest)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	w.file = f
	w.buf = bufio.NewWriterSize(f, 64<<10)
	w.path = path
	w.day = day
	w.size = info.Size()
	return nil
}

func latestArchiveForDay(dir, day string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	prefix := "requests-" + day
	best := ""
	bestIndex := -1
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		idx := 0
		if name[len(prefix):len(name)-len(".jsonl")] != "" {
			part := strings.TrimSuffix(strings.TrimPrefix(name[len(prefix):], "."), ".jsonl")
			n, err := strconv.Atoi(part)
			if err != nil {
				continue
			}
			idx = n
		}
		if idx > bestIndex {
			bestIndex = idx
			best = name
		}
	}
	if best == "" {
		return ""
	}
	return best
}

func nextArchivePath(dir, day string) string {
	base := filepath.Join(dir, "requests-"+day)
	path := base + ".jsonl"
	for i := 1; fileExists(path); i++ {
		path = fmt.Sprintf("%s.%d.jsonl", base, i)
	}
	return path
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func (w *archiveWriter) flush() {
	if w.buf == nil {
		return
	}
	if err := w.buf.Flush(); err != nil {
		w.setErr(err)
	}
}

func (w *archiveWriter) closeFile() {
	if w.buf != nil {
		_ = w.buf.Flush()
	}
	if w.file != nil {
		_ = w.file.Close()
	}
	w.file = nil
	w.buf = nil
	w.path = ""
	w.day = ""
	w.size = 0
}

func (w *archiveWriter) prune() {
	if !w.enabled() {
		return
	}
	if w.sink != nil {
		if err := w.sink.PruneLogs(w.cfg.RetentionDays, w.cfg.MaxBytes); err != nil {
			w.setErr(err)
		}
		return
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		w.setErr(err)
		return
	}
	type item struct {
		path  string
		size  int64
		mtime time.Time
	}
	items := make([]item, 0, len(entries))
	var total int64
	cutoff := time.Now().AddDate(0, 0, -w.cfg.RetentionDays)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(w.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		items = append(items, item{path: path, size: info.Size(), mtime: info.ModTime()})
		total += info.Size()
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].mtime.Equal(items[j].mtime) {
			return items[i].path < items[j].path
		}
		return items[i].mtime.Before(items[j].mtime)
	})
	for _, it := range items {
		if it.path != w.path && (it.mtime.Before(cutoff) || total > w.cfg.MaxBytes) {
			if err := os.Remove(it.path); err == nil {
				total -= it.size
			}
		}
	}
}

func (w *archiveWriter) read(limit int, filter Filter) ([]Event, error) {
	if !w.enabled() {
		return nil, nil
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > defaultReadMax {
		limit = defaultReadMax
	}
	if w.sink != nil {
		return w.sink.ReadLogs(limit, filter)
	}
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		return nil, err
	}
	// 文件读取顺序无关紧要：结果一律按事件时间排序。不能依赖归档文件的 mtime 还原时间
	// 顺序——同一秒内连续轮转写出的多个文件 mtime 经常完全相同（Linux 文件时间戳粒度粗），
	// os.ReadDir 的字典序又会把装着最早事件的基准文件 requests-<day>.jsonl 排在
	// requests-<day>.N.jsonl 之后；目录被整体拷贝 / 恢复备份后 mtime 更不可信。
	var out []Event
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		rows, err := readFile(filepath.Join(w.cfg.Dir, name), filter)
		if err != nil {
			return out, err
		}
		out = append(out, rows...)
	}
	// 契约：按事件时间倒序返回最近 limit 条（同一时刻用 Stable 保留落盘先后）。
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func readFile(path string, filter Filter) ([]Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	var out []Event
	for scanner.Scan() {
		var e Event
		if json.Unmarshal(scanner.Bytes(), &e) != nil || !filter.match(e) {
			continue
		}
		out = append(out, e)
	}
	return out, scanner.Err()
}

func (f Filter) match(e Event) bool {
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	if !containsFold(e.Account, f.Account) {
		return false
	}
	if !containsFold(e.APIKey, f.APIKey) {
		return false
	}
	if !containsFold(e.Model, f.Model) {
		return false
	}
	if !containsFold(e.Provider, f.Provider) {
		return false
	}
	if !containsFold(e.ClientIP, f.ClientIP) {
		return false
	}
	if !containsFold(e.UserAgent, f.UserAgent) {
		return false
	}
	if !f.From.IsZero() && e.Time.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && e.Time.After(f.To) {
		return false
	}
	return true
}

// containsFold 大小写不敏感的子串匹配；needle 为空视为命中（不筛该字段）。
func containsFold(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

func (w *archiveWriter) stats() ArchiveStats {
	s := ArchiveStats{Enabled: w.enabled(), DroppedWrites: w.dropped.Load(), LastError: w.errString()}
	s.RetentionDays = w.cfg.RetentionDays
	if !s.Enabled {
		return s
	}
	if w.sink != nil {
		rows, bytes, errMsg := w.sink.LogStats()
		s.Backend = "postgres"
		s.Dir = "postgres"
		s.Files = int(rows)
		s.Bytes = bytes
		if errMsg != "" {
			s.LastError = errMsg
		}
		return s
	}
	s.Dir = w.cfg.Dir
	entries, err := os.ReadDir(w.cfg.Dir)
	if err != nil {
		s.LastError = err.Error()
		return s
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		if info, err := entry.Info(); err == nil {
			s.Files++
			s.Bytes += info.Size()
		}
	}
	return s
}

func (w *archiveWriter) close() {
	if !w.enabled() {
		return
	}
	w.closeOnce.Do(func() {
		close(w.stop)
		<-w.done
	})
}

func (w *archiveWriter) setErr(err error) {
	if err == nil {
		return
	}
	w.lastErrMu.Lock()
	w.lastErr = err.Error()
	w.lastErrMu.Unlock()
}

func (w *archiveWriter) errString() string {
	w.lastErrMu.Lock()
	defer w.lastErrMu.Unlock()
	return w.lastErr
}
