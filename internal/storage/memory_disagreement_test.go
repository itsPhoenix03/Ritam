package storage

import (
	"context"
	"errors"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func TestMemDisagreementRepository(t *testing.T) {
	ctx := context.Background()
	repo := NewMemDisagreementRepository()

	d := &domain.Disagreement{
		ID: "DIS-1", TaskID: "TASK-001",
		Claimant: "opus", Challenger: "codex",
		Claim: "Path=/api is the root cause", Counterclaim: "that is not the root cause",
		Evidence: []string{"integration test clears the cookie correctly"},
	}
	if err := repo.Create(ctx, d); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := repo.Create(ctx, d); !errors.Is(err, ports.ErrAlreadyExists) {
		t.Fatalf("duplicate create error = %v, want ErrAlreadyExists", err)
	}

	got, err := repo.Get(ctx, "DIS-1")
	if err != nil {
		t.Fatal(err)
	}
	// Stored state must not be mutable through a returned value.
	got.Evidence[0] = "tampered"
	again, _ := repo.Get(ctx, "DIS-1")
	if again.Evidence[0] == "tampered" {
		t.Fatal("Get must return a deep copy")
	}

	open, err := repo.ListOpen(ctx, "TASK-001")
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("open disagreements = %d, want 1", len(open))
	}

	resolved := again.Clone()
	resolved.Resolved = true
	resolved.Resolution = "centralize cookie options"
	resolved.ResolvedBy = "opus"
	if err := repo.Update(ctx, resolved); err != nil {
		t.Fatal(err)
	}

	open, _ = repo.ListOpen(ctx, "TASK-001")
	if len(open) != 0 {
		t.Errorf("open disagreements after resolution = %d, want 0", len(open))
	}
	// Resolving must not discard the record.
	all, _ := repo.ListByTask(ctx, "TASK-001")
	if len(all) != 1 {
		t.Fatalf("history length = %d, want the record to be kept", len(all))
	}
	if all[0].Counterclaim != "that is not the root cause" {
		t.Error("resolving must not erase the counterclaim")
	}

	if _, err := repo.Get(ctx, "DIS-absent"); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Get(absent) = %v, want ErrNotFound", err)
	}
	if err := repo.Update(ctx, &domain.Disagreement{ID: "DIS-absent"}); !errors.Is(err, ports.ErrNotFound) {
		t.Errorf("Update(absent) = %v, want ErrNotFound", err)
	}
}

func TestNewInMemoryPopulatesEveryRepository(t *testing.T) {
	s := NewInMemory()
	if s.Tasks == nil || s.Messages == nil || s.Events == nil ||
		s.Decisions == nil || s.Disagreements == nil || s.Knowledge == nil || s.Artifacts == nil {
		t.Fatal("NewInMemory must populate every repository in ports.Storage")
	}
}
