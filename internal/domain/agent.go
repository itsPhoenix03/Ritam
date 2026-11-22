package domain

import "time"

// AgentRole is the role label an agent is registered with. What the
// orchestrator enforces is task-level authority (Task.AuthorityAgent and
// MessageType.RequiresAuthority), not this label: on a task, every
// participant other than the task's authority may send the same message
// types, whatever its role. The label is shown in the roster and the UI.
type AgentRole string

const (
	// RoleAuthority marks an agent meant to hold authority. Holding it on a
	// task is decided per task, by Task.AuthorityAgent.
	RoleAuthority AgentRole = "AUTHORITY"
	// RoleImplementer is the default label: investigates, proposes,
	// implements and reports results.
	RoleImplementer AgentRole = "IMPLEMENTER"
	// RoleReviewer labels an agent meant to review, challenge and provide
	// evidence. Not enforced: nothing makes its review independent or binding.
	RoleReviewer AgentRole = "REVIEWER"
	// RoleObserver labels an agent meant only to receive information. Not
	// enforced: an observer on a task may send what any supporting agent may.
	RoleObserver AgentRole = "OBSERVER"
)

// AgentState is the lifecycle state of an agent's runtime as tracked by the
// agent manager. It is deliberately distinct from task state.
type AgentState string

const (
	AgentStateStopped     AgentState = "STOPPED"
	AgentStateStarting    AgentState = "STARTING"
	AgentStateIdle        AgentState = "IDLE"
	AgentStateWorking     AgentState = "WORKING"
	AgentStateUnavailable AgentState = "AGENT_UNAVAILABLE"
	AgentStateError       AgentState = "AGENT_ERROR"
)

// AgentKind identifies which adapter implementation backs an agent.
type AgentKind string

const (
	AgentKindClaudeCode AgentKind = "claude-code"
	AgentKindCodex      AgentKind = "codex"
	AgentKindMock       AgentKind = "mock"
)

// Agent is the orchestrator's view of a participant. It carries no
// provider-specific detail; that lives behind the adapter.
type Agent struct {
	ID          AgentID
	DisplayName string
	Kind        AgentKind
	Role        AgentRole
	// Model is the model the agent's CLI runs, "" for the CLI's default.
	Model   string
	Enabled bool
	State   AgentState
	// LastError holds the most recent runtime failure message, if any. It must
	// never contain credentials; adapters are responsible for redaction.
	LastError   string
	LastChanged time.Time
}

// CanIssueFinalDecision reports whether the role label is the authority's.
// It is not what decides who may send a FINAL_DECISION: the orchestrator
// checks Task.AuthorityAgent, which may be any participant whatever its role.
func (r AgentRole) CanIssueFinalDecision() bool { return r == RoleAuthority }

// CanImplement reports whether the role label is one meant to implement. The
// UI uses it for display; whether an agent can change files is its write
// access, which the orchestrator checks for IMPLEMENT.
func (r AgentRole) CanImplement() bool {
	return r == RoleImplementer || r == RoleAuthority
}

// CanChallenge reports whether the role label is one meant to disagree. The
// orchestrator does not call it: every participant may send CHALLENGE and
// REVIEW, and disagreement is a first-class outcome.
func (r AgentRole) CanChallenge() bool { return r != RoleObserver }
