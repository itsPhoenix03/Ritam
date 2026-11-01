package orchestrator_test

import (
	"context"
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
