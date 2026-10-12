package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGeminiWebManageRoutesRecognized: the management routes must be routed (not
// the generic 404) and must fail cleanly when there is no account to act on.
func TestGeminiWebManageRoutesRecognized(t *testing.T) {
	s := fingerprintTestServer(t)
	cases := []struct {
		name, method, path, body string
	}{
		{"gems list, no account", http.MethodGet, "/admin/geminiweb/gems?provider=gw", ""},
		{"chats list, no account", http.MethodGet, "/admin/geminiweb/chats?provider=gw", ""},
		{"chats delete, no account", http.MethodPost, "/admin/geminiweb/chats/delete", `{"provider":"gw","id":"c_1"}`},
		{"chats cleanup, no account", http.MethodPost, "/admin/geminiweb/chats/cleanup", `{"provider":"gw"}`},
		{"gems, unknown action", http.MethodPost, "/admin/geminiweb/gems", `{"provider":"gw","action":"bogus"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			w := httptest.NewRecorder()
			s.handleGeminiWebAdmin(w, r)
			if w.Code == http.StatusNotFound {
				t.Fatalf("route not recognized: %s", w.Body.String())
			}
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "admin_error") {
				t.Fatalf("expected an admin_error body, got %s", w.Body.String())
			}
		})
	}
}

// TestGeminiWebManageUnknownSubpath: a path outside the switch is still a 404.
func TestGeminiWebManageUnknownSubpath(t *testing.T) {
	s := fingerprintTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/admin/geminiweb/nope", nil)
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

// TestGeminiWebKeepAliveEmptyPool: keeping alive an empty pool is a no-op that
// still answers 200 with an (empty) result list.
func TestGeminiWebKeepAliveEmptyPool(t *testing.T) {
	s := fingerprintTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/admin/geminiweb/keepalive", strings.NewReader(`{"provider":"gw"}`))
	w := httptest.NewRecorder()
	s.handleGeminiWebAdmin(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"results":[]`) {
		t.Fatalf("body = %s", w.Body.String())
	}
}
