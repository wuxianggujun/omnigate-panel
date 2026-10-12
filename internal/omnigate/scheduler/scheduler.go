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
	go s.loop()
	s.log.Info("自动签到已启用，共 %d 个任务", len(s.cfg.Checkin.Tasks))
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
