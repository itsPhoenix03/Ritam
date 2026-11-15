package domain

import (
	"strings"
	"time"
)

// MessageType is the internal, provider-neutral protocol vocabulary. Adapters
// translate between these types and whatever a concrete CLI actually speaks.
type MessageType string

const (
	MsgTaskAssignment       MessageType = "TASK_ASSIGNMENT"
	MsgDelegate             MessageType = "DELEGATE"
	MsgQuestion             MessageType = "QUESTION"
	MsgAnswer               MessageType = "ANSWER"
	MsgInformation          MessageType = "INFORMATION"
	MsgProposal             MessageType = "PROPOSAL"
	MsgReview               MessageType = "REVIEW"
	MsgChallenge            MessageType = "CHALLENGE"
	MsgApprove              MessageType = "APPROVE"
	MsgReject               MessageType = "REJECT"
	MsgRequestChange        MessageType = "REQUEST_CHANGE"
	MsgImplement            MessageType = "IMPLEMENT"
	MsgImplementationResult MessageType = "IMPLEMENTATION_RESULT"
	MsgTestResult           MessageType = "TEST_RESULT"
	MsgFinalDecision        MessageType = "FINAL_DECISION"
	MsgStatusUpdate         MessageType = "STATUS_UPDATE"
	MsgError                MessageType = "ERROR"
)

// AllMessageTypes lists every known message type, in protocol order.
func AllMessageTypes() []MessageType {
	return []MessageType{
		MsgTaskAssignment, MsgDelegate, MsgQuestion, MsgAnswer, MsgInformation,
		MsgProposal, MsgReview, MsgChallenge, MsgApprove, MsgReject,
		MsgRequestChange, MsgImplement, MsgImplementationResult, MsgTestResult,
		MsgFinalDecision, MsgStatusUpdate, MsgError,
	}
}

// IsKnownMessageType reports whether t is part of the protocol. Agent output is
// untrusted, so unknown types must be rejected rather than passed through.
func IsKnownMessageType(t MessageType) bool {
	for _, k := range AllMessageTypes() {
		if k == t {
			return true
		}
	}
	return false
}

// ExpectsResponse reports whether this message type inherently asks another
// agent for something back.
//
// This is inferred from the type rather than read from Message.RequiresResponse
// because the adapters' wire formats carry no such flag: a real agent emits a
// type and a recipient, so a delegation it never hears back on would otherwise
// be indistinguishable from a completed exchange.
func (t MessageType) ExpectsResponse() bool {
	switch t {
	case MsgDelegate, MsgQuestion, MsgImplement, MsgRequestChange:
		return true
	}
	return false
}

// RequiresAuthority reports whether only the task's authority agent may send
// this message type. This is enforced by the orchestrator, never by prompting.
func (t MessageType) RequiresAuthority() bool {
	switch t {
	case MsgFinalDecision, MsgApprove, MsgReject, MsgRequestChange, MsgImplement, MsgDelegate:
		return true
	}
	return false
}

// HumanAgentID is the reserved identity used for human-originated messages and
// overrides. It can never be an agent-controlled runtime.
const HumanAgentID AgentID = "human"

// OrchestratorID is the sender of the orchestrator's own notes to agents:
// reminders, checkpoints, notices about unreadable or refused replies, resume
// and authority-change notes. It is never a participant and never a
// recipient.
//
// Those notes used to be sent as the human. The request ledger then took a
// checkpoint for the human answering an agent's question, so a decision that
// should have waited for the human's answer was accepted without it; and
// agents, told an error came FROM human, replied to the human about their own
// format mistakes.
const OrchestratorID AgentID = "orchestrator"

// BroadcastAgentID addresses every participant of a task.
const BroadcastAgentID AgentID = "*"

// IsReservedAgentID reports whether id belongs to the system rather than to
// an agent: an agent with one of these ids could speak as the human or the
// orchestrator.
func IsReservedAgentID(id AgentID) bool {
	return id == HumanAgentID || id == OrchestratorID || id == BroadcastAgentID
}

// Metadata keys the orchestrator puts on its own notes.
const (
	// MetaOrchestrator names the kind of note: "reminder", "checkpoint",
	// "resumed", "authority", "closed-notify" and so on.
	MetaOrchestrator = "orchestrator"
	// MetaReason marks a notice about an agent's unreadable reply
	// ("malformed-reply") or partly readable one ("partial-reply").
	MetaReason = "reason"
	// MetaViolation carries the kind of a refused action on the notice that
	// tells the agent about it.
	MetaViolation = "violation"
)

