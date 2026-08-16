// Package domain contains the core, dependency-free domain model of the
// orchestrator: tasks, agents, messages, decisions, artifacts, knowledge and
// events. Nothing in this package may import provider SDKs, the TUI, storage
// engines or any other outer layer.
package domain

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

type (
	// TaskID identifies a task, e.g. "TASK-001".
	TaskID string
	// AgentID identifies a registered agent, e.g. "opus".
	AgentID string
	// MessageID identifies a single message on the bus.
	MessageID string
	// EventID identifies an entry in the event log.
	EventID string
	// DecisionID identifies a durable decision, e.g. "D-001".
	DecisionID string
	// ArtifactID identifies an artifact produced by an agent.
	ArtifactID string
	// KnowledgeID identifies a shared-knowledge entry.
	KnowledgeID string
	// DisagreementID identifies a recorded conflict between two agents.
	DisagreementID string
)

// randomSuffix returns a short random hex string. It is used to keep generated
// identifiers unique within a process without pulling in a UUID dependency.
func randomSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal for identity generation; fall back to a
		// clearly-marked value rather than panicking inside the domain layer.
		return "000000000000"
	}
	return hex.EncodeToString(b)
}

// NewMessageID returns a fresh message identifier.
func NewMessageID() MessageID { return MessageID("MSG-" + randomSuffix()) }

// NewEventID returns a fresh event identifier.
func NewEventID() EventID { return EventID("EVT-" + randomSuffix()) }

// NewArtifactID returns a fresh artifact identifier.
func NewArtifactID() ArtifactID { return ArtifactID("ART-" + randomSuffix()) }

// NewKnowledgeID returns a fresh knowledge identifier.
func NewKnowledgeID() KnowledgeID { return KnowledgeID("KN-" + randomSuffix()) }

// NewDisagreementID returns a fresh disagreement identifier.
func NewDisagreementID() DisagreementID { return DisagreementID("DIS-" + randomSuffix()) }

// FormatTaskID renders a sequential task number as a stable, human-readable id.
func FormatTaskID(n int) TaskID { return TaskID(fmt.Sprintf("TASK-%03d", n)) }

// FormatDecisionID renders a sequential decision number.
func FormatDecisionID(n int) DecisionID { return DecisionID(fmt.Sprintf("D-%03d", n)) }
