package storage

import (
	"errors"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestConformance_ATurnIsRecordedAtItsStartAndCompletedAtItsEnd: the same
// record on both backends, from hand-over to outcome and usage.
func TestConformance_ATurnIsRecordedAtItsStartAndCompletedAtItsEnd(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		start := time.Now()
		turn := &domain.Turn{ID: "TURN-1", TaskID: "T", AgentID: "codex", Messages: []domain.MessageID{"M1", "M2"}, StartedAt: start}
		if err := s.Turns.Create(c, turn); err != nil {
			t.Fatal(err)
		}
		if err := s.Turns.Create(c, turn); !errors.Is(err, ports.ErrAlreadyExists) {
			t.Fatalf("the same turn recorded twice: %v", err)
		}
		_ = s.Turns.Create(c, &domain.Turn{ID: "TURN-0", TaskID: "T", AgentID: "opus", StartedAt: start})
		turn.Messages[0] = "mutated"

		end := start.Add(time.Minute)
		done := &domain.Turn{ID: "TURN-1", TaskID: "T", AgentID: "codex", Messages: []domain.MessageID{"M1", "M2"},
			StartedAt: start, EndedAt: &end, Outcome: domain.TurnFailed, FailureKind: domain.FailureTimeout,
			CostUSD: 0.25, InputTokens: 100, OutputTokens: 20, CacheReadTokens: 5000}
		if err := s.Turns.Update(c, done); err != nil {
			t.Fatal(err)
		}
		if err := s.Turns.Update(c, &domain.Turn{ID: "TURN-9"}); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("updating an unknown turn: %v", err)
		}

		list, err := s.Turns.ListByTask(c, "T")
		if err != nil || len(list) != 2 || list[0].ID != "TURN-1" || list[1].ID != "TURN-0" {
			t.Fatalf("turns come back in the order they started: %v %+v", err, list)
		}
		got := list[0]
		if len(got.Messages) != 2 || got.Messages[0] != "M1" || !got.StartedAt.Equal(start) || got.EndedAt == nil ||
			!got.EndedAt.Equal(end) || got.Outcome != domain.TurnFailed || got.FailureKind != domain.FailureTimeout ||
			got.CostUSD != 0.25 || got.InputTokens != 100 || got.OutputTokens != 20 || got.CacheReadTokens != 5000 {
			t.Fatalf("round trip lost data: %+v", got)
		}
		if list[1].EndedAt != nil || list[1].Outcome != domain.TurnRunning || list[1].Messages != nil {
			t.Fatalf("a running turn with no messages: %+v", list[1])
		}
		if other, _ := s.Turns.ListByTask(c, "OTHER"); len(other) != 0 {
			t.Fatalf("another task's turns: %+v", other)
		}
	})
}

func TestConformance_TurnsTakePartInTransactions(t *testing.T) {
	backends(t, func(t *testing.T, s *ports.Storage) {
		c := ctx()
		err := s.WithTx(c, func(tx *ports.Storage) error {
			if err := tx.Turns.Create(c, &domain.Turn{ID: "TURN-1", TaskID: "T", StartedAt: time.Now()}); err != nil {
				return err
			}
			return errBoom
		})
		if !errors.Is(err, errBoom) {
			t.Fatal(err)
		}
		if list, _ := s.Turns.ListByTask(c, "T"); len(list) != 0 {
			t.Error("a rolled-back turn survived")
		}
	})
}
