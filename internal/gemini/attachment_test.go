package gemini

import (
	"encoding/json"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/openai"
)

// TestInlineImageBecomesPartsArray: an inlineData part must reach the chat body as
// an image_url part. Emitting the text as a plain string would drop the image
// before the gateway ever saw it.
func TestInlineImageBecomesPartsArray(t *testing.T) {
	cr, body := mustChat(t, `{
		"contents": [{"role":"user","parts":[
			{"text":"what is this"},
			{"inlineData":{"mimeType":"image/png","data":"QUJD"}}
		]}]
	}`, false)

	if len(body.Messages) != 1 {
		t.Fatalf("messages = %v", body.Messages)
	}
	parts, ok := body.Messages[0]["content"].([]any)
	if !ok {
		t.Fatalf("content = %#v want a parts array", body.Messages[0]["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("parts = %d want 2 (text + image)", len(parts))
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Fatalf("parts[0] = %#v want the text part first", parts[0])
	}
	img, ok := parts[1].(map[string]any)
	if !ok || img["type"] != "image_url" {
		t.Fatalf("parts[1] = %#v", parts[1])
	}
	if got := img["image_url"].(map[string]any)["url"]; got != "data:image/png;base64,QUJD" {
		t.Fatalf("url = %v", got)
	}

	// The chat layer has to recover the bytes, or the provider never learns there
	// was an attachment.
	var req openai.ChatRequest
	if err := json.Unmarshal(cr.Body, &req); err != nil {
		t.Fatal(err)
	}
	atts := req.Messages[0].Content.Attachments
	if len(atts) != 1 {
		t.Fatalf("attachments = %d want 1", len(atts))
	}
	if atts[0].MIME != "image/png" || string(atts[0].Data) != "ABC" {
		t.Fatalf("attachment = %+v", atts[0])
	}
	if req.Messages[0].Content.Text != "what is this" {
		t.Fatalf("text = %q", req.Messages[0].Content.Text)
	}
}

// TestInlineImageMalformedDegrades: an undecodable part must leave a trace in the
// text rather than vanishing.
func TestInlineImageMalformedDegrades(t *testing.T) {
	_, body := mustChat(t, `{
		"contents": [{"role":"user","parts":[
			{"text":"look"},
			{"inlineData":{"mimeType":"image/png","data":"not base64!"}}
		]}]
	}`, false)

	content, ok := body.Messages[0]["content"].(string)
	if !ok {
		t.Fatalf("content = %#v want a plain string when nothing decodes", body.Messages[0]["content"])
	}
	if content != "look [image omitted]" {
		t.Fatalf("content = %q", content)
	}
}

// TestInlineImageOnlyNoText: an image with no accompanying text still produces a
// message, and still a parts array — there is no text to carry it.
func TestInlineImageOnlyNoText(t *testing.T) {
	_, body := mustChat(t, `{
		"contents": [{"role":"user","parts":[{"inlineData":{"mimeType":"image/jpeg","data":"QUJD"}}]}]
	}`, false)

	if len(body.Messages) != 1 {
		t.Fatalf("messages = %v want 1", body.Messages)
	}
	parts, ok := body.Messages[0]["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %#v want a single image part", body.Messages[0]["content"])
	}
}

// TestInlineImageSnakeCase: the alias-tolerant spelling must work too.
func TestInlineImageSnakeCase(t *testing.T) {
	_, body := mustChat(t, `{
		"contents": [{"role":"user","parts":[{"inline_data":{"mime_type":"image/webp","data":"QUJD"}}]}]
	}`, false)

	parts, ok := body.Messages[0]["content"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("content = %#v", body.Messages[0]["content"])
	}
	img := parts[0].(map[string]any)
	if got := img["image_url"].(map[string]any)["url"]; got != "data:image/webp;base64,QUJD" {
		t.Fatalf("url = %v", got)
	}
}
