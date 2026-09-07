package orchestrator

import (
	"fmt"

	"projectorch/internal/domain"
)

// ViolationKind classifies a rejected agent action.
type ViolationKind string

const (
	// ViolationAuthority is an agent attempting an action reserved for the
	// task's authority agent, e.g. a supporting agent issuing FINAL_DECISION.
	ViolationAuthority ViolationKind = "AUTHORITY_VIOLATION"
	// ViolationRouting is an attempt to address a non-participant.
	ViolationRouting ViolationKind = "ROUTING_VIOLATION"
	// ViolationProtocol is malformed or unknown agent output.
	ViolationProtocol ViolationKind = "PROTOCOL_VIOLATION"
	// ViolationIdentity is an agent claiming to be a different sender.
	ViolationIdentity ViolationKind = "IDENTITY_VIOLATION"
	// ViolationState is an action that is not legal in the task's current state.
	ViolationState ViolationKind = "STATE_VIOLATION"
)

// Violation is a rejected action. Rejections are recorded rather than silently
// dropped, because a misbehaving agent is itself useful signal.
type Violation struct {
	Kind    ViolationKind
	TaskID  domain.TaskID
	AgentID domain.AgentID
	Message domain.MessageType
	Reason  string
}

func (v *Violation) Error() string {
	return fmt.Sprintf("%s: agent %s attempted %s on %s: %s",
		v.Kind, v.AgentID, v.Message, v.TaskID, v.Reason)
}

// AuthorityManager enforces the authority model in code. Prompt instructions
// alone are not trusted: if a supporting agent emits FINAL_DECISION while
// another agent holds authority, the action is rejected here regardless of how
// convincingly the agent asserted its own authority.
type AuthorityManager struct{}

// NewAuthorityManager returns an AuthorityManager.
func NewAuthorityManager() *AuthorityManager { return &AuthorityManager{} }

// IsAuthority reports whether agent holds authority over the task.
func (a *AuthorityManager) IsAuthority(t *domain.Task, agent domain.AgentID) bool {
	return t.AuthorityAgent == agent
}

// Authorize validates a message an agent wants to send. It returns a
// *Violation when the action must be rejected.
//
// The human (domain.HumanAgentID) is always permitted: the human remains
// authoritative over the whole system even when an agent holds task authority.
func (a *AuthorityManager) Authorize(t *domain.Task, m *domain.Message) error {
	if !domain.IsKnownMessageType(m.Type) {
		return &Violation{
			Kind: ViolationProtocol, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: "unknown message type",
		}
	}

	if m.FromAgent == domain.HumanAgentID {
		return a.checkRouting(t, m)
	}

	// A terminal or paused task is closed to agents. Only the human can move
	// it again, which is what keeps a runaway agent from continuing to act on
	// work that has been stopped.
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		return &Violation{
			Kind: ViolationState, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: fmt.Sprintf("task is %s; only the human can act on it", t.State),
		}
	}

	if !t.HasParticipant(m.FromAgent) {
		return &Violation{
			Kind: ViolationIdentity, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: "sender is not a participant of this task",
		}
	}

	if m.Type.RequiresAuthority() && !a.IsAuthority(t, m.FromAgent) {
		reason := fmt.Sprintf("only the authority agent %q may send %s", t.AuthorityAgent, m.Type)
		// Say what to send instead. A bare refusal left agents guessing, and
		// the commonest case -- a supporting agent asked to verify something
		// and reaching for APPROVE -- has a legitimate equivalent.
		if alt := domain.SupportingAlternative(m.Type); alt != "" {
			reason += "; instead, " + alt
		}
		return &Violation{
			Kind: ViolationAuthority, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: reason,
		}
	}

	if err := checkVerdict(t, m); err != nil {
		return err
	}

	return a.checkRouting(t, m)
}

// checkVerdict validates a REVIEW's verdict and normalises its spelling. A
// verdict on any other type has no meaning and is dropped rather than refused:
// the message itself is still valid.
func checkVerdict(t *domain.Task, m *domain.Message) error {
	raw, ok := m.Metadata[domain.MetaVerdict]
	if !ok {
		return nil
	}
	if m.Type != domain.MsgReview {
		delete(m.Metadata, domain.MetaVerdict)
		return nil
	}
	v, ok := domain.ParseVerdict(raw)
	if !ok {
		return &Violation{
			Kind: ViolationProtocol, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: fmt.Sprintf("unknown verdict %q; use approve, changes or reject", raw),
		}
	}
	m.Metadata[domain.MetaVerdict] = string(v)
	return nil
}

// checkRouting verifies the recipient. Agents may only address participants of
// the same task or the broadcast address; they can never reach an arbitrary
// agent, and never an OS process directly.
func (a *AuthorityManager) checkRouting(t *domain.Task, m *domain.Message) error {
	switch m.ToAgent {
	case domain.BroadcastAgentID, domain.HumanAgentID:
		return nil
	case "":
		return &Violation{
			Kind: ViolationRouting, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: "no recipient",
		}
	}
	if m.ToAgent == m.FromAgent {
		return &Violation{
			Kind: ViolationRouting, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: "agent addressed itself",
		}
	}
	if !t.HasParticipant(m.ToAgent) {
		return &Violation{
			Kind: ViolationRouting, TaskID: t.ID, AgentID: m.FromAgent, Message: m.Type,
			Reason: fmt.Sprintf("recipient %q is not a participant of this task", m.ToAgent),
		}
	}
	return nil
}

// CanCompleteTask reports whether agent may move the task to COMPLETED. Only
// the task's authority agent, or the human, may do so.
func (a *AuthorityManager) CanCompleteTask(t *domain.Task, agent domain.AgentID) bool {
	return agent == domain.HumanAgentID || a.IsAuthority(t, agent)
}
