package orchestrator_test

import (
	"context"
	"strings"
	"testing"

	"ritam/internal/domain"
)

// The day's limit over every task caps a turn too. It was checked only once
// a turn's spending had been added, and a turn's cap ignored it, so each
// running task could take the day a whole turn past its limit.

// soloRig is dailyRig with codex dropped from the task, so that opus's turn
// shares the money with nobody.
func soloRig(t *testing.T, limits domain.TaskLimits, daily *memLedger, maxDaily float64) *inboxRig {
	t.Helper()
	r := dailyRig(t, limits, daily, maxDaily, "codex")
	if _, err := r.h.orch.Tasks.DropAgent(context.Background(), r.task.ID, "codex", "kept out of this test"); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestWithThirtyCentsLeftOfTheDayATurnIsCappedAtThem: $5 is left on the
// task but only $0.30 of the day, so the turn may spend $0.30.
func TestWithThirtyCentsLeftOfTheDayATurnIsCappedAtThem(t *testing.T) {
	r := soloRig(t, costLimit(5), &memLedger{cost: 49.70}, 50)
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	if got := allowance(t, r.opus, 1); !about(got, 0.30) {
		t.Errorf("the turn must be capped at what is left of the day: got $%.4f, want $0.30", got)
	}
}

// TestADailyLimitCapsATurnOfATaskWithNoBudgetOfItsOwn: a task with no cost
// limit is still under the day's.
func TestADailyLimitCapsATurnOfATaskWithNoBudgetOfItsOwn(t *testing.T) {
	r := soloRig(t, defaultLimits(), &memLedger{cost: 49.70}, 50)
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")
	if got := allowance(t, r.opus, 1); !about(got, 0.30) {
		t.Errorf("the turn must be capped at what is left of the day: got $%.4f, want $0.30", got)
	}
}

// TestWithNothingLeftOfTheDayNoTurnStartsAndTheTaskPauses: a new task on a
// spent day used to take a whole turn before the day's limit was seen.
func TestWithNothingLeftOfTheDayNoTurnStartsAndTheTaskPauses(t *testing.T) {
	r := dailyRig(t, costLimit(5), &memLedger{cost: 50}, 50, "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")

	if n := len(r.opus.deliveries()); n != 0 {
		t.Errorf("no turn may start on a spent day, opus was handed %d", n)
	}
	got, _ := r.h.orch.Tasks.Get(context.Background(), r.task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonDailyBudget) ||
		!strings.Contains(got.PauseReason, "$50.00 of $50.00 today") {
		t.Errorf("the task must pause for the day's limit, saying what is spent: %s (%s)", got.State, got.PauseReason)
	}
}

// TestWithTheDailyRecordUnreadableNoTurnStarts: the day's spending not
// known is not the day under its limit. As after a turn, the task pauses
// saying why (fail closed), and here before any turn is paid for.
func TestWithTheDailyRecordUnreadableNoTurnStarts(t *testing.T) {
	r := dailyRig(t, costLimit(5), brokenLedger{}, 50, "codex")
	r.send(t, domain.HumanAgentID, "opus", domain.MsgQuestion, "Plan the fix.")

	if n := len(r.opus.deliveries()); n != 0 {
		t.Errorf("no turn may start while the day's spending cannot be read, opus was handed %d", n)
	}
	got, _ := r.h.orch.Tasks.Get(context.Background(), r.task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonDailyBudget) ||
		!strings.Contains(got.PauseReason, "could not be read") {
		t.Errorf("the task must pause saying the daily record could not be read: %s (%s)", got.State, got.PauseReason)
	}
}

// TestTodaysSpendIsReportedAgainstTheDailyLimit: what the header shows.
func TestTodaysSpendIsReportedAgainstTheDailyLimit(t *testing.T) {
	r := dailyRig(t, costLimit(5), &memLedger{cost: 3.20}, 50, "codex")
	s, err := r.h.orch.SpendToday()
	if err != nil || !about(s.CostUSD, 3.20) || s.MaxCostUSD != 50 {
		t.Errorf("today's spending = %+v (%v), want $3.20 of $50", s, err)
	}
	none := budgetRig(t, costLimit(5), "codex")
	if s, err := none.h.orch.SpendToday(); err != nil || s.MaxCostUSD != 0 || s.MaxTokens != 0 {
		t.Errorf("with no daily limit there is nothing to report: %+v (%v)", s, err)
	}
}
