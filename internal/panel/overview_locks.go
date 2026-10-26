package panel

// overview_locks.go 把「自动禁用中的上游密钥」提炼成总览卡片需要的形状。
//
// 数据来自主进程注入的 OmniKeyHealth（与 /panel/api/omni/key-health 同源），
// 这里只保留「哪家供应商、几个密钥、什么原因、何时恢复」，不外泄掩码与错误原文。
// 返回 nil 表示「读不到」（未装配回调 / 上游报错），与「读到且为空」区分开：
// 前者前端必须说「不可见」，不能顺口说「没有锁」。

import (
	"encoding/json"
	"sort"
	"time"
)

// lockedKeyRow 是 key-health 回调返回结构里本文件关心的部分。
type lockedKeyRow struct {
	Key      string `json:"key"`
	Disabled bool   `json:"disabled"`
	Reason   string `json:"reason"`
	Until    int64  `json:"until"`
}

type lockedKeyProvider struct {
	Name string         `json:"name"`
	Keys []lockedKeyRow `json:"keys"`
}

type keyHealthPayload struct {
	Providers []lockedKeyProvider `json:"providers"`
}

// lockedUpstreamKeys 汇总当前被自动禁用的上游密钥。nil = 读不到。
func (p *Panel) lockedUpstreamKeys() []map[string]any {
	if p.cfg.OmniKeyHealth == nil {
		return nil
	}
	res, err := p.cfg.OmniKeyHealth()
	if err != nil || res == nil {
		return nil
	}
	// 回调返回 any，经 JSON 往返归一成结构体（与 key-health 端点的序列化同源）。
	raw, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	var payload keyHealthPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	now := time.Now()
	out := make([]map[string]any, 0, len(payload.Providers))
	for _, prov := range payload.Providers {
		count := 0
		var until int64
		var kind string
		for _, k := range prov.Keys {
			if !k.Disabled {
				continue
			}
			// 已过禁用截止的不算「当前锁定」：网关会在下一次使用时自动复活它，
			// 卡片继续挂着只会让人以为还没恢复。
			if k.Until > 0 && k.Until <= now.Unix() {
				continue
			}
			count++
			if k.Reason != "" && kind == "" {
				kind = lockKind(k.Reason)
			}
			if k.Until > until {
				until = k.Until
			}
		}
		if count == 0 {
			continue
		}
		out = append(out, map[string]any{
			"provider": prov.Name,
			"keys":     count,
			"kind":     kind,
			"until":    until,
		})
	}
	// 恢复时间早的排前面：最急的事在最上面。没有恢复时间的排最后。
	sort.SliceStable(out, func(i, j int) bool {
		ui, _ := out[i]["until"].(int64)
		uj, _ := out[j]["until"].(int64)
		if ui == 0 {
			return false
		}
		if uj == 0 {
			return true
		}
		return ui < uj
	})
	return out
}

// lockKind 把上游失败分类（openaibackend.Health.Reason）映成卡片上的词。
// 未知分类原样返回，宁可显示一个生词，也不要把不认识的失败说成「限流」。
func lockKind(reason string) string {
	switch reason {
	case "quota":
		return "quota"
	case "rate":
		return "rate_limit"
	case "auth":
		return "auth"
	default:
		return reason
	}
}
