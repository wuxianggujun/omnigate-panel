// Package credithist 记录 WorkBuddy 账号余额的滚动快照历史（「近 N 小时消耗」的数据源）。
//
// 为什么需要它：余额是唯一能反映「积分到底有没有被消耗」的观测量，但 pool 只保留
// 当前值——面板因此无法回答「这个号从昨天到现在掉了多少」。本包按余额刷新节奏
// （缺省 5 分钟一次）采样，保留一个滚动窗口（Retention = 26h，比 24h 口径多留 2h，
// 免得整点前后「近 24h」的起点刚滑出窗口就闪断），面板据此算净变化并与 usage 的
// 计费用量对账：
//
//	净变化  = credits(窗口末) - credits(窗口首)      （负 = 掉了）
//	对账差额 = 计费用量合计 + 净变化合计
//	          > 0 → 余额掉得比记录的用量还多（多半是积分过期，或上游口径差异）
//	          < 0 → 余额掉得比用量少（窗口内有签到奖励 / 充值入账）
//
// 设计取舍：
//   - 只采「余额刷新 / 签到」拿到的上游权威余额，不采 NoteModelCost 的本地扣减：
//     本地扣减是 d=int64(credit+0.5)，小额请求根本不扣，掺进来只会让曲线锯齿化，
//     而 5 分钟一次的上游快照本来就会把真实余额纠回来。
//   - 同值采样节流：余额没变且距上次采样不足 minInterval 就跳过——面板的「刷新余额」
//     按钮可以连点，不节流会把窗口塞满重复点。
//   - 落盘用原子写（tmp + rename），失败只记日志：历史是观测数据，丢了不影响网关。
package credithist

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

const (
	// Retention 保留窗口：24h 口径 + 2h 余量（见包注释）。
	Retention = 26 * time.Hour
	// minInterval 同值采样的最小间隔（余额未变时）。
	minInterval = 90 * time.Second
	// maxSamplesPerUID 每账号样本上限（防御性：5 分钟一次 × 26h ≈ 312）。
	maxSamplesPerUID = 420
	// MinSpan 净变化可用的最短覆盖时长：刚重启 / 新账号只有一两个样本时，
	// 「近 2 分钟 −0」这种数字只会误导，宁可不显示（前端按 OK 决定是否展示）。
	MinSpan = 10 * time.Minute
	// DefaultWindow 面板缺省对账窗口（与用量页缺省一致）。
	DefaultWindow = 24 * time.Hour
	// fileVersion 落盘格式版本。
	fileVersion = 1
	// flushEvery 落盘节流：观测数据，攒一会儿再写。
	flushEvery = 2 * time.Minute
)

// Sample 一次余额采样。T 为 unix 秒（与时区无关，前端按浏览器时区渲染）。
type Sample struct {
	// T 采样时刻（unix 秒）。
	T int64 `json:"t"`
	// C 采样时的剩余积分。
	C int64 `json:"c"`
	// E 采样时「窗口内快过期」的积分子集（omitempty：绝大多数账号恒为 0）。
	E int64 `json:"e,omitempty"`
}

// file 落盘格式。
type file struct {
	Version int                 `json:"version"`
	Saved   string              `json:"saved,omitempty"`
	Series  map[string][]Sample `json:"series"`
}

// Recorder 余额快照历史。并发安全；零值不可用，用 New 构造。
type Recorder struct {
	mu     sync.Mutex
	fp     string
	series map[string][]Sample
	dirty  bool
	fails  int

	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}
}

