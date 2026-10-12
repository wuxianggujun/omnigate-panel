// Package fingerprint 移植 gemini2api 的「反检测与协议伪装」层。
//
// 它由五个互相配合的模块组成（与 Python 参考实现一一对应）：
//
//	config.go   —— 可热更新的浏览器指纹配置（Chrome 版本 / 平台 / 请求头顺序）
//	headers.go  —— 按 Chrome 真实顺序构建请求头，保证 UA / Sec-CH-UA / 版本三者一致
//	cookies.go  —— 带域名隔离、过期清理、删除语义的 Cookie 容器（可选持久化）
//	version.go  —— 后台同步 Chrome 稳定版版本号，避免指纹长期停留在旧版本
//	jitter.go   —— 请求间隔抖动，抹掉「机器节奏」特征
//	tls.go      —— 用 utls 伪造 Chrome 的 TLS ClientHello（JA3）
//
// 这些模块共同回答一个问题：怎样让本网关发往 Google 的流量，在 TLS 层与 HTTP 层
// 都尽可能接近一个真实 Chrome 浏览器，而不是一个「用 Go 写的爬虫」。
package fingerprint
