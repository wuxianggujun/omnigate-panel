// omni_accounts.go 统一账号池接口：把内置 OmniGate 各供应商的账号归一化后透出
// 给面板「账号池」视图（与 WorkBuddy 池同一张表展示，加「来源」列区分），并提供
// 单账号签到 / 移除。所有重活都在 main 注入的闭包里（进程内直连 OmniGate 运行时，
// 不经过 /omni/* HTTP 往返）。
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
// 未知结构原样返回（宁可多给也不崩）。
func filterOmniAccountList(d any, owned map[string]bool) any {
	raw, err := json.Marshal(d)
	if err != nil {
		return d
	}
	var m struct {
		Providers []struct {
			Name     string           `json:"name"`
			Type     string           `json:"type"`
			Accounts []map[string]any `json:"accounts"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return d
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

// omniAccountAddReq 面板侧「添加配置型 OmniGate 账号」请求体（runable 类：粘贴
// session_token Cookie 后落进 omnigate.json 的 providers[].accounts[]）。
type omniAccountAddReq struct {
	Provider string `json:"provider"`
	Label    string `json:"label"`
	Cookie   string `json:"cookie"`
}

// omniAccountAdd 服务端把账号写进 omnigate.json 并热生效。
//
// 为什么需要：配置型账号（runable）原先由前端整份保存 omnigate.json —— 那是管理员
// 级操作（会看到/覆盖别人的账号）。多租户下改为服务端定向追加，普通 user 只提供
// provider/label/cookie，看不到也改不动其它账号。
func (p *Panel) omniAccountAdd(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOmniConfig == nil || p.cfg.SaveOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req omniAccountAddReq
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Label = strings.TrimSpace(req.Label)
	if req.Provider == "" || req.Label == "" {
		writeErr(w, http.StatusBadRequest, "provider/label 不能为空")
		return
	}
	if !p.claimOmniLabel(r, req.Provider, req.Label) {
		writeErr(w, http.StatusConflict, "账号名「"+req.Label+"」已被其他用户占用，请换一个")
		return
	}
	if err := p.omniCfgAddAccount(req.Provider, req.Label, req.Cookie); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	log.Printf("panel: OmniGate 添加配置账号 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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

// omniCfgAddAccount 定向追加/替换一个配置账号。
func (p *Panel) omniCfgAddAccount(provider, label, cookie string) error {
	return p.omniCfgEditAccount(provider, func(accounts []any) ([]any, bool) {
		entry := map[string]any{"label": label}
		if cookie != "" {
			entry["cookie"] = cookie
		}
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

// omniAccountLoginReq 一键登录请求体（runable：服务端会话校验）。
type omniAccountLoginReq struct {
	Provider string `json:"provider"`
	Email    string `json:"email"`
	Password string `json:"password"`
	Cookie   string `json:"cookie"`
}

// omniAccountLogin 校验 runable 类账号：服务端会话探测（get-session）+ 取积分，返回
// 身份与积分。面板「浏览器登录」调用；成功后由前端把 session_token 写入 omnigate.json
// 并热生效。runable 只支持 Google/Facebook 登录、会话是 httpOnly Cookie（网页读不到），
// 所以只能由用户粘贴 session_token——面板无法自动抓取。
func (p *Panel) omniAccountLogin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniLogin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req omniAccountLoginReq
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Provider == "" {
		writeErr(w, http.StatusBadRequest, "provider 不能为空")
		return
	}
	d, err := p.cfg.OmniLogin(req.Provider, req.Email, req.Password, req.Cookie)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: OmniGate 登录校验 %s（%s）", req.Provider, req.Email)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": d})
}
