package fingerprint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VersionCheckURL 是 Google 的 Chrome 版本历史接口：取 Windows 稳定版、过滤
// 主版本 > 100、按版本降序、只取一条。
const VersionCheckURL = "https://versionhistory.googleapis.com/v1/chrome/platforms/win/channels/stable/versions" +
	"?filter=version>100&order_by=version%20desc&page_size=1"

// VersionSyncer 负责查询 Chrome 稳定版并更新指纹配置。
type VersionSyncer struct {
	mgr  *Manager
	http *http.Client
	logf func(format string, args ...any)
	// URL 覆盖版本接口地址（测试用）；空则用 VersionCheckURL。
	URL string
	// OnTarget 在版本升级并把 impersonate 目标写进配置后回调，用于让运行中的 TLS
	// 伪装器立刻切到新目标。参考实现里这一步由 _ensure_session_current 每次请求
	// 重读 header_builder.get_impersonate_target() 隐式完成；Go 侧靠这个回调等价实现。
	OnTarget func(target string)
}

// NewVersionSyncer 创建同步器。httpClient 为空时使用一个带 15s 超时的默认客户端。
func NewVersionSyncer(mgr *Manager, httpClient *http.Client, logf func(string, ...any)) *VersionSyncer {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &VersionSyncer{mgr: mgr, http: httpClient, logf: logf}
}

func (v *VersionSyncer) url() string {
	if strings.TrimSpace(v.URL) != "" {
		return v.URL
	}
	return VersionCheckURL
}

func (v *VersionSyncer) log(format string, args ...any) {
	if v.logf != nil {
		v.logf(format, args...)
	}
}

// CheckLatest 查询最新的 Chrome 稳定版，返回 (主版本, 完整版本号)。
func (v *VersionSyncer) CheckLatest(ctx context.Context) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url(), nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := v.http.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return 0, "", fmt.Errorf("版本检查失败: HTTP %d", resp.StatusCode)
	}
	var data struct {
		Versions []struct {
			Version string `json:"version"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return 0, "", err
	}
	if len(data.Versions) == 0 {
		return 0, "", fmt.Errorf("版本接口未返回任何版本")
	}
	full := strings.TrimSpace(data.Versions[0].Version)
	if full == "" {
		return 0, "", fmt.Errorf("版本号为空")
	}
	majorStr, _, _ := strings.Cut(full, ".")
	major, err := strconv.Atoi(majorStr)
	if err != nil {
		return 0, "", fmt.Errorf("无法解析主版本号 %q", full)
	}
	return major, full, nil
}

// FindBestTarget 为给定主版本挑选最佳 impersonate 目标：精确匹配优先，否则取
// version_map 中不高于它的最大版本（降级），都不可用则返回空串。
func (v *VersionSyncer) FindBestTarget(major int) string {
	cfg := v.mgr.Config()
	if t := cfg.VersionMap[strconv.Itoa(major)]; t != "" && ValidateImpersonateTarget(t) {
		return t
	}
	for _, ver := range cfg.VersionKeys() {
		if ver > major {
			continue
		}
		fallback := cfg.VersionMap[strconv.Itoa(ver)]
		if fallback != "" && ValidateImpersonateTarget(fallback) {
			v.log("fingerprint: Chrome %d 无精确 TLS 目标，降级使用 %s", major, fallback)
			return fallback
		}
	}
	return ""
}

// Run 后台同步循环：启动先等 60s，之后每 interval 检查一次。
// interval<=0 时按 24h 处理。ctx 取消即退出。
func (v *VersionSyncer) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if !sleepCtx(ctx, 60*time.Second) {
		return
	}
	for {
		v.tick(ctx)
		if !sleepCtx(ctx, interval) {
			return
		}
	}
}

// Sync performs one immediate version check/update. It backs both the periodic
// loop and the panel's manual "sync now" action.
func (v *VersionSyncer) Sync(ctx context.Context) { v.tick(ctx) }

// tick performs one check: query the latest stable Chrome, and if it is newer
// than the configured one, register its target and promote the version.
func (v *VersionSyncer) tick(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	major, full, err := v.CheckLatest(cctx)
	if err != nil {
		v.log("fingerprint: Chrome 版本检查异常: %v", err)
		return
	}
	current := v.mgr.Config().Chrome.Major
	if major <= current {
		return
	}
	target := v.FindBestTarget(major)
	if target == "" {
		v.log("fingerprint: 检测到 Chrome %d，但当前 TLS 库不支持，保持 %d", major, current)
		return
	}
	if err := v.mgr.SetVersionMap(major, target); err != nil {
		v.log("fingerprint: 写入 version_map 失败: %v", err)
		return
	}
	changed, err := v.mgr.UpdateChromeVersion(major, full)
	if err != nil {
		v.log("fingerprint: 更新 Chrome 版本失败: %v", err)
		return
	}
	if changed {
		// 让运行中的 TLS 伪装器立刻跟随新目标（等价参考实现的 _ensure_session_current）。
		if v.OnTarget != nil {
			v.OnTarget(target)
		}
		v.log("fingerprint: Chrome 版本已更新: %d -> %d (target: %s)", current, major, target)
	}
}

// sleepCtx 睡 d，ctx 取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
