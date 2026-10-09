// outbound.go 出站代理配置页接口：读写 config.json 的 outbound 段（命名代理 +
// 「目标 → 代理」路由）。保存后由 main 注入的 SaveOutbound 闭包完成进程内热应用
// （面板上游 Transport 与内置 OmniGate 各供应商即时生效，无需重启）。
//
// 分工与 config.go / omniconfig.go 一致：面板只做 HTTP 编排，校验/落盘/热应用都在 main。
package panel

import (
	"io"
	"log"
	"net/http"
)

// getOutbound 返回当前出站代理配置（proxies + routes）。
func (p *Panel) getOutbound(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOutbound == nil {
		writeErr(w, http.StatusNotImplemented, "outbound config not available")
		return
	}
	cfg, err := p.cfg.LoadOutbound()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load outbound config: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"outbound": cfg,
	})
}

// saveOutbound 保存出站代理配置：body 为 {proxies:[...], routes:{...}}。
// SaveOutbound 闭包内部完成合并+校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveOutbound(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveOutbound == nil {
		writeErr(w, http.StatusNotImplemented, "outbound config not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveOutbound(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 出站代理配置已保存并热生效")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
