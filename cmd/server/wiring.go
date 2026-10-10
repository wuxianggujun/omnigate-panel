package main

import (
	"github.com/wuxianggujun/omnigate-panel/internal/pool"
	"github.com/wuxianggujun/omnigate-panel/internal/server"
)

// realmAwareAvailableForModel 构造会话粘性路由按模型可用口径的域感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"），也可能是不带
// 前缀的裸名。闭包用 RealmRouter 把模型名解析成**有序候选域列表**，再交给分池选号域过滤
// ——否则裸名取池子全集，global 号会被粘性分配给 CN 请求（跨 realm 泄漏）。
//
// 这里不传 provides（目录探测）：粘性分配不需要「该域是否提供该模型」的精确判定，粗略
// 按配置顺序给候选即可；真正的域过滤在请求路径（handler.realmsFor 带 provides）与粘性
// 校验（acct.Realm() ∈ realms）处兜底，粗分配不会造成跨域错配。
//
// rr 为 nil 时退化为老行为（resolveModel 剥前缀，裸名走 cn）。
func realmAwareAvailableForModel(p *pool.Pool, rr *server.RealmRouter) func(model string) []string {
	return func(model string) []string {
		if rr == nil {
			realm, bare := server.ResolveModel(model)
			return p.WeightedAvailableUIDsForModelRealm(bare, realm)
		}
		bare, realms := rr.Ordered(model, nil)
		return p.WeightedAvailableUIDsForModelRealms(bare, realms)
	}
}
