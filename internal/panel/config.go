// config.go 面板配置页接口：读取当前配置、校验并保存（热生效 + 重启项标注）。
//
// 分工：cmd/server 持有 Config 类型与校验逻辑（Load/normalize），此处只做
// HTTP 编排——GET 回显、POST 透传给注入的 SaveConfig 闭包（由 main 完成
// "校验 → 落盘 → 热应用 → 返回需重启字段列表"）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
)

// getConfig 返回当前配置文件内容与路径（前端按 schema 渲染表单）。
func (p *Panel) getConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	cfg, err := p.cfg.LoadConfig()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "load config: "+err.Error())
		return
	}
	gen, err := toGeneric(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}
	// 凭证（upstash.token / upstream.device_token 等）只回显脱敏形态；顶层 api_key
	// 刻意跳过：面板前端要用它调内置 OmniGate 的 /omni/*，见 secret.go 注释。
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.ConfigPath,
		"config": maskSecrets(gen, "api_key"),
	})
}

// saveConfig 保存配置：body 直接是配置 JSON（前端按 schema 组装完整对象）。
// SaveConfig 闭包内部完成校验+落盘+热应用；校验失败返回 400 且不写盘。
func (p *Panel) saveConfig(w http.ResponseWriter, r *http.Request) {
	if p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	// 表单可能把脱敏后的凭证原样回传：按「掩码 = 未改动」从落盘配置还原。
	var incoming any
	if err := json.Unmarshal(raw, &incoming); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid config json: "+err.Error())
		return
	}
	restored, err := json.Marshal(restoreMaskedSecrets(incoming, p.cfg.LoadConfig))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "encode config: "+err.Error())
		return
	}
	restartRequired, err := p.cfg.SaveConfig(restored)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if restartRequired == nil {
		restartRequired = []string{}
	}
	log.Printf("panel: 配置已保存（热生效完成；需重启字段 %d 个）", len(restartRequired))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"restart_required": restartRequired,
	})
}

// regenerateKey 重新生成网关 api_key（仅管理员）：轮换密钥防泄漏被滥用。
// RegenerateKey 闭包内部完成「生成 → 落盘 → 热生效（含内置 OmniGate 复用的
// api_keys）」，并把新 key 明文返回给前端展示一次；此后面板不再回显它。
func (p *Panel) regenerateKey(w http.ResponseWriter, r *http.Request) {
	if p.cfg.RegenerateKey == nil {
		writeErr(w, http.StatusNotImplemented, "regenerate key not available")
		return
	}
	key, err := p.cfg.RegenerateKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "regenerate key: "+err.Error())
		return
	}
	log.Printf("panel: 网关 api_key 已重新生成（操作人 %s）", authFromContext(r.Context()).session.Username)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "api_key": key})
}
