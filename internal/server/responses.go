package server

// Responses API 兼容端点：POST /v1/responses
//
// 上游（WorkBuddy 逆向网关）只实现 chat/completions；本文件让网关对外同时提供
// OpenAI Responses API。做法是「翻译 + 复用」：把 Responses 请求翻译成 chat 请求，
// 在进程内直接投给既有 chatCompletions（换号重试 / 429 冷却 / 内容拦截降级 /
// gateway_hint / 请求日志全部自动生效），用一个 ResponseWriter 拦截器接住输出再
// 翻译回 Responses 格式。翻译细节见 internal/responses。
//
// 边界能力：store / previous_response_id 会话续写（进程内 30 分钟 TTL 存储）、
// metadata 回显、instructions 字符串或部件数组、user 透传。reasoning 的
// encrypted_content 不做（无上游对应物，reasoning 项在入站侧被丢弃）。

import (
	"bytes"
	"io"
	"net/http"
	"net/url"

	"github.com/wuxianggujun/omnigate-panel/internal/responses"
)

// responses 处理 POST /v1/responses。
func (h *Handler) responses(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	cr, err := responses.RequestToChat(raw, responses.DefaultStore)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// 进程内投递：把转换后的 chat 请求交给既有 chatCompletions，复用全部逻辑。
	// 沿用同一 context（保留 requestTrace / 客户端取消信号），只替换 body 与 path。
	r2 := r.Clone(r.Context())
	r2.Method = http.MethodPost
	r2.URL = &url.URL{Path: "/v1/chat/completions"}
	r2.RequestURI = "/v1/chat/completions"
	r2.Body = io.NopCloser(bytes.NewReader(cr.Body))
	r2.ContentLength = int64(len(cr.Body))
	r2.Header = r.Header.Clone()
	r2.Header.Set("Content-Type", "application/json")

	tw := responses.NewTranslator(w, cr.Stream, cr.Model, cr.Metadata)
	h.chatCompletions(tw, r2)
	tw.Finish()

	// store 会话：把本轮完整消息（含助手回复）存起来，供 previous_response_id 续写。
	if cr.Store {
		if msg := tw.AssistantMessage(); msg != nil {
			msgs := make([]map[string]any, 0, len(cr.Messages)+1)
			msgs = append(msgs, cr.Messages...)
			msgs = append(msgs, msg)
			responses.DefaultStore.Put(tw.ResponseID(), cr.Model, msgs)
		}
	}
}
