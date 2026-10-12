package geminiweb

import "testing"

// TestCookieJarHeaderCanonical locks the invariant ensureSession's session-reuse
// check depends on: the jar always renders a stable, sorted cookie string, and
// re-parsing that string is a no-op. Without this, the session would be treated
// as stale on every request and re-handshake each time.
func TestCookieJarHeaderCanonical(t *testing.T) {
	raw := "__Secure-1PSIDTS=b; __Secure-1PSID=a"
	h := parseCookies(raw).header()
	if h != "__Secure-1PSID=a; __Secure-1PSIDTS=b" {
		t.Fatalf("header = %q want sorted __Secure-1PSID first", h)
	}
	if again := parseCookies(h).header(); again != h {
		t.Fatalf("re-parse = %q want %q (must be idempotent)", again, h)
	}
	// A rotation must change the canonical form so the session is rebuilt.
	j := parseCookies(raw)
	j.set("__Secure-1PSIDTS", "c")
	if j.header() == h {
		t.Fatal("rotation did not change the canonical header")
	}
}

func TestCookieJarIgnoresJunk(t *testing.T) {
	j := parseCookies("  ; novalue ; =empty ; a=1 ; a=2 ")
	if got := j.get("a"); got != "2" {
		t.Fatalf("a = %q want 2 (last wins)", got)
	}
	if got := j.get("novalue"); got != "" {
		t.Fatalf("novalue = %q want empty", got)
	}
	if got := j.get(""); got != "" {
		t.Fatalf("empty key = %q want empty", got)
	}
}
