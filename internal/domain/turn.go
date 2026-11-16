package domain

import (
	"slices"
	"time"
)

// Turn records one agent turn: what it was handed, when, how it ended and
// what it cost. Nothing recorded which messages went into which turn, so the
// history could not show what an agent was actually given. It holds no
// prompt and no diff, only ids and figures: it is the hook later records
// (accounting, change capture) attach to.
type Turn struct {
	ID      TurnID
	TaskID  TaskID
	AgentID AgentID
	// Messages are the stored messages the turn carried; none for a turn
	// that carried only an orchestrator's retry or a usage report that no
	// known turn could take.
	Messages  []MessageID
	StartedAt time.Time
	EndedAt   *time.Time
	Outcome   TurnOutcome
	// FailureKind says why a failed turn failed.
	FailureKind FailureKind
	// The usage billed to the turn, as the agent's adapter reported it; a
	// killed turn has what it reported before it was stopped, if anything.
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// Clone returns a deep copy of the turn.
func (t *Turn) Clone() *Turn {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Messages = slices.Clone(t.Messages)
	if t.EndedAt != nil {
		at := *t.EndedAt
		cp.EndedAt = &at
	}
	return &cp
}

// TurnOutcome is how an agent turn ended.
type TurnOutcome string

const (
	// TurnRunning has not ended yet.
	TurnRunning TurnOutcome = ""
	// TurnCompleted ended without a failure.
	TurnCompleted TurnOutcome = "completed"
	// TurnFailed ended on a failure of some kind (see FailureKind).
	TurnFailed TurnOutcome = "failed"
	// TurnKilled was ended by the orchestrator before it finished: the run
	// stopped (a pause, a cancel, a completion, ritam exiting) while the
	// agent was still working, or the process died with it.
	TurnKilled TurnOutcome = "killed"
)
