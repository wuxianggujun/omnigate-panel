// omni_modelmap.go 面板 → 进程内 OmniGate 的「全局模型名映射」代理。
//
// 背景：参考实现 gemini2api 有一张全局「别名 → 实际模型」表
// （app/core/model_mapping.py，落盘 data/model-mapping.json），运维可以把客户端
// 请求里的模型名改指到别的上游模型（如 gpt-4o → geminiweb/gemini-pro），无需改
// 客户端。它属于平台级设置（与 /panel/api/omni/config 同级），因此读取需登录
// （privileged），写入仅管理员（admin，见 panel.go 注册处）。面板注入网关
// api_key 转发到 OmniGate 的 /admin/model-mapping[/*]，前端不持有 api_key。
package panel

import (
	"bytes"
	"io"
	"net/http"
	"strings"
)

// omniModelMapProxy proxies /panel/api/omni/model-mapping[/*] to the in-process
// OmniGate /admin/model-mapping[/*].
func (p *Panel) omniModelMapProxy(w http.ResponseWriter, r *http.Request) {
	if p.cfg.OmniAdmin == nil {
		writeErr(w, http.StatusNotImplemented, "omnigate not enabled")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	_ = r.Body.Close()

	// 保留别名子路径（DELETE /.../model-mapping/<alias>）。
	sub := strings.TrimPrefix(r.URL.Path, "/panel/api/omni/model-mapping")
	fwd := r.Clone(r.Context())
	fwd.URL.Path = "/admin/model-mapping" + sub
	fwd.Body = io.NopCloser(bytes.NewReader(raw))
	fwd.ContentLength = int64(len(raw))
	fwd.Header.Set("Authorization", "Bearer "+p.apiKey())
	fwd.Header.Del("Cookie") // 面板会话 Cookie 不外传

	cw := newCaptureWriter()
	p.cfg.OmniAdmin.ServeHTTP(cw, fwd)
	status := cw.status
	if status == 0 {
		status = http.StatusOK
	}
	// 上游 401 = 网关 api_key 不匹配，不是面板会话失效：改写为 502，避免前端误弹登录门。
	if status == http.StatusUnauthorized {
		writeErr(w, http.StatusBadGateway, "omnigate_unauthorized")
		return
	}
	if ct := cw.header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(status)
	_, _ = w.Write(cw.body.Bytes())
}
