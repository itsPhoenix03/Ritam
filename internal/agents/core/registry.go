// Package core provides the shared agent-runtime plumbing used by every
// adapter: a goroutine-safe registry of agents, the runtime manager that
// starts/stops/routes to them, and a generic os/exec-backed process runtime.
//
// Nothing in this package knows about a specific agent CLI (Claude Code,
// Codex, ...); that lives in the sibling claude/, codex/ and mock/ packages,
// all of which implement ports.AgentAdapter and can be registered here
// interchangeably.
package core

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// entry bundles a registered agent's domain record with what drives it: one
// shared adapter, or a factory that makes a fresh adapter for every task the
// agent works on.
type entry struct {
	agent   domain.Agent
	adapter ports.AgentAdapter
	factory AdapterFactory
}

// AdapterFactory makes a new adapter for an agent. An agent registered with
// one gets its own adapter -- its own CLI processes and sessions -- on every
// task it works on, so the same agent (the same model) can work on several
// tasks at once.
type AdapterFactory func() (ports.AgentAdapter, error)

// Registry is a goroutine-safe collection of agents and their adapters. It
// makes no assumption about how many agents are registered: the orchestrator
// may configure any number of participants.
type Registry struct {
	mu      sync.RWMutex
	entries map[domain.AgentID]*entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: make(map[domain.AgentID]*entry)}
}

// Register adds a new agent and its adapter. Registering the same id twice is
// an error: the registry is the single source of truth for identity.
func (r *Registry) Register(a domain.Agent, ad ports.AgentAdapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[a.ID]; exists {
		return fmt.Errorf("core: agent %q already registered", a.ID)
	}
	if a.LastChanged.IsZero() {
		a.LastChanged = time.Now()
	}
	r.entries[a.ID] = &entry{agent: a, adapter: ad}
	return nil
}

// RegisterFactory adds a new agent whose adapters are made per task by f.
func (r *Registry) RegisterFactory(a domain.Agent, f AdapterFactory) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.entries[a.ID]; exists {
		return fmt.Errorf("core: agent %q already registered", a.ID)
	}
	if a.LastChanged.IsZero() {
		a.LastChanged = time.Now()
	}
	r.entries[a.ID] = &entry{agent: a, factory: f}
	return nil
}

// Factory returns the per-task adapter factory for id, if it has one.
func (r *Registry) Factory(id domain.AgentID) (AdapterFactory, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.factory == nil {
		return nil, false
	}
	return e.factory, true
}

// Get returns the domain record for id.
func (r *Registry) Get(id domain.AgentID) (domain.Agent, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok {
		return domain.Agent{}, false
	}
	return e.agent, true
}

// Adapter returns the adapter backing id.
func (r *Registry) Adapter(id domain.AgentID) (ports.AgentAdapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[id]
	if !ok || e.adapter == nil {
		return nil, false
	}
	return e.adapter, true
}

// List returns every registered agent sorted by id, giving the TUI a stable
// render order across calls.
func (r *Registry) List() []domain.Agent {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Agent, 0, len(r.entries))
	for _, e := range r.entries {
		out = append(out, e.agent)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// SetState updates an agent's lifecycle state and optional last-error text.
// It is a no-op (aside from returning false) when the agent is unknown.
func (r *Registry) SetState(id domain.AgentID, state domain.AgentState, errMsg string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok {
		return false
	}
	e.agent.State = state
	e.agent.LastError = errMsg
	e.agent.LastChanged = time.Now()
	return true
}

// Unregister removes an agent entirely.
func (r *Registry) Unregister(id domain.AgentID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}
