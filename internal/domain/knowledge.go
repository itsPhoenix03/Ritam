package domain

import "time"

// KnowledgeCategory classifies a shared-knowledge entry so agents can retrieve
// only what is relevant instead of replaying the whole project history.
type KnowledgeCategory string

const (
	KnowledgeProjectContext KnowledgeCategory = "PROJECT_CONTEXT"
	KnowledgeArchitecture   KnowledgeCategory = "ARCHITECTURE"
	KnowledgeDecision       KnowledgeCategory = "DECISION"
	KnowledgeTask           KnowledgeCategory = "TASK"
	KnowledgeAgentNote      KnowledgeCategory = "AGENT_NOTE"
	KnowledgeInvestigation  KnowledgeCategory = "INVESTIGATION"
	KnowledgeArtifact       KnowledgeCategory = "ARTIFACT"
	KnowledgeTestResult     KnowledgeCategory = "TEST_RESULT"
)

// AllKnowledgeCategories lists every category.
func AllKnowledgeCategories() []KnowledgeCategory {
	return []KnowledgeCategory{
		KnowledgeProjectContext, KnowledgeArchitecture, KnowledgeDecision,
		KnowledgeTask, KnowledgeAgentNote, KnowledgeInvestigation,
		KnowledgeArtifact, KnowledgeTestResult,
	}
}

// KnowledgeEntry is a single durable piece of shared project state.
type KnowledgeEntry struct {
	ID       KnowledgeID
	Category KnowledgeCategory
	// TaskID scopes the entry to a task; empty means project-wide.
	TaskID  TaskID
	AgentID AgentID
	Title   string
	Body    string
	Tags    []string
	// Refs points at related records (decision ids, artifact ids, ...).
	Refs      []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Clone returns a deep copy of the entry.
func (k *KnowledgeEntry) Clone() *KnowledgeEntry {
	if k == nil {
		return nil
	}
	cp := *k
	cp.Tags = append([]string(nil), k.Tags...)
	cp.Refs = append([]string(nil), k.Refs...)
	return &cp
}

// KnowledgeQuery describes a shared-knowledge search. All fields are optional
// and combine with AND semantics.
type KnowledgeQuery struct {
	Text       string
	Categories []KnowledgeCategory
	TaskID     TaskID
	AgentID    AgentID
	Tags       []string
	Limit      int
}

// KnowledgeHit is a scored search result. Scoring is intentionally naive for
// now; the interface leaves room for a vector-backed implementation later.
type KnowledgeHit struct {
	Entry *KnowledgeEntry
	Score float64
}
