package geminiweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// newTestProvider returns a provider whose client talks to the given fake
// batchexecute endpoint, plus a pre-seeded healthy session so the management
// calls skip the (network-bound) handshake.
func newTestProvider(t *testing.T, batchURL string, httpClient *http.Client) (*Provider, *provider.Account) {
	t.Helper()
	raw := "__Secure-1PSID=x"
	p := &Provider{name: "gw", client: &Client{http: httpClient, batchURL: batchURL}, sessions: map[string]*session{}}
	p.sessions["acc1"] = &session{
		jar:          fingerprint.Parse(raw),
		sessionToken: "tok",
		source:       raw,
		healthy:      true,
	}
	return p, &provider.Account{Label: "acc1", Cookie: raw}
}

func TestProviderListGems(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rpc := r.URL.Query().Get("rpcids"); rpc != "CNgdBe" {
			t.Errorf("unexpected rpc %q", rpc)
		}
		body := []any{nil, nil, []any{[]any{"g1", []any{"Name", "Desc"}, []any{"Prompt"}}}}
		_, _ = w.Write([]byte(wrbResponse(t, "CNgdBe", body)))
	}))
	defer ts.Close()

	p, acc := newTestProvider(t, ts.URL, ts.Client())
	gems, err := p.ListGems(context.Background(), acc)
	if err != nil {
		t.Fatalf("ListGems: %v", err)
	}
	if len(gems) != 1 || gems[0].ID != "g1" || gems[0].Prompt != "Prompt" {
		t.Fatalf("gems = %+v", gems)
	}
	if acc.Cookie == "" {
		t.Fatal("acc.Cookie was cleared")
	}
}

func TestProviderCleanupChats(t *testing.T) {
	ts, isDeleted := chatServer(t, func() []any {
		return []any{
			[]any{"c_old", "Old", false, nil, nil, []any{float64(1), 0}},
			[]any{"c_pinned", "Pinned", true, nil, nil, []any{float64(1), 0}},
		}
	})
	defer ts.Close()

	p, acc := newTestProvider(t, ts.URL, ts.Client())
	res, err := p.CleanupChats(context.Background(), acc, 24, true)
	if err != nil {
		t.Fatalf("CleanupChats: %v", err)
	}
	if res.Deleted != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	if !isDeleted("c_old") || isDeleted("c_pinned") {
		t.Fatalf("deleted set wrong: old=%v pinned=%v", isDeleted("c_old"), isDeleted("c_pinned"))
	}
}

// TestKeepAliveRequiresCookie: a cookie-less account must fail fast, without any
// network round trip.
func TestKeepAliveRequiresCookie(t *testing.T) {
	p := &Provider{name: "gw", client: &Client{}, sessions: map[string]*session{}}
	if err := p.KeepAlive(context.Background(), &provider.Account{Label: "a"}); err == nil {
		t.Fatal("KeepAlive succeeded with no cookie")
	}
}

// TestStartKeepAliveGating: a non-positive interval disables the loop; a real
// interval starts it exactly once (idempotent).
func TestStartKeepAliveGating(t *testing.T) {
	p := &Provider{name: "gw", client: &Client{}, sessions: map[string]*session{}}
	p.StartKeepAlive(context.Background(), 0, func() []*provider.Account { return nil })
	if p.KeepAliveRunning() {
		t.Fatal("interval<=0 must not start the loop")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the loop's first wait sees a done context and exits immediately
	p.StartKeepAlive(ctx, time.Minute, func() []*provider.Account { return nil })
	p.StartKeepAlive(ctx, time.Minute, func() []*provider.Account { return nil })
	if !p.KeepAliveRunning() {
		t.Fatal("the loop should report running after a real start")
	}
}
