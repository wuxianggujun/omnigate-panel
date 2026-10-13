package server

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rateLimiter is a per-client fixed-window request counter. It mirrors the
// reference's slowapi limiter (app/core/limiter.py): at most Max requests per
// Window seconds per client IP, opt-in via rate_limit.enabled, and keyed on the
// real client IP — honouring X-Forwarded-For / X-Real-IP only when the operator
// trusts proxy headers (the reference's TRUST_PROXY_HEADERS).
//
// Like slowapi's default handler, an over-limit request is answered with a plain
// 429 {"error": "Rate limit exceeded: N per W seconds"} — intentionally not
// dialect-specific, because the reference's limiter is applied uniformly.
type rateLimiter struct {
	mu   sync.Mutex
	hits map[string]*rateBucket
	now  func() time.Time
}

type rateBucket struct {
	start time.Time
	count int
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{hits: map[string]*rateBucket{}, now: time.Now}
}

// allow reports whether key may proceed within the window. The second return is
// the remaining time until the window resets, for the Retry-After header.
func (l *rateLimiter) allow(key string, max int, window time.Duration) (bool, time.Duration) {
	if max <= 0 || window <= 0 {
		return true, 0
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	// Opportunistic sweep keeps the map bounded when many distinct IPs appear.
	if len(l.hits) > 8192 {
		for k, b := range l.hits {
			if now.Sub(b.start) >= window {
				delete(l.hits, k)
			}
		}
	}
	b := l.hits[key]
	if b == nil || now.Sub(b.start) >= window {
		l.hits[key] = &rateBucket{start: now, count: 1}
		return true, 0
	}
	if b.count >= max {
		return false, window - now.Sub(b.start)
	}
	b.count++
	return true, 0
}

// rateLimitOK enforces the optional per-client rate limit for a chat-style API
// request. It returns false after writing a 429 when the client is over quota.
func (s *Server) rateLimitOK(w http.ResponseWriter, r *http.Request) bool {
	rc := s.cfg.RateLimit
	if !rc.On() {
		return true
	}
	window := time.Duration(rc.WindowSeconds()) * time.Second
	max := rc.MaxPerWindow()
	allowed, retryAfter := s.rl.allow(s.clientKey(r, rc.TrustProxy), max, window)
	if allowed {
		return true
	}
	secs := int(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	s.writeValue(w, http.StatusTooManyRequests, map[string]any{
		"error": "Rate limit exceeded: " + strconv.Itoa(max) + " per " + strconv.Itoa(rc.WindowSeconds()) + " seconds",
	})
	return false
}

// clientKey resolves the rate-limit key: the real client IP, or the proxy-supplied
// address when trustProxy is set and a usable header is present.
func (s *Server) clientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
		if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
			return xri
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
