package domain

import "time"

// AgentRole describes what an agent is permitted and expected to do within a
// task. Roles are orchestrator-enforced, not merely suggested in a prompt.
type AgentRole string

const (
	// RoleAuthority may accept/reject proposals, resolve disagreements, issue
	// a FINAL_DECISION and complete a task.
	RoleAuthority AgentRole = "AUTHORITY"
	// RoleImplementer may investigate, propose, implement and report results.
	RoleImplementer AgentRole = "IMPLEMENTER"
	// RoleReviewer may review, challenge and provide evidence.
	RoleReviewer AgentRole = "REVIEWER"
	// RoleObserver may only receive information.
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

// CanIssueFinalDecision reports whether the role is permitted, in principle, to
// issue a FINAL_DECISION. Task-level authority is checked separately: holding
// RoleAuthority in the registry is necessary but not sufficient.
func (r AgentRole) CanIssueFinalDecision() bool { return r == RoleAuthority }

// CanImplement reports whether the role may perform IMPLEMENT work.
func (r AgentRole) CanImplement() bool {
	return r == RoleImplementer || r == RoleAuthority
}

// CanChallenge reports whether the role may issue CHALLENGE/REVIEW messages.
// Every non-observer role may disagree; disagreement is a first-class outcome.
func (r AgentRole) CanChallenge() bool { return r != RoleObserver }
