// omniconfig.go OmniGate 供应商配置页接口：读取 / 校验 / 保存内置 OmniGate 引擎
// 的供应商配置（omnigate.json），保存后由 main 注入的 SaveOmniConfig 闭包完成
// 进程内热重载（无需重启）。
//
// 分工与 config.go 一致：面板只做 HTTP 编排，校验/落盘/热重载都在 main。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// getOmniConfig 返回 OmniGate 当前供应商配置与文件路径。
func (p *Panel) getOmniConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	cfg, err := p.cfg.LoadOmniConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load omnigate config: "+err.Error())
		return
	}
	gen, err := toGeneric(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode omnigate config: "+err.Error())
		return
	}
	// 凭证只回显脱敏形态（前缀 + 长度）：providers[].api_key 与 accounts[].cookie /
	// password / refresh_token 等原始值绝不离开服务端（viewer 也能 GET 本接口）。
	// 保存时按「掩码 = 未改动」还原，见 saveOmniConfig 与 secret.go。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.OmniConfigPath,
		"config": maskSecrets(gen),
	})
}

// saveOmniConfig 保存 OmniGate 供应商配置：body 为完整配置 JSON。
// SaveOmniConfig 闭包内部完成校验+落盘+热重载；校验失败返回 400 且不写盘。
func (p *Panel) saveOmniConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// 前端整份回传配置，凭证字段是掩码：先按「掩码 = 未改动」用落盘配置还原，
	// 否则保存会把 cookie/password/api_key 写成 "sk-6d06…(len=48)" 这类掩码串。
	var incoming any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid config json: "+err.Error())
		return
	}
	restored, err := json.Marshal(restoreMaskedSecrets(incoming, p.cfg.LoadOmniConfig))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveOmniConfig(restored)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: OmniGate 供应商配置已保存（热重载完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}
