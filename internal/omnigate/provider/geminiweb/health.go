package geminiweb

import (
	"sync"
	"time"
)

// healthHistoryLimit is how many recent probes are retained per account,
// mirroring gemini2api's `deque(maxlen=20)` on each client.
const healthHistoryLimit = 20

// HealthRecord is one session-probe outcome, ported from gemini2api's
// check_account() result dict ({"valid", "has_token", "models_count",
// "checked_at", "error"?}). HasToken duplicates Valid because, like the
// reference, an account is only "valid" when the SNlM0e token was parsed.
type HealthRecord struct {
	Valid       bool      `json:"valid"`
	HasToken    bool      `json:"has_token"`
	ModelsCount int       `json:"models_count"`
	CheckedAt   time.Time `json:"checked_at"`
	Error       string    `json:"error,omitempty"`
}

// healthLedger keeps a per-account ring buffer of probe outcomes plus the most
// recent one. It is safe for concurrent use and nil-tolerant (a provider built
// before this feature simply records nothing).
type healthLedger struct {
	mu      sync.Mutex
	history map[string][]HealthRecord
	last    map[string]HealthRecord
}

func newHealthLedger() *healthLedger {
	return &healthLedger{
		history: map[string][]HealthRecord{},
		last:    map[string]HealthRecord{},
	}
}

// record appends one outcome for label, trimming the ring to healthHistoryLimit.
func (h *healthLedger) record(label string, r HealthRecord) {
	if h == nil || label == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ring := append(h.history[label], r)
	if len(ring) > healthHistoryLimit {
		ring = ring[len(ring)-healthHistoryLimit:]
	}
	h.history[label] = ring
	h.last[label] = r
}

// forLabel returns a copy of one account's history (oldest first).
func (h *healthLedger) forLabel(label string) []HealthRecord {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]HealthRecord(nil), h.history[label]...)
}

// lastFor returns the most recent outcome for label.
func (h *healthLedger) lastFor(label string) (HealthRecord, bool) {
	if h == nil {
		return HealthRecord{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.last[label]
	return r, ok
}

// all returns a copy of every account's history keyed by label.
func (h *healthLedger) all() map[string][]HealthRecord {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string][]HealthRecord, len(h.history))
	for k, v := range h.history {
		out[k] = append([]HealthRecord(nil), v...)
	}
	return out
}

// forget drops any retained history for a label (used after account removal).
func (h *healthLedger) forget(label string) {
	if h == nil || label == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.history, label)
	delete(h.last, label)
}

// HealthHistory returns the probe history for one account label (oldest first).
func (p *Provider) HealthHistory(label string) []HealthRecord { return p.health.forLabel(label) }

// AllHealthHistory returns every account's probe history keyed by label.
func (p *Provider) AllHealthHistory() map[string][]HealthRecord { return p.health.all() }

// LastCheck returns the most recent probe outcome for a label.
func (p *Provider) LastCheck(label string) (HealthRecord, bool) { return p.health.lastFor(label) }
