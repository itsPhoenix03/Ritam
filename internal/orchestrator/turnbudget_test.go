package orchestrator_test

import (
	"context"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Each turn's spending cap is decided by the orchestrator when the turn is
// handed over, and held in reserve until the turn is billed, fails or is
// killed (turnbudget.go). Most of these drive the inbox step by step, with
// agents that work until the test finishes their turn.

// budgetRig is a task of opus (the authority) and the other agents named,
// with limits, its agents started and its inbox open, and no run.
func budgetRig(t *testing.T, limits domain.TaskLimits, others ...domain.AgentID) *inboxRig {
	t.Helper()
	r := &inboxRig{opus: newPromptAgent("opus"), codex: newPromptAgent("codex"), scout: newPromptAgent("scout")}
	all := map[domain.AgentID]*promptAgent{"codex": r.codex, "scout": r.scout}
	gw := newGateway()
	gw.add(r.opus, domain.RoleAuthority)
	_ = r.opus.Start(context.Background())
	for _, id := range others {
		gw.add(all[id], domain.RoleImplementer)
		_ = all[id].Start(context.Background())
	}
	r.h = newHarness(t, limits, time.Minute, gw)
	r.task = createTask(t, r.h, "opus", append([]domain.AgentID{"opus"}, others...)...)
	r.h.orch.OpenInbox(r.task.ID)
	return r
}

// costLimit is the default limits with a task budget of usd.
func costLimit(usd float64) domain.TaskLimits {
	l := defaultLimits()
	l.MaxCostUSD = usd
	return l
}

// allowance is the cap agent a's turn-th delivery carried, -1 for none.
func allowance(t *testing.T, a *promptAgent, turn int) float64 {
	t.Helper()
	got := a.deliveries()
	if len(got) < turn {
		t.Fatalf("%s was handed %d turns, want at least %d", a.id, len(got), turn)
	}
	v, ok := got[turn-1].Metadata[ports.TurnBudgetMetadataKey]
	if !ok {
		return -1
	}
	return mustFloat(t, v)
}

func mustFloat(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatalf("allowance %q is not a number", s)
	}
	return f
}

func about(a, b float64) bool { return math.Abs(a-b) < 0.0005 }

// bill ends agent a's turn, reporting usd spent.
func (r *inboxRig) bill(t *testing.T, a *promptAgent, usd float64) {
	t.Helper()
	a.finish()
	if _, err := r.h.orch.HandleOutboundForTest(context.Background(), r.task.ID,
		ports.Outbound{AgentID: a.id, Usage: &ports.Usage{InputTokens: 10, CostUSD: usd}}); err != nil {
		t.Fatal(err)
	}
}

// TestTwoAgentsWorkingAtOnceShareWhatIsLeftOfTheBudget: every agent at work
// used to be capped at the whole remainder, so two of them on $1 left could
// spend $2 between them. opus's turn, with codex able to start beside it,
// gets half; codex, starting while opus works, gets the other half.
func TestTwoAgentsWorkingAtOnceShareWhatIsLeftOfTheBudget(t *testing.T) {
	r := budgetRig(t, costLimit(1), "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")

	o, c := allowance(t, r.opus, 1), allowance(t, r.codex, 1)
	if !about(o, 0.50) || !about(c, 0.50) {
		t.Errorf("two agents on $1 must get about $0.50 each, got opus $%.4f and codex $%.4f", o, c)
	}
	if o+c > 1+1e-9 {
		t.Errorf("together they may never be allowed more than is left: $%.4f", o+c)
	}
}

