package domain

import (
	"fmt"
	"time"
)

// EventType names something that happened inside the orchestrator. Every
// meaningful operation emits one so that the TUI, the log and any future
// consumer all observe the same history.
type EventType string

const (
	EvtTaskCreated      EventType = "TaskCreated"
	EvtTaskStateChanged EventType = "TaskStateChanged"
	EvtTaskPaused       EventType = "TaskPaused"
	EvtTaskResumed      EventType = "TaskResumed"
	EvtTaskCancelled    EventType = "TaskCancelled"
	EvtTaskCompleted    EventType = "TaskCompleted"
	EvtTaskFailed       EventType = "TaskFailed"
	// EvtTaskReopened records a human reopening a completed task to follow it
	// up. It is the only way out of a terminal state, and only a human can
	// take it.
	EvtTaskReopened         EventType = "TaskReopened"
	EvtAgentRegistered      EventType = "AgentRegistered"
	EvtAgentStarted         EventType = "AgentStarted"
	EvtAgentStopped         EventType = "AgentStopped"
	EvtAgentStateChanged    EventType = "AgentStateChanged"
	EvtAgentFailed          EventType = "AgentFailed"
	EvtAgentMessageSent     EventType = "AgentMessageSent"
	EvtAgentMessageReceived EventType = "AgentMessageReceived"
	EvtAgentDelegatedTask   EventType = "AgentDelegatedTask"
	EvtAgentChallenged      EventType = "AgentChallenged"
	EvtDecisionCreated      EventType = "DecisionCreated"
	EvtDecisionAccepted     EventType = "DecisionAccepted"
	EvtDecisionRejected     EventType = "DecisionRejected"
	EvtDecisionSuperseded   EventType = "DecisionSuperseded"
	EvtAuthorityViolation   EventType = "AuthorityViolation"
	EvtValidationRejected   EventType = "ValidationRejected"
	// EvtDecisionCaveat records a weakness in an accepted decision, such as
	// one taken while a request the authority sent is still unanswered. It is
	// deliberately not EvtValidationRejected: nothing was rejected, and a log
	// that says "rejected" for something that went through misleads whoever
	// reads it.
	EvtDecisionCaveat EventType = "DecisionCaveat"
	EvtLimitReached   EventType = "LimitReached"
	// EvtCheckpoint marks another interval of autonomous agent work: the human
	// is told where things stand, and the work carries on.
	EvtCheckpoint EventType = "Checkpoint"
	// EvtAgentUsage records what one agent CLI turn cost: tokens in, out and
	// read from the cache, and dollars where the CLI reports them.
	EvtAgentUsage EventType = "AgentUsage"
	// EvtDeliveryHeld records that an agent's messages are waiting for the
	// answers it asked co-workers for, rather than waking it piecemeal.
	EvtDeliveryHeld EventType = "DeliveryHeld"
	// EvtModelUnavailable records that an agent's CLI could not run its
	// model for this account: no credits, not in the plan, an unknown model,
	// not logged in, or a usage limit. Details: provider, model, kind.
	EvtModelUnavailable EventType = "ModelUnavailable"
	// EvtRequestAbandoned records a request closed as unanswered: the agent
	// owing the reply stayed silent after a reminder.
	EvtRequestAbandoned EventType = "RequestAbandoned"
	EvtHumanOverride    EventType = "HumanOverride"
	// EvtAgentActivity carries live agent output for display. It is published
	// ephemerally: never stored, never logged, never acted on.
	EvtAgentActivity    EventType = "AgentActivity"
	EvtKnowledgeWritten EventType = "KnowledgeWritten"
	EvtArtifactRecorded EventType = "ArtifactRecorded"
)

// Event is one entry in a task's chronological log. Events are append-only.
type Event struct {
	ID        EventID
	Type      EventType
	TaskID    TaskID
	AgentID   AgentID
	MessageID MessageID
	Timestamp time.Time
	// Summary is a short human-readable line rendered by the TUI and the log.
	Summary string
	// Details carries structured extras. Never put secrets in here.
	Details map[string]string
}

// String renders the event in event-log form, e.g. "12:01:20 AgentMessageSent
// opus -> codex DELEGATE".
func (e Event) String() string {
	return fmt.Sprintf("%s %-22s %s", e.Timestamp.Format("15:04:05"), e.Type, e.Summary)
}

// EventHandler consumes events. Handlers must not block the orchestrator.
type EventHandler func(Event)
