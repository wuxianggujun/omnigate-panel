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
	Account  string // 实际服务账号 label（api-key 上游为 "api-key"）
	// Usage 是上游回报的 OpenAI 格式 token 用量（raccoon 等会在末尾 chunk 带
	// usage；不回报时为 nil），供请求记录填充 Token/思考列。
	Usage *provider.Usage
	// Effort 是实际透传给上游的思考档位（reasoning_effort）。空 = 未请求或
	// 上游不支持。
	Effort string
	// Outcome 覆盖流式请求的结果口径：上游 error 帧 / 空流 → stream_error，
	// 客户端断连 → interrupted。空 = 按 HTTP 状态码推断（与 WorkBuddy 侧 event()
	// 同口径）。流式失败时 HTTP 头已发 200，靠它才能把假成功纠回来。
	Outcome string
	// Status 覆盖请求记录里的观测状态码（流式失败时用 5xx）。0 = 用实际写出的
	// 状态码。
	Status int
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
