// quota.go OpenCode Go 订阅套餐的额度查询（面板「额度」按钮的后端）。
//
// 用配置的 go 密钥调官方额度接口 GET {go_base}/v1/usage（Bearer key），把每把密钥
// 当作一个「账号」返回三个窗口（rolling=5 小时 / weekly=每周 / monthly=每月）的
// 已用百分比、剩余百分比与重置时间。接口事实来自 OpenCode 控制台源码
// （packages/console/app/src/routes/zen/go/v1/usage.ts，未列入正式 API 文档）：
// 返回的是**套餐共享**窗口百分比，不是逐模型余额，也没有金额。对齐的参考实现是
// opencode2api 的 model-quotas 看板（internal/admin/model_quotas.go）。
//
// 边界：
//   - 只查 go 池。zen 是按量付费，没有套餐额度（/v1/usage 对 zen key 返回 403）；
//     匿名免费通道的用量同样不走套餐额度。
//   - 明文密钥绝不出门：结果里只有掩码（前 6 + … + 后 4 + …(len=N)，与面板
//     secret.go 同形态）和指纹（sha256 前 8 位 hex），面板和日志都拿不到原值。
//   - 结果缓存 60 秒（成功）/ 15 秒（失败），且整个刷新在实例级锁下合并——
//     额度页会被多个浏览器标签同时轮询，第二个调用者等第一个刷完后直接读新鲜
//     缓存，不再打一遍上游（对齐 opencode2api 的 model-quotas）。缓存与锁都
//     挂在 Provider 上——改配置会重建实例，旧密钥的缓存条目随旧实例一起消失，
//     不会串到新密钥上。
package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// quotaTTL 是额度结果的缓存时长：成功 60 秒、失败 15 秒（失败要能较快重试，
// 但也不能让连点刷新变成打上游的放大器）。
const (
	quotaTTLSuccess = 60 * time.Second
	quotaTTLFailure = 15 * time.Second
)

// quotaFetchTimeout 是单次额度查询的超时：额度接口是个轻量 GET，挂 300 秒
// （p.http 的响应头超时）只会让「额度」按钮白转；15 秒足够慢网络完成握手。
const quotaFetchTimeout = 15 * time.Second

// QuotaWindow 是一个额度窗口的快照。
type QuotaWindow struct {
	Status           string    `json:"status"` // ok / rate-limited
	UsedPercent      float64   `json:"used_percent"`
	RemainingPercent float64   `json:"remaining_percent"`
	ResetsAt         time.Time `json:"resets_at"`
}

// KeyQuota 是一把 go 密钥的额度查询结果。
type KeyQuota struct {
	// Fingerprint 是密钥的短指纹（sha256 前 8 位 hex）：面板用它区分同前缀的多把密钥。
	Fingerprint string `json:"fingerprint"`
	// Masked 是密钥的掩码形态（前 6 + … + 后 4 + …(len=N)）。明文不出本层。
	Masked string `json:"masked"`
	// Status 是本次查询的结论：ok（拿到完整窗口）/ stale（本次失败，展示上次成功
	// 的窗口，FetchedAt 注明历史时点）/ unavailable（从未成功过）。
	Status    string                 `json:"status"`
	Error     string                 `json:"error,omitempty"`
	FetchedAt *time.Time             `json:"fetched_at,omitempty"`
	CheckedAt time.Time              `json:"checked_at"`
	Windows   map[string]QuotaWindow `json:"windows,omitempty"`
}

// quotaCacheEntry 是一把密钥的缓存条目。
type quotaCacheEntry struct {
	data    KeyQuota
	expires time.Time
}

// goPool 返回 go tier 的密钥池（没配 go 密钥时为 nil）。
func (p *Provider) goPool() *pool {
	for _, pl := range p.all {
		if pl.name == TierGo {
			return pl
		}
	}
	return nil
}

// GoQuota 查询全部 go 密钥的套餐额度。每把密钥独立查询、独立报错：一把失效
// 不影响其它密钥的结果。没有配置 go 密钥时返回空切片（额度看板只覆盖 go
// 订阅套餐，这是「没得查」而不是错误，前端据此显示空态说明）。
//
// 并发合并：整个刷新在 quotaRefreshMu 下进行。额度页会被多个浏览器标签同时
// 轮询——第二个调用者等第一个刷完才拿到锁，逐把读到新鲜缓存直接返回，不再
// 打一遍上游。对齐 opencode2api 的 model-quotas 合并语义（quotaMu + wg.Wait）。
func (p *Provider) GoQuota(ctx context.Context) ([]KeyQuota, error) {
	pl := p.goPool()
	if pl == nil {
		return []KeyQuota{}, nil
	}
	p.quotaRefreshMu.Lock()
	defer p.quotaRefreshMu.Unlock()
	keys := pl.snapshot()
	out := make([]KeyQuota, 0, len(keys))
	now := time.Now().UTC()
	for _, key := range keys {
		fp := keyFingerprint(key)
		if entry, ok := p.quotaCached(fp); ok {
			out = append(out, entry)
			continue
		}
		entry := p.fetchQuota(ctx, pl.base, key, fp)
		entry.CheckedAt = now
		// 上次成功过的失败要降级为 stale 并保留旧窗口：额度页在缓存间隙里
		// 拿到「历史数据 + 失败原因」比拿到一张白板有用得多。
		if prev, ok := p.quotaPrev(fp); ok && entry.Status != "ok" {
			if prev.Status == "ok" && prev.FetchedAt != nil {
				entry.Status = "stale"
				entry.Windows = prev.Windows
				entry.FetchedAt = prev.FetchedAt
			}
		}
		ttl := quotaTTLFailure
		if entry.Status == "ok" {
			ttl = quotaTTLSuccess
		}
		p.quotaStore(fp, entry, now.Add(ttl))
		out = append(out, entry)
	}
	return out, nil
}

