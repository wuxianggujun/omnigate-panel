// omni_provider_models.go 面板侧「供应商模型目录」探测 / 刷新接口。
//
// 两件事：
//
//	probe   — 添加供应商时，用表单里尚未保存的配置先探一次上游 /models，让用户
//	          勾选后再写入 models 字段（而不是存一个留空、只能等运行时自动发现）。
//	refresh — 已有供应商的「模型」页手动绕过 10 分钟 TTL 重新拉取上游目录。
//
// 分工与 omniconfig.go 一致：面板只做 HTTP 编排，真正的工厂构造与出网在 main
// 注入的闭包里（进程内直连引擎运行时）。明文密钥不出服务端：probe 的配置来自
// 面板表单（含用户刚填的明文），服务端用完即弃，不落库、不回显。
package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// providerModelsProbeMaxBody 探测请求体上限（单个供应商配置，含密钥池）。
const providerModelsProbeMaxBody = 1 << 20

// omniProbeModels POST /panel/api/omni/provider-models/probe
// body: {"provider": { ... 供应商配置 ... }}。返回 {models:[...]}；账号型上游无
// 账号时返回内置降级目录并带 warning。
func (p *Panel) omniProbeModels(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniProbeModels == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req struct {
		Provider json.RawMessage `json:"provider"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, providerModelsProbeMaxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if len(req.Provider) == 0 {
		writeErr(w, http.StatusBadRequest, "missing provider")
		return
	}
	res, err := p.cfg.OmniProbeModels(req.Provider)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "probe models: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": res})
}

// omniRefreshModels POST /panel/api/omni/provider-models/refresh
// body: {"provider": "name"}。绕过 TTL 重新拉取并刷新缓存。
func (p *Panel) omniRefreshModels(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniRefreshModels == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	var req struct {
		Provider string `json:"provider"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Provider)
	if name == "" {
		writeErr(w, http.StatusBadRequest, "missing provider")
		return
	}
	res, err := p.cfg.OmniRefreshModels(name)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "refresh models: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": res})
}
