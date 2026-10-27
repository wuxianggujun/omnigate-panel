// keyusage.go 按「入站密钥」聚合请求记录。
//
// 为什么单独一个文件：这是**可选能力**，只有能跑 SQL 的后端（PostgreSQL）才实现。
// 文件归档后端只能逐条扫 JSONL，规模一大就会把请求路径拖慢，因此不实现——
// 面板拿到 ErrKeyUsageUnsupported 时显示「当前存储不支持」而不是假装空数据。
//
// 维度选择：按 api_keys[].name 聚合，而不是按上游账号（account）或客户端 IP。
//   - account 是「上游谁在服务」，一个用户可能落到多个上游账号，按它统计会把
//     同一个人拆成好几行；
//   - client_ip 在反向代理后面是代理地址，直连时又随网络漂移，都不是身份；
//   - 入站密钥标签才是「我们自己发给谁的凭证」，也是唯一能对账的维度。
package reqlog

import (
	"errors"
	"time"
)

// ErrKeyUsageUnsupported 表示当前归档后端不支持按密钥聚合。
var ErrKeyUsageUnsupported = errors.New("reqlog: 当前归档后端不支持按入站密钥统计")

// KeyUsageModel 是某个密钥在单个模型上的用量。
//
// 保留逐模型拆分是为了回答「这个用户到底在用什么」：只看总量无法区分
// 「一个用户跑满 deepseek」和「一个用户每次都试不同模型」，而这两种情况
// 在容量规划和排障上的处理方式完全不同。
type KeyUsageModel struct {
	Model            string `json:"model"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
}

// KeyUsage 是按入站密钥聚合的一行。
//
// Errors 计数口径与请求记录页一致：取 `ok = false`，即 HTTP 非 2xx，或流式
// 假成功被 outcome 纠正后的失败。客户端主动中断（interrupted）同样计入
// Errors——它确实没拿到完整结果，但 token 已经在上游消耗掉了，不能当成功。
type KeyUsage struct {
	Key              string `json:"key"`
	Requests         int64  `json:"requests"`
	Errors           int64  `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	TotalTokens      int64  `json:"total_tokens"`
	// AvgDurationMs 含失败请求的端到端平均耗时；0 表示没有样本。
	AvgDurationMs int64           `json:"avg_duration_ms"`
	LastSeen      time.Time       `json:"last_seen"`
	Models        []KeyUsageModel `json:"models"`
}

// KeyUsageSink 是 Sink 的可选扩展：按入站密钥聚合 [from, to] 区间内的请求。
//
// 用独立接口而不是塞进 Sink：加方法会破坏所有既有实现（测试里的内存 sink
// 也要跟着改），而聚合本来就只对能下推 SQL 的后端有意义。调用方用类型断言
// 探测能力，缺失时自然降级。
type KeyUsageSink interface {
	AggregateKeyUsage(from, to time.Time) ([]KeyUsage, error)
}

// KeyUsage 返回 [from, to] 区间内按入站密钥聚合的用量，按请求数降序。
//
// 只统计 api_key 非空的记录：开放模式（未配任何密钥）与旧归档条目没有身份，
// 混进来会变成一行巨大的 "（未知）"，把真正可对账的用户数据压没了。
func (r *Recorder) KeyUsage(from, to time.Time) ([]KeyUsage, error) {
	if r == nil || r.archive == nil || r.archive.sink == nil {
		return nil, ErrKeyUsageUnsupported
	}
	ks, ok := r.archive.sink.(KeyUsageSink)
	if !ok {
		return nil, ErrKeyUsageUnsupported
	}
	return ks.AggregateKeyUsage(from, to)
}
