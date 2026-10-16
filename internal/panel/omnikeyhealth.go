// omnikeyhealth.go 上游密钥池健康状态接口（自动禁用 / 连击记账 / 自动清理倒计时）。
//
// 与 omniquota.go 同口径：面板只做 HTTP 编排，数据来自 main 注入的 OmniKeyHealth
// 闭包（进程内运行时，manager 侧组装）。回显的是掩码密钥 + 状态，绝无明文；
// viewer 也能看（与「上游供应商」配置页同权限）。no-store：一次额度失败就会
// 改变禁用标记，缓存读到的是旧世界。
package panel

import "net/http"

// omniKeyHealth 返回各供应商上游密钥池的健康状态：谁被自动禁用了（额度耗尽）、
// 连败几次、何时会被自动清理。OmniGate 未启用时 501。
func (p *Panel) omniKeyHealth(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniKeyHealth == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	res, err := p.cfg.OmniKeyHealth()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}
