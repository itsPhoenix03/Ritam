package orchestrator_test

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// sumTurns adds up what the turns were billed.
func sumTurns(turns []*domain.Turn) (cost float64, in, out int64) {
	for _, tr := range turns {
		cost += tr.CostUSD
		in += tr.InputTokens
		out += tr.OutputTokens
	}
	return cost, in, out
}

// TestEveryTurnOfAShortTaskIsRecordedWithWhatItCarriedAndCost: nothing said
// which messages went into which turn, so the history could not show what an
// agent was actually given. After a three-turn task each turn is on record
// with the messages it carried, its outcome and its usage -- the deciding
// turn's, billed after the run ended, too -- and the turns add up to what
// the task was billed.
func TestEveryTurnOfAShortTaskIsRecordedWithWhatItCarriedAndCost(t *testing.T) {
	authority := func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return fake.Turn{Reply: fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."),
				Usage: &ports.Usage{InputTokens: 300, OutputTokens: 40, CostUSD: 0.03}}
		case strings.Contains(in.Content, "Logout clears"):
			return fake.Turn{Reply: fake.Header(domain.MsgFinalDecision, "*", "Clear the cookie with Path=/."),
				Usage: &ports.Usage{InputTokens: 500, OutputTokens: 60, CostUSD: 0.05}}
		}
		return fake.Turn{}
	}
	codex := fake.New("codex", fake.Steps(fake.Turn{Reply: codexAnswer.Reply,
		Usage: &ports.Usage{InputTokens: 900, OutputTokens: 120, CostUSD: 0.11}}))
	gw := newGateway()
	gw.add(fake.New("opus", authority), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := h.orch.RunTask(runCtx, task.ID); err != nil {
		t.Fatal(err)
	}
	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s, want COMPLETED", final.State)
	}

	turns, err := h.store.Turns.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var agents []domain.AgentID
	for _, tr := range turns {
		agents = append(agents, tr.AgentID)
		if tr.Outcome != domain.TurnCompleted || tr.EndedAt == nil || tr.EndedAt.Before(tr.StartedAt) ||
			tr.InputTokens == 0 || tr.CostUSD == 0 || len(tr.Messages) == 0 {
			t.Errorf("an incomplete record: %+v", tr)
		}
		for _, id := range tr.Messages {
			if m, err := h.store.Messages.Get(ctx, id); err != nil || m.ToAgent != tr.AgentID {
				t.Errorf("turn %s of %s carried %s, which is not a message to it: %v %+v", tr.ID, tr.AgentID, id, err, m)
			}
		}
	}
	if len(turns) != 3 || agents[0] != "opus" || agents[1] != "codex" || agents[2] != "opus" {
		t.Fatalf("want opus, codex, opus in the order they started, got %v", agents)
	}
	cost, in, out := sumTurns(turns)
	if math.Abs(cost-final.CostUSD) > 1e-9 || in != final.InputTokens || out != final.OutputTokens {
		t.Errorf("the turns add up to $%.4f, %d in, %d out; the task was billed $%.4f, %d in, %d out",
			cost, in, out, final.CostUSD, final.InputTokens, final.OutputTokens)
	}
}

// TestAKilledTurnIsRecordedAsKilledWithItsPartialUsage: codex was working
// when the task was paused. Its turn is recorded as killed, with the tokens
// it reported before it was stopped, which the task was billed too.
func TestAKilledTurnIsRecordedAsKilledWithItsPartialUsage(t *testing.T) {
	codex := fake.New("codex", fake.Steps(fake.Turn{Latency: 10 * time.Second,
		KilledUsage: &ports.Usage{InputTokens: 7, OutputTokens: 1, CostUSD: 0.01}}))
	gw := newGateway()
	gw.add(fake.New("opus", fakeAuthority()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool { return codex.Turns() == 1 })
	endRun(t, h, task.ID, done)

	turns, _ := h.store.Turns.ListByTask(ctx, task.ID)
	var killed *domain.Turn
	for _, tr := range turns {
		if tr.AgentID == "codex" {
			killed = tr
		}
	}
	if killed == nil || killed.Outcome != domain.TurnKilled || killed.EndedAt == nil ||
		killed.InputTokens != 7 || killed.CostUSD != 0.01 || len(killed.Messages) != 1 {
		t.Fatalf("codex's turn must be recorded killed, with its partial usage: %+v", killed)
	}
	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	if cost, in, _ := sumTurns(turns); math.Abs(cost-final.CostUSD) > 1e-9 || in != final.InputTokens {
		t.Errorf("the turns add up to $%.4f and %d in; the task was billed $%.4f and %d in",
			cost, in, final.CostUSD, final.InputTokens)
	}
}
