package orchestrator_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/decisions"
	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/logging"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

// Every turn an agent takes is billed to its task and to the day, including
// the turn whose message ends the run (its usage is sent after the message)
// and the turns the end of the run kills. These run on the fake agent, which
// reports usage after its messages as the real adapters do.

func billingHarness(t *testing.T, gw *testGateway, limits domain.TaskLimits, approval bool) (*harness, *memLedger) {
	t.Helper()
	store := storage.NewInMemory()
	h := &harness{store: *store, gw: gw}
	ledger := &memLedger{}
	h.orch = orchestrator.New(orchestrator.Deps{
		Storage: *store, Agents: gw, Knowledge: knowledge.New(store.Knowledge),
		Decisions: decisions.New(store.Decisions), Logger: logging.NewDiscard(),
		Limits: limits, MaxConcurrentTasks: 2, IdleTimeout: 5 * time.Second,
		RequireHumanApproval: approval, Daily: ledger,
	})
	h.orch.Events.Subscribe(func(e domain.Event) {
		h.mu.Lock()
		h.events = append(h.events, e)
		h.mu.Unlock()
	})
	return h, ledger
}

func usd(c float64) *ports.Usage { return &ports.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: c} }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// checkBilled checks the task's total and each agent's, and that the day's
// record got the same total: each turn counted once, on both.
func checkBilled(t *testing.T, h *harness, ledger *memLedger, id domain.TaskID, want map[domain.AgentID]float64) {
	t.Helper()
	got, _ := h.orch.Tasks.Get(context.Background(), id)
	total := 0.0
	for a, c := range want {
		total += c
		if u := got.AgentUsage[a]; !near(u.CostUSD, c) {
			t.Errorf("%s billed $%.2f over %d turns, want $%.2f", a, u.CostUSD, u.Turns, c)
		}
	}
	if !near(got.CostUSD, total) {
		t.Errorf("task billed $%.2f, want $%.2f", got.CostUSD, total)
	}
	ledger.mu.Lock()
	day := ledger.cost
	ledger.mu.Unlock()
	if !near(day, total) {
		t.Errorf("the day's record has $%.2f, want $%.2f", day, total)
	}
}

// deciding is an authority that delegates to codex ($0.10) and decides
// ($0.75) once codex answers, or when the human says "Decide now.".
func deciding() fake.Script {
	return func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return fake.Turn{Reply: fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."), Usage: usd(0.10)}
		case in.FromAgent == "codex" || strings.Contains(in.Content, "from codex") || strings.Contains(in.Content, "Decide now."):
			return fake.Turn{Reply: fake.Header(domain.MsgFinalDecision, "*", "Clear the cookie with Path=/."), Usage: usd(0.75)}
		}
		return fake.Turn{Usage: usd(0)}
	}
}

// slowCodex works for a long time on its first turn; killed, it reports
// $0.30 of usage.
func slowCodex() *fake.Agent {
	return fake.New("codex", fake.Steps(fake.Turn{Latency: time.Minute, Reply: codexAnswer.Reply,
		Usage: usd(0.50), KilledUsage: usd(0.30)}))
}

func startBilling(t *testing.T, codex *fake.Agent, limits domain.TaskLimits, approval bool) (*harness, *memLedger, *domain.Task, chan error) {
	t.Helper()
	gw := newGateway()
	gw.add(fake.New("opus", deciding()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h, ledger := billingHarness(t, gw, limits, approval)
	task := createTask(t, h, "opus", "opus", "codex")
	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(context.Background(), task.ID) }()
	return h, ledger, task, done
}

// TestTheDecidingTurnIsBilled: the authority's FINAL_DECISION ended the run
// before the usage sent after it was read, and nobody read the channel
// afterwards: in a probe the task ended COMPLETED at $0.00 after a $0.75
// deciding turn. The deciding turn of every task went unbilled.
func TestTheDecidingTurnIsBilled(t *testing.T) {
	codex := fake.New("codex", fake.Steps(fake.Turn{Reply: codexAnswer.Reply, Usage: usd(0.20)}))
	h, ledger, task, done := startBilling(t, codex, defaultLimits(), false)
	_ = runResult(t, done)

	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s)", got.State, got.PauseReason)
	}
	checkBilled(t, h, ledger, task.ID, map[domain.AgentID]float64{"opus": 0.85, "codex": 0.20})
	if got.AgentUsage["opus"].Turns != 2 || got.AgentUsage["codex"].Turns != 1 {
		t.Errorf("each turn is counted once: %+v", got.AgentUsage)
	}
}

