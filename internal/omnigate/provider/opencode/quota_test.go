// quota_test.go 钉住 Go 套餐额度查询的行为：上游状态码的翻译、窗口解析的
// 合法性、缓存与 stale 降级、明文密钥不出门。
//
// 接口事实（响应形状与状态码语义）全部按 quota.go 的解析实现对照——上游
// /v1/usage 未列入正式 API 文档，格式一变这里先亮红灯，而不是线上静默报错。
package opencode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// usageBody 是一次合法的额度响应（三个窗口齐全）。
func usageBody(rolling, weekly, monthly float64) string {
	return `{"usage":{` +
		`"rolling":{"status":"ok","percent":` + jsonNum(rolling) + `,"resetsAt":"2026-10-16T18:00:00Z"},` +
		`"weekly":{"status":"ok","percent":` + jsonNum(weekly) + `,"resetsAt":"2026-10-19T00:00:00Z"},` +
		`"monthly":{"status":"ok","percent":` + jsonNum(monthly) + `,"resetsAt":"2026-11-01T00:00:00Z"}}}`
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// quotaSrv 起一个假的 OpenCode 额度接口，返回 status 与 body，并返回命中计数器
// （缓存行为测试用它数上游被打了几次）。
func quotaSrv(t *testing.T, status int, body string) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		// 额度接口必须带 Bearer key 与会话头：身份申报是 zen/go 通道的策略，
		// 额度查询走同一条路（匿名通道的镜像身份不适用）。
		if r.Header.Get("Authorization") == "" || r.Header.Get("x-opencode-session") == "" {
			t.Errorf("额度请求缺身份头: Authorization=%q session=%q",
				r.Header.Get("Authorization"), r.Header.Get("x-opencode-session"))
		}
		if !strings.HasSuffix(r.URL.Path, "/v1/usage") {
			t.Errorf("额度请求路径应为 /v1/usage，得到 %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, func() int {
		mu.Lock()
		defer mu.Unlock()
		return hits
	}
}

// quotaProvider 构造一个只有 go 池的供应商（GoBase 指向假上游）。
func quotaProvider(t *testing.T, goBase string, keys ...string) *Provider {
	t.Helper()
	return New(Options{Name: "oc", GoKeys: keys, GoBase: goBase, Anonymous: false})
}

// TestGoQuotaFetch 各状态码的翻译：200 → 完整窗口；401/403/429 → 可操作的话术；
// 缺窗口的 200 → 「格式可能已变化」。一把密钥的状态绝不能静默成「正常」。
func TestGoQuotaFetch(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantOK  bool
		wantErr string
	}{
		{"200 完整窗口", http.StatusOK, usageBody(42.5, 10, 3.2), true, ""},
		{"401 密钥无效", http.StatusUnauthorized, `{"error":{"type":"AuthError"}}`, false, "Go API Key 无效或已被撤销"},
		{"403 无额度权限", http.StatusForbidden, `{}`, false, "未获准读取 Go 套餐额度"},
		{"429 查询频繁", http.StatusTooManyRequests, `{}`, false, "额度查询过于频繁"},
		{"500 上游错误", http.StatusInternalServerError, `boom`, false, "HTTP 500"},
		{"200 缺 monthly 窗口", http.StatusOK, `{"usage":{"rolling":{"status":"ok","percent":1,"resetsAt":"2026-10-16T18:00:00Z"},"weekly":{"status":"ok","percent":2,"resetsAt":"2026-10-19T00:00:00Z"}}}`, false, "额度窗口不完整"},
		{"200 非 JSON", http.StatusOK, `<html>nope</html>`, false, "额度格式无法识别"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := quotaSrv(t, tc.status, tc.body)
			p := quotaProvider(t, srv.URL, "sk-go-test-0123456789abcdef")
			keys, err := p.GoQuota(context.Background())
			if err != nil {
				t.Fatalf("GoQuota 不应报错（逐把独立报错在条目里）: %v", err)
			}
			if len(keys) != 1 {
				t.Fatalf("应返回 1 把密钥，实际 %d", len(keys))
			}
			k := keys[0]
			if tc.wantOK {
				if k.Status != "ok" {
					t.Fatalf("status = %q, want ok (err=%q)", k.Status, k.Error)
				}
				if len(k.Windows) != 3 {
					t.Fatalf("应有 3 个窗口，实际 %d", len(k.Windows))
				}
				if got := k.Windows["rolling"].RemainingPercent; got != 57.5 {
					t.Fatalf("rolling 剩余 = %v, want 57.5（100 - 42.5）", got)
				}
				if k.FetchedAt == nil {
					t.Fatal("成功结果应带 FetchedAt")
				}
			} else {
				if k.Status != "unavailable" {
					t.Fatalf("status = %q, want unavailable", k.Status)
				}
				if !strings.Contains(k.Error, tc.wantErr) {
					t.Fatalf("error = %q, want 含 %q", k.Error, tc.wantErr)
				}
			}
		})
	}
}

