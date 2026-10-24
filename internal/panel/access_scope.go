// access_scope.go 用户页的「按用户分配供应商权限」写入口。
//
// 权限的唯一事实来源仍是引擎配置里的 api_keys[].providers（空 = 全量）。这里不
// 引入第二套分组表：分组只是 UI 对「相同 providers 集合」的展示，避免出现
// 「配置里写了一份、分组表里又写了一份」的双写不一致。
//
// 为什么单独开一个接口而不是让前端走 /omni/config 全量保存：
//   - 用户页改的只是某个 name 下的 providers，没必要把整份配置（含上游凭证）
//     在浏览器里转一圈；
//   - newapi 导入经常出现同一个用户名下多把 token（同名多条 api_keys），
//     全量保存只能按索引改一条，这里按 name 一次改全，语义就是「这个用户」。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
)

// accessScopeRequest 是 POST /panel/api/access/scope 的请求体。
//
// Providers 为空 = 全量（不限制）。这是刻意保留的语义，与 config.APIKey.Allows
// 完全一致；前端必须在 UI 上明确写「清空 = 全量」，不能当成「禁止访问」。
type accessScopeRequest struct {
	Name      string   `json:"name"`
	Providers []string `json:"providers"`
}

// updateAccessScope 更新某个用户名下**所有** api_keys 的供应商范围。
//
// 只允许管理员调用（路由上已套 p.admin）。返回实际改动的密钥条数，方便前端提示
// 「同名的 3 把密钥一起更新了」。
func (p *Panel) updateAccessScope(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOmniConfig == nil || p.cfg.SaveOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}

	var req accessScopeRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name 不能为空")
		return
	}

	cfg, err := p.loadOmniConfigMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load omnigate config: "+err.Error())
		return
	}

	order := providerOrder(cfg)
	valid := make(map[string]bool, len(order))
	for _, n := range order {
		valid[n] = true
	}
	providers := normalizeProviderList(req.Providers, order)
	for _, n := range providers {
		if !valid[n] {
			writeErr(w, http.StatusBadRequest, "未知供应商: "+n)
			return
		}
	}

	list, _ := cfg["api_keys"].([]any)
	updated := 0
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		current, _ := obj["name"].(string)
		if strings.TrimSpace(current) != name {
			continue
		}
		if len(providers) == 0 {
			// 空 = 全量：删字段而不是写 []，让配置结构与「没配过」一致。
			delete(obj, "providers")
		} else {
			obj["providers"] = providers
		}
		updated++
	}
	if updated == 0 {
		writeErr(w, http.StatusNotFound, "配置里没有名为 "+name+" 的入站密钥")
		return
	}

	cfg["api_keys"] = list
	raw, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode omnigate config: "+err.Error())
		return
	}
	restart, err := p.cfg.SaveOmniConfig(raw)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "save omnigate config: "+err.Error())
		return
	}
	if restart == nil {
		restart = []string{}
	}
	log.Printf("panel: %s 把用户 %q 的供应商范围改为 %v（%d 把密钥）",
		authFromContext(r.Context()).session.Username, name, providers, updated)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"name":             name,
		"providers":        providers,
		"updated":          updated,
		"restart_required": restart,
	})
}

// normalizeProviderList 去空白、去重，并按配置里 providers 的声明顺序稳定排序；
// 未知名字排在已知名字之后（随后由 updateAccessScope 统一拒绝）。
func normalizeProviderList(in, order []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return sortByOrder(out, order)
}
