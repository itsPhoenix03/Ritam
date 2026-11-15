package storage

import (
	"context"
	"errors"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

var errBoom = errors.New("boom")

// writeAll writes to every repository, the way a transaction that records a
// whole step of a task would. It changes the existing task "T-OLD" too.
func writeAll(t *testing.T, c context.Context, s *ports.Storage) {
	t.Helper()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.Tasks.NextSequence(c)
	must(err)
	must(s.Tasks.Create(c, &domain.Task{ID: "T-NEW", Title: "new"}))
	old, err := s.Tasks.Get(c, "T-OLD")
	must(err)
	old.Title = "changed"
	must(s.Tasks.Update(c, old))
	must(s.Messages.Append(c, &domain.Message{ID: "M1", TaskID: "T-NEW", Content: "hi"}))
	must(s.Events.Append(c, domain.Event{ID: "E1", Type: domain.EvtTaskCreated, TaskID: "T-NEW"}))
	_, err = s.Decisions.NextSequence(c)
	must(err)
	must(s.Decisions.Create(c, &domain.Decision{ID: "D-NEW", TaskID: "T-NEW", Status: domain.DecisionProposed}))
	d, err := s.Decisions.Get(c, "D-OLD")
	must(err)
	d.Status = domain.DecisionAccepted
	must(s.Decisions.Update(c, d))
	must(s.Disagreements.Create(c, &domain.Disagreement{ID: "DIS-1", TaskID: "T-NEW"}))
	must(s.Knowledge.Put(c, &domain.KnowledgeEntry{ID: "K1", TaskID: "T-NEW", Title: "note"}))
	must(s.Artifacts.Create(c, &domain.Artifact{ID: "A1", TaskID: "T-NEW"}))
}

// seed gives writeAll the records it changes.
func seed(t *testing.T, s *ports.Storage) {
	t.Helper()
	c := ctx()
	if err := s.Tasks.Create(c, &domain.Task{ID: "T-OLD", Title: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Decisions.Create(c, &domain.Decision{ID: "D-OLD", TaskID: "T-OLD", Status: domain.DecisionProposed}); err != nil {
		t.Fatal(err)
	}
}

// written reports, for each write of writeAll, whether it is in the store.
func written(t *testing.T, c context.Context, s *ports.Storage) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	_, err := s.Tasks.Get(c, "T-NEW")
	out["task created"] = err == nil
	old, err := s.Tasks.Get(c, "T-OLD")
	if err != nil {
		t.Fatal(err)
	}
	out["task updated"] = old.Title == "changed"
	n, _ := s.Messages.CountByTask(c, "T-NEW")
	out["message"] = n == 1
	evs, _ := s.Events.ListByTask(c, "T-NEW")
	out["event"] = len(evs) == 1
	_, err = s.Decisions.Get(c, "D-NEW")
	out["decision created"] = err == nil
	d, _ := s.Decisions.Get(c, "D-OLD")
	out["decision updated"] = d.Status == domain.DecisionAccepted
	_, err = s.Disagreements.Get(c, "DIS-1")
	out["disagreement"] = err == nil
	_, err = s.Knowledge.Get(c, "K1")
	out["knowledge"] = err == nil
	_, err = s.Artifacts.Get(c, "A1")
	out["artifact"] = err == nil
	return out
}

func TestConformance_AFailingTransactionLeavesNothingBehind(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		seed(t, s)
		err := s.WithTx(c, func(tx *ports.Storage) error {
			writeAll(t, c, tx)
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatalf("WithTx must return the function's error, got %v", err)
		}
		for what, ok := range written(t, c, s) {
			if ok {
				t.Errorf("%s survived the rollback", what)
			}
		}
		// The sequences are rolled back too, so no task or decision number
		// is skipped because of a failed step.
		if n, _ := s.Tasks.NextSequence(c); n != 1 {
			t.Errorf("task sequence = %d after the rollback, want 1", n)
		}
		if n, _ := s.Decisions.NextSequence(c); n != 1 {
			t.Errorf("decision sequence = %d after the rollback, want 1", n)
		}
	})
}

func TestConformance_ASucceedingTransactionKeepsEverything(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		seed(t, s)
		if err := s.WithTx(c, func(tx *ports.Storage) error {
			writeAll(t, c, tx)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for what, ok := range written(t, c, s) {
			if !ok {
				t.Errorf("%s was lost by the commit", what)
			}
		}
		if n, _ := s.Tasks.NextSequence(c); n != 2 {
			t.Errorf("task sequence = %d after the commit, want 2", n)
		}
	})
}

func TestConformance_ATransactionReadsItsOwnWrites(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		seed(t, s)
		if err := s.WithTx(c, func(tx *ports.Storage) error {
			writeAll(t, c, tx)
			for what, ok := range written(t, c, tx) {
				if !ok {
					t.Errorf("%s is not visible inside the transaction that wrote it", what)
				}
			}
			return errBoom
		}); !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
	})
}

