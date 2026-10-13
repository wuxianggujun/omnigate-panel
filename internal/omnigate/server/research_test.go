package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/config"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/gateway"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/logx"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/state"
)

// researchUpstream is a minimal OpenAI-compatible upstream that answers each
// deep-research step deterministically from the prompt it receives.
func researchUpstream() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{"id": "m1", "object": "model"}},
			})
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Messages []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.Unmarshal(body, &req)
			prompt := ""
			if len(req.Messages) > 0 {
				prompt = req.Messages[0].Content
			}
			reply := "generic"
			switch {
			case strings.Contains(prompt, "SUB-QUESTIONS:"):
				reply = "SUB-QUESTIONS:\n1. What is X?\n2. Why does X matter?\n\nOUTLINE:\n- intro"
			case strings.Contains(prompt, "write a comprehensive report"):
				reply = "FINAL REPORT"
			case strings.Contains(prompt, "Research the following question"):
				reply = "finding: " + prompt
			}
			w.Header().Set("Content-Type", "text/event-stream")
			chunk, _ := json.Marshal(map[string]any{
				"choices": []map[string]any{{"delta": map[string]any{"content": reply}}},
			})
			fmt.Fprintf(w, "data: %s\n\n", chunk)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		default:
			http.NotFound(w, r)
		}
	}))
}

func researchTestServer(t *testing.T) *Server {
	t.Helper()
	up := researchUpstream()
	t.Cleanup(up.Close)
	dir := t.TempDir()
	cfg := &config.Config{
		DataDir:         dir,
		DefaultProvider: "oa",
		Providers: []config.ProviderConfig{{
			Name:    "oa",
			Type:    "openai",
			BaseURL: up.URL + "/v1",
			APIKey:  "k",
			Models:  []string{"m1"},
		}},
	}
	st, err := state.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := gateway.New(cfg, st, logx.New(10))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{cfg: cfg, gw: gw}
}

func TestExtractSubQuestions(t *testing.T) {
	plan := "SUB-QUESTIONS:\n1. What is 100 best ways?\n2) Second?\n- Third?\n\nOUTLINE:\n- a\n- b"
	got := extractSubQuestions(plan)
	want := []string{"What is 100 best ways?", "Second?", "Third?"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sub-question %d = %q, want %q", i, got[i], want[i])
		}
	}
	// No section → generic fallback.
	if fb := extractSubQuestions("no structure here"); len(fb) != 1 {
		t.Fatalf("fallback = %v", fb)
	}
}

// TestDeepResearchSync drives the full plan → research → synthesize workflow
// against a fake upstream and checks the assembled result.
func TestDeepResearchSync(t *testing.T) {
	s := researchTestServer(t)
	body := `{"query":"What is X?","model":"oa/m1","language":"en","max_sources":5}`
	r := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/deepresearch", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleDeepResearch(w, s.withImageBase(r))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		TaskID       string   `json:"task_id"`
		Plan         string   `json:"plan"`
		SubQuestions []string `json:"sub_questions"`
		Findings     []string `json:"findings"`
		Report       string   `json:"report"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if res.TaskID == "" || res.Plan == "" {
		t.Fatalf("missing task/plan: %+v", res)
	}
	if len(res.SubQuestions) != 2 || len(res.Findings) != 2 {
		t.Fatalf("sub-questions=%v findings=%d", res.SubQuestions, len(res.Findings))
	}
	if res.Report != "FINAL REPORT" {
		t.Fatalf("report = %q", res.Report)
	}
}

// TestDeepResearchStream checks the SSE event sequence and the terminal result.
func TestDeepResearchStream(t *testing.T) {
	s := researchTestServer(t)
	body := `{"query":"What is X?","model":"oa/m1","max_sources":5}`
	r := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/deepresearch/stream", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleDeepResearchStream(w, s.withImageBase(r))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	out := w.Body.String()
	for _, want := range []string{`"type":"progress"`, `"step":"plan"`, `"step":"research"`, `"step":"synthesize"`, `"type":"result"`, `"type":"done"`, "FINAL REPORT"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream missing %s\n%s", want, out)
		}
	}
}

// TestDeepResearchInteractSync covers the synchronous interactive form.
func TestDeepResearchInteractSync(t *testing.T) {
	s := researchTestServer(t)
	body := `{"input":"Tell me about X","language":"en"}`
	r := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/deepresearch/interact", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.handleDeepResearchInteract(w, s.withImageBase(r))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var res struct {
		Response string `json:"response"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("body not json: %v (%s)", err, w.Body.String())
	}
	if res.Response == "" {
		t.Fatalf("empty response: %s", w.Body.String())
	}
}

// TestDeepResearchRejectsEmptyQuery guards the 400 path.
func TestDeepResearchRejectsEmptyQuery(t *testing.T) {
	s := researchTestServer(t)
	r := httptest.NewRequest(http.MethodPost, "/gemini/v1beta/deepresearch", strings.NewReader(`{"query":"  "}`))
	w := httptest.NewRecorder()
	s.handleDeepResearch(w, s.withImageBase(r))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}
