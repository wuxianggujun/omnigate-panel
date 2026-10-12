package fingerprint

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJarParseGetSetRemove(t *testing.T) {
	j := Parse("  ; novalue ; =empty ; a=1 ; a=2 ")
	if j.Get("a") != "2" {
		t.Fatalf("a = %q want 2 (last wins)", j.Get("a"))
	}
	if j.Get("novalue") != "" || j.Get("") != "" {
		t.Fatal("junk keys must be ignored")
	}
	j.Set("b", "3")
	if j.Get("b") != "3" {
		t.Fatal("Set failed")
	}
	j.Set("b", "")
	if j.Get("b") != "" {
		t.Fatal("Set with empty value must delete")
	}
	j.Remove("a")
	if j.Get("a") != "" {
		t.Fatal("Remove failed")
	}
}

func TestJarHeaderCanonicalAndIdempotent(t *testing.T) {
	raw := "__Secure-1PSIDTS=b; __Secure-1PSID=a"
	j := Parse(raw)
	h := j.CookieHeader()
	if h != "__Secure-1PSID=a; __Secure-1PSIDTS=b" {
		t.Fatalf("header = %q want sorted", h)
	}
	if again := Parse(h).CookieHeader(); again != h {
		t.Fatalf("re-parse = %q want %q", again, h)
	}
	j.Set("__Secure-1PSIDTS", "c")
	if j.CookieHeader() == h {
		t.Fatal("rotation did not change the canonical header")
	}
}

func TestJarDomainIsolation(t *testing.T) {
	j := Parse("a=1")
	j.mu.Lock()
	j.cookies["b"] = Cookie{Name: "b", Value: "2", Domain: ".example.com", Path: "/"}
	j.mu.Unlock()

	if got := j.CookieHeader("google.com"); got != "a=1" {
		t.Fatalf("google filter = %q want a=1", got)
	}
	if got := j.CookieHeader(); got != "a=1; b=2" {
		t.Fatalf("no filter = %q", got)
	}
}

func respWithSetCookie(values ...string) *http.Response {
	h := http.Header{}
	for _, v := range values {
		h.Add("Set-Cookie", v)
	}
	return &http.Response{Header: h}
}

func TestJarUpdateRotation(t *testing.T) {
	j := Parse("__Secure-1PSID=a; __Secure-1PSIDTS=old")
	j.UpdateFromResponse(respWithSetCookie("__Secure-1PSIDTS=new; Path=/; Secure; HttpOnly"))
	if j.Get("__Secure-1PSIDTS") != "new" {
		t.Fatalf("PSIDTS = %q want new", j.Get("__Secure-1PSIDTS"))
	}
	if j.Get("__Secure-1PSID") != "a" {
		t.Fatal("stable PSID must survive")
	}
}

func TestJarUpdateDeletion(t *testing.T) {
	for _, tc := range []struct {
		name string
		sc   string
	}{
		{"empty value", "__Secure-1PSID=; Path=/"},
		{"max-age zero", "__Secure-1PSID=x; Max-Age=0"},
		{"max-age negative", "__Secure-1PSID=x; Max-Age=-1"},
		{"expires past", "__Secure-1PSID=x; Expires=Thu, 01 Jan 1970 00:00:00 GMT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := Parse("__Secure-1PSID=live")
			j.UpdateFromResponse(respWithSetCookie(tc.sc))
			if j.Get("__Secure-1PSID") != "" {
				t.Fatalf("cookie survived deletion: %q", j.Get("__Secure-1PSID"))
			}
		})
	}
}

