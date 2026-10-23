// usage_by_key.go 面板「用户」页的数据源：按入站密钥聚合请求记录。
//
// 与「用量」页的分工：
//   - 用量页回答「这个模型/供应商被用了多少」，维度是上游资源；
//   - 用户页回答「谁用了多少」，维度是我们发出去的凭证（api_keys[].name）。
//
// 数据来自 request_logs 的 SQL 聚合（reqlog.KeyUsageSink），因此只覆盖请求
// 记录的保留窗口。这里不做任何「补偿估算」：宁可显示「近 7 天」并在页面上
// 写明口径，也不把不同窗口的数字摆在同一个表里让人误读。
package panel

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/reqlog"
)

// usageByKeyWindow 是未显式指定区间时的默认窗口。与请求记录默认保留期一致，
// 保证「默认看到的」总是「一定还在库里的」。
const usageByKeyWindow = 7 * 24 * time.Hour

func (p *Panel) usageByKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RequestLog == nil {
		writeErr(w, http.StatusNotImplemented, "请求记录不可用")
		return
	}
	q := r.URL.Query()
	to := parseTimeParam(q.Get("to"))
	from := parseTimeParam(q.Get("from"))
	if to.IsZero() {
		to = time.Now()
	}
	if from.IsZero() {
		from = to.Add(-usageByKeyWindow)
	}
	if from.After(to) {
		writeErr(w, http.StatusBadRequest, "from 晚于 to")
		return
	}

	rows, err := p.cfg.RequestLog.KeyUsage(from, to)
	if err != nil {
		if errors.Is(err, reqlog.ErrKeyUsageUnsupported) {
			writeErr(w, http.StatusNotImplemented,
				"当前归档后端不支持按密钥统计（需要 PostgreSQL 请求记录）")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 已配置但本窗口内没有流量的密钥也要出现：用户页的第一用途是「对账」，
	// 只显示有流量的人会让运营以为某把密钥不存在，实际是刚发出去还没用。
	// 顺序上把这些零流量行排在最后，不打扰正常阅读。
	configured := p.configuredKeyNames()
	if rows == nil {
		rows = []reqlog.KeyUsage{}
	}
	seen := make(map[string]bool, len(rows))
	for i := range rows {
		seen[rows[i].Key] = true
		if rows[i].Models == nil {
			rows[i].Models = []reqlog.KeyUsageModel{}
		}
	}
	for _, name := range configured {
		if seen[name] {
			continue
		}
		seen[name] = true
		rows = append(rows, reqlog.KeyUsage{
			Key:    name,
			Models: []reqlog.KeyUsageModel{},
		})
	}

	// 合计按「已观测」行算：零流量行不能把活跃用户数算进去。
	var total reqlog.KeyUsage
	for _, row := range rows {
		total.Requests += row.Requests
		total.Errors += row.Errors
		total.PromptTokens += row.PromptTokens
		total.CompletionTokens += row.CompletionTokens
		total.TotalTokens += row.TotalTokens
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"from":  from.UTC().Format(time.RFC3339),
		"to":    to.UTC().Format(time.RFC3339),
		"rows":  rows,
		"total": total,
		// 归档保留天数：前端据此把「N 天内没有任何流量」和「数据已被清理」
		// 区分开，否则用户会把「查不到」当成「没用过」。
		"retention_days": p.cfg.RequestLog.RetentionDays(),
		// 让前端能区分「配置里有这把钥匙」与「只见过历史流量」——后者常见于
		// 密钥已删除但归档还在，标出来以免对账时找不到人。
		"configured": configured,
	})
}

// configuredKeyNames 从引擎配置里取所有写了 name 的入站密钥标签。
//
// 裸串形式的密钥（api_keys: ["sk-..."]）没有名字，无法与归档里的匿名摘要对应，
// 直接跳过；这种密钥在用户页本来也只能显示成 unnamed-xxxx，强行合并反而更乱。
func (p *Panel) configuredKeyNames() []string {
	m, err := p.loadOmniConfigMap()
	if err != nil {
		return nil
	}
	list, _ := m["api_keys"].([]any)
	names := make([]string, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
