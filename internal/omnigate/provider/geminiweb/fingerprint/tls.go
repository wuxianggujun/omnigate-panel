package fingerprint

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	utls "github.com/refraction-networking/utls"
)

// chromeProfiles 是当前构建里可用的 utls Chrome ClientHello 集合。utls v1.6.x
// 最高只到 Chrome 120，更高的目标名（chrome123/124/…）会降级到不高于它的最大
// 版本（见 tlsProfileForTarget），这正是参考实现 find_best_target 的「最近低版本」
// 语义。
var chromeProfiles = map[int]utls.ClientHelloID{
	100: utls.HelloChrome_100,
	102: utls.HelloChrome_102,
	106: utls.HelloChrome_106_Shuffle,
	115: utls.HelloChrome_115_PQ,
	120: utls.HelloChrome_120,
}

// SupportedChromeMajors 返回本构建支持的 Chrome 主版本（升序），供面板展示。
func SupportedChromeMajors() []int {
	out := make([]int, 0, len(chromeProfiles))
	for m := range chromeProfiles {
		out = append(out, m)
	}
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && out[k-1] > out[k]; k-- {
			out[k-1], out[k] = out[k], out[k-1]
		}
	}
	return out
}

// tlsProfileForTarget 把 "chromeNNN" 映射到 utls 的 ClientHello。精确命中优先，
// 否则取不高于 N 的最大已知版本。
func tlsProfileForTarget(target string) (utls.ClientHelloID, bool) {
	major, ok := chromeMajorFromTarget(target)
	if !ok {
		return utls.ClientHelloID{}, false
	}
	if id, ok := chromeProfiles[major]; ok {
		return id, true
	}
	best := -1
	for m := range chromeProfiles {
		if m <= major && m > best {
			best = m
		}
	}
	if best < 0 {
		return utls.ClientHelloID{}, false
	}
	return chromeProfiles[best], true
}

// ValidateImpersonateTarget 报告某个 impersonate 目标在当前构建里是否可用。
// 对应参考实现里对 curl_cffi 目标可用性的探测，只是这里无需真的发请求。
func ValidateImpersonateTarget(target string) bool {
	_, ok := tlsProfileForTarget(target)
	return ok
}

func chromeMajorFromTarget(target string) (int, bool) {
	t := strings.ToLower(strings.TrimSpace(target))
	if !strings.HasPrefix(t, "chrome") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(t, "chrome"))
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// TLSImpersonator 持有当前 ClientHello 目标，并可把它接到 http.Transport 上。
//
// 局限：Go 的 net/http 只在**直连 HTTPS** 时走 DialTLSContext；经由 HTTP/SOCKS
// 代理时会在 CONNECT 之后用 crypto/tls 完成握手，因而代理链路不会套用 utls 指纹。
// 这是有意的取舍——不为代理场景引入复杂的自定义握手，优先保证代理可用。
type TLSImpersonator struct {
	mu sync.RWMutex
	id utls.ClientHelloID
	ok bool

	// rootCAs / insecureSkipVerify 供测试注入（生产为 nil/false，走系统根证书）。
	rootCAs            *x509.CertPool
	insecureSkipVerify bool
}

// NewTLSImpersonator 用目标名创建；目标不可用时保持禁用状态。
func NewTLSImpersonator(target string) *TLSImpersonator {
	t := &TLSImpersonator{}
	t.SetTarget(target)
	return t
}

// SetTarget 切换目标。返回目标是否被支持。
func (t *TLSImpersonator) SetTarget(target string) bool {
	id, ok := tlsProfileForTarget(target)
	t.mu.Lock()
	t.id, t.ok = id, ok
	t.mu.Unlock()
	return ok
}

// Disable 关闭 TLS 伪装（不改动目标名）。
func (t *TLSImpersonator) Disable() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.ok = false
	t.mu.Unlock()
}

// Enabled 报告是否已启用 TLS 伪装。
func (t *TLSImpersonator) Enabled() bool {
	if t == nil {
		return false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.ok
}

func (t *TLSImpersonator) current() (utls.ClientHelloID, bool) {
	if t == nil {
		return utls.ClientHelloID{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.id, t.ok
}

// DialTLSContext 返回一个可用作 http.Transport.DialTLSContext 的函数：
// 先用 base 建立 TCP 连接，再用 utls 以 Chrome 指纹完成 TLS 握手。
func (t *TLSImpersonator) DialTLSContext(base *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if base == nil {
		base = &net.Dialer{}
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		raw, err := base.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		id, ok := t.current()
		if !ok {
			// 目标不可用：退回标准库 TLS，避免把明文连接当 TLS 用。
			raw.Close()
			return nil, errTLSDisabled
		}
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			host = addr
		}
		uconn := utls.UClient(raw, &utls.Config{
			ServerName:         host,
			NextProtos:         []string{"h2", "http/1.1"},
			RootCAs:            t.rootCAs,
			InsecureSkipVerify: t.insecureSkipVerify,
		}, id)
		if err := uconn.HandshakeContext(ctx); err != nil {
			raw.Close()
			return nil, err
		}
		return uconn, nil
	}
}

// Apply 把伪装接到 tr 上。未启用时不动 tr。
func (t *TLSImpersonator) Apply(tr *http.Transport, base *net.Dialer) {
	if tr == nil || !t.Enabled() {
		return
	}
	tr.DialTLSContext = t.DialTLSContext(base)
	// 让 net/http 在自定义 DialTLSContext 下仍自动装配 HTTP/2（Chrome 的 ALPN
	// 会协商 h2，没有 h2 处理链会握手成功但协议不匹配）。
	tr.ForceAttemptHTTP2 = true
}
