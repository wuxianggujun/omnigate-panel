// index.go 面板静态资源与安全响应头。
//
// 新前端是 web/ 下的原生 ES Module SPA，经 go:embed 打进二进制；无 npm、无外部
// 构建步骤。静态资源全部同源，配合严格 CSP。
package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed web
var webFS embed.FS

var webRoot = func() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("panel: embed web: " + err.Error())
	}
	return sub
}()

// panelBuild 前端资源指纹：web/ 下所有文件内容按路径排序后取 sha256 前 12 位。
var panelBuild = func() string {
	h := sha256.New()
	_ = fs.WalkDir(webRoot, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(webRoot, path)
		if err != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write(b)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}()

// csp 内容安全策略（严格版，无需 unsafe-inline）。
const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"connect-src 'self'; img-src 'self' data:; font-src 'self' data:; form-action 'none'; " +
	"frame-ancestors 'none'; base-uri 'none'"

// setSecurityHeaders 写入面板统一安全响应头（页面与 API 都要）。
func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

// setNoCache 页面与静态资源必须每次回源校验。
func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
}

// index 输出面板页面（静态无秘密；数据接口 /panel/api/* 才走鉴权）。
func (p *Panel) index(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	setNoCache(w)
	raw, err := fs.ReadFile(webRoot, "index.html")
	if err != nil {
		http.Error(w, "panel asset missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// themeStyle 输出设计系统样式表。
func (p *Panel) themeStyle(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	setNoCache(w)
	raw, err := fs.ReadFile(webRoot, "theme.css")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

// assets 输出 /panel/js/ 下的 ES Module 文件。
func (p *Panel) assets(w http.ResponseWriter, r *http.Request) {
	setSecurityHeaders(w)
	setNoCache(w)
	http.StripPrefix("/panel/", http.FileServerFS(webRoot)).ServeHTTP(w, r)
}
