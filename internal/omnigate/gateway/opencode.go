// opencode.go 面板对 opencode 供应商的管理入口：Go 套餐额度查询。
package gateway

import (
	"context"
	"fmt"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/opencode"
)

// OpencodeQuota 查询某 opencode 供应商全部 go 密钥的套餐额度（面板「额度」按钮）。
// 明文密钥不出 provider 层（结果只有掩码与指纹，见 opencode/quota.go）；一把密钥
// 失效不影响其它密钥的结果。zen 池不参与：按量付费没有套餐额度可查。
func (g *Gateway) OpencodeQuota(ctx context.Context, providerName string) (any, error) {
	pcfg := g.cfg.Provider(providerName)
	if pcfg == nil || pcfg.Type != "opencode" {
		return nil, fmt.Errorf("provider %q 不是 opencode 类型", providerName)
	}
	oc, ok := g.providers[providerName].(*opencode.Provider)
	if !ok {
		return nil, fmt.Errorf("provider %q 不是 opencode 类型", providerName)
	}
	keys, err := oc.GoQuota(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"provider": providerName,
		"keys":     keys,
	}, nil
}
