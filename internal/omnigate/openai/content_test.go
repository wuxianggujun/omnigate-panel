package openai

import (
	"encoding/json"
	"testing"
)

// TestContentStringForm: the common case must stay exactly as it was — text only,
// no attachments.
func TestContentStringForm(t *testing.T) {
	var c Content
	if err := json.Unmarshal([]byte(`"hello"`), &c); err != nil {
		t.Fatal(err)
	}
	if c.Text != "hello" {
		t.Fatalf("text = %q", c.Text)
	}
	if len(c.Attachments) != 0 {
		t.Fatalf("attachments = %v", c.Attachments)
	}
}

// TestContentTextParts: an array of text parts is flattened, and nothing is
// mistaken for an attachment.
func TestContentTextParts(t *testing.T) {
	var c Content
	body := `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Text != "ab" {
		t.Fatalf("text = %q want %q", c.Text, "ab")
	}
	if len(c.Attachments) != 0 {
		t.Fatalf("attachments = %v", c.Attachments)
	}
}

// TestContentImageDataURL: an inline image is decoded once here, so the provider
// receives bytes and never has to re-parse a data: URL.
func TestContentImageDataURL(t *testing.T) {
	var c Content
	body := `[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Text != "look" {
		t.Fatalf("text = %q", c.Text)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %d want 1", len(c.Attachments))
	}
	att := c.Attachments[0]
	if att.Type != "image_url" {
		t.Fatalf("type = %q", att.Type)
	}
	if string(att.Data) != "ABC" {
		t.Fatalf("data = %q want ABC", att.Data)
	}
	if att.MIME != "image/png" {
		t.Fatalf("mime = %q", att.MIME)
	}
	if att.URL != "" {
		t.Fatalf("url = %q want empty for an inline part", att.URL)
	}
	if !att.Inline() {
		t.Fatal("Inline() = false for a part with bytes")
	}
}

// TestContentImageRemoteURL: a part that points at a URL keeps the URL (the
// provider fetches it, guarded) and carries no bytes.
func TestContentImageRemoteURL(t *testing.T) {
	var c Content
	body := `[{"type":"image_url","image_url":"https://example.com/a.png"}]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %d want 1", len(c.Attachments))
	}
	att := c.Attachments[0]
	if att.URL != "https://example.com/a.png" {
		t.Fatalf("url = %q", att.URL)
	}
	if att.Inline() {
		t.Fatal("Inline() = true for a remote part")
	}
}

// TestContentFileDataURL: the file part carries its display name, which the
// upload sends as the multipart filename.
func TestContentFileDataURL(t *testing.T) {
	var c Content
	body := `[{"type":"file","file":{"filename":"notes.txt","file_data":"data:text/plain;base64,aGk="}}]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 1 {
		t.Fatalf("attachments = %d want 1", len(c.Attachments))
	}
	att := c.Attachments[0]
	if att.Filename != "notes.txt" {
		t.Fatalf("filename = %q", att.Filename)
	}
	if string(att.Data) != "hi" {
		t.Fatalf("data = %q", att.Data)
	}
	if att.MIME != "text/plain" {
		t.Fatalf("mime = %q", att.MIME)
	}
}

// TestContentFileIDOnlyDropped: a part that only names a file_id cannot be
// resolved by anything here, so it is dropped rather than uploaded empty.
func TestContentFileIDOnlyDropped(t *testing.T) {
	var c Content
	body := `[{"type":"file","file":{"file_id":"file-123"}}]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Attachments) != 0 {
		t.Fatalf("attachments = %v want none", c.Attachments)
	}
}

// TestContentUnknownShapeKeepsRaw: an array that holds no part objects must not
// turn into an empty string — the previous behaviour kept the raw JSON so nothing
// was lost silently, and that must survive the attachment work.
func TestContentUnknownShapeKeepsRaw(t *testing.T) {
	var c Content
	body := `["a","b"]`
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		t.Fatal(err)
	}
	if c.Text != body {
		t.Fatalf("text = %q want the raw body %q", c.Text, body)
	}
}

// TestContentMarshalStaysString: replies are text-only, so marshalling must never
// grow a parts array.
func TestContentMarshalStaysString(t *testing.T) {
	c := Content{Text: "hi", Attachments: []Attachment{{Type: "image_url", Data: []byte("x")}}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"hi"` {
		t.Fatalf("marshal = %s want \"hi\"", b)
	}
}

// TestDecodeDataURLRejects: anything that is not a base64 data: URL must be left
// for the URL path rather than decoded into garbage.
func TestDecodeDataURLRejects(t *testing.T) {
	for _, s := range []string{
		"https://example.com/a.png",
		"data:image/png,notbase64",
		"data:image/png;base64,",
		"data:image/png;base64,!!!!",
		"",
	} {
		if _, _, ok := decodeDataURL(s); ok {
			t.Fatalf("decodeDataURL(%q) succeeded", s)
		}
	}
}
