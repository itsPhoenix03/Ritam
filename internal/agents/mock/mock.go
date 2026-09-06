// Package mockagent provides an in-process ports.AgentAdapter driven by a
// pure Go Behavior function instead of a real CLI process. It exists so the
// whole orchestration flow — routing, authority enforcement, deliberation
// loops, timeouts, crash handling — can be exercised in tests with zero LLM
// access.
package mockagent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Behavior decides what an agent emits in response to an inbound message.
// Returning nil means "stay silent". Behaviors receive the adapter's own id
// (so a single Behavior func can be reused across differently-named agents)
// and the task, which supplies participants/authority context.
type Behavior func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message

// Adapter is an in-process ports.AgentAdapter driven by a Behavior.
type Adapter struct {
	id   domain.AgentID
	kind domain.AgentKind
	b    Behavior

	mu       sync.Mutex
	state    domain.AgentState
	sent     []domain.Message
	out      chan ports.Outbound
	activity chan ports.AgentActivity
	started  bool
	stopped  bool

	// crashOnDeliver, when set, is pushed onto Outbound as a runtime failure
	// immediately after any message is delivered. Used by NewCrashing to
	// simulate a runtime that dies while handling input.
	crashOnDeliver error
}

// New builds a mock adapter with the given identity and behavior.
func New(id domain.AgentID, kind domain.AgentKind, b Behavior) *Adapter {
	return &Adapter{
		id:       id,
		kind:     kind,
		b:        b,
		state:    domain.AgentStateStopped,
		out:      make(chan ports.Outbound, 64),
		activity: make(chan ports.AgentActivity, 128),
	}
}

// ID returns the agent's identity.
func (a *Adapter) ID() domain.AgentID { return a.id }

// Kind returns the adapter kind.
func (a *Adapter) Kind() domain.AgentKind { return a.kind }

// Start moves the adapter to IDLE and reopens it for sends. It is safe to
// call again after Stop.
func (a *Adapter) Start(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.started && !a.stopped {
		return nil // already running
	}
	a.state = domain.AgentStateIdle
	a.started = true
	a.stopped = false
	a.out = make(chan ports.Outbound, 64)
	return nil
}

// Stop moves the adapter to STOPPED and closes Outbound. Safe to call twice.
func (a *Adapter) Stop(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.started || a.stopped {
		return nil
	}
	a.stopped = true
	a.state = domain.AgentStateStopped
	close(a.out)
	return nil
}

// SendTask delivers the initial task assignment.
func (a *Adapter) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return a.deliver(t, m)
}

// SendMessage delivers a routed protocol message. The task travels with it, so
// a behavior can still resolve its counterparts even though supporting agents
// never receive an opening assignment.
func (a *Adapter) SendMessage(_ context.Context, t *domain.Task, m domain.Message) error {
	return a.deliver(t, m)
}

// Activity yields simulated live output so the UI can be exercised offline.
// Mock behaviors are instant, so this reports what the agent did rather than
// streaming it token by token.
func (a *Adapter) Activity() <-chan ports.AgentActivity { return a.activity }

// deliver records the inbound message and synchronously runs the behavior,
// pushing any resulting messages onto Outbound.
func (a *Adapter) deliver(t *domain.Task, m domain.Message) error {
	a.mu.Lock()
	if !a.started || a.stopped {
		a.mu.Unlock()
		return fmt.Errorf("mockagent: %q is not running", a.id)
	}
	a.sent = append(a.sent, *m.Clone())
	a.noteActivity(t, m)
	a.state = domain.AgentStateWorking
	behavior := a.b
	out := a.out
	crash := a.crashOnDeliver
	a.mu.Unlock()

	var results []domain.Message
	if behavior != nil {
		results = behavior(a.id, t, m)
	}

	for _, r := range results {
		if r.ID == "" {
			r.ID = domain.NewMessageID()
		}
		if r.TaskID == "" {
			r.TaskID = m.TaskID
		}
		r.FromAgent = a.id
		if r.Timestamp.IsZero() {
			r.Timestamp = time.Now()
		}
		if r.InReplyTo == "" {
			r.InReplyTo = m.ID
		}
		select {
		case out <- ports.Outbound{AgentID: a.id, Message: r}:
		default:
			// Outbound is buffered generously (64); a full buffer means the
			// consumer has stopped reading, which is a test/caller bug, not
			// something the adapter should block or panic on.
		}
	}

	if crash != nil {
		select {
		case out <- ports.Outbound{AgentID: a.id, Err: crash}:
		default:
		}
	}

	a.mu.Lock()
	if !a.stopped {
		if crash != nil {
			a.state = domain.AgentStateError
		} else {
			a.state = domain.AgentStateIdle
		}
	}
	a.mu.Unlock()
	return nil
}

// noteActivity publishes a short description of what the mock is doing, so the
// UI's per-agent panes have something to show when running offline. The caller
// must hold a.mu.
func (a *Adapter) noteActivity(t *domain.Task, m domain.Message) {
	var taskID domain.TaskID
	if t != nil {
		taskID = t.ID
	} else {
		taskID = m.TaskID
	}
	select {
	case a.activity <- ports.AgentActivity{
		AgentID: a.id,
		TaskID:  taskID,
		Kind:    ports.ActivityStatus,
		Text:    fmt.Sprintf("handling %s from %s", m.Type, m.FromAgent),
		Time:    time.Now(),
	}:
	default:
	}
}

// Interrupt is a no-op for the mock adapter: there is no in-flight work to
// abandon since behaviors run synchronously.
func (a *Adapter) Interrupt(ctx context.Context) error {
	return nil
}

// Status returns the adapter's current lifecycle state.
func (a *Adapter) Status() domain.AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Outbound yields protocol messages the behavior produced.
func (a *Adapter) Outbound() <-chan ports.Outbound {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out
}

// Sent returns every message the adapter received, in order, for assertions.
func (a *Adapter) Sent() []domain.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]domain.Message, len(a.sent))
	copy(out, a.sent)
	return out
}

var _ ports.AgentAdapter = (*Adapter)(nil)
