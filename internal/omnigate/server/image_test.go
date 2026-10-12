package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/imagegen"
)

// TestHandleImageServesStoredImage: the route is the only way a client can turn
// the link in a reply into pixels, so it must serve the exact bytes with an image
// content type.
func TestHandleImageServesStoredImage(t *testing.T) {
	s := &Server{images: imagegen.NewStore(t.TempDir())}
	id, err := s.images.Save([]byte("PNGDATA"), "image/png")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.handleImage(rec, httptest.NewRequest(http.MethodGet, "/images/"+id, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q", ct)
	}
	if body := rec.Body.String(); body != "PNGDATA" {
		t.Fatalf("body = %q", body)
	}
}

func TestHandleImageNotFound(t *testing.T) {
	s := &Server{images: imagegen.NewStore(t.TempDir())}
	for _, path := range []string{
		"/images/0123456789abcdef0123456789abcdef.png", // valid shape, no such file
		"/images/../secret.txt",                        // traversal attempt
		"/images/",                                     // empty id
	} {
		rec := httptest.NewRecorder()
		s.handleImage(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d want 404", path, rec.Code)
		}
	}
}

// TestHandleImageWithoutStore: a Server built without a data dir (as in tests)
// must answer 404 rather than panic.
func TestHandleImageWithoutStore(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleImage(rec, httptest.NewRequest(http.MethodGet, "/images/0123456789abcdef0123456789abcdef.png", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d want 404", rec.Code)
	}
}
