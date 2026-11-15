package decisions

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

func backends(t *testing.T, run func(t *testing.T, s *ports.Storage)) {
	t.Run("memory", func(t *testing.T) { run(t, storage.NewInMemory()) })
	t.Run("sqlite", func(t *testing.T) {
		db, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "ritam.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		run(t, db.Storage())
	})
}

var errCrash = errors.New("crash between the two writes")

// failingUpdate stands for a crash after the new decision is proposed and
// before the old one is marked superseded: its Update, the second write,
// fails.
type failingUpdate struct{ ports.DecisionRepository }

func (failingUpdate) Update(context.Context, *domain.Decision) error { return errCrash }

func TestSupersede_AFailureBetweenItsTwoWritesKeepsNeither(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := context.Background()
		m := newInStorage(s, func(r ports.DecisionRepository) ports.DecisionRepository { return failingUpdate{r} })
		old, err := m.Propose(c, &domain.Decision{TaskID: "TASK-001", Decision: "use postgres"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.Accept(c, old.ID); err != nil {
			t.Fatal(err)
		}

		if _, err := m.Supersede(c, old.ID, &domain.Decision{Decision: "use sqlite"}); !errors.Is(err, errCrash) {
			t.Fatalf("Supersede must report the failed write, got %v", err)
		}
		all, _ := m.ListByTask(c, "TASK-001")
		if len(all) != 1 {
			t.Fatalf("the new decision was kept without the old one being superseded: %d decisions", len(all))
		}
		if all[0].Status != domain.DecisionAccepted || all[0].SupersededBy != "" {
			t.Fatalf("the old decision changed: %+v", all[0])
		}
		// The rolled-back proposal does not use up a decision number.
		next, err := NewInStorage(s).Propose(c, &domain.Decision{TaskID: "TASK-001"})
		if err != nil || next.ID != "D-002" {
			t.Fatalf("next proposal: %v %v", err, next)
		}
	})
}

func TestSupersede_InOneTransactionKeepsBothWrites(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := context.Background()
		m := NewInStorage(s)
		old, _ := m.Propose(c, &domain.Decision{TaskID: "TASK-001", Decision: "use postgres"})
		created, err := m.Supersede(c, old.ID, &domain.Decision{Decision: "use sqlite"})
		if err != nil {
			t.Fatal(err)
		}
		gotOld, _ := m.Get(c, old.ID)
		if gotOld.Status != domain.DecisionSuperseded || gotOld.SupersededBy != created.ID || created.Supersedes != old.ID {
			t.Fatalf("old %+v, new %+v", gotOld, created)
		}
		latest, err := m.Latest(c, "TASK-001")
		if err != nil || latest.ID != created.ID {
			t.Fatalf("latest: %v %+v", err, latest)
		}
		if _, err := m.Supersede(c, old.ID, &domain.Decision{}); err == nil {
			t.Fatal("superseding a superseded decision must still be refused")
		}
	})
}
