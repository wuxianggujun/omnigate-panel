// omni_accounts.go 统一账号池接口：把各供应商的账号归一化后透出给面板
// 「供应商」视图，并提供单账号添加 / 签到 / 移除。所有重活都在 main 注入的
// 闭包里（进程内直连引擎运行时，不经过 HTTP 往返）。
//
// 分工与 omniconfig.go / outbound.go 一致：面板只做 HTTP 编排。
package panel

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/wuxianggujun/omnigate-panel/internal/panelauth"
)

// omniAccounts 返回归一化的 OmniGate 账号（?balance=1 时实时查上游余额）。
// 匿名只读访问者忽略 balance：实时余额要拿账号 cookie 打上游，不该由未登录访客触发。
// 多租户：admin/viewer 看全部；普通 user 只看自己拥有的账号（provider 元信息保留，
// 账号明细过滤）；未登录访客看不到任何账号明细（只看得到 WorkBuddy 的匿名统计）。
func (p *Panel) omniAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniAccounts == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	withBalance := r.URL.Query().Get("balance") == "1" && authed(r)
	d, err := p.cfg.OmniAccounts(withBalance)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list omnigate accounts: "+err.Error())
		return
	}
	owned, all := p.ownFilter(r)
	if !all {
		d = filterOmniAccountList(d, owned)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": d})
}

