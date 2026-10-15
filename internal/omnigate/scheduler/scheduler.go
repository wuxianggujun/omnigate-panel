// Package scheduler runs periodic check-in / keep-alive tasks.
package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
)

// Scheduler drives the configured check-in tasks.
type Scheduler struct {
	cfg  *config.Config
	gw   *gateway.Gateway
	log  *logx.Logger
	stop chan struct{}

	// startedAt / firstOff 给每个任务一个随机的「首次执行」错峰：进程刚起来时
	// 不要所有签到任务在同一秒齐发（既像机器人，又可能触发上游风控）。
	startedAt time.Time
	firstOff  map[string]time.Duration

	// skipNoted 记录已提示过「不具备执行条件」的任务及其原因。守卫每周期都复检
	// （补齐凭证后自动恢复），但提示只在原因变化时打一次，不刷屏。
	skipNoted map[string]string
}

// New builds a Scheduler.
func New(cfg *config.Config, gw *gateway.Gateway, log *logx.Logger) *Scheduler {
	s := &Scheduler{
		cfg:       cfg,
		gw:        gw,
		log:       log,
		stop:      make(chan struct{}),
		startedAt: time.Now(),
		firstOff:  map[string]time.Duration{},
		skipNoted: map[string]string{},
	}
	def := cfg.DefaultInterval()
	for _, t := range cfg.Checkin.Tasks {
		s.firstOff[taskName(t)] = firstRunOffset(taskName(t), t.Interval(def))
	}
	return s
}

// Start launches the scheduling loop (no-op when disabled).
func (s *Scheduler) Start() {
	if !s.cfg.Checkin.Enabled || len(s.cfg.Checkin.Tasks) == 0 {
		s.log.Info("自动签到未启用")
		return
	}
	// 启动先盘一遍执行条件：供应商不存在 / 账号为 0 的任务不具备执行条件。
	// 这里报一次总账，之后 runDue 的复检只在状态变化时提示。
	total := len(s.cfg.Checkin.Tasks)
	ready := 0
	for _, t := range s.cfg.Checkin.Tasks {
		name := taskName(t)
		ok, why := s.taskReady(t)
		if ok {
			ready++
			continue
		}
		if s.skipNoted[name] != why {
			s.skipNoted[name] = why
			s.log.Warn("签到任务 %q 已挂起：%s（补齐凭证后自动恢复）", name, why)
		}
	}
	switch {
	case ready == 0:
		s.log.Warn("自动签到已启用，但 %d 个任务全部不具备执行条件", total)
	case ready < total:
		s.log.Info("自动签到已启用：%d 个任务可执行，%d 个挂起", ready, total-ready)
	default:
		s.log.Info("自动签到已启用，共 %d 个任务", total)
	}
	go s.loop()
}

// Stop halts the loop.
func (s *Scheduler) Stop() { close(s.stop) }

func (s *Scheduler) loop() {
	// Run due tasks shortly after startup, then poll every minute.
	s.runDue()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.runDue()
		}
	}
}

func (s *Scheduler) runDue() {
	def := s.cfg.DefaultInterval()
	now := time.Now()
	for _, task := range s.cfg.Checkin.Tasks {
		name := taskName(task)
		// 执行条件守卫：不具备条件的任务直接跳过，且**不** MarkCheckin——这样补齐
		// 凭证后的下一个周期就能自动恢复，而不是白等一个完整间隔。
		if ok, why := s.taskReady(task); !ok {
			if s.skipNoted[name] != why {
				s.skipNoted[name] = why
				s.log.Warn("签到任务 %q 已挂起：%s（补齐凭证后自动恢复）", name, why)
			}
			continue
		}
		if why, noted := s.skipNoted[name]; noted {
			delete(s.skipNoted, name)
			s.log.Info("签到任务 %q 恢复执行（此前挂起原因：%s）", name, why)
		}
		interval := task.Interval(def)
		jitter := task.Jitter(s.cfg.Checkin.JitterMinutes)
		last := s.gw.LastCheckin(name)
		if last.IsZero() {
			// 首次执行：随机错峰（同一 task 名在同一进程生命周期内固定）。
			if off := s.firstOff[name]; off > 0 && now.Sub(s.startedAt) < off {
				continue
			}
		} else {
			// 到点判定 = 上次 + 间隔 ± 随机抖动（种子 = 上次时间，故一个周期内稳定、
			// 跨周期各不相同 → 每天签到时刻不再固定）。
			due := last.Add(interval).Add(jitterOffset(name, last.Unix(), jitter))
			if now.Before(due) {
				continue
			}
		}
		s.runTask(task)
		s.gw.MarkCheckin(name)
	}
}

// taskName returns a task's stable identity (its name, or type:provider).
func taskName(task config.CheckinTask) string {
	if task.Name != "" {
		return task.Name
	}
	return task.Type + ":" + task.Provider
}

