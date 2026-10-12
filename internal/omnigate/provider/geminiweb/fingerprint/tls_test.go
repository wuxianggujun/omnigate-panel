package fingerprint

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSupportedChromeMajors(t *testing.T) {
	got := SupportedChromeMajors()
	want := []int{100, 102, 106, 115, 120}
	if len(got) != len(want) {
		t.Fatalf("majors = %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("majors = %v want %v", got, want)
		}
	}
}

func TestValidateImpersonateTarget(t *testing.T) {
	cases := map[string]bool{
		"chrome100":  true,
		"chrome120":  true,
		"chrome124":  true, // 降级到 120
		"chrome126":  true, // 降级到 120
		"chrome50":   false,
		"firefox120": false,
		"":           false,
		"chrome":     false,
		"chromeX":    false,
	}
	for target, want := range cases {
		if got := ValidateImpersonateTarget(target); got != want {
			t.Errorf("ValidateImpersonateTarget(%q) = %v want %v", target, got, want)
		}
	}
}

func TestTLSProfileForTargetDowngrades(t *testing.T) {
	cases := []struct {
		target      string
		wantVersion string
		ok          bool
	}{
		{"chrome100", "100", true},
		{"chrome102", "102", true},
		{"chrome106", "106", true},
		{"chrome115", "115_PQ", true},
		{"chrome120", "120", true},
		{"chrome124", "120", true}, // 无精确目标，取不高于 124 的最大版本
		{"chrome119", "115_PQ", true},
		{"chrome90", "", false},
	}
	for _, tc := range cases {
		id, ok := tlsProfileForTarget(tc.target)
		if ok != tc.ok {
			t.Errorf("%s: ok = %v want %v", tc.target, ok, tc.ok)
			continue
		}
		if ok && id.Version != tc.wantVersion {
			t.Errorf("%s: version = %q want %q", tc.target, id.Version, tc.wantVersion)
		}
	}
}

func TestTLSImpersonatorEnableDisable(t *testing.T) {
	imp := NewTLSImpersonator("chrome120")
	if !imp.Enabled() {
		t.Fatal("chrome120 should be enabled")
	}
	if !imp.SetTarget("chrome124") || !imp.Enabled() {
		t.Fatal("chrome124 should downgrade and stay enabled")
	}
	if imp.SetTarget("bogus") {
		t.Fatal("bogus target should not be accepted")
	}
	if imp.Enabled() {
		t.Fatal("unsupported target must disable impersonation")
	}
	imp.SetTarget("chrome120")
	imp.Disable()
	if imp.Enabled() {
		t.Fatal("Disable did not take effect")
	}
	// nil 接收者应安全。
	var nilImp *TLSImpersonator
	if nilImp.Enabled() {
		t.Fatal("nil impersonator must report disabled")
	}
	nilImp.Disable()
}

func TestTLSImpersonatorRealHandshake(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer ts.Close()

	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())

	imp := NewTLSImpersonator("chrome120")
	imp.rootCAs = pool

	tr := &http.Transport{}
	imp.Apply(tr, &net.Dialer{})
	if tr.DialTLSContext == nil {
		t.Fatal("Apply did not install DialTLSContext")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("Apply should set ForceAttemptHTTP2")
	}

	client := &http.Client{Transport: tr}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("utls handshake request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestTLSImpersonatorDisabledDialErrors(t *testing.T) {
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ts.Close()

	imp := NewTLSImpersonator("chrome120")
	imp.Disable()
	fn := imp.DialTLSContext(&net.Dialer{})
	conn, err := fn(context.Background(), "tcp", ts.Listener.Addr().String())
	if conn != nil {
		_ = conn.Close()
		t.Fatal("disabled impersonator returned a connection")
	}
	if !errors.Is(err, errTLSDisabled) {
		t.Fatalf("err = %v want errTLSDisabled", err)
	}
}

func TestTLSImpersonatorApplyNoopWhenDisabled(t *testing.T) {
	imp := NewTLSImpersonator("bogus")
	tr := &http.Transport{}
	imp.Apply(tr, nil)
	if tr.DialTLSContext != nil || tr.ForceAttemptHTTP2 {
		t.Fatal("disabled impersonator must not modify the transport")
	}
	// nil transport 也不应 panic。
	imp2 := NewTLSImpersonator("chrome120")
	imp2.Apply(nil, nil)
}
