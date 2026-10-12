package geminiweb

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/imagegen"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// imageGenPayload builds a payload whose candidate carries one generated image
// at candidate[12][7][0], in the shape the upstream observes live: the image list
// holds an entry, the entry wraps a list, and that list's fourth element is the
// metadata. metaAt3=false emits the shallower variant instead.
func imageGenPayload(conv, text, imgURL string, metaAt3 bool) []any {
	meta := []any{"ref", 0, "generated.png", imgURL, nil, "image/png", nil,
		[]any{float64(1024), float64(1024), float64(1234)}}
	entry := []any{meta}
	if metaAt3 {
		entry = []any{[]any{nil, 1, nil, meta}}
	}
	candidate := make([]any, 38)
	candidate[1] = []any{text}
	candidate[12] = []any{nil, nil, nil, nil, nil, nil, nil, []any{[]any{entry}}}
	return []any{nil, conv, nil, nil, []any{candidate}}
}

func TestExtractGeneratedImages(t *testing.T) {
	for _, metaAt3 := range []bool{true, false} {
		payload := imageGenPayload("c1", "hi", "https://lh3.googleusercontent.com/abc", metaAt3)
		got := extractGeneratedImages(payload[4].([]any)[0].([]any))
		if len(got) != 1 {
			t.Fatalf("metaAt3=%v: images = %d want 1", metaAt3, len(got))
		}
		img := got[0]
		if img.URL != "https://lh3.googleusercontent.com/abc" {
			t.Fatalf("metaAt3=%v: url = %q", metaAt3, img.URL)
		}
		if img.Mime != "image/png" {
			t.Fatalf("metaAt3=%v: mime = %q", metaAt3, img.Mime)
		}
		if img.Width != 1024 || img.Height != 1024 {
			t.Fatalf("metaAt3=%v: size = %dx%d want 1024x1024", metaAt3, img.Width, img.Height)
		}
		if img.Filename != "generated.png" {
			t.Fatalf("metaAt3=%v: filename = %q", metaAt3, img.Filename)
		}
	}
}

func TestExtractGeneratedImagesAbsent(t *testing.T) {
	// A plain text candidate has no images — and must not panic.
	candidate := []any{nil, []any{"just text"}}
	if got := extractGeneratedImages(candidate); got != nil {
		t.Fatalf("images = %v want nil", got)
	}
}

func TestCleanGeneratedText(t *testing.T) {
	// A placeholder next to real text is stripped, the text survives.
	got := cleanGeneratedText("here you go http://googleusercontent.com/image_generation_content/1", true)
	if strings.Contains(got, "googleusercontent") {
		t.Fatalf("placeholder survived: %q", got)
	}
	if !strings.Contains(got, "here you go") {
		t.Fatalf("text lost: %q", got)
	}
	// Nothing but a placeholder and no image: a hint beats an empty reply.
	if got := cleanGeneratedText("http://googleusercontent.com/image_retrieval/1", false); got != noImageHint {
		t.Fatalf("got %q want the hint", got)
	}
	// Plain text is untouched.
	if got := cleanGeneratedText("plain", false); got != "plain" {
		t.Fatalf("got %q", got)
	}
}

// TestWebStreamBuffersImageTurn drives a full image-generation turn: the text is
// held back, the image is downloaded and stored, and the reply leads with a link
// the client can fetch.
func TestWebStreamBuffersImageTurn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	}))
	defer ts.Close()

	store := imagegen.NewStore(t.TempDir())
	body := "[" + wrbFrame(t, imageGenPayload("c1", "here you go", ts.URL+"/img", true)) + "]"
	s := newWebStream(context.Background(), &Client{http: &http.Client{}},
		io.NopCloser(strings.NewReader(body)), webStreamOpts{
			buffer:    true,
			store:     store,
			imageBase: "https://gw.example.com/omni",
			jar:       parseCookies("__Secure-1PSID=x"),
		})

	var text string
	finished := false
	for {
		ev, err := s.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("recv: %v", err)
		}
		switch ev.Type {
		case provider.EventText:
			text += ev.Text
		case provider.EventFinish:
			finished = true
		}
	}
	if !finished {
		t.Fatal("no finish event")
	}
	const prefix = "![generated image](https://gw.example.com/omni/images/"
	if !strings.HasPrefix(text, prefix) {
		t.Fatalf("reply = %q want it to lead with %q", text, prefix)
	}
	if !strings.HasSuffix(text, "here you go") {
		t.Fatalf("reply = %q want the text after the link", text)
	}

	// The id in the link must resolve to the bytes the server returned.
	id := strings.TrimSuffix(strings.TrimPrefix(text, prefix), ")\nhere you go")
	f, mime, err := store.Open(id)
	if err != nil {
		t.Fatalf("open %q: %v", id, err)
	}
	defer f.Close()
	if mime != "image/png" {
		t.Fatalf("mime = %q", mime)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "PNGDATA" {
		t.Fatalf("stored bytes = %q", data)
	}
}