// taskReady 判断一个签到任务当前是否具备执行条件，返回 (可执行, 不可执行的原因)。
//
// 内置任务（runable / raccoon / trae / newapi / newapi-refresh / geminiweb）的语义
// 都是「维护某个供应商已有账号的会话或额度」：供应商不存在、或账号数为 0 时，任务
// 既没有可续的 cookie 也没有可签的账号，跑起来只会在每次到点时留下一条必然失败的
// 日志。历史上 4 个空壳供应商各自挂着签到任务，就是这么每天空转的——没有账号的
// 供应商本就不该被排进签到。
//
// 通用 HTTP 任务（type=http 或空）自带 url，凭证靠 {{cookie}} 等占位符替换，没有
// 供应商也能执行，所以不设门槛（此时 runHTTP 会以空替换跑一次）。
func (s *Scheduler) taskReady(task config.CheckinTask) (bool, string) {
	if task.Type == "" || task.Type == "http" {
		return true, ""
	}
	name := task.Provider
	if name == "" {
		name = s.cfg.DefaultProvider
	}
	if name == "" {
		return false, "任务未指定供应商，且配置里没有默认供应商"
	}
	if s.cfg.Provider(name) == nil {
		return false, fmt.Sprintf("供应商 %q 不存在", name)
	}
	if len(s.gw.Accounts(name)) == 0 {
		return false, fmt.Sprintf("供应商 %q 尚未添加账号", name)
	}
	return true, ""
}

// firstRunOffset derives a stable per-task first-run stagger in [0, cap), where
// cap is a quarter of the interval clamped to 30 minutes.
func firstRunOffset(name string, interval time.Duration) time.Duration {
	cap := interval / 4
	if cap > 30*time.Minute {
		cap = 30 * time.Minute
	}
	if cap <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return time.Duration(h.Sum64() % uint64(cap))
}

// jitterOffset derives a stable pseudo-random offset in [-jitter, +jitter) from
// the task name and its last-run time. Because the seed changes every cycle, the
// effective interval drifts run to run instead of landing on a fixed clock time.
func jitterOffset(name string, last int64, jitter time.Duration) time.Duration {
	if jitter <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%s|%d", name, last)
	frac := float64(h.Sum64()%2001)/1000.0 - 1.0 // [-1, 1)
	return time.Duration(float64(jitter) * frac)
}

func (s *Scheduler) runTask(task config.CheckinTask) {
	switch task.Type {
	case "runable":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("签到任务：Runable 保活/刷新（%s）", provider)
		s.gw.RunRunableCheckin(provider)
	case "raccoon":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("签到任务：小浣熊桌面登录奖励（%s）", provider)
		s.gw.RunRaccoonCheckin(provider)
	case "trae":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("签到任务：TRAE 每日签到（%s）", provider)
		s.gw.RunTraeCheckin(provider)
	case "newapi":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("签到任务：New API 每日签到（%s）", provider)
		s.gw.RunNewAPICheckin(provider)
	case "newapi-refresh":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("刷新任务：New API 统一刷新（%s）", provider)
		s.gw.RunNewAPIRefresh(provider)
	case "geminiweb":
		provider := task.Provider
		if provider == "" {
			provider = s.cfg.DefaultProvider
		}
		s.log.Info("签到任务：Gemini Web 会话健康检查（%s）", provider)
		s.gw.RunGeminiWebCheckin(provider)
	case "http", "":
		s.runHTTP(task)
	default:
		s.log.Warn("未知签到任务类型 %q", task.Type)
	}
}

func (s *Scheduler) runHTTP(task config.CheckinTask) {
	if task.URL == "" {
		s.log.Warn("签到任务 %q 缺少 url", task.Name)
		return
	}
	accts := s.gw.Accounts(task.Provider)
	if len(accts) == 0 {
		// No provider/accounts: run once with empty substitutions.
		s.doHTTP(task, "", "", "", "")
		return
	}
	for _, acc := range accts {
		s.doHTTP(task, acc.Cookie, acc.RefreshToken, acc.Label, acc.Email)
	}
}

func (s *Scheduler) doHTTP(task config.CheckinTask, cookie, refreshToken, label, email string) {
	method := task.Method
	if method == "" {
		method = http.MethodPost
	}
	sub := func(v string) string {
		r := strings.NewReplacer(
			"{{cookie}}", cookie,
			"{{access_token}}", cookie,
			"{{refresh_token}}", refreshToken,
			"{{label}}", label,
			"{{email}}", email,
		)
		return r.Replace(v)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var bodyReader io.Reader
	if task.Body != "" {
		bodyReader = bytes.NewReader([]byte(sub(task.Body)))
	}
	req, err := http.NewRequestWithContext(ctx, method, sub(task.URL), bodyReader)
	if err != nil {
		s.log.Warn("签到任务 %q 构造请求失败: %v", task.Name, err)
		return
	}
	for k, v := range task.Headers {
		req.Header.Set(k, sub(v))
	}
	if cookie != "" && req.Header.Get("Cookie") == "" {
		req.Header.Set("Cookie", cookie)
	}
	if req.Header.Get("Content-Type") == "" && task.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.log.Warn("签到任务 %q（%s）请求失败: %v", task.Name, label, err)
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		s.log.Warn("签到任务 %q（%s）返回 HTTP %d: %s", task.Name, label, resp.StatusCode, clip(string(raw)))
		return
	}
	s.log.Info("签到任务 %q（%s）成功 HTTP %d", task.Name, label, resp.StatusCode)
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
