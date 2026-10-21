// provider_build.go 供应商实例构造与「未保存配置」的模型目录探测。
//
// 背景：面板在「添加供应商」时希望先拉到上游模型清单再保存，而不是存下一个
// models 留空的供应商、只能等运行时自动发现。探测必须与网关真正构造运行时
// 供应商走同一条路径，否则会出现「探测得到、保存后拉不到」——所以这里把
// ProviderSpec 映射与 Factory 调用抽出来，供 gateway.New 与探测共用。
package gateway

import (
	"context"
	"fmt"
	"net/url"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/plugin"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/outbound"
)

// specOf 把供应商配置映射为插件工厂的 ProviderSpec。这是配置 → 供应商的
// 唯一映射点：网关启动与面板探测都经它，两边字段口径不会漂移。
func specOf(p *config.ProviderConfig) plugin.ProviderSpec {
	return plugin.ProviderSpec{
		Name:         p.Name,
		BaseURL:      p.BaseURL,
		DeviceHeader: p.DeviceHeader,
		APIKey:       p.APIKey,
		APIKeys:      p.APIKeys,
		Models:       p.Models,
		ZenKeys:      p.ZenKeys,
		GoKeys:       p.GoKeys,
		TierPrefer:   p.TierPrefer,
		GoBaseURL:    p.GoBaseURL,
		Anonymous:    p.AnonymousEnabled(),
		MainOrigin:   p.MainOrigin,
		LLMBase:      p.LLMBase,
		AuthBase:     p.AuthBase,
		AgentHost:    p.AgentHost,
		UgHost:       p.UgHost,
		OAuthHost:    p.OAuthHost,
	}
}

// newProvider 按描述符的 Factory 构造供应商实例。不做网关级的附属注入
// （指纹、代理、密钥健康存储等）——那些由 New 在构造后按类型补上。
func newProvider(p *config.ProviderConfig, deps plugin.Deps) (provider.Provider, error) {
	desc, err := plugin.Get(p.Type)
	if err != nil {
		return nil, fmt.Errorf("provider %q: %w", p.Name, err)
	}
	if desc.Factory == nil {
		return nil, fmt.Errorf("provider %q: 插件 %q 未提供 Factory", p.Name, p.Type)
	}
	prov, err := desc.Factory(specOf(p), deps)
	if err != nil {
		return nil, fmt.Errorf("provider %q: 构建失败: %w", p.Name, err)
	}
	return prov, nil
}

// ProbeProvider 用一份「尚未保存」的供应商配置拉取上游模型目录。它不注册进
// 网关、不写模型缓存，只做一次即用即弃的探测；出站代理沿用当前网关的解析器，
// 使「探测」与「保存后运行」走同一条出网路径。
//
// 返回的 models 即便 err 非空也可能非空：账号型上游（raccoon / trae / newapi）
// 无账号时会退回内置目录，这是「可用但不完整」的结果，由调用方决定是否采用。
func (g *Gateway) ProbeProvider(ctx context.Context, p config.ProviderConfig) ([]openai.Model, error) {
	prov, err := newProvider(&p, plugin.Deps{DataDir: g.cfg.DataDir, Logger: g.log.Warn})
	if err != nil {
		return nil, err
	}
	// 上游密钥池与 New 一样在构造后注入：openaibackend 的工厂不接收 api_keys
	// （只认单个 api_key），漏了这步「只填密钥池」的配置会探测成匿名请求。
	if len(p.APIKeys) > 0 {
		if ks, ok := prov.(interface{ SetAPIKeys([]string) }); ok {
			ks.SetAPIKeys(p.APIKeys)
		}
	}
	if g.poolFor != nil {
		if sel := g.poolFor(p.Name); sel != nil {
			if ps, ok := prov.(interface{ SetProxySelector(outbound.Selector) }); ok {
				ps.SetProxySelector(sel)
			}
		}
	}
	if g.proxyFor != nil {
		if u := g.proxyFor(p.Name); u != nil {
			if ps, ok := prov.(interface{ SetProxy(*url.URL) }); ok {
				ps.SetProxy(u)
			}
		}
	}
	return prov.ListModels(ctx)
}

// RefreshModels 绕过 TTL 重新拉取某供应商的上游目录并刷新缓存（面板「刷新
// 上游目录」）。失败时返回上游错误，同时附带最近一次成功目录供展示——调用方
// 据此提示「刷新失败，当前展示的是缓存」。
func (g *Gateway) RefreshModels(ctx context.Context, name string) ([]openai.Model, error) {
	if _, ok := g.providers[name]; !ok {
		return nil, fmt.Errorf("unknown provider %q", name)
	}
	models, err := g.fetchModels(ctx, name)
	if err != nil {
		if cached, ok := g.cachedModels(name); ok {
			return cached, err
		}
		return models, err
	}
	return models, nil
}