// New 构造记录器。fp 非空时尝试加载既有历史：文件缺失 / 损坏 / 版本不符都按空历史
// 启动（历史是观测数据，不能因为它坏了让进程起不来）。
func New(fp string) *Recorder {
	r := &Recorder{
		fp:     fp,
		series: map[string][]Sample{},
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	r.load()
	return r
}

// Record 以当前时间采一次样（余额刷新 / 签到成功后调用）。
func (r *Recorder) Record(uid string, credits, expiring int64) {
	r.RecordAt(uid, credits, expiring, time.Now())
}

// RecordAt 指定时刻采样（测试用）。uid 为空或 credits 无意义时忽略。
func (r *Recorder) RecordAt(uid string, credits, expiring int64, now time.Time) {
	if r == nil || uid == "" {
		return
	}
	t := now.Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	ss := r.series[uid]
	if n := len(ss); n > 0 {
		last := ss[n-1]
		// 同一秒内重复调用（签到与余额刷新撞在一起）：原地覆盖，别插重复点。
		if last.T == t {
			if last.C == credits && last.E == expiring {
				return
			}
			ss[n-1] = Sample{T: t, C: credits, E: expiring}
			r.series[uid] = ss
			r.dirty = true
			return
		}
		// 余额没变且距上次采样不足 minInterval：跳过（省空间，曲线不受影响）。
		if last.C == credits && last.E == expiring && t-last.T < int64(minInterval/time.Second) {
			return
		}
	}
	ss = append(ss, Sample{T: t, C: credits, E: expiring})
	if len(ss) > maxSamplesPerUID {
		ss = ss[len(ss)-maxSamplesPerUID:]
	}
	ss = prune(ss, now.Add(-Retention).Unix())
	r.series[uid] = ss
	r.dirty = true
}

// prune 丢掉早于 cutoff 的样本，但**至少保留最后一个**（否则窗口整体滑出后
// 这个账号会瞬间「没有历史」，净变化也跟着消失）。
func prune(ss []Sample, cutoff int64) []Sample {
	drop := 0
	for drop < len(ss) && ss[drop].T < cutoff {
		drop++
	}
	switch {
	case drop == 0:
		return ss
	case drop >= len(ss):
		return ss[len(ss)-1:]
	default:
		return append([]Sample(nil), ss[drop:]...)
	}
}

// Net 返回 uid 在 [now-window, now] 内的余额净变化（负 = 掉了）。ok=false 表示样本
// 不足（没有历史 / 覆盖时长 < MinSpan），调用方不应展示数字；from/to 是**实际**
// 覆盖区间（样本不足窗口时前端据此如实标注「近 3.2h」而不是硬说 24h）。
func (r *Recorder) Net(uid string, window time.Duration, now time.Time) (net int64, from, to time.Time, samples int, ok bool) {
	if r == nil || uid == "" || window <= 0 {
		return 0, time.Time{}, time.Time{}, 0, false
	}
	r.mu.Lock()
	ss := r.series[uid]
	if len(ss) < 2 {
		r.mu.Unlock()
		return 0, time.Time{}, time.Time{}, len(ss), false
	}
	// 窗口内最早的样本：从尾部往前找，第一个 >= cutoff 的即所求。
	cut := now.Add(-window).Unix()
	idx := -1
	for i := len(ss) - 1; i >= 0; i-- {
		if ss[i].T >= cut {
			idx = i
			continue
		}
		break
	}
	if idx < 0 || idx == len(ss)-1 {
		r.mu.Unlock()
		return 0, time.Time{}, time.Time{}, len(ss), false
	}
	first, last := ss[idx], ss[len(ss)-1]
	n := len(ss) - idx
	r.mu.Unlock()
	from, to = time.Unix(first.T, 0), time.Unix(last.T, 0)
	if to.Sub(from) < MinSpan {
		return 0, from, to, n, false
	}
	return last.C - first.C, from, to, n, true
}

// Series 返回窗口内样本的降采样（最多 maxPoints 点，含首尾）。无样本时返回 nil。
func (r *Recorder) Series(uid string, window time.Duration, maxPoints int, now time.Time) []Sample {
	if r == nil || uid == "" || window <= 0 || maxPoints <= 0 {
		return nil
	}
	r.mu.Lock()
	ss := r.series[uid]
	cut := now.Add(-window).Unix()
	idx := -1
	for i, s := range ss {
		if s.T >= cut {
			idx = i
			break
		}
	}
	if idx < 0 {
		r.mu.Unlock()
		return nil
	}
	out := append([]Sample(nil), ss[idx:]...)
	r.mu.Unlock()
	if len(out) <= maxPoints {
		return out
	}
	res := make([]Sample, 0, maxPoints)
	step := float64(len(out)-1) / float64(maxPoints-1)
	for i := 0; i < maxPoints; i++ {
		res = append(res, out[int(math.Round(float64(i)*step))])
	}
	return res
}

// Account 一个账号在窗口内的余额视图（面板 /panel/api/credits/history 的一行）。
// OK=false 表示样本不足以给出净变化（刚重启 / 新账号），此时只有 Series 可能有值。
type Account struct {
	UID string `json:"uid"`
	// Net 窗口内净变化（负 = 掉了）。
	Net int64 `json:"net"`
	// From/To 实际覆盖区间（RFC3339）。样本不足窗口时 To-From < 请求窗口。
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Samples 窗口内样本数。
	Samples int `json:"samples"`
	// SpanSec 实际覆盖时长（秒）；前端据此如实标注「近 3.2h」而不是硬说 24h。
	SpanSec int64 `json:"span_sec,omitempty"`
	// OK 净变化是否可用。
	OK bool `json:"ok"`
	// Series 窗口内降采样序列（maxPoints<=0 时不返回）。
	Series []Sample `json:"series,omitempty"`
}

// Snapshot 返回窗口内全部账号（allow==nil = 不限）的净变化与降采样序列，按 uid 升序。
// maxPoints<=0 时不带 Series（面板账号池只要一个数字，不必传曲线）。
func (r *Recorder) Snapshot(window time.Duration, maxPoints int, allow func(uid string) bool, now time.Time) []Account {
	if r == nil || window <= 0 {
		return nil
	}
	uids := r.UIDs()
	out := make([]Account, 0, len(uids))
	for _, uid := range uids {
		if allow != nil && !allow(uid) {
			continue
		}
		a := Account{UID: uid}
		if net, from, to, samples, ok := r.Net(uid, window, now); ok {
			a.Net, a.Samples, a.OK = net, samples, true
			a.From, a.To = from.Format(time.RFC3339), to.Format(time.RFC3339)
			a.SpanSec = int64(to.Sub(from).Seconds())
		} else {
			a.Samples = samples
		}
		if maxPoints > 0 {
			a.Series = r.Series(uid, window, maxPoints, now)
		}
		out = append(out, a)
	}
	return out
}

// UIDs 返回当前有历史的账号（供面板与账号池求交：只展示有基线的账号）。
func (r *Recorder) UIDs() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.series))
	for uid := range r.series {
		out = append(out, uid)
	}
	sort.Strings(out)
	return out
}

