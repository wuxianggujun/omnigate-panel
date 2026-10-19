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
	"strings"
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
	// password / refresh_token 等原始值绝不离开服务端。顶层网关密钥 api_keys 只回显
	// 给管理员，viewer 与匿名都抹掉。保存时按「掩码 = 未改动」还原，见 secret.go。
	view := maskSecrets(gen)
	if !authFromContext(r.Context()).session.IsAdmin() {
		view = maskGatewayKeys(view)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"path":   p.cfg.OmniConfigPath,
		"config": view,
	})
}

// providerSecretFields 查看完整密钥端点允许回显的字段白名单（不是遍历全字段）：
// 覆盖 openai 型的单 key + 密钥池，与 opencode 的两个 tier 池。其余字段不经此端点。
var providerSecretFields = []string{"api_key", "api_keys", "zen_keys", "go_keys"}

// providerSecret 管理员查看某供应商的完整密钥（明文回显，全面板仅此一处）。
//
// 为什么独立成端点而不是把明文塞进 GET /panel/api/omni/config：配置 GET 是页面
// 常驻回显（每次打开面板都拉），明文塞进去等于把全部上游密钥常态性送到浏览器；
// 本端点只在管理员**显式点击「查看完整」**时按供应商取一次，响应 no-store 不落
// 缓存，服务端打审计日志。落盘配置始终是真密钥（掩码还原见 secret.go），所以这里
// 直接回显 LoadOmniConfig 的原值，不需要任何还原动作。
//
// 权限：p.admin 包装（未登录 401 / 非管理员 403）。viewer 只能继续看掩码。
func (p *Panel) providerSecret(w http.ResponseWriter, r *http.Request) {
	if p.cfg.LoadOmniConfig == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		name = strings.TrimSpace(r.URL.Query().Get("provider"))
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "missing name")
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
	root, _ := gen.(map[string]any)
	provs, _ := root["providers"].([]any)
	for _, pv := range provs {
		pm, _ := pv.(map[string]any)
		if pm == nil || pm["name"] != name {
			continue
		}
		secrets := map[string]any{}
		for _, f := range providerSecretFields {
			if v, ok := pm[f]; ok {
				secrets[f] = v
			}
		}
		if len(secrets) == 0 {
			writeErr(w, http.StatusNotFound, "该供应商没有配置密钥字段")
			return
		}
		log.Printf("panel: 管理员 %q 查看了供应商 %q 的完整密钥（%d 个字段）",
			authFromContext(r.Context()).session.Username, name, len(secrets))
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name, "secrets": secrets})
		return
	}
	writeErr(w, http.StatusNotFound, "no such provider: "+name)
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
