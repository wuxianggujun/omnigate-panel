package gateway

import "context"

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
