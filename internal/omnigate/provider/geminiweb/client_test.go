package geminiweb

import (
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// TestCookieJarHeaderCanonical locks the invariant ensureSession's session-reuse
// check depends on: the jar always renders a stable, sorted cookie string, and
// re-parsing that string is a no-op. Without this, the session would be treated
// as stale on every request and re-handshake each time.
func TestCookieJarHeaderCanonical(t *testing.T) {
	raw := "__Secure-1PSIDTS=b; __Secure-1PSID=a"
	h := fingerprint.Parse(raw).CookieHeader()
	if h != "__Secure-1PSID=a; __Secure-1PSIDTS=b" {
		t.Fatalf("header = %q want sorted __Secure-1PSID first", h)
	}
	if again := fingerprint.Parse(h).CookieHeader(); again != h {
		t.Fatalf("re-parse = %q want %q (must be idempotent)", again, h)
	}
	// A rotation must change the canonical form so the session is rebuilt.
	j := fingerprint.Parse(raw)
	j.Set("__Secure-1PSIDTS", "c")
	if j.CookieHeader() == h {
		t.Fatal("rotation did not change the canonical header")
	}
}

func TestCookieJarIgnoresJunk(t *testing.T) {
	j := fingerprint.Parse("  ; novalue ; =empty ; a=1 ; a=2 ")
	if got := j.Get("a"); got != "2" {
		t.Fatalf("a = %q want 2 (last wins)", got)
	}
	if got := j.Get("novalue"); got != "" {
		t.Fatalf("novalue = %q want empty", got)
	}
	if got := j.Get(""); got != "" {
		t.Fatalf("empty key = %q want empty", got)
	}
}