// TestADecisionHeldForApprovalBillsItsTurn: a decision held for the human
// ends the run as well, and its turn was lost the same way.
func TestADecisionHeldForApprovalBillsItsTurn(t *testing.T) {
	codex := fake.New("codex", fake.Steps(fake.Turn{Reply: codexAnswer.Reply, Usage: usd(0.20)}))
	h, ledger, task, done := startBilling(t, codex, defaultLimits(), true)
	_ = runResult(t, done)

	if got, _ := h.orch.Tasks.Get(context.Background(), task.ID); got.State != domain.TaskAwaitingDecision {
		t.Fatalf("state = %s, want AWAITING_DECISION", got.State)
	}
	checkBilled(t, h, ledger, task.ID, map[domain.AgentID]float64{"opus": 0.85, "codex": 0.20})
}

// TestATurnKilledByAPauseOrACancelIsBilled: stopping the agents kills the
// turn in progress. Its usage was dropped by the adapter, which discarded
// everything once stopped, and by the run, which no longer read the channel.
func TestATurnKilledByAPauseOrACancelIsBilled(t *testing.T) {
	for _, how := range []string{"pause", "cancel"} {
		codex := slowCodex()
		h, ledger, task, done := startBilling(t, codex, defaultLimits(), false)
		waitFor(t, func() bool { return codex.Status() == domain.AgentStateWorking })
		ctx := context.Background()
		var err error
		if how == "pause" {
			err = h.orch.Pause(ctx, task.ID, "")
		} else {
			err = h.orch.Cancel(ctx, task.ID, "")
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = runResult(t, done)
		if codex.Killed() != 1 {
			t.Fatalf("%s: codex's turn must have been killed: %d", how, codex.Killed())
		}
		checkBilled(t, h, ledger, task.ID, map[domain.AgentID]float64{"opus": 0.10, "codex": 0.30})
	}
}

// TestATurnKilledByAnotherAgentsCompletionIsBilled: codex, dropped from the
// task while still mid-turn, does not hold the authority's decision; the
// task completes, its turn is killed, and both the deciding turn and the
// killed one are billed.
func TestATurnKilledByAnotherAgentsCompletionIsBilled(t *testing.T) {
	codex := slowCodex()
	h, ledger, task, done := startBilling(t, codex, defaultLimits(), false)
	waitFor(t, func() bool { return codex.Status() == domain.AgentStateWorking })
	ctx := context.Background()
	if _, err := h.orch.Tasks.DropAgent(ctx, task.ID, "codex", "taken off by the test"); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.SendInstruction(ctx, task.ID, "opus", "Decide now."); err != nil {
		t.Fatal(err)
	}
	_ = runResult(t, done)

	if got, _ := h.orch.Tasks.Get(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s)", got.State, got.PauseReason)
	}
	if codex.Killed() != 1 {
		t.Fatalf("codex's turn must have been killed by the completion: %d", codex.Killed())
	}
	checkBilled(t, h, ledger, task.ID, map[domain.AgentID]float64{"opus": 0.85, "codex": 0.30})
}

// TestACoWorkersTurnKilledByABudgetPauseIsBilled: one agent crossing the
// budget pauses the task, and stopping the agents killed every co-worker's
// turn unrecorded -- so the task ended under-counted, by the very turns that
// were running when it was found over budget.
func TestACoWorkersTurnKilledByABudgetPauseIsBilled(t *testing.T) {
	limits := defaultLimits()
	// The authority's $0.10 first turn reaches it. Less, and codex's turn
	// would not start: the authority's turn holds half and a turn needs
	// at least $0.05 (turnbudget.go).
	limits.MaxCostUSD = 0.10
	codex := slowCodex()
	h, ledger, task, done := startBilling(t, codex, limits, false)
	_ = runResult(t, done)

	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonBudget) {
		t.Fatalf("state = %s (%s), want a budget pause", got.State, got.PauseReason)
	}
	if codex.Killed() != 1 {
		t.Fatalf("codex was working when the task paused: killed %d", codex.Killed())
	}
	checkBilled(t, h, ledger, task.ID, map[domain.AgentID]float64{"opus": 0.10, "codex": 0.30})
}
