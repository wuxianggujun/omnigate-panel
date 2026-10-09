// accounts_io.go 账号迁移：把两个账号池（WorkBuddy 面板账号池 + OmniGate
// 供应商账号）打成一个 JSON 包，一次导出 / 一次导入，用于备份与跨机迁移。
//
// 与 import.go（cockpit tools 单格式导入）互补：这里用的是**本服务原生格式**，
// 导出后原样导回即可无损往返（含 realm / device_token 等字段）。
//
//   - WorkBuddy 池：直接读 auths/workbuddy*.json 原样导出；导入走
//     auth.Parse → SaveAtomic → pool.Add，与登录/重启后的目录天然对齐。
//   - OmniGate 池：DynAccount（网页登录运行时添加的账号）按 provider 分组，
//     经 main 注入的 Export/ImportOmniAccounts 闭包读写；导入后热重建网关即生效。
//
// 安全：包里含 access/refresh token，接口与 /panel/api/* 同口径（Bearer api_key）。
package panel

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/auth"
)

// accountsBundle 账号迁移包。
//
// WorkBuddy 用 []json.RawMessage 保存**原始 auth 文件字节**：Auth 的 realm 字段
// 未导出、无自定义 Marshal，直接 marshal *Auth 会丢 realm，故原样透传。
type accountsBundle struct {
	Version    int               `json:"version"`
	ExportedAt string            `json:"exported_at"`
	WorkBuddy  []json.RawMessage `json:"workbuddy"`
	// OmniGate 为 {provider: [DynAccount...]} 的原始 JSON；未启用时省略。
	OmniGate json.RawMessage `json:"omnigate,omitempty"`
}

// importBodyLimit 导入体上限（账号包可能含大量凭证，给足余量）。
const importBodyLimit = 32 << 20

// exportAccounts 导出两个账号池为单个 JSON 文件（附件下载）。
//
//	GET /panel/api/accounts/export
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	bundle := accountsBundle{Version: 1, ExportedAt: time.Now().Format(time.RFC3339), WorkBuddy: []json.RawMessage{}}

	// WorkBuddy：原样读 auths/workbuddy*.json（Parse 校验通过才纳入，跳过损坏文件）。
	files, _ := filepath.Glob(filepath.Join(p.cfg.AuthDir, "workbuddy*.json"))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if _, err := auth.Parse(raw); err != nil {
			continue
		}
		bundle.WorkBuddy = append(bundle.WorkBuddy, json.RawMessage(raw))
	}

	omniCount := 0
	if p.cfg.ExportOmniAccounts != nil {
		raw, err := p.cfg.ExportOmniAccounts()
		if err != nil {
			log.Printf("panel: 导出 OmniGate 账号失败：%v", err)
		} else if len(raw) > 0 {
			bundle.OmniGate = raw
			var byProv map[string][]json.RawMessage
			if json.Unmarshal(raw, &byProv) == nil {
				for _, list := range byProv {
					omniCount += len(list)
				}
			}
		}
	}

	body, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "marshal bundle: "+err.Error())
		return
	}
	fname := "omnigate-accounts-" + time.Now().Format("20060102-150405") + ".json"
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fname+"\"")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
	log.Printf("panel: 账号导出完成 workbuddy=%d omnigate=%d 文件=%s", len(bundle.WorkBuddy), omniCount, fname)
}

// importAccounts 导入账号包并合并进两个池。
//
//	POST /panel/api/accounts/import
//	Content-Type: application/json（裸包）或 multipart/form-data（file=<包>）
//
// 兼容两种顶层形态：对象包 {workbuddy, omnigate}，或裸数组 [auth...]（仅 WorkBuddy）。
func (p *Panel) importAccounts(w http.ResponseWriter, r *http.Request) {
	raw, err := readImportBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		writeErr(w, http.StatusBadRequest, "empty body")
		return
	}

	var bundle accountsBundle
	trimmed := strings.TrimSpace(string(raw))
	if strings.HasPrefix(trimmed, "[") {
		// 裸数组：仅 WorkBuddy 账号。
		if err := json.Unmarshal(raw, &bundle.WorkBuddy); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid json array: "+err.Error())
			return
		}
	} else if err := json.Unmarshal(raw, &bundle); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json bundle: "+err.Error())
		return
	}

	wbImported, wbSkipped, wbErrs := p.importWorkBuddy(bundle.WorkBuddy)

	var omniImported, omniSkipped int
	var omniErr string
	if len(bundle.OmniGate) > 0 {
		if p.cfg.ImportOmniAccounts == nil {
			omniErr = "OmniGate 未启用，已跳过 omnigate 段"
		} else if omniImported, omniSkipped, err = p.cfg.ImportOmniAccounts(bundle.OmniGate); err != nil {
			omniErr = err.Error()
		}
	}

	log.Printf("panel: 账号导入完成 workbuddy(成功=%d 跳过=%d) omnigate(成功=%d 跳过=%d) omniErr=%q",
		wbImported, wbSkipped, omniImported, omniSkipped, omniErr)
	resp := map[string]any{
		"ok": true,
		"workbuddy": map[string]any{
			"imported": wbImported,
			"skipped":  wbSkipped,
			"errors":   wbErrs,
		},
		"omnigate": map[string]any{
			"imported": omniImported,
			"skipped":  omniSkipped,
		},
	}
	if omniErr != "" {
		resp["omnigate_error"] = omniErr
	}
	writeJSON(w, http.StatusOK, resp)
}

// importWorkBuddy 把原始 auth 对象合并进 WorkBuddy 池：Parse → 落盘 → 进池。
// 逐条容错：单条失败只计 skipped + 记原因，不打断其余导入。
func (p *Panel) importWorkBuddy(items []json.RawMessage) (imported, skipped int, errs []string) {
	for _, raw := range items {
		a, err := auth.Parse(raw)
		if err != nil {
			skipped++
			errs = append(errs, "parse: "+err.Error())
			continue
		}
		uid := strings.TrimSpace(a.UID)
		if !validImportUID(uid) {
			skipped++
			errs = append(errs, "invalid uid")
			continue
		}
		a.FilePath = filepath.Join(p.cfg.AuthDir, "workbuddy-"+uid+".json")
		// realm 缺失时按 domain 推断补全（与 LoadDir 的存量迁移同口径）；已有 realm 为幂等空操作。
		a.BackfillRealm()
		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, "uid="+uid+": save: "+err.Error())
			continue
		}
		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)
		imported++
	}
	return imported, skipped, errs
}

// readImportBody 取导入体：multipart 时读 file 字段，否则读裸 body。
func readImportBody(r *http.Request) ([]byte, error) {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(importBodyLimit); err != nil {
			return nil, err
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return io.ReadAll(io.LimitReader(f, importBodyLimit))
	}
	return io.ReadAll(io.LimitReader(r.Body, importBodyLimit))
}
