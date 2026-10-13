package geminiweb

import (
	"context"
	"testing"
	"time"

	"github.com/wuxianggujun/omnigate-panel/internal/omnigate/provider"
)

// The ledger keeps at most healthHistoryLimit records per account and always
// exposes the most recent one (gemini2api's deque(maxlen=20) + last_check_result).
func TestHealthLedgerRingAndLast(t *testing.T) {
	h := newHealthLedger()
	for i := 0; i < healthHistoryLimit+5; i++ {
		h.record("a", HealthRecord{Valid: i%2 == 0, CheckedAt: time.Unix(int64(i), 0)})
	}
	recs := h.forLabel("a")
	if len(recs) != healthHistoryLimit {
		t.Fatalf("ring len = %d, want %d", len(recs), healthHistoryLimit)
	}
	if recs[0].CheckedAt.Unix() != 5 {
		t.Fatalf("oldest retained = %d, want 5", recs[0].CheckedAt.Unix())
	}
	last, ok := h.lastFor("a")
	if !ok || last.CheckedAt.Unix() != int64(healthHistoryLimit+4) {
		t.Fatalf("last = %+v ok=%v", last, ok)
	}
	h.forget("a")
	if _, ok := h.lastFor("a"); ok {
		t.Fatal("forget did not drop history")
	}
}

// CheckAccount records every probe — including failures — into the ledger, which
// is what the panel's health-history view reads.
func TestCheckAccountRecordsFailure(t *testing.T) {
	p := NewProvider("gw", nil)
	acc := &provider.Account{Label: "a"} // no cookie → the probe must fail offline
	res := p.CheckAccount(context.Background(), acc)
	if res.Valid {
		t.Fatal("expected an invalid result for an empty cookie")
	}
	recs := p.HealthHistory("a")
	if len(recs) != 1 || recs[0].Valid || recs[0].Error == "" {
		t.Fatalf("unexpected history: %+v", recs)
	}
	if _, ok := p.LastCheck("a"); !ok {
		t.Fatal("LastCheck missing after a probe")
	}
	if got := p.AllHealthHistory(); len(got["a"]) != 1 {
		t.Fatalf("AllHealthHistory = %+v", got)
	}
}