func TestJarUpdatePrefersCanonicalDomain(t *testing.T) {
	j := Parse("")
	// 同一名字跨域：规范域应胜出，无论顺序如何。
	j.UpdateFromResponse(respWithSetCookie(
		"NID=canonical; Domain=.google.com; Path=/",
		"NID=country; Domain=.google.com.hk; Path=/",
	))
	if j.Get("NID") != "canonical" {
		t.Fatalf("NID = %q want canonical", j.Get("NID"))
	}
	// 反过来也应胜出。
	j2 := Parse("")
	j2.UpdateFromResponse(respWithSetCookie(
		"NID=country; Domain=.google.com.hk; Path=/",
		"NID=canonical; Domain=.google.com; Path=/",
	))
	if j2.Get("NID") != "canonical" {
		t.Fatalf("NID = %q want canonical (reversed)", j2.Get("NID"))
	}
}

func TestJarExpiryCleanup(t *testing.T) {
	j := Parse("")
	j.mu.Lock()
	j.cookies["old"] = Cookie{Name: "old", Value: "x", Expires: float64(time.Now().Add(-time.Hour).Unix())}
	j.mu.Unlock()
	if got := j.CookieHeader(); got != "" {
		t.Fatalf("expired cookie leaked into header: %q", got)
	}
	if len(j.Names()) != 0 {
		t.Fatal("expired cookie not cleaned up")
	}
}

func TestJarPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	key := "psid-abc"
	j := NewJar(key, dir)
	j.Set("__Secure-1PSID", "abc")
	j.Set("__Secure-1PSIDTS", "ts1")

	// 文件应存在且名为 sha256(key)[:16].json
	path := filepath.Join(dir, cookieFileName(key))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("cookie file missing: %v", err)
	}

	reloaded := NewJar(key, dir)
	if reloaded.Get("__Secure-1PSID") != "abc" || reloaded.Get("__Secure-1PSIDTS") != "ts1" {
		t.Fatalf("reload = %q / %q", reloaded.Get("__Secure-1PSID"), reloaded.Get("__Secure-1PSIDTS"))
	}
}

func TestJarMergeConfigWinsOverDisk(t *testing.T) {
	dir := t.TempDir()
	j := NewJar("k", dir)
	j.Set("__Secure-1PSID", "from-disk")
	j.Set("__Secure-1PSIDTS", "stale-disk-ts")
	// 传入的配置/状态值是权威来源：同名覆盖磁盘值，磁盘独有的项保留。
	j.MergeFrom("__Secure-1PSID=from-config")
	if j.Get("__Secure-1PSID") != "from-config" {
		t.Fatalf("config did not win: %q", j.Get("__Secure-1PSID"))
	}
	if j.Get("__Secure-1PSIDTS") != "stale-disk-ts" {
		t.Fatalf("disk-only cookie dropped: %q", j.Get("__Secure-1PSIDTS"))
	}
}

func TestJarCorruptFileIgnored(t *testing.T) {
	dir := t.TempDir()
	key := "k"
	if err := os.WriteFile(filepath.Join(dir, cookieFileName(key)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	j := NewJar(key, dir)
	if got := j.CookieHeader(); got != "" {
		t.Fatalf("corrupt file should yield empty jar, got %q", got)
	}
}

func TestAccountKeyUsesPSID(t *testing.T) {
	if got := AccountKey("__Secure-1PSID=abc; __Secure-1PSIDTS=ts"); got != "abc" {
		t.Fatalf("AccountKey = %q want abc", got)
	}
	// 无 PSID 时退化为整串。
	if got := AccountKey("a=b"); got != "a=b" {
		t.Fatalf("AccountKey = %q want a=b", got)
	}
}

func TestParseSetCookie(t *testing.T) {
	name, value, domain, expires, ok := parseSetCookie("NID=abc; Domain=.google.com; Path=/; Max-Age=3600")
	if !ok || name != "NID" || value != "abc" || domain != ".google.com" {
		t.Fatalf("parse = %q %q %q ok=%v", name, value, domain, ok)
	}
	if expires == nil || *expires < float64(time.Now().Unix()) {
		t.Fatalf("expires = %v", expires)
	}
	if _, _, _, _, ok := parseSetCookie("garbage"); ok {
		t.Fatal("garbage parsed as valid")
	}
}