// TestATurnHandedOverAfterACoWorkersUsageWasRecordedUsesTheNewerCost: the cap
// is worked out from the task as it stands at the handover, not from a copy
// taken when the turn was queued.
func TestATurnHandedOverAfterACoWorkersUsageWasRecordedUsesTheNewerCost(t *testing.T) {
	r := budgetRig(t, costLimit(1), "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	r.bill(t, r.opus, 0.40)
	r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")

	// $0.60 is left, shared with opus, which could start a turn beside it.
	if got := allowance(t, r.codex, 1); !about(got, 0.30) {
		t.Errorf("codex's cap must count opus's $0.40 turn: got $%.4f, want $0.30", got)
	}
}

// TestATurnHandedOverBeforeACoWorkersUsageArrivesLeavesRoomForIt: usage is
// sent after a turn's messages, so an agent can be handed a turn the moment
// a co-worker's reply is routed, before that turn's cost is known. The
// co-worker's reservation counts until its usage is in.
func TestATurnHandedOverBeforeACoWorkersUsageArrivesLeavesRoomForIt(t *testing.T) {
	r := budgetRig(t, costLimit(1), "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	opus := allowance(t, r.opus, 1)
	r.opus.finish() // it has replied; its usage is still on its way
	r.send(t, "opus", "codex", domain.MsgImplement, "Clear the cookie with Path=/.")

	if got := allowance(t, r.codex, 1); got+opus > 1+1e-9 || !about(got, (1-opus)/2) {
		t.Errorf("codex's cap must leave room for opus's unbilled turn ($%.4f): got $%.4f", opus, got)
	}
}

// TestAReservationIsReleasedWhenItsTurnIsBilledFailsOrIsKilled: a turn's
// reservation is freed once the turn is over, however it ended. One left
// held would shrink every later turn's cap until the task paused with its
// budget unspent.
func TestAReservationIsReleasedWhenItsTurnIsBilledFailsOrIsKilled(t *testing.T) {
	t.Run("billed", func(t *testing.T) {
		r := budgetRig(t, costLimit(1), "codex")
		r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
		r.bill(t, r.opus, 0)
		if held := r.h.orch.TurnsHeldForTest(r.task.ID); held != 0 {
			t.Errorf("a billed turn holds nothing, $%.4f is held", held)
		}
		r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")
		if got := allowance(t, r.codex, 1); !about(got, 0.50) {
			t.Errorf("codex shares the whole $1 with opus: got $%.4f", got)
		}
	})
	t.Run("fails", func(t *testing.T) {
		r := budgetRig(t, costLimit(1), "codex")
		r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")
		r.codex.finish()
		// codex cannot run its model: its turn is over and it is dropped,
		// which opus is told in a turn of its own.
		if _, err := r.h.orch.HandleOutboundForTest(context.Background(), r.task.ID, ports.Outbound{AgentID: "codex",
			Err: domain.NewFailure(domain.FailureAuth, "not logged in", nil)}); err != nil {
			t.Fatal(err)
		}
		if got := allowance(t, r.opus, 1); !about(got, 1) {
			t.Errorf("with codex's failed turn holding nothing, opus may use all of the $1: got $%.4f", got)
		}
	})
	t.Run("killed", func(t *testing.T) {
		// The authority's $0.10 turn reaches the budget while codex works;
		// the pause kills codex's turn, and the end of the run releases
		// its reservation.
		h, _, task, done := startBilling(t, slowCodex(), costLimit(0.10), false)
		_ = runResult(t, done)
		got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
		if got.State != domain.TaskPaused || got.AgentUsage["codex"].Turns != 1 {
			t.Fatalf("codex's turn must have been running when the task paused: %s, %+v", got.State, got.AgentUsage)
		}
		if held := h.orch.TurnsHeldForTest(task.ID); held != 0 {
			t.Errorf("a killed turn holds nothing once the run has ended, $%.4f is held", held)
		}
	})
}

// TestATaskWithNoCostLimitPassesNoCap: no budget, no cap, as before.
func TestATaskWithNoCostLimitPassesNoCap(t *testing.T) {
	r := budgetRig(t, defaultLimits(), "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	if got := allowance(t, r.opus, 1); got != -1 {
		t.Errorf("a task with no cost limit must pass no cap, got $%.4f", got)
	}
}

// TestWhenNotEvenTheFloorIsLeftNoTurnStartsAndTheTaskPausesForBudget: a turn
// capped at a few cents stops at its first API call, paid for and useless.
// With less than a turn needs left and no turn in progress to free any, the
// turn is not handed over and the task pauses for its budget; the message
// waits for the human to raise the limit and resume.
func TestWhenNotEvenTheFloorIsLeftNoTurnStartsAndTheTaskPausesForBudget(t *testing.T) {
	r := budgetRig(t, costLimit(1), "codex")
	r.bill(t, r.opus, 0.97)
	r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")

	if n := len(r.codex.deliveries()); n != 0 {
		t.Errorf("no turn may start with $0.03 left, codex was handed %d", n)
	}
	got, _ := r.h.orch.Tasks.Get(context.Background(), r.task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonBudget) {
		t.Fatalf("the task must pause for its budget: %s (%s)", got.State, got.PauseReason)
	}
	if !strings.Contains(got.PauseReason, "$0.97 of $1.00") {
		t.Errorf("the reason must say what is spent: %q", got.PauseReason)
	}
	if b := r.h.orch.Backlog(r.task.ID); b["codex"] != 1 {
		t.Errorf("the message must still wait for codex: %v", b)
	}
}

// TestATurnWaitsWhileACoWorkersTurnHoldsTheMoneyItNeeds: too little left
// unreserved is not the budget running out while co-workers' turns hold
// money they may not spend. The turn waits, without pausing the task (which
// would kill those turns), and starts once one of them is billed.
func TestATurnWaitsWhileACoWorkersTurnHoldsTheMoneyItNeeds(t *testing.T) {
	r := budgetRig(t, costLimit(0.12), "codex", "scout")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	r.send(t, domain.HumanAgentID, "codex", domain.MsgQuestion, "Where is the cookie cleared?")
	r.send(t, domain.HumanAgentID, "scout", domain.MsgQuestion, "Where is the cookie set?")

	if n := len(r.scout.deliveries()); n != 0 {
		t.Fatalf("scout must wait while opus and codex hold all but $0.02, was handed %d", n)
	}
	if got, _ := r.h.orch.Tasks.Get(context.Background(), r.task.ID); got.State == domain.TaskPaused {
		t.Fatalf("the task must not pause while turns in progress hold the money: %s", got.PauseReason)
	}

	r.bill(t, r.opus, 0.01)
	r.dispatch()
	if got := allowance(t, r.scout, 1); got < 0.05-1e-9 {
		t.Errorf("once opus is billed scout starts with at least the floor: got $%.4f", got)
	}
}

// TestATurnStoppedByItsCapPausesTheTaskForItsBudget: a tighter cap stops
// more turns, and a stopped turn must not become an agent silently producing
// nothing. The CLI's budget stop pauses the task, saying so, and what the
// turn spent is billed.
func TestATurnStoppedByItsCapPausesTheTaskForItsBudget(t *testing.T) {
	opus := fake.New("opus", fake.Steps(fake.Turn{Fail: domain.FailureBudgetStop, Usage: usd(0.30)}))
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps()), domain.RoleImplementer)
	h, _ := billingHarness(t, gw, costLimit(1), false)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)

	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonBudget) ||
		!strings.Contains(got.PauseReason, "stopped opus's turn") {
		t.Fatalf("a turn stopped at its cap must pause the task for its budget: %s (%s)", got.State, got.PauseReason)
	}
	if in := opus.Received(); len(in) == 0 || !about(mustFloat(t, in[0].Metadata[ports.TurnBudgetMetadataKey]), 0.50) {
		t.Errorf("the turn was handed over with its share of the budget as its cap: %+v", in)
	}
	if !near(got.CostUSD, 0.30) {
		t.Errorf("what the stopped turn spent is billed: $%.2f", got.CostUSD)
	}
}
