package geminiweb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// TestUploadAttachment drives one upload end to end against a local server: the
// request must be a multipart form with the single "file" part, and must carry the
// minimal header set (any X-Goog-Upload-* makes Google parse it as the resumable
// protocol and reject the body).
func TestUploadAttachment(t *testing.T) {
	var pushID, tenant, origin, referer, cookie string
	var filename, partMime string
	var payload string

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushID = r.Header.Get("Push-ID")
		tenant = r.Header.Get("X-Tenant-Id")
		origin = r.Header.Get("Origin")
		referer = r.Header.Get("Referer")
		cookie = r.Header.Get("Cookie")
		for name := range r.Header {
			if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Goog-Upload") {
				t.Errorf("resumable-protocol header leaked: %s", name)
			}
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			t.Errorf("form file: %v", err)
			return
		}
		defer f.Close()
		filename = hdr.Filename
		partMime = hdr.Header.Get("Content-Type")
		b, _ := io.ReadAll(f)
		payload = string(b)
		_, _ = w.Write([]byte("  /contrib_service/ttl_1d/xyz\n"))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), uploadURL: ts.URL}
	ref, ok := c.uploadAttachment(context.Background(), parseCookies("__Secure-1PSID=x"), "feeds/abc",
		openai.Attachment{Type: "image_url", MIME: "image/png", Filename: "a.png", Data: []byte("ABC")})
	if !ok {
		t.Fatal("upload reported failure")
	}
	if ref.ID != "/contrib_service/ttl_1d/xyz" {
		t.Fatalf("id = %q want the trimmed response body", ref.ID)
	}
	if ref.Name != "a.png" {
		t.Fatalf("name = %q", ref.Name)
	}
	if payload != "ABC" {
		t.Fatalf("uploaded bytes = %q", payload)
	}
	if filename != "a.png" {
		t.Fatalf("multipart filename = %q", filename)
	}
	if partMime != "image/png" {
		t.Fatalf("multipart part content-type = %q", partMime)
	}
	if pushID != "feeds/abc" {
		t.Fatalf("Push-ID = %q", pushID)
	}
	if tenant != "bard-storage" {
		t.Fatalf("X-Tenant-Id = %q", tenant)
	}
	if origin != "https://gemini.google.com" || referer != "https://gemini.google.com/" {
		t.Fatalf("origin = %q referer = %q", origin, referer)
	}
	if !strings.Contains(cookie, "__Secure-1PSID=x") {
		t.Fatalf("cookie = %q", cookie)
	}
}

// TestUploadAttachmentDefaults: an account whose app page exposed no push id still
// uploads, using the protocol default.
func TestUploadAttachmentDefaults(t *testing.T) {
	var pushID, filename string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pushID = r.Header.Get("Push-ID")
		_, hdr, err := r.FormFile("file")
		if err == nil {
			filename = hdr.Filename
		}
		_, _ = w.Write([]byte("id-1"))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), uploadURL: ts.URL}
	// No filename and no mime: both must fall back rather than produce an empty
	// part or an unparseable header.
	_, ok := c.uploadAttachment(context.Background(), parseCookies("a=b"), "", openai.Attachment{Type: "image_url", Data: []byte("x")})
	if !ok {
		t.Fatal("upload reported failure")
	}
	if pushID != defaultPushID {
		t.Fatalf("Push-ID = %q want %q", pushID, defaultPushID)
	}
	if filename != "upload.bin" {
		t.Fatalf("filename = %q want upload.bin", filename)
	}
}

func TestUploadAttachmentFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"upstream error", http.StatusForbidden, "nope"},
		{"empty body", http.StatusOK, "   "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()
			c := &Client{http: ts.Client(), uploadURL: ts.URL}
			if _, ok := c.uploadAttachment(context.Background(), parseCookies("a=b"), "", openai.Attachment{Data: []byte("x")}); ok {
				t.Fatal("upload reported success")
			}
		})
	}
}

// TestUploadAttachmentUnresolvable: an attachment with neither bytes nor a URL
// cannot be uploaded, and must not be reported as uploaded.
func TestUploadAttachmentUnresolvable(t *testing.T) {
	c := &Client{http: &http.Client{}}
	if _, ok := c.uploadAttachment(context.Background(), parseCookies("a=b"), "", openai.Attachment{Type: "image_url"}); ok {
		t.Fatal("upload reported success for an empty attachment")
	}
}

