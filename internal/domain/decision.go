package domain

import "time"

// DecisionStatus is the lifecycle status of a decision. Decisions are durable:
// they are superseded, never deleted or silently rewritten.
type DecisionStatus string

const (
	DecisionProposed   DecisionStatus = "PROPOSED"
	DecisionAccepted   DecisionStatus = "ACCEPTED"
	DecisionRejected   DecisionStatus = "REJECTED"
	DecisionSuperseded DecisionStatus = "SUPERSEDED"
)

// Decision records what was decided, by whom, and on what evidence.
type Decision struct {
	ID       DecisionID
	TaskID   TaskID
	Author   AgentID
	Decision string
	// Reasoning is the author's stated rationale.
	Reasoning string
	// Evidence lists the observations supporting the decision. Disagreement is
	// resolved by evidence, not by consensus or by rank.
	Evidence     []string
	Participants []AgentID
	Status       DecisionStatus
	// Supersedes points at an earlier decision this one replaces.
	Supersedes DecisionID
	// SupersededBy is set on the older decision when it is replaced.
	SupersededBy DecisionID
	Timestamp    time.Time
}

// Clone returns a deep copy of the decision.
func (d *Decision) Clone() *Decision {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Evidence = append([]string(nil), d.Evidence...)
	cp.Participants = append([]AgentID(nil), d.Participants...)
	return &cp
}

// Disagreement records an explicit conflict between two agents.
//
// The goal is not to force consensus. A challenge is stored as claim,
// counterclaim, evidence and resolution so the disagreement stays visible and
// is settled by evidence rather than by rank or by quietly dropping it.
type Disagreement struct {
	ID     DisagreementID
	TaskID TaskID
	// Claimant made the original claim; Challenger disputed it.
	Claimant   AgentID
	Challenger AgentID
	// ChallengeMessage links back to the CHALLENGE that opened the conflict.
	ChallengeMessage MessageID
	Claim            string
	Counterclaim     string
	Evidence         []string
	Resolved         bool
	Resolution       string
	ResolvedBy       AgentID
	Timestamp        time.Time
	ResolvedAt       *time.Time
}

// Clone returns a deep copy of the disagreement.
func (d *Disagreement) Clone() *Disagreement {
	if d == nil {
		return nil
	}
	cp := *d
	cp.Evidence = append([]string(nil), d.Evidence...)
	if d.ResolvedAt != nil {
		v := *d.ResolvedAt
		cp.ResolvedAt = &v
	}
	return &cp
}
