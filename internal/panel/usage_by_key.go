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
	configuredKeys := p.configuredKeys()
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
	configured := make([]string, 0, len(configuredKeys))
	for _, k := range configuredKeys {
		configured = append(configured, k.Name)
		if seen[k.Name] {
			continue
		}
		seen[k.Name] = true
		rows = append(rows, reqlog.KeyUsage{
			Key:    k.Name,
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
		// configured_keys 比 configured 多带每把密钥的供应商范围，用户页据此
		// 展示「这个用户能访问哪些渠道」，并按范围自动分组。同名多把密钥
		// （newapi 一个用户多 token）合并成一行，范围取并集。
		"configured_keys": configuredKeys,
	})
}

// configuredKeyAccess 是用户页需要的「密钥 → 供应商范围」视图。它只暴露标签和
// 供应商名，绝不带 key 明文——用户页是 viewer 也能看的页面。
type configuredKeyAccess struct {
	Name      string   `json:"name"`
	Providers []string `json:"providers,omitempty"`
	// Mixed 表示同名多把密钥写了不同的范围；UI 需要提示「改一次会一起改」。
	Mixed bool `json:"mixed,omitempty"`
}

// configuredKeys 从引擎配置里取所有写了 name 的入站密钥及其供应商范围。
//
// 裸串形式的密钥（api_keys: ["sk-..."]）没有名字，无法与归档里的匿名摘要对应，
// 直接跳过；这种密钥在用户页本来也只能显示成 unnamed-xxxx，强行合并反而更乱。
//
// 同名多把密钥合并：全部是空范围（全量）时结果为全量；否则取并集。并集与网关
// 的实际授权口径一致——同一个人拿任意一把钥匙能摸到的供应商，就是他的总范围。
func (p *Panel) configuredKeys() []configuredKeyAccess {
	m, err := p.loadOmniConfigMap()
	if err != nil {
		return []configuredKeyAccess{}
	}
	order := providerOrder(m)
	list, _ := m["api_keys"].([]any)
	byName := make(map[string]*configuredKeyAccess, len(list))
	full := make(map[string]bool, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		provs, _ := stringList(obj["providers"])
		entry := byName[name]
		if entry == nil {
			entry = &configuredKeyAccess{Name: name}
			byName[name] = entry
		}
		if len(provs) == 0 {
			// 空范围 = 全量，优先级最高；同名里只要有一把全量，这个用户就是全量。
			// 但「一把全量 + 一把收窄」确实不一致，仍要标记 mixed，提示用户页
			// 「编辑后所有同名密钥会合并成同一组」。
			if len(entry.Providers) > 0 {
				entry.Mixed = true
			}
			full[name] = true
			continue
		}
		if full[name] {
			entry.Mixed = true
			continue
		}
		if len(entry.Providers) > 0 && !sameStringSet(entry.Providers, provs) {
			entry.Mixed = true
		}
		entry.Providers = unionStrings(entry.Providers, provs)
	}
	out := make([]configuredKeyAccess, 0, len(byName))
	for name, entry := range byName {
		if full[name] {
			entry.Providers = nil
		} else {
			entry.Providers = sortByOrder(entry.Providers, order)
		}
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// configuredKeyNames 保留给既有调用方/测试：只取合并后的名字列表。
func (p *Panel) configuredKeyNames() []string {
	keys := p.configuredKeys()
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Name)
	}
	return names
}

// providerOrder 取配置里 providers 的声明顺序，用于范围标签的稳定排序。
func providerOrder(m map[string]any) []string {
	list, _ := m["providers"].([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := obj["name"].(string)
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func stringList(v any) ([]string, bool) {
	switch list := v.(type) {
	case []string:
		return append([]string(nil), list...), true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
		return out, true
	default:
		return nil, false
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]bool, len(a))
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
	}
	return true
}

func unionStrings(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, s := range list {
			if s == "" || seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func sortByOrder(values, order []string) []string {
	if len(values) == 0 {
		return nil
	}
	rank := make(map[string]int, len(order))
	for i, name := range order {
		rank[name] = i
	}
	out := append([]string(nil), values...)
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i]]
		rj, jok := rank[out[j]]
		switch {
		case iok && jok:
			return ri < rj
		case iok:
			return true
		case jok:
			return false
		default:
			return out[i] < out[j]
		}
	})
	return out
}
