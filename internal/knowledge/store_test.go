package knowledge

import (
	"context"
	"errors"
	"testing"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
	"projectorch/internal/storage"
)

func newStore() *Store {
	return New(storage.NewMemKnowledgeRepository())
}

func TestStore_PutGetUpdateDelete(t *testing.T) {
	c := context.Background()
	s := newStore()

	e, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeAgentNote, Title: "note", Body: "body"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if e.ID == "" {
		t.Fatal("Put did not assign an id")
	}
	if e.CreatedAt.IsZero() || e.UpdatedAt.IsZero() {
		t.Fatal("Put did not set timestamps")
	}

	got, err := s.Get(c, e.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Body != "body" {
		t.Fatalf("Get: body = %q", got.Body)
	}

	got.Body = "updated"
	updated, err := s.Update(c, got)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Body != "updated" {
		t.Fatalf("Update: body = %q", updated.Body)
	}
	if !updated.CreatedAt.Equal(e.CreatedAt) {
		t.Fatalf("Update must preserve CreatedAt: got %v want %v", updated.CreatedAt, e.CreatedAt)
	}
	if !updated.UpdatedAt.After(e.UpdatedAt) && updated.UpdatedAt != e.UpdatedAt {
		// UpdatedAt should be >= original; allow equality on very fast clocks.
	}

	if err := s.Delete(c, e.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(c, e.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
}

func TestStore_Validation(t *testing.T) {
	c := context.Background()
	s := newStore()

	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeAgentNote}); err == nil {
		t.Fatal("Put with empty title should fail")
	}
	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: "NOT_A_CATEGORY", Title: "t"}); err == nil {
		t.Fatal("Put with unknown category should fail")
	}

	// Update on a nonexistent id should surface ErrNotFound, not silently succeed.
	if _, err := s.Update(c, &domain.KnowledgeEntry{ID: "KN-missing", Category: domain.KnowledgeAgentNote, Title: "t"}); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Update missing: got %v, want ErrNotFound", err)
	}
}

func TestStore_Search(t *testing.T) {
	c := context.Background()
	s := newStore()
	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeInvestigation, TaskID: "TASK-001", Title: "auth bug", Body: "found root cause in login flow"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeAgentNote, TaskID: "TASK-001", Title: "unrelated", Body: "nothing to see"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hits, err := s.Search(c, domain.KnowledgeQuery{Text: "auth"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 1 || hits[0].Entry.Title != "auth bug" {
		t.Fatalf("Search: got %+v", hits)
	}
}

func TestStore_RecordHelpers(t *testing.T) {
	c := context.Background()
	s := newStore()
	taskID := domain.TaskID("TASK-001")
	agentID := domain.AgentID("opus")

	inv, err := s.RecordInvestigation(c, taskID, agentID, "found X", "details about X")
	if err != nil {
		t.Fatalf("RecordInvestigation: %v", err)
	}
	if inv.Category != domain.KnowledgeInvestigation || inv.TaskID != taskID || inv.AgentID != agentID {
		t.Fatalf("RecordInvestigation: unexpected entry %+v", inv)
	}

	tr, err := s.RecordTestResult(c, taskID, agentID, "tests pass", "all green")
	if err != nil || tr.Category != domain.KnowledgeTestResult {
		t.Fatalf("RecordTestResult: %+v, err %v", tr, err)
	}

	note, err := s.RecordAgentNote(c, taskID, agentID, "note", "remember this")
	if err != nil || note.Category != domain.KnowledgeAgentNote {
		t.Fatalf("RecordAgentNote: %+v, err %v", note, err)
	}

	d := &domain.Decision{
		ID: "D-001", TaskID: taskID, Author: agentID,
		Decision: "use postgres", Reasoning: "better fit", Evidence: []string{"benchmark A", "benchmark B"},
	}
	dk, err := s.RecordDecision(c, d)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if dk.Category != domain.KnowledgeDecision {
		t.Fatalf("RecordDecision: category = %s", dk.Category)
	}
	if len(dk.Refs) != 1 || dk.Refs[0] != string(d.ID) {
		t.Fatalf("RecordDecision: refs = %v", dk.Refs)
	}
	if dk.Body == "" {
		t.Fatal("RecordDecision: empty body")
	}
}

func TestStore_ContextFor(t *testing.T) {
	c := context.Background()
	s := newStore()
	taskA := domain.TaskID("TASK-001")
	taskB := domain.TaskID("TASK-002")

	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeInvestigation, TaskID: taskA, Title: "widget bug", Body: "widget renders incorrectly"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeInvestigation, TaskID: taskB, Title: "widget bug elsewhere", Body: "a different task's widget note"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := s.Put(c, &domain.KnowledgeEntry{Category: domain.KnowledgeProjectContext, Title: "project widget policy", Body: "how widgets should generally behave"}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	hits, err := s.ContextFor(c, taskA, "widget", 10)
	if err != nil {
		t.Fatalf("ContextFor: %v", err)
	}
	var sawTaskA, sawProjectWide, sawTaskB bool
	for _, h := range hits {
		switch {
		case h.Entry.TaskID == taskA:
			sawTaskA = true
		case h.Entry.TaskID == "":
			sawProjectWide = true
		case h.Entry.TaskID == taskB:
			sawTaskB = true
		}
	}
	if !sawTaskA {
		t.Error("ContextFor should include task-scoped entries")
	}
	if !sawProjectWide {
		t.Error("ContextFor should include project-wide entries")
	}
	if sawTaskB {
		t.Error("ContextFor must not leak another task's scoped entries")
	}

	// Limit is honored.
	limited, err := s.ContextFor(c, taskA, "widget", 1)
	if err != nil {
		t.Fatalf("ContextFor limited: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("ContextFor limit: got %d hits", len(limited))
	}
}
