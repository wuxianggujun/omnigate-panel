package responses

import (
	"sync"
	"time"
)

// StoredConversation is the chat-format message list captured for one stored
// response. A later request can replay it by passing previous_response_id.
type StoredConversation struct {
	Model    string
	Messages []map[string]any
	at       time.Time
}

// Store is a bounded, TTL-expiring, in-memory map of response id →
// conversation. It backs the Responses API's store / previous_response_id
// chaining. Entries are dropped once older than ttl and the map never holds
// more than max entries (oldest evicted first).
type Store struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	byID  map[string]*StoredConversation
	order []string // insertion order, for FIFO eviction
}

// NewStore builds a Store. ttl<=0 disables expiry; max<=0 disables the cap.
func NewStore(ttl time.Duration, max int) *Store {
	if max <= 0 {
		max = 1 << 30
	}
	return &Store{ttl: ttl, max: max, byID: map[string]*StoredConversation{}}
}

// DefaultStore is the process-wide store shared by the /v1 and /omni handlers.
// 30-minute TTL, 2000 conversations: enough for multi-turn chaining without
// unbounded memory growth.
var DefaultStore = NewStore(30*time.Minute, 2000)

// Put records a conversation under id, first evicting expired/over-cap entries.
func (s *Store) Put(id, model string, msgs []map[string]any) {
	if s == nil || id == "" || len(msgs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.evictLocked(now)
	s.byID[id] = &StoredConversation{Model: model, Messages: msgs, at: now}
	s.order = append(s.order, id)
}

// Get returns a stored conversation; ok is false when it is missing or expired.
func (s *Store) Get(id string) (*StoredConversation, bool) {
	if s == nil || id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.byID[id]
	if !ok {
		return nil, false
	}
	if s.ttl > 0 && time.Since(c.at) >= s.ttl {
		delete(s.byID, id)
		return nil, false
	}
	return c, true
}

// evictLocked drops expired head entries, then enforces the size cap.
func (s *Store) evictLocked(now time.Time) {
	for len(s.order) > 0 {
		head := s.order[0]
		c, ok := s.byID[head]
		if !ok {
			s.order = s.order[1:]
			continue
		}
		if s.ttl > 0 && now.Sub(c.at) >= s.ttl {
			delete(s.byID, head)
			s.order = s.order[1:]
			continue
		}
		break
	}
	for len(s.byID) >= s.max && len(s.order) > 0 {
		delete(s.byID, s.order[0])
		s.order = s.order[1:]
	}
}
