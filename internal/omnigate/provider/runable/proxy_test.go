package runable

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// strictEmptySelector 模拟「严格模式的空代理池」：Next 直接返回错误，绝不回退直连。
type strictEmptySelector struct{ calls int }

var _ outbound.Selector = (*strictEmptySelector)(nil)

func (s *strictEmptySelector) Next() (*url.URL, error) {
	s.calls++
	return nil, errors.New("outbound: 代理池「test」无可用代理（严格模式不回退直连）")
}
func (s *strictEmptySelector) Report(*url.URL, error) {}
func (s *strictEmptySelector) MaxAttempts() int       { return 1 }

// TestProviderSetProxySelector 验证网关在 Provider 上安装的池选择器会真正生效：
// 严格空池下请求必须失败。历史上 SetProxySelector 只实现在 Client 上，网关对
// Provider 的类型断言静默失败，导致代理池从未生效、请求悄悄走了直连。
func TestProviderSetProxySelector(t *testing.T) {
	p := NewProvider("runable", "http://127.0.0.1:1", "")
	sel := &strictEmptySelector{}
	p.SetProxySelector(sel)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := p.StreamChat(ctx, &provider.Account{Cookie: "x"},
		provider.ChatInput{ChatID: "c", Model: "m", Prompt: "hi", Mode: "text"})
	if err == nil {
		t.Fatal("严格空池下请求应失败，却成功了（选择器未生效）")
	}
	if sel.calls == 0 {
		t.Fatal("池选择器未被调用（Provider.SetProxySelector 未转发到 Client）")
	}
	if !strings.Contains(err.Error(), "代理池") {
		t.Fatalf("错误应来自池选择器，得到: %v", err)
	}
}
