package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
)

// TestRateLimitEnforced drives the real ServeHTTP path: with a limit of 2 per
// window, the third chat request must be answered 429 (slowapi-equivalent).
func TestRateLimitEnforced(t *testing.T) {
	s := researchTestServer(t)
	s.rl = newRateLimiter()
	s.cfg.RateLimit = &config.RateLimitConfig{Enabled: true, Window: 60, Max: 2}

	body := `{"model":"oa/m1","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		s.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly rate limited: %s", i+1, w.Body.String())
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	s.ServeHTTP(w, r)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("third request status = %d body=%s, want 429", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Rate limit exceeded") {
		t.Fatalf("429 body = %s", w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 missing Retry-After header")
	}
}

// TestRateLimitDisabledByDefault: with no rate_limit config, requests are never
// limited (zero regression).
func TestRateLimitDisabledByDefault(t *testing.T) {
	s := researchTestServer(t)
	s.rl = newRateLimiter()
	s.cfg.RateLimit = &config.RateLimitConfig{} // enabled=false

	body := `{"model":"oa/m1","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 5; i++ {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		s.ServeHTTP(w, r)
		if w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d was limited while disabled", i+1)
		}
	}
}

// TestRateLimiterWindowResets checks the fixed-window reset.
func TestRateLimiterWindowResets(t *testing.T) {
	l := newRateLimiter()
	base := time.Unix(1000, 0)
	l.now = func() time.Time { return base }
	if ok, _ := l.allow("ip", 1, time.Minute); !ok {
		t.Fatal("first request should pass")
	}
	if ok, _ := l.allow("ip", 1, time.Minute); ok {
		t.Fatal("second request should be limited")
	}
	// Advance past the window.
	l.now = func() time.Time { return base.Add(61 * time.Second) }
	if ok, _ := l.allow("ip", 1, time.Minute); !ok {
		t.Fatal("request after window reset should pass")
	}
}

// TestClientKeyTrustProxy: proxy headers are honoured only when trusted.
func TestClientKeyTrustProxy(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.1")
	r.Header.Set("X-Real-IP", "203.0.113.9")

	if got := s.clientKey(r, false); got != "10.0.0.1" {
		t.Fatalf("untrusted key = %q, want TCP peer", got)
	}
	if got := s.clientKey(r, true); got != "203.0.113.7" {
		t.Fatalf("trusted key = %q, want first XFF hop", got)
	}
	// X-Real-IP fallback when XFF is absent.
	r.Header.Del("X-Forwarded-For")
	if got := s.clientKey(r, true); got != "203.0.113.9" {
		t.Fatalf("trusted key = %q, want X-Real-IP", got)
	}
}
