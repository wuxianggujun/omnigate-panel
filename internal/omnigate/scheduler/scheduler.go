// Package scheduler runs periodic check-in / keep-alive tasks.
package scheduler

import (
	"bytes"
	"context"
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
}

// New builds a Scheduler.
func New(cfg *config.Config, gw *gateway.Gateway, log *logx.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, gw: gw, log: log, stop: make(chan struct{})}
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
	for _, task := range s.cfg.Checkin.Tasks {
		name := task.Name
		if name == "" {
			name = task.Type + ":" + task.Provider
		}
		interval := task.Interval(def)
		if last := s.gw.LastCheckin(name); !last.IsZero() && time.Since(last) < interval {
			continue
		}
		s.runTask(task)
		s.gw.MarkCheckin(name)
	}
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
