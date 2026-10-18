package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// AgentGateway is the slice of the agent manager the message bus needs. It is
// declared here, at the point of use, so the orchestrator core does not depend
// on a concrete agent-manager implementation.
//
// Agents are started per task: each task gets its own instance of each of
// its agents, so one agent can work on several tasks at once.
type AgentGateway interface {
	StartFor(ctx context.Context, task domain.TaskID, id domain.AgentID) error
	StopFor(ctx context.Context, task domain.TaskID, id domain.AgentID) error
	Send(ctx context.Context, t *domain.Task, m domain.Message) error
	SendTask(ctx context.Context, t *domain.Task, m domain.Message) error
	AdapterFor(task domain.TaskID, id domain.AgentID) (ports.AgentAdapter, bool)
	Statuses() []domain.Agent
	// ReportOutboundFor feeds crash and error signals back into agent state.
	// The orchestrator owns the adapter's outbound channel, so it is the only
	// component that can observe a runtime failing.
	ReportOutboundFor(task domain.TaskID, id domain.AgentID, out ports.Outbound)
}

// MessageBus validates and routes every message. Agents never reach each other
// by any other path: an agent emits a message, the bus checks identity,
// routing and authority, records it, and only then delivers it.
type MessageBus struct {
	messages  ports.MessageRepository
	events    *EventBus
	authority *AuthorityManager
	agents    AgentGateway
	log       *logging.Logger

	// Enrich optionally augments a message just before it is handed to an
	// adapter. The orchestrator uses it to attach relevant shared knowledge,
	// which is what gives a stateless one-shot agent invocation the context it
	// would otherwise have lost. It never changes what is recorded in the
	// message log, only what the recipient is told.
	Enrich func(ctx context.Context, t *domain.Task, m *domain.Message)

	// RecordOnly optionally reports messages that are to be recorded and
	// announced to their recipients but not handed to their adapters. The
	// orchestrator uses it for a FINAL_DECISION that is about to complete the
	// task: invoking every recipient would start a paid, multi-minute CLI call
	// whose answer is thrown away the moment the task ends.
	RecordOnly func(t *domain.Task, m *domain.Message) bool

	// Handoff optionally takes over delivery of an enriched message to one
	// recipient, returning true when it has. The orchestrator uses it to hold
	// messages for a running task in that agent's inbox and hand them over
	// together when the agent is free (see inbox.go). Without it, or when it
	// returns false, the message goes to the adapter at once.
	Handoff func(ctx context.Context, t *domain.Task, m *domain.Message) bool
}

// NewMessageBus builds a MessageBus.
func NewMessageBus(messages ports.MessageRepository, events *EventBus, authority *AuthorityManager, agents AgentGateway, log *logging.Logger) *MessageBus {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &MessageBus{messages: messages, events: events, authority: authority, agents: agents, log: log}
}

// Route validates, records and delivers a message.
//
// The pipeline is deliberately explicit: agent -> message -> validation ->
// permission check -> action -> event -> state update. A rejected message is
// recorded as a violation event and is never delivered.
func (b *MessageBus) Route(ctx context.Context, t *domain.Task, m *domain.Message) error {
	if m.ID == "" {
		m.ID = domain.NewMessageID()
	}
	if m.Timestamp.IsZero() {
		m.Timestamp = time.Now()
	}
	m.TaskID = t.ID
	m.Round = t.Round

	if err := b.authority.Authorize(t, m); err != nil {
		var v *Violation
		if errors.As(err, &v) {
			evtType := domain.EvtValidationRejected
			if v.Kind == ViolationAuthority {
				evtType = domain.EvtAuthorityViolation
			}
			b.events.emit(ctx, evtType, t.ID, m.FromAgent, v.Error(), map[string]string{
				"kind":        string(v.Kind),
				"messageType": string(m.Type),
				"to":          string(m.ToAgent),
			})
		}
		return err
	}

	if err := b.messages.Append(ctx, m); err != nil {
		return fmt.Errorf("record message: %w", err)
	}

	b.events.emit(ctx, domain.EvtAgentMessageSent, t.ID, m.FromAgent,
		fmt.Sprintf("%s -> %s %s", m.FromAgent, m.ToAgent, m.Type),
		map[string]string{"messageType": string(m.Type), "to": string(m.ToAgent)})

	switch m.Type {
	case domain.MsgDelegate:
		b.events.emit(ctx, domain.EvtAgentDelegatedTask, t.ID, m.FromAgent,
			fmt.Sprintf("%s delegated to %s", m.FromAgent, m.ToAgent), nil)
	case domain.MsgChallenge:
		b.events.emit(ctx, domain.EvtAgentChallenged, t.ID, m.FromAgent,
			fmt.Sprintf("%s challenged %s", m.FromAgent, m.ToAgent), nil)
	}

	return b.deliver(ctx, t, m)
}

// deliver hands the message to the recipient's adapter. Messages addressed to
// the human are not delivered to any runtime: the TUI reads them from history.
func (b *MessageBus) deliver(ctx context.Context, t *domain.Task, m *domain.Message) error {
	targets := []domain.AgentID{m.ToAgent}
	if m.ToAgent == domain.BroadcastAgentID {
		targets = targets[:0]
		for _, p := range t.Participants {
			if p != m.FromAgent {
				targets = append(targets, p)
			}
		}
	}

	recordOnly := b.RecordOnly != nil && b.RecordOnly(t, m)

	var firstErr error
	for _, to := range targets {
		if to == domain.HumanAgentID {
			continue
		}
		if recordOnly {
			// The message is already in history; only the invocation is
			// skipped. The event is kept so the timeline still shows who was
			// told what.
			b.events.emit(ctx, domain.EvtAgentMessageReceived, t.ID, to,
				fmt.Sprintf("%s received %s from %s (recorded, not invoked)", to, m.Type, m.FromAgent),
				map[string]string{"messageId": string(m.ID), "invoked": "false"})
			continue
		}
		delivered := m.Clone()
		delivered.ToAgent = to
		if b.Enrich != nil {
			b.Enrich(ctx, t, delivered)
		}
		if b.Handoff != nil && b.Handoff(ctx, t, delivered) {
			continue
		}

		// The task record travels with every delivery, not just the opening
		// assignment. Supporting agents are never briefed up front, so the
		// first thing they ever see is a DELEGATE — and without the task they
		// would not know the task, their own role, or which agent ids they are
		// allowed to reply to.
		var err error
		if m.Type == domain.MsgTaskAssignment {
			err = b.agents.SendTask(ctx, t, *delivered)
		} else {
			err = b.agents.Send(ctx, t, *delivered)
		}
		if err != nil {
			b.log.Error("deliver message", "taskId", string(t.ID), "agentId", string(to),
				"messageId", string(m.ID), "error", err)
			b.events.emit(ctx, domain.EvtAgentFailed, t.ID, to,
				fmt.Sprintf("delivery to %s failed: %v", to, err), nil)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		b.events.emit(ctx, domain.EvtAgentMessageReceived, t.ID, to,
			fmt.Sprintf("%s received %s from %s", to, m.Type, m.FromAgent),
			map[string]string{"messageId": string(m.ID)})
	}
	return firstErr
}

// History returns the recorded messages of a task.
func (b *MessageBus) History(ctx context.Context, id domain.TaskID) ([]*domain.Message, error) {
	return b.messages.ListByTask(ctx, id)
}