// FromOrchestrator reports whether m is one of the orchestrator's notes.
//
// History written before the orchestrator had an id of its own records those
// notes as sent by the human. They are recognised by the metadata every one
// of them carried, so a task started before the change reads the same as one
// started after it. The rule lives here so every reader of the history (the
// ledger, the run loop, the TUI) applies the same one.
func FromOrchestrator(m *Message) bool {
	if m == nil {
		return false
	}
	switch m.FromAgent {
	case OrchestratorID:
		return true
	case HumanAgentID:
		if m.Metadata[MetaOrchestrator] != "" || m.Metadata[MetaViolation] != "" {
			return true
		}
		switch m.Metadata[MetaReason] {
		case "malformed-reply", "partial-reply":
			return true
		}
	}
	return false
}

// Message is a single routed interaction. Every agent interaction becomes one
// of these; agents never talk to each other by any other means.
type Message struct {
	ID               MessageID
	TaskID           TaskID
	FromAgent        AgentID
	ToAgent          AgentID
	Type             MessageType
	Content          string
	Timestamp        time.Time
	RequiresResponse bool
	// InReplyTo links an answer back to the message that prompted it.
	InReplyTo MessageID
	// Round is the deliberation round in which the message was sent.
	Round int
	// Metadata carries adapter- or type-specific extras. It must not contain
	// credentials of any kind.
	Metadata map[string]string
}

// Clone returns a deep copy of the message.
func (m *Message) Clone() *Message {
	if m == nil {
		return nil
	}
	cp := *m
	if m.Metadata != nil {
		cp.Metadata = make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			cp.Metadata[k] = v
		}
	}
	return &cp
}

// MetaVerdict is the message metadata key carrying a REVIEW's verdict. It is
// read from the reply header, e.g. `type=REVIEW to=human verdict=approve`.
const MetaVerdict = "verdict"

// Verdict is a supporting agent's non-binding judgement on something it was
// asked to check.
//
// It exists because supporting agents had no legitimate way to say "I checked
// it and I agree": asked by a human to verify something, a supporting agent
// reached for APPROVE, which only the authority may send, and was rejected.
// A verdict is advice. Only the authority's APPROVE / REJECT / FINAL_DECISION
// change a task.
type Verdict string

const (
	VerdictApprove Verdict = "approve"
	VerdictChanges Verdict = "changes"
	VerdictReject  Verdict = "reject"
)

// ParseVerdict normalises a verdict as written by an agent, accepting the
// spellings models commonly reach for.
func ParseVerdict(s string) (Verdict, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "approve", "approved", "lgtm", "agree", "pass", "ok":
		return VerdictApprove, true
	case "changes", "changes_requested", "request_changes", "request-changes", "needs_changes", "needs-changes":
		return VerdictChanges, true
	case "reject", "rejected", "disagree", "fail":
		return VerdictReject, true
	}
	return "", false
}

// VerdictOf returns the message's verdict, if it carries a valid one.
func VerdictOf(m *Message) (Verdict, bool) {
	if m == nil || m.Metadata == nil {
		return "", false
	}
	return ParseVerdict(m.Metadata[MetaVerdict])
}

// AllowedTypes lists the message types an agent may send. The authority may
// send everything; a supporting agent everything that does not require
// authority. Prompts list exactly these, so an agent is never offered a type
// the orchestrator will refuse.
func AllowedTypes(authority bool) []MessageType {
	var out []MessageType
	for _, t := range AllMessageTypes() {
		if authority || !t.RequiresAuthority() {
			out = append(out, t)
		}
	}
	return out
}

// SupportingAlternative says what a supporting agent should send instead of
// an authority-only type, so a rejection can tell it how to recover rather
// than only that it was wrong.
func SupportingAlternative(t MessageType) string {
	switch t {
	case MsgApprove:
		return "send REVIEW with verdict=approve; it records your agreement without deciding"
	case MsgReject:
		return "send REVIEW with verdict=reject, or CHALLENGE with your evidence"
	case MsgRequestChange:
		return "send REVIEW with verdict=changes and say what should change"
	case MsgDelegate, MsgImplement:
		return "send a QUESTION to the agent you need, or ask the authority to delegate"
	case MsgFinalDecision:
		return "send your recommendation to the authority as a PROPOSAL"
	}
	return ""
}