// Describe 供启动日志：当前规模与保留窗口。
func (r *Recorder) Describe() string {
	if r == nil || r.fp == "" {
		return "未启用"
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ss := range r.series {
		n += len(ss)
	}
	return fmt.Sprintf("%d 个账号 / %d 个样本，保留 %s", len(r.series), n, Retention)
}

// Start 启动后台落盘 goroutine（每 flushEvery 一次，无新样本时不写盘）。
// fp 为空时不起 goroutine（纯内存，测试用）。
func (r *Recorder) Start() {
	if r == nil || r.fp == "" {
		return
	}
	go func() {
		defer close(r.done)
		t := time.NewTicker(flushEvery)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				_ = r.Save()
				return
			case <-t.C:
				_ = r.Save()
			}
		}
	}()
}

// Stop 停止后台落盘并做最后一次保存（幂等）。
func (r *Recorder) Stop() {
	if r == nil || r.fp == "" {
		return
	}
	r.stopOnce.Do(func() {
		close(r.stop)
		<-r.done
	})
}

// Save 原子落盘（tmp + rename）。持锁完成「序列化 + 写盘」：文件只有几百 KB，
// 换来的是绝不丢样本（释放锁后再写会让并发的 Record 被 dirty=false 抹掉）。
// 失败只记日志（首败打完整错误，其后每 10 次提醒一次）。
func (r *Recorder) Save() error {
	if r == nil || r.fp == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.dirty {
		return nil
	}
	raw, err := json.Marshal(file{Version: fileVersion, Saved: time.Now().Format(time.RFC3339), Series: r.series})
	if err != nil {
		r.noteFail(err)
		return err
	}
	if dir := filepath.Dir(r.fp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := r.fp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		r.noteFail(err)
		return err
	}
	if err := os.Rename(tmp, r.fp); err != nil {
		r.noteFail(err)
		return err
	}
	if r.fails > 0 {
		log.Printf("credithist: 落盘恢复（此前连续失败 %d 次）", r.fails)
		r.fails = 0
	}
	r.dirty = false
	return nil
}

func (r *Recorder) noteFail(err error) {
	if r.fails == 0 {
		log.Printf("credithist: 落盘失败: %v", err)
	} else if r.fails%10 == 0 {
		log.Printf("credithist: 连续落盘失败 %d 次: %v", r.fails, err)
	}
	r.fails++
}

func (r *Recorder) load() {
	if r.fp == "" {
		return
	}
	raw, err := os.ReadFile(r.fp)
	if err != nil {
		return
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil || f.Version != fileVersion {
		log.Printf("credithist: 历史文件格式不符，按空历史启动 (%s)", r.fp)
		return
	}
	cut := time.Now().Add(-Retention).Unix()
	for uid, ss := range f.Series {
		sort.Slice(ss, func(i, j int) bool { return ss[i].T < ss[j].T })
		ss = prune(ss, cut)
		if len(ss) > maxSamplesPerUID {
			ss = ss[len(ss)-maxSamplesPerUID:]
		}
		if len(ss) > 0 {
			r.series[uid] = ss
		}
	}
	if len(r.series) > 0 {
		log.Printf("credithist: 恢复 %d 个账号的余额历史 (%s)", len(r.series), r.fp)
	}
}
