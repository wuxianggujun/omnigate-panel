// omni_accounts.go 统一账号池接口：把内置 OmniGate 各供应商的账号归一化后透出
// 给面板「账号池」视图（与 WorkBuddy 池同一张表展示，加「来源」列区分），并提供
// 单账号签到 / 移除。所有重活都在 main 注入的闭包里（进程内直连 OmniGate 运行时，
// 不经过 /omni/* HTTP 往返）。
//
// 分工与 omniconfig.go / outbound.go 一致：面板只做 HTTP 编排。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// omniAccounts 返回归一化的 OmniGate 账号（?balance=1 时实时查上游余额）。
func (p *Panel) omniAccounts(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniAccounts == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	withBalance := r.URL.Query().Get("balance") == "1"
	d, err := p.cfg.OmniAccounts(withBalance)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "list omnigate accounts: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": d})
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

// omniAccountCheckin 单账号签到（OmniGate）。
func (p *Panel) omniAccountCheckin(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniCheckin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	req, ok := decodeOmniAccountReq(w, r)
	if !ok {
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

// omniAccountRemove 移除 OmniGate 运行时账号。
func (p *Panel) omniAccountRemove(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniRemove == nil {
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
	d, err := p.cfg.OmniRemove(req.Provider, req.Label)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	log.Printf("panel: OmniGate 移除账号 %s/%s", req.Provider, req.Label)
	writeJSON(w, http.StatusOK, d)
}
