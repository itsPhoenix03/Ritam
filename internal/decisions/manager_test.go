package decisions

import (
	"context"
	"errors"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

func newManager() *Manager {
	return New(storage.NewMemDecisionRepository())
}

func TestManager_ProposeAcceptReject(t *testing.T) {
	c := context.Background()
	m := newManager()
	taskID := domain.TaskID("TASK-001")

	d, err := m.Propose(c, &domain.Decision{TaskID: taskID, Author: "opus", Decision: "use postgres"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if d.ID != "D-001" {
		t.Fatalf("Propose: id = %s, want D-001", d.ID)
	}
	if d.Status != domain.DecisionProposed {
		t.Fatalf("Propose: status = %s", d.Status)
	}

	d2, err := m.Propose(c, &domain.Decision{TaskID: taskID, Author: "opus", Decision: "second"})
	if err != nil || d2.ID != "D-002" {
		t.Fatalf("Propose sequential: %+v, err %v", d2, err)
	}

	accepted, err := m.Accept(c, d.ID)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if accepted.Status != domain.DecisionAccepted {
		t.Fatalf("Accept: status = %s", accepted.Status)
	}

	// Illegal: cannot accept an already-accepted decision.
	if _, err := m.Accept(c, d.ID); err == nil {
		t.Fatal("Accept twice should fail")
	}
	// Illegal: cannot reject an already-accepted decision.
	if _, err := m.Reject(c, d.ID, "changed mind"); err == nil {
		t.Fatal("Reject after accept should fail")
	}

	rejected, err := m.Reject(c, d2.ID, "not enough evidence")
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.Status != domain.DecisionRejected || rejected.Reasoning != "not enough evidence" {
		t.Fatalf("Reject: %+v", rejected)
	}
	// Illegal: cannot accept an already-rejected decision.
	if _, err := m.Accept(c, d2.ID); err == nil {
		t.Fatal("Accept after reject should fail")
	}
}

func TestManager_SupersedeChain(t *testing.T) {
	c := context.Background()
	m := newManager()
	taskID := domain.TaskID("TASK-001")

	original, err := m.Propose(c, &domain.Decision{TaskID: taskID, Author: "opus", Decision: "use mysql"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	if _, err := m.Accept(c, original.ID); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	replacement, err := m.Supersede(c, original.ID, &domain.Decision{Author: "opus", Decision: "use postgres", Reasoning: "better fit"})
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	if replacement.Supersedes != original.ID {
		t.Fatalf("Supersede: new.Supersedes = %s, want %s", replacement.Supersedes, original.ID)
	}
	if replacement.Status != domain.DecisionProposed {
		t.Fatalf("Supersede: new.Status = %s", replacement.Status)
	}
	if replacement.TaskID != taskID {
		t.Fatalf("Supersede: new.TaskID = %s, want %s", replacement.TaskID, taskID)
	}

	old, err := m.Get(c, original.ID)
	if err != nil {
		t.Fatalf("Get old: %v", err)
	}
	if old.Status != domain.DecisionSuperseded {
		t.Fatalf("Supersede: old.Status = %s", old.Status)
	}
	if old.SupersededBy != replacement.ID {
		t.Fatalf("Supersede: old.SupersededBy = %s, want %s", old.SupersededBy, replacement.ID)
	}

	// The original decision must still be retrievable: superseding never deletes.
	list, err := m.ListByTask(c, taskID)
	if err != nil {
		t.Fatalf("ListByTask: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("ListByTask: got %d decisions, want 2 (history preserved)", len(list))
	}

	// Superseding an already-superseded decision is rejected.
	if _, err := m.Supersede(c, original.ID, &domain.Decision{Author: "opus", Decision: "use sqlite"}); err == nil {
		t.Fatal("Supersede of an already-superseded decision should fail")
	}
}

func TestManager_Latest(t *testing.T) {
	c := context.Background()
	m := newManager()
	taskID := domain.TaskID("TASK-001")

	if _, err := m.Latest(c, taskID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Latest with no decisions: got %v, want ErrNotFound", err)
	}

	d1, err := m.Propose(c, &domain.Decision{TaskID: taskID, Author: "opus", Decision: "first"})
	if err != nil {
		t.Fatalf("Propose: %v", err)
	}
	latest, err := m.Latest(c, taskID)
	if err != nil || latest.ID != d1.ID {
		t.Fatalf("Latest: %+v, err %v", latest, err)
	}

	d2, err := m.Supersede(c, d1.ID, &domain.Decision{Author: "opus", Decision: "second"})
	if err != nil {
		t.Fatalf("Supersede: %v", err)
	}
	latest, err = m.Latest(c, taskID)
	if err != nil {
		t.Fatalf("Latest after supersede: %v", err)
	}
	if latest.ID != d2.ID {
		t.Fatalf("Latest after supersede: got %s, want %s (superseded decisions must be excluded)", latest.ID, d2.ID)
	}
}

func TestManager_GetNotFound(t *testing.T) {
	c := context.Background()
	m := newManager()
	if _, err := m.Get(c, "D-999"); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("Get missing: got %v, want ErrNotFound", err)
	}
}