// filterOmniAccountList 按归属过滤 OmniAccounts 的返回值（providers[].accounts[]）。
// 解析失败时 fail-closed：返回空 providers，绝不把未过滤的全量数据交给普通用户。
func filterOmniAccountList(d any, owned map[string]bool) any {
	empty := map[string]any{"providers": []any{}}
	raw, err := json.Marshal(d)
	if err != nil {
		return empty
	}
	var m struct {
		Providers []struct {
			Name     string           `json:"name"`
			Type     string           `json:"type"`
			Accounts []map[string]any `json:"accounts"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return empty
	}
	for i := range m.Providers {
		prov := m.Providers[i].Name
		kept := make([]map[string]any, 0, len(m.Providers[i].Accounts))
		for _, a := range m.Providers[i].Accounts {
			label, _ := a["label"].(string)
			if label != "" && owned[omniOwnerKey(prov, label)] {
				kept = append(kept, a)
			}
		}
		m.Providers[i].Accounts = kept
	}
	return m
}

// omniAccountReq 单账号运维请求体。
type omniAccountReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
}

func decodeOmniAccountReq(w http.ResponseWriter, r *http.Request) (omniAccountReq, bool) {
	var req omniAccountReq
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return req, false
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return req, false
	}
	if req.Provider == "" {
		writeErr(w, http.StatusBadRequest, "provider 不能为空")
		return req, false
	}
	return req, true
}

// omniAccountCheckin 单账号签到（OmniGate）。普通用户只能签到自己的账号。
func (p *Panel) omniAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniCheckin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	req, ok := decodeOmniAccountReq(w, r)
	if !ok {
		return
	}
	if !p.requireOmni(w, r, req.Provider, req.Label) {
		return
	}
	d, err := p.cfg.OmniCheckin(req.Provider, req.Label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: OmniGate 签到 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, d)
}

// omniAccountRemove 移除 OmniGate 账号：先尝试运行时账号（raccoon/TRAE 的 dyn
// account），未命中则回退到配置账号（runable 等写在 omnigate.json 里的账号）。
// 普通用户只能移除自己的账号。
func (p *Panel) omniAccountRemove(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniRemove == nil && p.cfg.SaveOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	req, ok := decodeOmniAccountReq(w, r)
	if !ok {
		return
	}
	if req.Label == "" {
		writeErr(w, http.StatusBadRequest, "label 不能为空")
		return
	}
	if !p.requireOmni(w, r, req.Provider, req.Label) {
		return
	}
	removed := false
	if p.cfg.OmniRemove != nil {
		d, err := p.cfg.OmniRemove(req.Provider, req.Label)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
		removed = jsonBoolField(d, "removed")
	}
	if !removed {
		if err := p.omniCfgRemoveAccount(req.Provider, req.Label); err != nil {
			writeErr(w, http.StatusBadGateway, err.Error())
			return
		}
	}
	p.ownership.Remove(omniOwnerKey(req.Provider, req.Label))
	log.Printf("panel: OmniGate 移除账号 %s/%s（运行时=%v）", req.Provider, req.Label, removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": true})
}

// jsonBoolField 从任意 JSON 值里取布尔字段（缺省 false）。
func jsonBoolField(v any, field string) bool {
	raw, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	b, _ := m[field].(bool)
	return b
}

// 账号归属冲突的两种可区分错误（HTTP 409）。
var (
	errAccountTaken  = errors.New("账号名已被其他用户占用，请换一个")
	errAccountShared = errors.New("账号名由管理员维护，普通用户不能覆盖")
)

// omniAccountAdd 服务端把账号写进 omnigate.json 并热生效。
//
// 请求体是插件 account_schema 生成的任意字段集合：provider / label 之外
// （access_token / refresh_token / cookie / api_key / uid / machine_id ...）
// 原样写入 providers[].accounts[]，不再只支持 cookie 一个字段。
func (p *Panel) omniAccountAdd(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOmniConfig == nil || p.cfg.SaveOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	body, err := decodeAccountBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := strings.TrimSpace(stringValue(body, "provider"))
	label := strings.TrimSpace(stringValue(body, "label"))
	if provider == "" || label == "" {
		writeErr(w, http.StatusBadRequest, "provider/label 不能为空")
		return
	}
	entry := accountEntry(body, label)
	if err := p.saveAccountForRequest(r, provider, entry); err != nil {
		if errors.Is(err, errAccountTaken) || errors.Is(err, errAccountShared) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: 添加配置账号 %s/%s", provider, label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// decodeAccountBody 读取账号请求体（限制 64KiB）。
func decodeAccountBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		return nil, errors.New("read body: " + err.Error())
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errors.New("invalid body: " + err.Error())
	}
	return body, nil
}

func stringValue(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// accountEntry 把请求体归一化成配置账号条目：label 固定，其余非空字符串保留，
// provider 不进入账号对象。
func accountEntry(body map[string]any, label string) map[string]any {
	entry := map[string]any{"label": label}
	for k, v := range body {
		if k == "provider" || k == "label" {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		entry[k] = v
	}
	return entry
}

// saveAccountForRequest 统一「归属校验 → 认领 → 写入配置」路径。
func (p *Panel) saveAccountForRequest(r *http.Request, provider string, entry map[string]any) error {
	label, _ := entry["label"].(string)
	if strings.TrimSpace(label) == "" {
		return errors.New("label 不能为空")
	}
	if roleOf(r) == panelauth.RoleUser {
		key := omniOwnerKey(provider, label)
		if p.omniCfgHasAccount(provider, label) && p.ownership.Owner(key) == "" {
			return errAccountShared
		}
	}
	if !p.claimOmniLabel(r, provider, label) {
		return errAccountTaken
	}
	return p.omniCfgAddAccount(provider, entry)
}

// omniCfgEditAccount 读 omnigate.json → 回调改写 providers[].accounts[] → 保存热生效。
func (p *Panel) omniCfgEditAccount(provider string, edit func(accounts []any) ([]any, bool)) error {
	cfg, err := p.cfg.LoadOmniConfig()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	provs, _ := m["providers"].([]any)
	found := false
	for i, pv := range provs {
		pm, _ := pv.(map[string]any)
		if pm == nil {
			continue
		}
		if name, _ := pm["name"].(string); name != provider {
			continue
		}
		found = true
		accts, _ := pm["accounts"].([]any)
		next, changed := edit(accts)
		if !changed {
			return errors.New("未找到该账号（label 不匹配）")
		}
		pm["accounts"] = next
		provs[i] = pm
		break
	}
	if !found {
		return errors.New("找不到供应商 " + provider)
	}
	m["providers"] = provs
	out, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := p.cfg.SaveOmniConfig(out); err != nil {
		return err
	}
	return nil
}

// omniCfgAddAccount 定向追加/替换一个配置账号（entry 由 account_schema 字段组成）。
func (p *Panel) omniCfgAddAccount(provider string, entry map[string]any) error {
	label, _ := entry["label"].(string)
	return p.omniCfgEditAccount(provider, func(accounts []any) ([]any, bool) {
		for i, a := range accounts {
			am, _ := a.(map[string]any)
			if am == nil {
				continue
			}
			if l, _ := am["label"].(string); l == label {
				accounts[i] = entry
				return accounts, true
			}
		}
		return append(accounts, entry), true
	})
}

// omniCfgHasAccount 报告配置里是否已存在同 label 的账号（用于阻止普通用户
// 覆盖管理员维护的无主账号）。
func (p *Panel) omniCfgHasAccount(provider, label string) bool {
	if p.cfg.LoadOmniConfig == nil {
		return false
	}
	cfg, err := p.cfg.LoadOmniConfig()
	if err != nil {
		return false
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return false
	}
	var m struct {
		Providers []struct {
			Name     string `json:"name"`
			Accounts []struct {
				Label string `json:"label"`
			} `json:"accounts"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	for _, pv := range m.Providers {
		if pv.Name != provider {
			continue
		}
		for _, a := range pv.Accounts {
			if a.Label == label {
				return true
			}
		}
	}
	return false
}

// omniCfgRemoveAccount 定向删除一个配置账号。
func (p *Panel) omniCfgRemoveAccount(provider, label string) error {
	return p.omniCfgEditAccount(provider, func(accounts []any) ([]any, bool) {
		out := make([]any, 0, len(accounts))
		found := false
		for _, a := range accounts {
			am, _ := a.(map[string]any)
			if am != nil {
				if l, _ := am["label"].(string); l == label {
					found = true
					continue
				}
			}
			out = append(out, a)
		}
		if !found {
			return accounts, false
		}
		return out, true
	})
}

// omniAccountLogin 校验账号并（label 非空时）直接落盘：服务端探测会话 +
// 取积分，成功后把同一份字段写进 omnigate.json 并热生效。面板的
// 「登录校验并添加」按钮因此是一条原子路径，不再依赖前端二次保存。
func (p *Panel) omniAccountLogin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniLogin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	body, err := decodeAccountBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	provider := strings.TrimSpace(stringValue(body, "provider"))
	if provider == "" {
		writeErr(w, http.StatusBadRequest, "provider 不能为空")
		return
	}
	label := strings.TrimSpace(stringValue(body, "label"))
	d, err := p.cfg.OmniLogin(provider,
		stringValue(body, "email"), stringValue(body, "password"), stringValue(body, "cookie"))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	saved := false
	if label != "" && p.cfg.LoadOmniConfig != nil && p.cfg.SaveOmniConfig != nil {
		if err := p.saveAccountForRequest(r, provider, accountEntry(body, label)); err != nil {
			if errors.Is(err, errAccountTaken) || errors.Is(err, errAccountShared) {
				writeErr(w, http.StatusConflict, err.Error())
				return
			}
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		saved = true
	}
	log.Printf("panel: 登录校验 %s（%s），已保存=%v", provider, stringValue(body, "email"), saved)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": d, "saved": saved})
}
