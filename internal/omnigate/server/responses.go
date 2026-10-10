package server

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
	"github.com/wuxianggujun/omnigate-panel/internal/responses"
)

// handleResponses serves POST /v1/responses: it translates the Responses request
// into a chat request, reuses HandleChat (provider routing / account rotation /
// tool emulation all apply), and translates the output back to the Responses
// shape. Translation details live in internal/responses.
//
// store / previous_response_id chaining uses the same process-wide store as the
// main gateway (both handlers live in one binary).
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, 400, openai.ErrorJSON("bad_request", "failed to read body: "+err.Error()))
		return
	}
	cr, err := responses.RequestToChat(body, responses.DefaultStore)
	if err != nil {
		writeJSON(w, 400, openai.ErrorJSON("invalid_json", err.Error()))
		return
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(cr.Body, &req); err != nil {
		writeJSON(w, 400, openai.ErrorJSON("invalid_json", "invalid chat body: "+err.Error()))
		return
	}
	req.Stream = cr.Stream

	tw := responses.NewTranslator(w, cr.Stream, cr.Model, cr.Metadata)
	s.gw.HandleChat(r.Context(), &req, tw)
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
