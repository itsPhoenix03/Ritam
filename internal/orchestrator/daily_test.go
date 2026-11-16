package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/decisions"
	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/logging"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

// memLedger is a day's spending kept in memory, starting from what earlier
// tasks spent.
type memLedger struct {
	mu   sync.Mutex
	cost float64
}

func (l *memLedger) Add(u ports.Usage) (float64, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cost += u.CostUSD
	return l.cost, 0, nil
}

func (l *memLedger) Today() (float64, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.cost, 0, nil
}

// TestADayOverItsLimitPausesANewTask: a new task has its own budget, but
// the day's spending over every task still has a limit.
func TestADayOverItsLimitPausesANewTask(t *testing.T) {
	limits := defaultLimits()
	limits.MaxCostUSD = 10 // far from this task's own budget
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(newUsageAgent("codex", ports.Usage{InputTokens: 100, CostUSD: 0.60}), domain.RoleImplementer)

	store := storage.NewInMemory()
	h := &harness{store: *store, gw: gw}
	ledger := &memLedger{cost: 4.50} // spent earlier today
	h.orch = orchestrator.New(orchestrator.Deps{
		Storage: *store, Agents: gw, Knowledge: knowledge.New(store.Knowledge),
		Decisions: decisions.New(store.Decisions), Logger: logging.NewDiscard(),
		Limits: limits, MaxConcurrentTasks: 2, IdleTimeout: time.Second,
		Daily: ledger, MaxDailyCostUSD: 5,
	})
	task := createTask(t, h, "opus", "opus", "codex")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)

	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if final.State != domain.TaskPaused || !strings.HasPrefix(final.PauseReason, domain.PauseReasonDailyBudget) {
		t.Errorf("past the day's limit the task must pause: %s (%s)", final.State, final.PauseReason)
	}
	if final.CostUSD > 1.2 {
		t.Errorf("it must pause on the first turn that crosses the day's limit: task spent $%.2f", final.CostUSD)
	}
}

// brokenLedger cannot lock, read or write the day's record -- or, readable,
// it can be read but no turn can be added to it.
type brokenLedger struct{ readable bool }

func (brokenLedger) Add(ports.Usage) (float64, int64, error) {
	return 0, 0, errors.New("spend record is locked by another process")
}

func (l brokenLedger) Today() (float64, int64, error) {
	if l.readable {
		return 0, 0, nil
	}
	return 0, 0, errors.New("spend record is locked by another process")
}

// TestAnUnreadableDailyRecordPausesTheTask: a daily spend record that could
// not be read or written used to be logged and the day treated as under its
// limit -- no limit at all, with nothing on screen. The task pauses on the
// first turn, saying why, and the turn's usage still counts on the task.
// (A record that cannot even be read starts no turn at all:
// TestWithTheDailyRecordUnreadableNoTurnStarts.)
func TestAnUnreadableDailyRecordPausesTheTask(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(newUsageAgent("codex", ports.Usage{InputTokens: 100, CostUSD: 0.60}), domain.RoleImplementer)

	store := storage.NewInMemory()
	h := &harness{store: *store, gw: gw}
	h.orch = orchestrator.New(orchestrator.Deps{
		Storage: *store, Agents: gw, Knowledge: knowledge.New(store.Knowledge),
		Decisions: decisions.New(store.Decisions), Logger: logging.NewDiscard(),
		Limits: defaultLimits(), MaxConcurrentTasks: 2, IdleTimeout: time.Second,
		Daily: brokenLedger{readable: true}, MaxDailyCostUSD: 5,
	})
	task := createTask(t, h, "opus", "opus", "codex")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)

	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if final.State != domain.TaskPaused || !strings.HasPrefix(final.PauseReason, domain.PauseReasonDailyBudget) {
		t.Fatalf("an unreadable daily record must pause the task as over the day's limit: %s (%s)",
			final.State, final.PauseReason)
	}
	for _, want := range []string{"could not be read or written", "locked by another process"} {
		if !strings.Contains(final.PauseReason, want) {
			t.Errorf("the pause reason must say %q: %q", want, final.PauseReason)
		}
	}
	// At least that turn: a second one of codex's, if the authority's reply
	// had started it before the pause, is billed too as the run ends.
	if final.CostUSD < 0.60 || final.InputTokens < 100 || final.AgentUsage["codex"].CostUSD != final.CostUSD {
		t.Errorf("the turn's usage is still recorded on the task: $%.2f, %d tokens in", final.CostUSD, final.InputTokens)
	}
}
