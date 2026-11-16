package claude

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestTheTurnIsCappedAtTheAllowanceTheOrchestratorGaveIt: the orchestrator
// shares what is left of the budget among the agents working at once and
// puts each turn's share on its delivery. The adapter used to cap every turn
// at the whole remainder, read from the task as it stood when the job was
// queued.
func TestTheTurnIsCappedAtTheAllowanceTheOrchestratorGaveIt(t *testing.T) {
	a, cliLog, _ := fakeAdapter(t, "overbudget")
	tk := testTask()
	tk.Limits.MaxCostUSD, tk.CostUSD = 1, 0.6
	m := msg("Go on.")
	m.Metadata = map[string]string{ports.TurnBudgetMetadataKey: "0.2500"}
	if err := a.SendMessage(context.Background(), tk, m); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		select {
		case o := <-a.Outbound():
			done = o.Usage != nil
		case <-deadline:
			t.Fatal("the turn never ended")
		}
	}
	log := readFile(cliLog)
	if !strings.Contains(log, "--max-budget-usd 0.2500") || strings.Contains(log, "--max-budget-usd 0.4000") {
		t.Errorf("the turn must be capped at its allowance, not at what is left of the task: %s", log)
	}
}

// TestWithoutAnAllowanceTheCapFallsBackToWhatIsLeftOfTheTask: a delivery
// that did not come through the orchestrator's inbox carries no allowance;
// one the orchestrator could not have written is ignored. Neither leaves a
// task with a budget uncapped, and a task without one is never capped.
func TestWithoutAnAllowanceTheCapFallsBackToWhatIsLeftOfTheTask(t *testing.T) {
	tk := testTask()
	tk.Limits.MaxCostUSD, tk.CostUSD = 1, 0.6
	for _, v := range []string{"", "lots", "0", "-1"} {
		m := msg("Go on.")
		if v != "" {
			m.Metadata = map[string]string{ports.TurnBudgetMetadataKey: v}
		}
		if got := budgetArgs(tk, m); !slices.Equal(got, []string{"--max-budget-usd", "0.4000"}) {
			t.Errorf("allowance %q: got %v, want what is left of the task", v, got)
		}
	}
	if got := budgetArgs(&domain.Task{}, msg("Go on.")); got != nil {
		t.Errorf("a task with no cost limit gets no cap: %v", got)
	}
}
