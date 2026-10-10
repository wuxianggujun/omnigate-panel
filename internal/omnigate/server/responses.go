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
func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeJSON(w, 400, openai.ErrorJSON("bad_request", "failed to read body: "+err.Error()))
		return
	}
	chatBody, stream, model, err := responses.RequestToChat(body)
	if err != nil {
		writeJSON(w, 400, openai.ErrorJSON("invalid_json", err.Error()))
		return
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(chatBody, &req); err != nil {
		writeJSON(w, 400, openai.ErrorJSON("invalid_json", "invalid chat body: "+err.Error()))
		return
	}
	req.Stream = stream

	tw := responses.NewTranslator(w, stream, model)
	s.gw.HandleChat(r.Context(), &req, tw)
	tw.Finish()
}