// TestGoQuotaNoKeys 没配 go 密钥 = 空切片而不是错误：这是「没得查」（额度看板
// 只覆盖 go 订阅套餐），前端据此显示空态说明，不该被 4xx 挡住。
func TestGoQuotaNoKeys(t *testing.T) {
	p := quotaProvider(t, "http://127.0.0.1:1", "")
	keys, err := p.GoQuota(context.Background())
	if err != nil {
		t.Fatalf("没配 go 密钥不应报错: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("应返回空切片，实际 %d", len(keys))
	}
}

// TestGoQuotaCache 缓存 60 秒：第二次查询不真打上游。额度页会被多个浏览器标签
// 轮询，不该每次点击都是一次上游请求。
func TestGoQuotaCache(t *testing.T) {
	srv, hits := quotaSrv(t, http.StatusOK, usageBody(10, 20, 30))
	p := quotaProvider(t, srv.URL, "sk-go-cache-0123456789")
	for i := 0; i < 3; i++ {
		keys, err := p.GoQuota(context.Background())
		if err != nil || len(keys) != 1 || keys[0].Status != "ok" {
			t.Fatalf("第 %d 次查询应成功: err=%v keys=%+v", i+1, err, keys)
		}
	}
	if n := hits(); n != 1 {
		t.Fatalf("缓存 60s 内 3 次查询应只打上游 1 次，实际 %d 次", n)
	}
}

// TestGoQuotaStaleDegradation 上次成功、这次失败 → stale + 保留旧窗口：
// 额度页在缓存间隙里拿到「历史数据 + 失败原因」比拿到一张白板有用。
func TestGoQuotaStaleDegradation(t *testing.T) {
	srv, _ := quotaSrv(t, http.StatusOK, usageBody(10, 20, 30))
	p := quotaProvider(t, srv.URL, "sk-go-stale-0123456789")
	if keys, err := p.GoQuota(context.Background()); err != nil || keys[0].Status != "ok" {
		t.Fatalf("首次查询应成功: err=%v", err)
	}
	// 换一个必然失败的上游，但密钥不变——stale 判定走缓存里的上一条。
	bad, _ := quotaSrv(t, http.StatusUnauthorized, `{}`)
	p2 := New(Options{Name: "oc", GoKeys: []string{"sk-go-stale-0123456789"}, GoBase: bad.URL, Anonymous: false})
	// 把第一个 provider 的成功条目搬进 p2 的缓存（**已过期**）：模拟「同一实例
	// 先成功、缓存过期后刷新失败」的真实流程——quotaCached 对过期条目放行去
	// 刷新，quotaPrev 仍能读到上一条做 stale 降级（quota cache 挂在实例上，
	// 跨实例本就不共享，这里手动搬运）。
	if keys, _ := p.GoQuota(context.Background()); len(keys) == 1 {
		p2.quotaStore(keys[0].Fingerprint, keys[0], time.Now().Add(-time.Second))
	}
	keys, err := p2.GoQuota(context.Background())
	if err != nil {
		t.Fatalf("GoQuota 不应报错: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("应返回 1 把密钥，实际 %d", len(keys))
	}
	k := keys[0]
	if k.Status != "stale" {
		t.Fatalf("status = %q, want stale（上次成功、本次失败）", k.Status)
	}
	if len(k.Windows) != 3 {
		t.Fatalf("stale 应保留上次的 3 个窗口，实际 %d", len(k.Windows))
	}
	if k.FetchedAt == nil {
		t.Fatal("stale 应保留上次的 FetchedAt（历史时点）")
	}
	if k.Error == "" {
		t.Fatal("stale 应带本次的失败原因")
	}
}

// TestQuotaMaskKey 明文密钥绝不出门：长密钥 → 前 6 + … + 后 4 + …(len=N)，
// 与面板 secret.go 的掩码同形态；短密钥只露前一半。
func TestQuotaMaskKey(t *testing.T) {
	long := "sk-6d06a1b2c3d4e5f6a7b8"
	want := "sk-6d0…" + long[len(long)-4:] + "…(len=" + itoa(len(long)) + ")"
	if got := maskKey(long); got != want {
		t.Fatalf("maskKey(长) = %q, want %q", got, want)
	}
	short := "abc12"
	if got := maskKey(short); got != "ab…(len=5)" {
		t.Fatalf("maskKey(短) = %q, want ab…(len=5)（短值只露前一半）", got)
	}
	if got := maskKey(""); got != "" {
		t.Fatalf("maskKey(空) = %q, want 空", got)
	}
}

// TestQuotaResultHidesPlaintext 额度结果的每一条都不得含明文密钥：GoQuota 是
// 面板可见的输出，明文出门就等于把上游密钥送进浏览器与日志。
func TestQuotaResultHidesPlaintext(t *testing.T) {
	const raw = "sk-secret-0123456789abcdef"
	srv, _ := quotaSrv(t, http.StatusOK, usageBody(1, 2, 3))
	p := quotaProvider(t, srv.URL, raw)
	keys, err := p.GoQuota(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("查询应成功: err=%v", err)
	}
	blob, _ := json.Marshal(keys[0])
	if strings.Contains(string(blob), raw) {
		t.Fatal("额度结果里出现了明文密钥")
	}
	if keys[0].Fingerprint == "" || keys[0].Masked == "" {
		t.Fatal("结果应带指纹与掩码（面板靠它们区分/展示密钥）")
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
