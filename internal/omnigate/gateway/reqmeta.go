package gateway

import (
	"context"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// ReqMeta 承载一次 chat 请求的路由结果，供请求记录使用。
//
// server 侧在进入 chat/responses 处理时创建并注入 ctx；gateway 侧在路由（命中
// 哪个 provider / 展示模型名）与选号（实际服务的账号）时回填；server 在请求出口
// 读回并写进 reqlog——与 WorkBuddy 网关共用同一份记录器，因此 OmniGate 的
// raccoon/runable 请求也会出现在面板「请求记录」页。零值字段表示「未知」。
type ReqMeta struct {
	Provider string // 命中的 provider 名（raccoon / runable / deepseek …）
	Model    string // 展示用模型名（displayModel，通常带 provider 前缀）
	// UpstreamModel 是**别名解析 + 目录补全之后**真正发给上游的模型 ID。
	//
	// Model 只能说明「客户端请求了什么」，说明不了「别名最后走到了哪」：别名
	// 命中后 Model 仍是客户端发来的那个名字，于是「别名到底生效没有」在请求记录
	// 里完全看不出来（实测过：别名被上游预改写成带前缀的 ID 后，日志与生效时
	// 一模一样，问题因此一直查不出来）。这里单独记真实目标。
	UpstreamModel string
	Account       string // 实际服务账号 label（api-key 上游为 "api-key"）
	// Usage 是上游回报的 OpenAI 格式 token 用量（raccoon 等会在末尾 chunk 带
	// usage；不回报时为 nil），供请求记录填充 Token/思考列。
	Usage *provider.Usage
	// Effort 是实际透传给上游的思考档位（reasoning_effort）。空 = 未请求或
	// 上游不支持。
	Effort string
	// Reasoning 本次上游是否产出过思考内容（收到过 reasoning 增量）。用于补齐
	// Usage 的空档：runable 会发 reasoning-delta 却不在流里回报 usage，于是
	// 「有没有想」可观测、「想了多少」不可观测。供请求记录「思考」列回填。
	Reasoning bool
	// Outcome 覆盖流式请求的结果口径：上游 error 帧 / 空流 → stream_error，
	// 客户端断连 → interrupted。空 = 按 HTTP 状态码推断（与 WorkBuddy 侧 event()
	// 同口径）。流式失败时 HTTP 头已发 200，靠它才能把假成功纠回来。
	Outcome string
	// Status 覆盖请求记录里的观测状态码（流式失败时用 5xx）。0 = 用实际写出的
	// 状态码。
	Status int
	// KeyProviders 是入站密钥的收窄范围（server 侧 keyScope 得到）。nil/空 =
	// 不限制（全量密钥或网关未配密钥）。非空时表示「这个 key 只准用这些
	// provider」——兜底选候选必须遵守它，否则收窄密钥能顺着引流链 / 全局兜底
	// 打到权限外的供应商。
	KeyProviders []string
}

// ProviderAllowed 报告某个 provider 是否在当前密钥的收窄范围内。范围为空时
// 恒为 true。供兜底挑选候选时使用。
func (m *ReqMeta) ProviderAllowed(name string) bool {
	if m == nil || len(m.KeyProviders) == 0 {
		return true
	}
	for _, p := range m.KeyProviders {
		if p == name {
			return true
		}
	}
	return false
}

type reqMetaKey struct{}

// WithReqMeta 把请求记账挂到 ctx 上。
func WithReqMeta(ctx context.Context, m *ReqMeta) context.Context {
	return context.WithValue(ctx, reqMetaKey{}, m)
}

// ReqMetaFrom 取出请求记账；未挂载时返回 nil。
func ReqMetaFrom(ctx context.Context) *ReqMeta {
	m, _ := ctx.Value(reqMetaKey{}).(*ReqMeta)
	return m
}
