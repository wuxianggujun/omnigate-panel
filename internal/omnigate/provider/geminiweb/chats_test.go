package geminiweb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider/geminiweb/fingerprint"
)

// chatFlag reads the [recent, null, [flag, null, 1]] payload's flag.
func chatFlag(t *testing.T, freq string) int {
	t.Helper()
	var p []any
	if err := json.Unmarshal([]byte(batchPayload(t, freq)), &p); err != nil {
		t.Fatalf("payload: %v", err)
	}
	triple, _ := p[2].([]any)
	if len(triple) == 0 {
		t.Fatalf("no flag triple in %s", freq)
	}
	f, _ := triple[0].(float64)
	return int(f)
}

// chatServer is a fake batchexecute endpoint that serves a chat list and applies
// deletes, so the list+delete cleanup loop terminates the way it does upstream.
func chatServer(t *testing.T, chats func() []any) (*httptest.Server, func(string) bool) {
	t.Helper()
	var mu sync.Mutex
	deleted := map[string]bool{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := r.URL.Query().Get("rpcids")
		mu.Lock()
		defer mu.Unlock()
		switch rpc {
		case "MaZiqc":
			var list []any
			for _, item := range chats() {
				arr, _ := item.([]any)
				if len(arr) > 0 {
					if id, _ := arr[0].(string); deleted[id] {
						continue
					}
				}
				list = append(list, item)
			}
			_, _ = w.Write([]byte(wrbResponse(t, "MaZiqc", []any{nil, nil, list})))
		case "GzXR5e":
			var p []any
			_ = json.Unmarshal([]byte(batchPayload(t, r.FormValue("f.req"))), &p)
			if len(p) > 0 {
				if id, _ := p[0].(string); id != "" {
					deleted[id] = true
				}
			}
			_, _ = w.Write([]byte(wrbResponse(t, "GzXR5e", []any{})))
		default:
			_, _ = w.Write([]byte(wrbResponse(t, rpc, []any{})))
		}
	}))
	isDeleted := func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		return deleted[id]
	}
	return ts, isDeleted
}

func TestListWebChats(t *testing.T) {
	now := float64(time.Now().Unix())
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var list []any
		switch chatFlag(t, r.FormValue("f.req")) {
		case 1:
			list = []any{[]any{"c_old", "Old", false, nil, nil, []any{now - 100000, 0}}}
		default:
			list = []any{
				[]any{"c_new", "New", false, nil, nil, []any{now, 0}},
				[]any{"c_old", "Old", false, nil, nil, []any{now - 100000, 0}}, // dup across calls
			}
		}
		_, _ = w.Write([]byte(wrbResponse(t, "MaZiqc", []any{nil, nil, list})))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	chats, err := c.listWebChats(context.Background(), fingerprint.Parse("a=b"), "tok", 300)
	if err != nil {
		t.Fatalf("listWebChats: %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("chats = %+v", chats)
	}
	if chats[0].ID != "c_new" || chats[1].ID != "c_old" {
		t.Fatalf("order = %+v", chats)
	}
	if chats[1].TS != now-100000 {
		t.Fatalf("ts = %v want %v", chats[1].TS, now-100000)
	}
}

func TestDeleteWebChat(t *testing.T) {
	var rpcs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rpc := r.URL.Query().Get("rpcids")
		rpcs = append(rpcs, rpc)
		_, _ = w.Write([]byte(wrbResponse(t, rpc, []any{})))
	}))
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	jar := fingerprint.Parse("a=b")
	if ok, err := c.deleteWebChat(context.Background(), jar, "tok", "c_abc"); err != nil || !ok {
		t.Fatalf("deleteWebChat = %v, %v", ok, err)
	}
	if len(rpcs) != 2 || rpcs[0] != "GzXR5e" || rpcs[1] != "qWymEb" {
		t.Fatalf("rpcs = %v", rpcs)
	}
	// A malformed cid is rejected locally, without a request.
	if ok, err := c.deleteWebChat(context.Background(), jar, "tok", "abc"); err != nil || ok {
		t.Fatalf("deleteWebChat(bad cid) = %v, %v", ok, err)
	}
	if len(rpcs) != 2 {
		t.Fatalf("a request was made for a bad cid: %v", rpcs)
	}
}

func TestCleanupWebChats(t *testing.T) {
	now := float64(time.Now().Unix())
	ts, isDeleted := chatServer(t, func() []any {
		return []any{
			[]any{"c_old", "Old", false, nil, nil, []any{now - 200000, 0}},
			[]any{"c_pinned", "Pinned", true, nil, nil, []any{now - 200000, 0}},
			[]any{"c_new", "New", false, nil, nil, []any{now, 0}},
		}
	})
	defer ts.Close()

	c := &Client{http: ts.Client(), batchURL: ts.URL}
	res, err := c.cleanupWebChats(context.Background(), fingerprint.Parse("a=b"), "tok", 24, true)
	if err != nil {
		t.Fatalf("cleanupWebChats: %v", err)
	}
	if res.Deleted != 1 || res.Failed != 0 {
		t.Fatalf("result = %+v", res)
	}
	// Round 1 lists 3 (delete 1, skip 2); round 2 lists 2 (skip 2) then stops.
	if res.Skipped != 4 || res.Listed != 5 {
		t.Fatalf("result = %+v", res)
	}
	if !isDeleted("c_old") {
		t.Fatal("c_old was not deleted")
	}
	if isDeleted("c_pinned") || isDeleted("c_new") {
		t.Fatal("a protected chat was deleted")
	}
}

// TestCleanupWebChatsKeepHoursFloor: a requested keep_hours below the 1-hour
// floor is clamped, so a stray 0.5 (or 0) cannot delete a chat that is only
// minutes old, while a genuinely old chat is still removed.
func TestCleanupWebChatsKeepHoursFloor(t *testing.T) {
	now := float64(time.Now().Unix())
	for _, tc := range []struct {
		name       string
		ageMinutes float64
		keepHours  float64
		want       int
	}{
		{"sub-floor window is clamped; 45-min chat kept", 45, 0.5, 0},
		{"2h window keeps a 45-min chat", 45, 2, 0},
		{"2h window deletes a 3h chat", 180, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := chatServer(t, func() []any {
				return []any{[]any{"c_x", "Chat", false, nil, nil, []any{now - tc.ageMinutes*60, 0}}}
			})
			defer ts.Close()
			c := &Client{http: ts.Client(), batchURL: ts.URL}
			res, err := c.cleanupWebChats(context.Background(), fingerprint.Parse("a=b"), "tok", tc.keepHours, true)
			if err != nil {
				t.Fatalf("cleanupWebChats: %v", err)
			}
			if res.Deleted != tc.want {
				t.Fatalf("deleted = %d want %d (%+v)", res.Deleted, tc.want, res)
			}
		})
	}
}

func TestParseChatsSkipsMalformed(t *testing.T) {
	now := float64(time.Now().Unix())
	body := []any{nil, nil, []any{
		"not-a-list",
		[]any{},
		[]any{123, "numeric id"}, // id not a string
		[]any{"c_ok", "Ok", float64(1), nil, nil, []any{now, 0}},
	}}
	into := map[string]WebChat{}
	parseChats(body, into)
	if len(into) != 1 {
		t.Fatalf("into = %+v", into)
	}
	if got := into["c_ok"]; got.Title != "Ok" || !got.Pinned {
		t.Fatalf("c_ok = %+v", got)
	}
}
