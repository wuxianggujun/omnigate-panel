// Package state persists runtime data (active account per provider, refreshed
// cookies, last check-in times) to data/state.json.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// State is the on-disk runtime state.
type State struct {
	mu          sync.Mutex
	path        string
	Active      map[string]int               `json:"active"`
	Cookies     map[string]map[string]string `json:"cookies"`
	Refresh     map[string]map[string]string `json:"refresh_tokens"`
	Runtime     map[string][]DynAccount      `json:"dyn_accounts"`
	LastCheckin map[string]int64             `json:"last_checkin"`
}

// DynAccount is an account added at runtime (e.g. through the Raccoon web
// login flow) rather than declared in config.json.
type DynAccount struct {
	Label        string `json:"label"`
	Name         string `json:"name,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	OrgName      string `json:"org_name,omitempty"`
	OrgRole      string `json:"org_role,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AddedAt      int64  `json:"added_at,omitempty"`
}

// Load reads (or creates) the state file inside dir.
func Load(dir string) (*State, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &State{
		path:        filepath.Join(dir, "state.json"),
		Active:      map[string]int{},
		Cookies:     map[string]map[string]string{},
		Refresh:     map[string]map[string]string{},
		Runtime:     map[string][]DynAccount{},
		LastCheckin: map[string]int64{},
	}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, s)
	}
	if s.Active == nil {
		s.Active = map[string]int{}
	}
	if s.Cookies == nil {
		s.Cookies = map[string]map[string]string{}
	}
	if s.Refresh == nil {
		s.Refresh = map[string]map[string]string{}
	}
	if s.Runtime == nil {
		s.Runtime = map[string][]DynAccount{}
	}
	if s.LastCheckin == nil {
		s.LastCheckin = map[string]int64{}
	}
	return s, nil
}

func (s *State) saveLocked() {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(s.path, b, 0o600)
}

// Save flushes the state to disk.
func (s *State) Save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveLocked()
}

// ActiveIndex returns the active account index for a provider (0 if unset).
func (s *State) ActiveIndex(provider string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Active[provider]
}

// SetActive records the active account index for a provider.
func (s *State) SetActive(provider string, idx int) {
	s.mu.Lock()
	s.Active[provider] = idx
	s.saveLocked()
	s.mu.Unlock()
}

// Cookie returns the stored cookie for an account, or fallback.
func (s *State) Cookie(provider, label, fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.Cookies[provider]; m != nil {
		if c := m[label]; c != "" {
			return c
		}
	}
	return fallback
}

// SetCookie stores a refreshed cookie for an account.
func (s *State) SetCookie(provider, label, cookie string) {
	s.mu.Lock()
	if s.Cookies[provider] == nil {
		s.Cookies[provider] = map[string]string{}
	}
	s.Cookies[provider][label] = cookie
	s.saveLocked()
	s.mu.Unlock()
}

// RefreshToken returns the stored refresh token for an account, or fallback.
func (s *State) RefreshToken(provider, label, fallback string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.Refresh[provider]; m != nil {
		if t := m[label]; t != "" {
			return t
		}
	}
	return fallback
}

// SetTokens stores an account's access token (cookie slot) and refresh token.
func (s *State) SetTokens(provider, label, access, refresh string) {
	s.mu.Lock()
	if s.Cookies[provider] == nil {
		s.Cookies[provider] = map[string]string{}
	}
	s.Cookies[provider][label] = access
	if s.Refresh[provider] == nil {
		s.Refresh[provider] = map[string]string{}
	}
	s.Refresh[provider][label] = refresh
	s.saveLocked()
	s.mu.Unlock()
}

// DynAccounts returns the runtime-added accounts for a provider.
func (s *State) DynAccounts(provider string) []DynAccount {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.Runtime[provider]
	out := make([]DynAccount, len(src))
	copy(out, src)
	return out
}

// UpsertDynAccount adds or replaces a runtime account, keyed by label.
func (s *State) UpsertDynAccount(provider string, acc DynAccount) {
	s.mu.Lock()
	list := s.Runtime[provider]
	replaced := false
	for i := range list {
		if list[i].Label == acc.Label {
			list[i] = acc
			replaced = true
			break
		}
	}
	if !replaced {
		list = append(list, acc)
	}
	s.Runtime[provider] = list
	if s.Cookies[provider] == nil {
		s.Cookies[provider] = map[string]string{}
	}
	s.Cookies[provider][acc.Label] = acc.AccessToken
	if s.Refresh[provider] == nil {
		s.Refresh[provider] = map[string]string{}
	}
	s.Refresh[provider][acc.Label] = acc.RefreshToken
	s.saveLocked()
	s.mu.Unlock()
}

// RemoveDynAccount deletes a runtime account by label; it reports whether the
// account existed.
func (s *State) RemoveDynAccount(provider, label string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.Runtime[provider]
	out := list[:0]
	found := false
	for _, a := range list {
		if a.Label == label {
			found = true
			continue
		}
		out = append(out, a)
	}
	s.Runtime[provider] = out
	if m := s.Cookies[provider]; m != nil {
		delete(m, label)
	}
	if m := s.Refresh[provider]; m != nil {
		delete(m, label)
	}
	if found {
		s.saveLocked()
	}
	return found
}

// LastCheckinAt returns when a task last ran.
func (s *State) LastCheckinAt(name string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.LastCheckin[name]; ok {
		return time.Unix(v, 0)
	}
	return time.Time{}
}

// MarkCheckin records that a task just ran.
func (s *State) MarkCheckin(name string) {
	s.mu.Lock()
	s.LastCheckin[name] = time.Now().Unix()
	s.saveLocked()
	s.mu.Unlock()
}
