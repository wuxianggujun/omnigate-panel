package fingerprint

import (
	"context"
	"math/rand/v2"
	"time"
)

// 抖动区间（毫秒）：按场景区分，模拟人类操作间隔。
var jitterProfiles = map[string][2]int{
	"navigation":    {200, 800},
	"api_call":      {50, 300},
	"cookie_rotate": {1000, 3000},
}

// Jitter 按场景给请求之间加随机延迟，抹掉「机器节奏」特征。
type Jitter struct {
	Enabled bool
}

// Apply 按场景睡一段随机时长。抖动关闭时立即返回。ctx 取消时提前退出。
func (j Jitter) Apply(ctx context.Context, profile string) {
	if !j.Enabled {
		return
	}
	minMs, maxMs := 50, 200
	if p, ok := jitterProfiles[profile]; ok {
		minMs, maxMs = p[0], p[1]
	}
	if maxMs <= minMs {
		return
	}
	delay := time.Duration(minMs+rand.IntN(maxMs-minMs)) * time.Millisecond
	sleepCtx(ctx, delay)
}

// DelayFactor 返回 0.8–1.2 的随机因子，用于扰动固定间隔。抖动关闭时返回 1.0。
func (j Jitter) DelayFactor() float64 {
	if !j.Enabled {
		return 1.0
	}
	return 0.8 + rand.Float64()*0.4
}