func TestConformance_ANestedTransactionRollsBackOnlyItsOwnWrites(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		if err := s.WithTx(c, func(tx *ports.Storage) error {
			if err := tx.Tasks.Create(c, &domain.Task{ID: "OUTER"}); err != nil {
				return err
			}
			inner := tx.WithTx(c, func(tx2 *ports.Storage) error {
				if err := tx2.Tasks.Create(c, &domain.Task{ID: "INNER"}); err != nil {
					return err
				}
				return errBoom
			})
			if !errors.Is(inner, errBoom) {
				t.Errorf("nested WithTx must return the function's error, got %v", inner)
			}
			return tx.WithTx(c, func(tx2 *ports.Storage) error {
				return tx2.Tasks.Create(c, &domain.Task{ID: "INNER-KEPT"})
			})
		}); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[domain.TaskID]bool{"OUTER": true, "INNER": false, "INNER-KEPT": true} {
			if _, err := s.Tasks.Get(c, id); (err == nil) != want {
				t.Errorf("task %s present = %v, want %v", id, err == nil, want)
			}
		}
	})
}

func TestConformance_APanickingTransactionRollsBackAndFreesTheStore(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic must reach the caller")
				}
			}()
			_ = s.WithTx(c, func(tx *ports.Storage) error {
				_ = tx.Tasks.Create(c, &domain.Task{ID: "HALF"})
				panic("boom")
			})
		}()
		done := make(chan error, 1)
		go func() { done <- s.Tasks.Create(c, &domain.Task{ID: "AFTER"}) }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the store stayed locked after a transaction panicked")
		}
		if _, err := s.Tasks.Get(c, "HALF"); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("a panicking transaction's write survived: %v", err)
		}
	})
}

func TestConformance_AWriteFromElsewhereWaitsForTheTransactionAndSurvivesItsRollback(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		other := make(chan error, 1)
		if err := s.WithTx(c, func(tx *ports.Storage) error {
			if err := tx.Tasks.Create(c, &domain.Task{ID: "IN-TX"}); err != nil {
				return err
			}
			go func() { other <- s.Tasks.Create(c, &domain.Task{ID: "OUTSIDE"}) }()
			// Give the other write every chance to slip in before the
			// rollback; it must wait instead.
			time.Sleep(50 * time.Millisecond)
			return errBoom
		}); !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
		if err := <-other; err != nil {
			t.Fatal(err)
		}
		if _, err := s.Tasks.Get(c, "OUTSIDE"); err != nil {
			t.Errorf("the write made from elsewhere during the transaction was lost: %v", err)
		}
		if _, err := s.Tasks.Get(c, "IN-TX"); !errors.Is(err, ports.ErrNotFound) {
			t.Errorf("the rolled-back write survived: %v", err)
		}
	})
}

func TestConformance_ATransactionsStorageRefusesUseAfterItEnds(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		var leaked *ports.Storage
		if err := s.WithTx(c, func(tx *ports.Storage) error { leaked = tx; return nil }); err != nil {
			t.Fatal(err)
		}
		if err := leaked.Tasks.Create(c, &domain.Task{ID: "LATE"}); err == nil {
			t.Error("a write through a finished transaction must fail, not bypass it")
		}
	})
}

func TestAStorageWithoutTransactionsSaysSo(t *testing.T) {
	s := &ports.Storage{Tasks: NewMemTaskRepository()}
	err := s.WithTx(ctx(), func(*ports.Storage) error { return nil })
	if !errors.Is(err, ports.ErrNoTransactions) {
		t.Fatalf("got %v, want ErrNoTransactions", err)
	}
}
