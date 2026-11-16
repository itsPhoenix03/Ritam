package storage

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func queuedFor(id domain.MessageID, to domain.AgentID) *domain.Delivery {
	return &domain.Delivery{MessageID: id, Recipient: to, TaskID: "T", QueuedAt: time.Now()}
}

func statuses(t *testing.T, s *ports.Storage, task domain.TaskID) map[string]domain.DeliveryStatus {
	t.Helper()
	list, err := s.Deliveries.ListByTask(ctx(), task)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]domain.DeliveryStatus{}
	for _, d := range list {
		out[string(d.MessageID)+">"+string(d.Recipient)] = d.Status
	}
	return out
}

// TestConformance_ADeliveryGoesFromQueuedToSentToAcknowledgedOrBack: the
// life of a delivery, the same on both backends.
func TestConformance_ADeliveryGoesFromQueuedToSentToAcknowledgedOrBack(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		for _, d := range []*domain.Delivery{queuedFor("M1", "codex"), queuedFor("M2", "codex"),
			queuedFor("M1", "scout"), queuedFor("M3", "codex")} {
			if err := s.Deliveries.Enqueue(c, d); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Deliveries.Enqueue(c, queuedFor("M1", "codex")); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("the same message queued twice for one recipient: %v", err)
		}
		list, _ := s.Deliveries.ListByTask(c, "T")
		if len(list) != 4 || list[0].MessageID != "M1" || list[1].MessageID != "M2" || list[3].MessageID != "M3" ||
			list[0].Status != domain.DeliveryQueued || list[0].SentAt != nil {
			t.Fatalf("deliveries must come back queued, in queue order: %+v", list)
		}

		at := time.Now()
		if err := s.Deliveries.MarkSent(c, "T", "codex", []domain.MessageID{"M1", "M2"}, "TURN-1", at); err != nil {
			t.Fatal(err)
		}
		list, _ = s.Deliveries.ListByTask(c, "T")
		if list[0].Status != domain.DeliverySent || list[0].TurnID != "TURN-1" || list[0].SentAt == nil ||
			!list[0].SentAt.Equal(at) || list[2].Status != domain.DeliveryQueued {
			t.Fatalf("only codex's M1 and M2 are sent, in TURN-1: %+v %+v", list[0], list[2])
		}

		// The turn was killed: back to the queue, remembering who carried it.
		_ = s.Deliveries.SettleTurn(c, "TURN-1", domain.DeliveryQueued)
		list, _ = s.Deliveries.ListByTask(c, "T")
		if list[0].Status != domain.DeliveryQueued || list[0].TurnID != "TURN-1" {
			t.Fatalf("a requeued delivery keeps the turn that last carried it: %+v", list[0])
		}
		_ = s.Deliveries.MarkSent(c, "T", "codex", []domain.MessageID{"M1", "M2"}, "TURN-2", at)
		_ = s.Deliveries.SettleTurn(c, "TURN-2", domain.DeliveryAcknowledged)
		// Settling an old turn again changes nothing: its deliveries moved on.
		_ = s.Deliveries.SettleTurn(c, "TURN-1", domain.DeliveryQueued)
		// A turn found to have failed after it was taken as done gives its
		// deliveries back to the queue.
		_ = s.Deliveries.MarkSent(c, "T", "codex", []domain.MessageID{"M3"}, "TURN-3", at)
		_ = s.Deliveries.SettleTurn(c, "TURN-3", domain.DeliveryAcknowledged)
		_ = s.Deliveries.SettleTurn(c, "TURN-3", domain.DeliveryQueued)
		if got := statuses(t, s, "T"); got["M3>codex"] != domain.DeliveryQueued {
			t.Fatalf("an acknowledged delivery of a turn that failed after all goes back to the queue: %v", got)
		}
		_ = s.Deliveries.SettleTurn(c, "TURN-3", domain.DeliveryAcknowledged)
		if got := statuses(t, s, "T"); got["M3>codex"] != domain.DeliveryQueued {
			t.Fatalf("a delivery back in the queue stays there: %v", got)
		}
		got := statuses(t, s, "T")
		if got["M1>codex"] != domain.DeliveryAcknowledged || got["M2>codex"] != domain.DeliveryAcknowledged ||
			got["M1>scout"] != domain.DeliveryQueued {
			t.Fatalf("statuses: %v", got)
		}

		_ = s.Deliveries.Discard(c, "T", "scout")
		if _, ok := statuses(t, s, "T")["M1>scout"]; ok {
			t.Error("Discard must forget what waits for the recipient")
		}
		_ = s.Deliveries.Clear(c, "T")
		got = statuses(t, s, "T")
		if len(got) != 2 || got["M1>codex"] != domain.DeliveryAcknowledged {
			t.Errorf("Clear keeps only what was acknowledged: %v", got)
		}
	})
}

func TestConformance_DeliveriesTakePartInTransactions(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		err := s.WithTx(c, func(tx *ports.Storage) error {
			if err := tx.Messages.Append(c, &domain.Message{ID: "M1", TaskID: "T"}); err != nil {
				return err
			}
			if err := tx.Deliveries.Enqueue(c, queuedFor("M1", "codex")); err != nil {
				return err
			}
			if len(statuses(t, tx, "T")) != 1 {
				t.Error("a transaction must see its own delivery")
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
		if len(statuses(t, s, "T")) != 0 {
			t.Error("a rolled-back delivery survived")
		}
	})
}

func TestSQLiteKeepsQueuedDeliveriesAcrossARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.db")
	db, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Storage().Deliveries.Enqueue(ctx(), queuedFor("M1", "codex"))
	_ = db.Close()
	db, err = OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := statuses(t, db.Storage(), "T"); got["M1>codex"] != domain.DeliveryQueued {
		t.Fatalf("a queued delivery must survive a restart: %v", got)
	}
}
