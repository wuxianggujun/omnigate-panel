package plugin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Registry errors. Callers can use errors.Is to distinguish them from I/O or
// validation failures.
var (
	ErrDuplicateType = errors.New("plugin: provider 类型重复注册")
	ErrUnknownType   = errors.New("plugin: 未知 provider 类型")
	ErrInvalidPlugin = errors.New("plugin: 无效的插件描述")
)

// Registry is an explicit, concurrency-safe descriptor registry. Registration
// happens at startup; reads dominate afterwards.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]Plugin
}

// NewRegistry returns an empty registry. Use it in tests or when embedding a
// custom provider set instead of the process-wide default.
func NewRegistry() *Registry {
	return &Registry{plugins: make(map[string]Plugin)}
}

var defaultRegistry = NewRegistry()

// Register adds p to the default registry. A duplicate type is reported as
// ErrDuplicateType; use MustRegister when a startup conflict should crash.
func Register(p Plugin) error { return defaultRegistry.Register(p) }

// MustRegister adds p to the default registry and panics on any error
// (including a duplicate type). Intended for bootstrap code.
func MustRegister(p Plugin) { defaultRegistry.MustRegister(p) }

// Get returns the descriptor registered for typ.
func Get(typ string) (Plugin, error) { return defaultRegistry.Get(typ) }

// All returns every registered descriptor, sorted by DisplayName then Type.
func All() []Plugin { return defaultRegistry.All() }

// Types returns every registered type, sorted lexicographically.
func Types() []string { return defaultRegistry.Types() }

// TaskByType resolves a scheduled task type (config.CheckinTask.Type) to the
// plugin that declares it and the declaration itself. The scheduler uses it to
// dispatch by descriptor instead of switching on the type string.
func TaskByType(taskType string) (Plugin, Task, bool) {
	return defaultRegistry.TaskByType(taskType)
}

// TypesByLoginTransport returns every type whose login flow is driven over the
// given transport, sorted lexically. The panel uses it to decide which kinds its
// /omni/admin/<type>/* proxy accepts, so a new admin-transport provider is a
// catalog-only change — no panel edit required.
func TypesByLoginTransport(t LoginTransport) []string {
	return defaultRegistry.TypesByLoginTransport(t)
}

// Register validates and stores p. The stored copy is deep-copied so later
// caller mutations cannot change what other layers see.
func (r *Registry) Register(p Plugin) error {
	p = p.normalized()
	if err := p.Validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.plugins == nil {
		r.plugins = make(map[string]Plugin)
	}
	if _, exists := r.plugins[p.Type]; exists {
		return fmt.Errorf("%w: %q", ErrDuplicateType, p.Type)
	}
	r.plugins[p.Type] = p
	return nil
}

// MustRegister is Register with panic-on-error semantics.
func (r *Registry) MustRegister(p Plugin) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Get returns a copy of the descriptor for typ.
func (r *Registry) Get(typ string) (Plugin, error) {
	r.mu.RLock()
	p, ok := r.plugins[typ]
	r.mu.RUnlock()
	if !ok {
		registered := r.Types()
		list := "none"
		if len(registered) > 0 {
			list = strings.Join(registered, ", ")
		}
		return Plugin{}, fmt.Errorf("%w: %q (registered: %s)", ErrUnknownType, typ, list)
	}
	return p.clone(), nil
}

// All returns copies of all descriptors sorted by DisplayName then Type.
func (r *Registry) All() []Plugin {
	r.mu.RLock()
	out := make([]Plugin, 0, len(r.plugins))
	for _, p := range r.plugins {
		out = append(out, p.clone())
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// Types returns all registered types in lexical order.
func (r *Registry) Types() []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.plugins))
	for typ := range r.plugins {
		out = append(out, typ)
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// TaskByType resolves a scheduled task type to its declaring plugin and the
// declaration. A task type is owned by at most one plugin (Validate rejects
// duplicates within a descriptor); if two plugins somehow claim the same task
// type, the one whose type sorts first wins so resolution stays deterministic.
func (r *Registry) TaskByType(taskType string) (Plugin, Task, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var (
		best     Plugin
		bestTask Task
		found    bool
	)
	for _, p := range r.plugins {
		for _, t := range p.Tasks {
			if t.Type != taskType {
				continue
			}
			if !found || p.Type < best.Type {
				best, bestTask, found = p, t, true
			}
			break
		}
	}
	if !found {
		return Plugin{}, Task{}, false
	}
	return best.clone(), bestTask, true
}

// TypesByLoginTransport returns every registered type whose Login.Transport
// equals t, sorted lexically. A nil Login (no interactive flow) never matches.
func (r *Registry) TypesByLoginTransport(t LoginTransport) []string {
	r.mu.RLock()
	out := make([]string, 0, len(r.plugins))
	for typ, p := range r.plugins {
		if p.Login != nil && p.Login.Transport == t {
			out = append(out, typ)
		}
	}
	r.mu.RUnlock()
	sort.Strings(out)
	return out
}

// normalized turns nil slices into empty ones so JSON consumers always get
// arrays ([] rather than null) for the schema/action/task lists.
func (p Plugin) normalized() Plugin {
	p = p.clone()
	if p.ConfigFields == nil {
		p.ConfigFields = []Field{}
	}
	if p.AccountFields == nil {
		p.AccountFields = []Field{}
	}
	if p.Actions == nil {
		p.Actions = []Action{}
	}
	if p.Tasks == nil {
		p.Tasks = []Task{}
	}
	if p.Capabilities == nil {
		p.Capabilities = []Capability{}
	}
	return p
}

// clone deep-copies the slices a caller could otherwise mutate in place.
func (p Plugin) clone() Plugin {
	out := p
	out.ConfigFields = cloneFields(p.ConfigFields)
	out.AccountFields = cloneFields(p.AccountFields)
	out.Actions = cloneActions(p.Actions)
	out.Tasks = cloneTasks(p.Tasks)
	out.Capabilities = cloneCapabilities(p.Capabilities)
	if p.Login != nil {
		login := *p.Login
		login.Fields = cloneFields(p.Login.Fields)
		out.Login = &login
	}
	return out
}

func cloneActions(in []Action) []Action {
	if in == nil {
		return nil
	}
	out := make([]Action, len(in))
	copy(out, in)
	return out
}

func cloneTasks(in []Task) []Task {
	if in == nil {
		return nil
	}
	out := make([]Task, len(in))
	copy(out, in)
	return out
}

func cloneCapabilities(in []Capability) []Capability {
	if in == nil {
		return nil
	}
	out := make([]Capability, len(in))
	copy(out, in)
	return out
}

func cloneFields(in []Field) []Field {
	if in == nil {
		return nil
	}
	out := make([]Field, len(in))
	for i, f := range in {
		if f.Enum != nil {
			f.Enum = append([]EnumOption(nil), f.Enum...)
		}
		if f.ShowIf != nil {
			c := *f.ShowIf
			f.ShowIf = &c
		}
		out[i] = f
	}
	return out
}
