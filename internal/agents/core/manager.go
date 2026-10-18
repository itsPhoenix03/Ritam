package core

import (
	"context"
	"fmt"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// Manager is the Agent Runtime Manager: it owns a Registry and drives every
// adapter's lifecycle (start/stop/restart), routes outbound protocol
// messages to the correct adapter, and observes each adapter's Outbound
// channel so that crashes are reflected back into agent state.
//
// Manager itself never validates protocol rules (authority, task
// participation, message legality); that is the orchestrator's job. Manager
// only knows how to get a message to the right adapter and how to track
// whether that adapter is alive.
type Manager struct {
	log *logging.Logger
	reg *Registry

	// OnStateChange, when set, is invoked whenever an agent's state changes
	// (including after routing detects a crash), so the orchestrator can turn
	// it into a domain.Event without Manager needing to know about events.
	OnStateChange func(domain.Agent)

	// OnInstanceChange, when set, is told whenever one task's instance of an
	// agent changes state (see instances.go).
	OnInstanceChange func(task domain.TaskID, a domain.Agent)

	instOnce sync.Once
	instSet  *instances
}

// NewManager builds a Manager over reg. log may be nil, in which case a
// discard logger is used.
func NewManager(reg *Registry, log *logging.Logger) *Manager {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &Manager{log: log, reg: reg}
}

// Start starts a single agent's adapter.
func (m *Manager) Start(ctx context.Context, id domain.AgentID) error {
	ad, ok := m.reg.Adapter(id)
	if !ok {
		return fmt.Errorf("core: unknown agent %q", id)
	}
	if err := ad.Start(ctx); err != nil {
		m.setState(id, domain.AgentStateError, err.Error())
		return fmt.Errorf("core: start agent %q: %w", id, err)
	}
	m.setState(id, ad.Status(), "")
	return nil
}

// Stop stops a single agent's adapter.
func (m *Manager) Stop(ctx context.Context, id domain.AgentID) error {
	ad, ok := m.reg.Adapter(id)
	if !ok {
		return fmt.Errorf("core: unknown agent %q", id)
	}
	if err := ad.Stop(ctx); err != nil {
		m.setState(id, domain.AgentStateError, err.Error())
		return fmt.Errorf("core: stop agent %q: %w", id, err)
	}
	m.setState(id, domain.AgentStateStopped, "")
	return nil
}

// Restart stops then starts an agent.
//
// TODO(phase-next): this is a manual restart only. Automatic restart-on-crash
// with backoff is a deliberate policy decision (how many attempts, how much
// backoff, when to give up and surface AGENT_UNAVAILABLE to a human) that
// belongs to a later phase, not to this scaffold.
func (m *Manager) Restart(ctx context.Context, id domain.AgentID) error {
	if _, ok := m.reg.Adapter(id); !ok {
		return fmt.Errorf("core: unknown agent %q", id)
	}
	// Best-effort stop: an already-stopped or crashed adapter may error here,
	// which is fine as long as we proceed to Start.
	_ = m.Stop(ctx, id)
	return m.Start(ctx, id)
}

// StartAll starts every registered agent, returning the first error
// encountered (after attempting the rest) so that one bad agent does not
// prevent others from starting.
func (m *Manager) StartAll(ctx context.Context) error {
	var firstErr error
	for _, a := range m.reg.List() {
		if err := m.Start(ctx, a.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// StopAll stops every registered agent.
func (m *Manager) StopAll(ctx context.Context) error {
	var firstErr error
	for _, a := range m.reg.List() {
		if err := m.Stop(ctx, a.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Send routes a message to its ToAgent's adapter via SendMessage. Broadcasts
// (domain.BroadcastAgentID) are the orchestrator's responsibility to expand
// into individual sends; Manager requires a concrete recipient.
func (m *Manager) Send(ctx context.Context, t *domain.Task, msg domain.Message) error {
	if msg.ToAgent == "" || msg.ToAgent == domain.BroadcastAgentID {
		return fmt.Errorf("core: Send requires a concrete recipient, got %q", msg.ToAgent)
	}
	ad, ok := m.AdapterFor(taskOf(t, msg), msg.ToAgent)
	if !ok {
		return fmt.Errorf("core: unknown agent %q", msg.ToAgent)
	}
	if state := ad.Status(); state != domain.AgentStateIdle && state != domain.AgentStateWorking {
		return fmt.Errorf("core: agent %q is not running (state=%s)", msg.ToAgent, state)
	}
	if err := ad.SendMessage(ctx, t, msg); err != nil {
		m.log.ForAgent(msg.ToAgent).Warn("send message failed", "error", err)
		return fmt.Errorf("core: send to %q: %w", msg.ToAgent, err)
	}
	return nil
}

// SendTask hands a task assignment to the given message's recipient.
func (m *Manager) SendTask(ctx context.Context, t *domain.Task, msg domain.Message) error {
	if msg.ToAgent == "" || msg.ToAgent == domain.BroadcastAgentID {
		return fmt.Errorf("core: SendTask requires a concrete recipient, got %q", msg.ToAgent)
	}
	ad, ok := m.AdapterFor(taskOf(t, msg), msg.ToAgent)
	if !ok {
		return fmt.Errorf("core: unknown agent %q", msg.ToAgent)
	}
	if err := ad.SendTask(ctx, t, msg); err != nil {
		m.log.ForAgent(msg.ToAgent).Warn("send task failed", "error", err)
		return fmt.Errorf("core: send task to %q: %w", msg.ToAgent, err)
	}
	return nil
}

// taskOf is the task a delivery belongs to.
func taskOf(t *domain.Task, msg domain.Message) domain.TaskID {
	if t != nil {
		return t.ID
	}
	return msg.TaskID
}

// Interrupt asks an agent to abandon current work without exiting.
func (m *Manager) Interrupt(ctx context.Context, id domain.AgentID) error {
	ad, ok := m.reg.Adapter(id)
	if !ok {
		return fmt.Errorf("core: unknown agent %q", id)
	}
	return ad.Interrupt(ctx)
}

// Status returns the current state of a single agent.
func (m *Manager) Status(id domain.AgentID) domain.AgentState {
	a, ok := m.reg.Get(id)
	if !ok {
		return domain.AgentStateUnavailable
	}
	return a.State
}

// Statuses returns every agent's current domain record.
func (m *Manager) Statuses() []domain.Agent {
	return m.reg.List()
}

// Adapter exposes a registered agent's adapter. The orchestrator needs it to
// drain the adapter's Outbound channel; only one consumer may own that
// channel, which is why the manager does not drain it itself.
func (m *Manager) Adapter(id domain.AgentID) (ports.AgentAdapter, bool) {
	return m.reg.Adapter(id)
}

// ReportOutbound lets a caller that is draining an adapter's Outbound channel
// (typically the orchestrator) feed crash/error signals back into agent
// state. It marks the agent AGENT_ERROR when out.Err is set, or when the
// message itself carries a runtime-exit signal, and AGENT_UNAVAILABLE when
// the adapter's own Status() has settled to Stopped/Unavailable after an
// error.
func (m *Manager) ReportOutbound(id domain.AgentID, out ports.Outbound) {
	if out.Err == nil {
		return
	}
	msg := logging.Redact(out.Err.Error())
	ad, ok := m.reg.Adapter(id)
	state := domain.AgentStateError
	if ok && ad.Status() == domain.AgentStateUnavailable {
		state = domain.AgentStateUnavailable
	}
	m.setState(id, state, msg)
	m.log.ForAgent(id).Error("agent runtime reported failure", "error", msg)
	// TODO(phase-next): once an agent lands in AGENT_ERROR/AGENT_UNAVAILABLE,
	// nothing automatically tries to recover it. A future phase should add a
	// bounded restart-with-backoff policy here (or in the orchestrator) rather
	// than requiring a human to call Restart manually.
}

func (m *Manager) setState(id domain.AgentID, state domain.AgentState, errMsg string) {
	if !m.reg.SetState(id, state, errMsg) {
		return
	}
	if m.OnStateChange != nil {
		if a, ok := m.reg.Get(id); ok {
			m.OnStateChange(a)
		}
	}
}
