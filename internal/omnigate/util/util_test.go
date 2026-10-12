package util

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPublicBase: the scheme is taken from X-Forwarded-Proto when a proxy set it
// (openresty does), and otherwise guessed from the host. Getting this wrong hands
// the client a link it cannot fetch.
func TestPublicBase(t *testing.T) {
	for _, tc := range []struct {
		name   string
		host   string
		fproto string
		want   string
	}{
		{"forwarded https", "wb.example.com", "https", "https://wb.example.com"},
		{"forwarded http", "wb.example.com", "http", "http://wb.example.com"},
		{"domain defaults to https", "wb.example.com", "", "https://wb.example.com"},
		{"host with port", "wb.example.com:8443", "", "https://wb.example.com:8443"},
		{"localhost is plain http", "localhost", "", "http://localhost"},
		{"literal ip is plain http", "127.0.0.1:7863", "", "http://127.0.0.1:7863"},
		{"bare hostname is plain http", "newapi:7863", "", "http://newapi:7863"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			r.Host = tc.host
			if tc.fproto != "" {
				r.Header.Set("X-Forwarded-Proto", tc.fproto)
			}
			if got := PublicBase(r); got != tc.want {
				t.Fatalf("PublicBase = %q want %q", got, tc.want)
			}
		})
	}
	if got := PublicBase(nil); got != "" {
		t.Fatalf("PublicBase(nil) = %q want \"\"", got)
	}
}
