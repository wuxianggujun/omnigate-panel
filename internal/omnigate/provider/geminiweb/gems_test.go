package geminiweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// wrbResponse renders a batchexecute response whose single wrb.fr frame for
// rpcID decodes to body, so the wire parsers can be exercised without the real
// upstream.
func wrbResponse(t *testing.T, rpcID string, body any) string {
	t.Helper()
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	outer, err := json.Marshal([]any{[]any{"wrb.fr", rpcID, string(bodyJSON), nil, nil, nil, "generic"}})
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	return ")]}'\n\n" + string(outer) + "\n"
}

// batchPayload extracts the inner RPC payload string from an f.req form value.
func batchPayload(t *testing.T, freq string) string {
	t.Helper()
	var outer []any
	if err := json.Unmarshal([]byte(freq), &outer); err != nil {
		t.Fatalf("f.req not JSON: %v", err)
	}
	rows, _ := outer[0].([]any)
	if len(rows) == 0 {
		t.Fatalf("f.req has no RPC row: %s", freq)
	}
	row, _ := rows[0].([]any)
	if len(row) < 2 {
		t.Fatalf("f.req RPC row too short: %s", freq)
	}
	s, _ := row[1].(string)
	return s
}

func TestListGems(t *testing.T) {
	var rpcID, at string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpcID = r.URL.Query().Get("rpcids")
		at = r.FormValue("at")
		body := []any{nil, nil, []any{
			[]any{"gem-1", []any{"Summarizer", "summarises"}, []any{"You summarise."}},
			[]any{"gem-2", []any{"Translator"}, []any{}},
			[]any{"gem-3"},
		}}
		_, _ = w.Write([]byte(wrbResponse(t, "CNgdBe", body)))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	gems, err := c.listGems(context.Background(), fingerprint.Parse("__Secure-1PSID=x"), "tok")
	if err != nil {
		t.Fatalf("listGems: %v", err)
	}
	if rpcID != "CNgdBe" {
		t.Fatalf("rpcids = %q want CNgdBe", rpcID)
	}
	if at != "tok" {
		t.Fatalf("at = %q want tok", at)
	}
	if len(gems) != 3 {
		t.Fatalf("gems = %+v", gems)
	}
	if gems[0] != (Gem{ID: "gem-1", Name: "Summarizer", Description: "summarises", Prompt: "You summarise."}) {
		t.Fatalf("gem[0] = %+v", gems[0])
	}
	if gems[1].Name != "Translator" || gems[1].Prompt != "" || gems[1].Description != "" {
		t.Fatalf("gem[1] = %+v", gems[1])
	}
	if gems[2].ID != "gem-3" || gems[2].Name != "" {
		t.Fatalf("gem[2] = %+v", gems[2])
	}
}

func TestCreateGemPayload(t *testing.T) {
	var freq string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		freq = r.FormValue("f.req")
		_, _ = w.Write([]byte(wrbResponse(t, "oMH3Zd", []any{"new-gem-id"})))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	id, err := c.createGem(context.Background(), fingerprint.Parse("a=b"), "tok", "Name", "Prompt", "Desc")
	if err != nil {
		t.Fatalf("createGem: %v", err)
	}
	if id != "new-gem-id" {
		t.Fatalf("id = %q", id)
	}
	payload := batchPayload(t, freq)
	var arr []any
	if err := json.Unmarshal([]byte(payload), &arr); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, payload)
	}
	inner, _ := arr[0].([]any)
	if len(inner) != 15 {
		t.Fatalf("inner len = %d want 15 (%s)", len(inner), payload)
	}
	if inner[0] != "Name" || inner[1] != "Desc" || inner[2] != "Prompt" {
		t.Fatalf("name/desc/prompt = %v/%v/%v", inner[0], inner[1], inner[2])
	}
	if inner[8] != float64(0) || inner[10] != float64(1) {
		t.Fatalf("flag slots = %v/%v", inner[8], inner[10])
	}
	if _, ok := inner[14].([]any); !ok {
		t.Fatalf("slot 14 not an array: %v", inner[14])
	}
}

func TestUpdateAndDeleteGem(t *testing.T) {
	var rpcs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := r.URL.Query().Get("rpcids")
		rpcs = append(rpcs, rpc)
		_, _ = w.Write([]byte(wrbResponse(t, rpc, []any{"ok"})))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	jar := fingerprint.Parse("a=b")
	if ok, err := c.updateGem(context.Background(), jar, "tok", "gem-1", "N", "P", "D"); err != nil || !ok {
		t.Fatalf("updateGem = %v, %v", ok, err)
	}
	if ok, err := c.deleteGem(context.Background(), jar, "tok", "gem-1"); err != nil || !ok {
		t.Fatalf("deleteGem = %v, %v", ok, err)
	}
	// An empty id is rejected locally, without a request.
	if ok, err := c.deleteGem(context.Background(), jar, "tok", "  "); err != nil || ok {
		t.Fatalf("deleteGem(empty) = %v, %v", ok, err)
	}
	if len(rpcs) != 2 || rpcs[0] != "kHv0Vd" || rpcs[1] != "UXcSJb" {
		t.Fatalf("rpcs = %v", rpcs)
	}
}

// TestGemsUpstreamError: a non-2xx batchexecute surfaces as an error rather than
// an empty list, so the caller can tell "none" from "call failed".
func TestGemsUpstreamError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("nope"))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	if _, err := c.listGems(context.Background(), fingerprint.Parse("a=b"), "tok"); err == nil {
		t.Fatal("listGems succeeded on a 403")
	}
}
