// omni_models.go 面板侧 OmniGate 模型目录只读代理。
//
// 为什么需要它：面板默认只读（匿名可看），而 OmniGate 的 /omni/v1/models 需要
// 网关 api_key；把 api_key 回显给匿名访问者等于把可用密钥公开。这里由面板进程
// 内直接向网关运行时取目录（与 /omni/v1/models 同源），匿名也能看模型与积分价，
// 但完全不需要、也不暴露 api_key。
package panel

import (
	"context"
	"net/http"
	"time"
)

// omniModels GET /panel/api/omni/models：返回 OmniGate 汇总模型目录。
func (p *Panel) omniModels(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniModels == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	models, err := p.cfg.OmniModels(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "omnigate models: "+err.Error())
		return
	}
	// 与 /omni/v1/models 同形：{object:"list", data:[...]}；前端只读 data。
	payload := map[string]any{
		"ok":     true,
		"object": "list",
		"data":   models,
	}
	// issues 说明「哪家目录这次没读到」。模型变少时不能让它们无声消失：有缓存的
	// 是「沿用上次成功目录」，没缓存的是「这家暂时看不到」。未装配回调时省略。
	if p.cfg.OmniModelIssues != nil {
		payload["issues"] = p.cfg.OmniModelIssues()
	}
	writeJSON(w, http.StatusOK, payload)
}