// fetchQuota 对单把密钥发一次额度查询，把上游状态码翻译成可操作的话。
// 请求走 p.http（带供应商的出站代理路由——直连打不到 opencode.ai 的网络里，
// 额度查询同样要过代理），超时由 ctx 上限 quotaFetchTimeout 兜住。
func (p *Provider) fetchQuota(ctx context.Context, base, key, fp string) KeyQuota {
	entry := KeyQuota{Fingerprint: fp, Masked: maskKey(key), Status: "unavailable"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/usage", nil)
	if err != nil {
		entry.Error = "额度查询地址无效"
		return entry
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")
	// 会话头按密钥派生一个稳定 id：上游按会话做路由优化，额度查询也如实申报身份
	// （与 zen/go 通道同策略，见包注释——匿名通道的镜像身份不适用这里）。
	req.Header.Set("x-opencode-session", "omnigate-quota-"+fp)
	ctx, cancel := context.WithTimeout(ctx, quotaFetchTimeout)
	defer cancel()
	resp, err := p.http.Do(req)
	if err != nil {
		entry.Error = "暂时无法连接 OpenCode，请稍后刷新"
		return entry
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			entry.Error = "Go API Key 无效或已被撤销"
		case http.StatusForbidden:
			entry.Error = "此 Key 未获准读取 Go 套餐额度（zen 按量付费密钥没有套餐额度）"
		case http.StatusTooManyRequests:
			entry.Error = "额度查询过于频繁，请稍后刷新"
		default:
			entry.Error = fmt.Sprintf("OpenCode 额度接口返回 HTTP %d", resp.StatusCode)
		}
		return entry
	}
	windows, err := parseQuota(raw)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	at := time.Now().UTC()
	entry.Status, entry.Error, entry.Windows, entry.FetchedAt = "ok", "", windows, &at
	return entry
}

// parseQuota 解析官方额度接口的响应：
//
//	{"usage": {"rolling": {"status": "ok", "percent": 42.5, "resetsAt": "…"}, …}}
//
// 三个窗口必须齐全且取值合法才认成功——上游只给这三个共享窗口，缺一个说明
// 格式变了，宁可信其无：显示「格式无法识别」也比虚构一个窗口强。percent 由
// 上游取整，这里不再加工；剩余百分比 = 100 - 已用。
func parseQuota(raw []byte) (map[string]QuotaWindow, error) {
	var body struct {
		Usage map[string]struct {
			Status   string    `json:"status"`
			Percent  *float64  `json:"percent"`
			ResetsAt time.Time `json:"resetsAt"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("OpenCode 返回的额度格式无法识别")
	}
	result := map[string]QuotaWindow{}
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		item, ok := body.Usage[name]
		if !ok || item.Percent == nil || *item.Percent < 0 || *item.Percent > 100 ||
			item.ResetsAt.IsZero() || (item.Status != "ok" && item.Status != "rate-limited") {
			return nil, errors.New("OpenCode 返回的额度窗口不完整（接口格式可能已变化）")
		}
		result[name] = QuotaWindow{
			Status:           item.Status,
			UsedPercent:      *item.Percent,
			RemainingPercent: 100 - *item.Percent,
			ResetsAt:         item.ResetsAt,
		}
	}
	return result, nil
}

// keyFingerprint 返回密钥的短指纹（sha256 前 8 位 hex）：只用于区分多把密钥，
// 反推不出原值。
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:4])
}

// maskKey 把密钥变成「前 6 + … + 后 4 + …(len=N)」的掩码形态，与面板 secret.go
// 的 maskSecret 同形（额度结果是只读展示、不经配置保存回传，这里独立实现一份，
// 不为此引入 panel 依赖）。短于 12 字符时只露前一半。
func maskKey(key string) string {
	n := len(key)
	if n == 0 {
		return ""
	}
	if n < 12 {
		return key[:n/2] + "…(len=" + strconv.Itoa(n) + ")"
	}
	return key[:6] + "…" + key[n-4:] + "…(len=" + strconv.Itoa(n) + ")"
}

// quotaCached / quotaPrev / quotaStore 是额度缓存的三个入口，共用 p.quotaMu。
// quotaPrev 只查不看过期时刻（stale 降级要读「上一条哪怕已过期」的结论）。

func (p *Provider) quotaCached(fp string) (KeyQuota, bool) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	e, ok := p.quotaCache[fp]
	if !ok || time.Now().After(e.expires) {
		return KeyQuota{}, false
	}
	return e.data, true
}

func (p *Provider) quotaPrev(fp string) (KeyQuota, bool) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	e, ok := p.quotaCache[fp]
	if !ok {
		return KeyQuota{}, false
	}
	return e.data, true
}

func (p *Provider) quotaStore(fp string, data KeyQuota, expires time.Time) {
	p.quotaMu.Lock()
	defer p.quotaMu.Unlock()
	if p.quotaCache == nil {
		p.quotaCache = map[string]quotaCacheEntry{}
	}
	p.quotaCache[fp] = quotaCacheEntry{data: data, expires: expires}
}
