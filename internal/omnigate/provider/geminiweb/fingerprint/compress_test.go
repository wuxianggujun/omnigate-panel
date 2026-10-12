package fingerprint

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func encode(t *testing.T, enc string, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch enc {
	case "gzip", "x-gzip":
		w := gzip.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "deflate":
		w := zlib.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "br":
		w := brotli.NewWriter(&buf)
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	case "zstd":
		w, err := zstd.NewWriter(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown encoding %q", enc)
	}
	return buf.Bytes()
}

func TestDecompressTransportDecodes(t *testing.T) {
	payload := []byte("hello fingerprint layer — 压缩往返")
	for _, enc := range []string{"gzip", "x-gzip", "deflate", "br", "zstd"} {
		t.Run(enc, func(t *testing.T) {
			body := encode(t, enc, payload)
			dt := DecompressTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				h := http.Header{}
				h.Set("Content-Encoding", enc)
				h.Set("Content-Length", "999")
				return &http.Response{
					StatusCode:    200,
					Header:        h,
					Body:          io.NopCloser(bytes.NewReader(body)),
					ContentLength: int64(len(body)),
				}, nil
			})}
			req, _ := http.NewRequest("GET", "https://example.com", nil)
			resp, err := dt.RoundTrip(req)
			if err != nil {
				t.Fatalf("RoundTrip: %v", err)
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			_ = resp.Body.Close()
			if !bytes.Equal(got, payload) {
				t.Fatalf("decoded = %q want %q", got, payload)
			}
			if resp.Header.Get("Content-Encoding") != "" {
				t.Fatal("Content-Encoding not stripped")
			}
			if resp.ContentLength != -1 || !resp.Uncompressed {
				t.Fatalf("ContentLength=%d Uncompressed=%v", resp.ContentLength, resp.Uncompressed)
			}
		})
	}
}

func TestDecompressTransportPassthrough(t *testing.T) {
	for _, enc := range []string{"", "identity", "utf-8"} {
		t.Run("enc="+enc, func(t *testing.T) {
			payload := []byte("plain")
			dt := DecompressTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
				h := http.Header{}
				if enc != "" {
					h.Set("Content-Encoding", enc)
				}
				return &http.Response{
					StatusCode: 200,
					Header:     h,
					Body:       io.NopCloser(bytes.NewReader(payload)),
				}, nil
			})}
			req, _ := http.NewRequest("GET", "https://example.com", nil)
			resp, err := dt.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !bytes.Equal(got, payload) {
				t.Fatalf("passthrough mutated body: %q", got)
			}
		})
	}
}

func TestDecompressTransportBadStreamReturnsUnchanged(t *testing.T) {
	// 声明 gzip 但内容不是合法 gzip：应原样返回而不是报错（避免打断调用方）。
	garbage := []byte("not really gzip")
	dt := DecompressTransport{Base: roundTripFunc(func(*http.Request) (*http.Response, error) {
		h := http.Header{}
		h.Set("Content-Encoding", "gzip")
		return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(bytes.NewReader(garbage))}, nil
	})}
	req, _ := http.NewRequest("GET", "https://example.com", nil)
	resp, err := dt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, garbage) {
		t.Fatalf("bad stream not returned unchanged: %q", got)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatal("Content-Encoding should be preserved when decoding fails")
	}
}

func TestDecompressTransportNilBaseUsesDefault(t *testing.T) {
	payload := []byte("over the wire")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(payload)
		_ = zw.Close()
	}))
	defer ts.Close()

	dt := DecompressTransport{}
	req, _ := http.NewRequest("GET", ts.URL, nil)
	// 显式声明 Accept-Encoding，避免 DefaultTransport 自动解压抢先一步。
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := dt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("nil-base decode = %q want %q", got, payload)
	}
}
