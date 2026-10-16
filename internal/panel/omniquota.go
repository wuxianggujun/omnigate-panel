// omniquota.go OpenCode 供应商的 Go 套餐额度接口（供应商行的「额度」按钮）。
//
// 分工与 omniconfig.go 一致：面板只做 HTTP 编排，额度查询/缓存/掩码都在
// OmniGate 的 provider 层（provider/opencode/quota.go），main 注入的 OmniQuota
// 闭包完成进程内取数。明文密钥全程不出 provider 层——这里拿到的是掩码 +
// 指纹 + 窗口百分比，与「查看完整密钥」（omniconfig.go 的 providerSecret）
// 是两条完全不同的路：那条是管理员显式取明文、每次审计留痕；这条是只读的
// 额度展示，viewer 也能看（与「上游供应商」配置页同权限）。
package panel

import (
	"net/http"
	"strings"
)

// omniProviderQuota 查询某 opencode 供应商的 Go 套餐额度。
//
// 响应 no-store：额度是实时上游数据，进缓存会让「刚刷新的额度」读成旧的。
// 查询失败回 502 并把 provider 层的可操作话术原样带给前端（「Go API Key 无效
// 或已被撤销」等）；没有配置 go 密钥回 200 空列表（这是「没得查」，不是错误）。
func (p *Panel) omniProviderQuota(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniQuota == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "missing name")
		return
	}
	res, err := p.cfg.OmniQuota(name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, res)
}
