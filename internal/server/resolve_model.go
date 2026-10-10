package server

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 注意：本函数是**老行为**（裸名恒 cn）的单一实现，供 RealmRouter 未配置时回退；
// 配置了 realm_routing 后由 RealmRouter.Ordered 接管裸名的域选择（见 realm_router.go）。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	if r, b, ok := splitRealmPrefix(model); ok {
		return r, b
	}
	return RealmCN, model
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }
