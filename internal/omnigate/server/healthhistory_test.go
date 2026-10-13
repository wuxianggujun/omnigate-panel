package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The health-history / check-account admin routes are wired onto the geminiweb
// provider (gemini2api /admin/health-history + /admin/check-account). With no
// accounts configured they must still answer with an empty, well-formed body.
func TestGeminiWebHealthHistoryEndpoint(t *testing.T) {
	s := fingerprintTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/admin/geminiweb/health-history?provider=gw", nil)
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		OK      bool  `json:"ok"`
		Total   int   `json:"total"`
		Records []any `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if !body.OK || body.Total != 0 || len(body.Records) != 0 {
		t.Fatalf("unexpected body: %+v", body)
	}
}

func TestGeminiWebCheckAccountEndpoint(t *testing.T) {
	s := fingerprintTestServer(t)

	r := httptest.NewRequest(http.MethodGet, "/admin/geminiweb/check-account?provider=gw", nil)
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var body struct {
		OK       bool  `json:"ok"`
		Accounts []any `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if !body.OK || len(body.Accounts) != 0 {
		t.Fatalf("unexpected body: %+v", body)
	}
}
