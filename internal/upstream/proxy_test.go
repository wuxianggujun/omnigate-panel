package upstream

import (
	"net/http"
	"net/url"
	"testing"
)

type fakeSel struct{}

func (fakeSel) Next() (*url.URL, error) { return nil, nil }
func (fakeSel) Report(*url.URL, error)  {}
func (fakeSel) MaxAttempts() int        { return 1 }

// TestSetProxySelectorAppliesAndResets 验证面板上游（WorkBuddy）能安装/卸载代理池：
// 安装后 HTTP 与 ChatHTTP 共享同一包装 transport；卸载后还原为基础 transport。
func TestSetProxySelectorAppliesAndResets(t *testing.T) {
	c := New()
	base, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("期望基础 transport 为 *http.Transport，得到 %T", c.HTTP.Transport)
	}
	if c.baseTr != base {
		t.Fatal("New() 应记录 baseTr")
	}

	c.SetProxySelector(fakeSel{})
	if c.HTTP.Transport == base {
		t.Fatal("安装代理池后不应还是基础 transport")
	}
	if c.HTTP.Transport != c.ChatHTTP.Transport {
		t.Fatal("HTTP 与 ChatHTTP 应共享同一包装后的 transport")
	}

	c.SetProxySelector(nil)
	if c.HTTP.Transport != base || c.ChatHTTP.Transport != base {
		t.Fatal("卸载代理池后应还原为基础 transport")
	}
}
