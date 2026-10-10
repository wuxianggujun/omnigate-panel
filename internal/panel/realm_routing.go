// realm_routing.go 域路由配置页接口：读写 config.json 的 realm_routing 段
//（域优先级 order + 模型优先域规则 prefer）。保存后由 main 注入的 SaveRealmRouting
// 闭包完成「校验 → 落盘 → 热更新同一 *RealmRouter」（请求路径即时生效，无需重启）。
//
// 分工与 outbound.go 一致：面板只做 HTTP 编排，校验/落盘/热更新都在 main。
package panel

import (
	"io"
	"log"
	"net/http"
)

// getRealmRouting 返回当前域路由配置（order + prefer）。
func (p *Panel) getRealmRouting(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadRealmRouting == nil {
		writeErr(w, http.StatusNotImplemented, "realm routing config not available")
		return
	}
	cfg, err := p.cfg.LoadRealmRouting()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load realm routing: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"realm_routing": cfg,
	})
}

// saveRealmRouting 保存域路由配置：body 为 {order:[...], prefer:{...}}。
// SaveRealmRouting 闭包内部完成合并+校验+落盘+热更新；校验失败返回 400 且不写盘。
func (p *Panel) saveRealmRouting(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveRealmRouting == nil {
		writeErr(w, http.StatusNotImplemented, "realm routing config not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveRealmRouting(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 域路由配置已保存并热生效")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