// TestSafeRemoteURL is the SSRF guard: a caller-supplied attachment URL must not
// be able to reach the gateway's own network, its loopback, or a cloud metadata
// endpoint.
func TestSafeRemoteURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1/x", false},
		{"http://127.0.0.1:8080/x", false},
		{"http://localhost/x", false},
		{"http://LocalHost/x", false},
		{"http://[::1]/x", false},
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://10.0.0.5/x", false},
		{"http://172.16.3.4/x", false},
		{"http://192.168.1.1/x", false},
		{"http://100.64.0.1/x", false},
		{"http://192.0.0.1/x", false},
		{"http://240.0.0.1/x", false},
		{"http://0.0.0.0/x", false},
		{"http://224.0.0.1/x", false},
		{"http://[fd00::1]/x", false},
		{"http://[fe80::1]/x", false},
		{"file:///etc/passwd", false},
		{"ftp://example.com/x", false},
		{"gopher://example.com/x", false},
		{"http:///x", false},
		{"", false},
		{"not a url", false},
		{"http://this-host-does-not-exist.invalid/x", false},
		// A public literal address needs no DNS, so the positive case is stable.
		{"http://1.1.1.1/x", true},
		{"https://1.1.1.1/x", true},
	} {
		if got := safeRemoteURL(tc.url); got != tc.want {
			t.Errorf("safeRemoteURL(%q) = %v want %v", tc.url, got, tc.want)
		}
	}
}

// TestFetchRemoteAttachmentRefusesUnsafe: the guard must run before any request
// is made.
func TestFetchRemoteAttachmentRefusesUnsafe(t *testing.T) {
	var hits int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer ts.Close()

	c := &Client{http: ts.Client()}
	// The test server listens on loopback, which the guard must refuse.
	if _, _, ok := c.fetchRemoteAttachment(context.Background(), ts.URL+"/a.png", "image/png"); ok {
		t.Fatal("fetched a loopback URL")
	}
	if _, _, ok := c.fetchRemoteAttachment(context.Background(), "http://169.254.169.254/latest/meta-data/", ""); ok {
		t.Fatal("fetched a metadata address")
	}
	if hits != 0 {
		t.Fatalf("guard let %d request(s) through", hits)
	}
}

func TestResolveRedirect(t *testing.T) {
	for _, tc := range []struct {
		base, loc, want string
	}{
		{"https://a.com/x/y", "/z", "https://a.com/z"},
		{"https://a.com/x/y", "z", "https://a.com/x/z"},
		{"https://a.com/x/y", "https://b.com/q", "https://b.com/q"},
		{"https://a.com/x/y", "  /z  ", "https://a.com/z"},
	} {
		if got := resolveRedirect(tc.base, tc.loc); got != tc.want {
			t.Errorf("resolveRedirect(%q, %q) = %q want %q", tc.base, tc.loc, got, tc.want)
		}
	}
}

// TestSafeFilename: a filename comes straight from the request body and is written
// into a Content-Disposition header, so it must not be able to break out.
func TestSafeFilename(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a.png", "a.png"},
		{`bad"name.png`, "badname.png"},
		{"line\r\nbreak.png", "linebreak.png"},
		{`back\slash.png`, "backslash.png"},
		{"   ", "upload.bin"},
		{"", "upload.bin"},
	} {
		if got := safeFilename(tc.in); got != tc.want {
			t.Errorf("safeFilename(%q) = %q want %q", tc.in, got, tc.want)
		}
	}
}

// TestAttachmentBytesMimeFallbacks: an attachment with no declared type must still
// produce a usable multipart part.
func TestAttachmentBytesMimeFallbacks(t *testing.T) {
	c := &Client{http: &http.Client{}}
	data, name, mime, ok := c.attachmentBytes(context.Background(), openai.Attachment{Data: []byte("x")})
	if !ok || string(data) != "x" || name != "upload.bin" || mime != "application/octet-stream" {
		t.Fatalf("data=%q name=%q mime=%q ok=%v", data, name, mime, ok)
	}
}
