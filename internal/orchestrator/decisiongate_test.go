package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
)

// implementAndDecide is the authority's opening reply that ended a real task
// with the implementer stopped mid-edit: an IMPLEMENT and, in the same reply,
// the FINAL_DECISION.
var implementAndDecide = fake.Reply(
	fake.Header(domain.MsgImplement, "codex", "Clear the cookie with Path=/ on logout.") +
		fake.Header(domain.MsgFinalDecision, "*", "Logout clears the cookie with Path=/."))

// gatedAuthority opens with implementAndDecide and decides again, after a
// short pause, once its delivery contains signal.
func gatedAuthority(signal string) fake.Script {
	return func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return implementAndDecide
		case strings.Contains(in.Content, signal) || string(in.Type) == signal:
			return fake.Turn{Latency: 50 * time.Millisecond,
				Reply: fake.Header(domain.MsgFinalDecision, "*", "Logout clears the cookie with Path=/; verified.")}
		}
		return fake.Turn{}
	}
}

func runGate(t *testing.T, opus, codex *fake.Agent, idle time.Duration) (*harness, *domain.Task) {
	t.Helper()
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), idle, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	return h, final
}

// deferredDecisionMessage reports whether a recorded FINAL_DECISION is marked
// as not taken up.
func deferredDecisionMessage(t *testing.T, h *harness, id domain.TaskID) bool {
	t.Helper()
	msgs, _ := h.store.Messages.ListByTask(context.Background(), id)
	for _, m := range msgs {
		if m.Type == domain.MsgFinalDecision && m.Metadata["decisionDeferred"] != "" {
			return true
		}
	}
	return false
}

// TestADecisionSentWithAnImplementWaitsForTheReport: the authority sent
// IMPLEMENT and FINAL_DECISION in one reply. The decision was accepted at
// once, completing the task, and ending it stopped the implementer in the
// middle of its turn with the workspace half-changed. Now the decision is not
// taken up -- no decision record, no AWAITING_DECISION -- the authority is
// told what is outstanding, and its decision sent again after the report
// completes the task. No agent is stopped while working.
func TestADecisionSentWithAnImplementWaitsForTheReport(t *testing.T) {
	opus := fake.New("opus", gatedAuthority(string(domain.MsgImplementationResult)))
	codex := fake.New("codex", fake.Steps(fake.Turn{Latency: 300 * time.Millisecond,
		Reply: fake.Header(domain.MsgImplementationResult, "opus", "Done: logout clears Path=/.")}))
	h, final := runGate(t, opus, codex, 5*time.Second)

	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", final.State, final.PauseReason)
	}
	ds, _ := h.store.Decisions.ListByTask(context.Background(), final.ID)
	if len(ds) != 1 || ds[0].Status != domain.DecisionAccepted || !strings.Contains(ds[0].Decision, "verified") {
		t.Fatalf("only the decision sent after the report is recorded: %+v", ds)
	}
	if len(h.eventsFor(domain.EvtDecisionDeferred, final.ID)) == 0 || !deferredDecisionMessage(t, h, final.ID) {
		t.Error("the decision not taken up must be recorded as such")
	}
	awaiting := 0
	for _, e := range h.eventsFor(domain.EvtTaskStateChanged, final.ID) {
		if e.Details["to"] == string(domain.TaskAwaitingDecision) {
			awaiting++
		}
	}
	if awaiting != 1 {
		t.Errorf("the task moves to AWAITING_DECISION once, for the decision taken up: %d times", awaiting)
	}
	msgs, _ := h.store.Messages.ListByTask(context.Background(), final.ID)
	told := false
	for _, m := range msgs {
		if m.ToAgent == "opus" && m.Metadata[domain.MetaOrchestrator] == "decision-deferred" {
			told = strings.Contains(m.Content, "IMPLEMENT to codex") && strings.Contains(m.Content, "FINAL_DECISION again")
		}
	}
	if !told {
		t.Error("the authority must be told what is outstanding and to decide again once it is reported")
	}
	if codex.Turns() != 1 || codex.Killed() != 0 || opus.Killed() != 0 {
		t.Errorf("no agent may be stopped mid-turn: codex %d turns, %d killed; opus %d killed",
			codex.Turns(), codex.Killed(), opus.Killed())
	}
}

// TestADecisionWithOnlyADelegateOpenCompletesWithItsCaveat: an open DELEGATE
// or QUESTION does not hold a decision. Deciding without the answer is the
// authority's call; it goes through, with the caveat recorded.
func TestADecisionWithOnlyADelegateOpenCompletesWithItsCaveat(t *testing.T) {
	opus := fake.New("opus", func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return fake.Reply(fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."))
		case in.FromAgent == "codex" || strings.Contains(in.Content, "from codex"):
			return fake.Turn{Latency: 50 * time.Millisecond,
				Reply: fake.Header(domain.MsgFinalDecision, "*", "Clear the cookie with Path=/.")}
		}
		return fake.Turn{}
	})
	codex := fake.New("codex", fake.Steps(fake.Reply(fake.Header(domain.MsgQuestion, "opus", "The session cookie or the CSRF one?"))))
	h, final := runGate(t, opus, codex, 5*time.Second)

	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", final.State, final.PauseReason)
	}
	if len(h.eventsFor(domain.EvtDecisionDeferred, final.ID)) != 0 {
		t.Error("an open DELEGATE must not hold the decision")
	}
	caveat := false
	for _, e := range h.eventsFor(domain.EvtDecisionCaveat, final.ID) {
		caveat = caveat || e.Details["kind"] == "UNANSWERED_REQUEST"
	}
	d, _ := h.orch.Decisions.Latest(context.Background(), final.ID)
	if !caveat || d == nil || !strings.Contains(strings.Join(d.Evidence, " "), "CAVEAT") {
		t.Errorf("the unanswered DELEGATE must be recorded as a caveat: %+v", d)
	}
}

// TestAnImplementerWhoseTurnsTimeOutReleasesAHeldDecision: the hold cannot
// last for ever. A working agent is bounded by its response timeout; timing
// out twice it is dropped, its IMPLEMENT is closed, and the authority's
// decision, sent again, completes the task.
func TestAnImplementerWhoseTurnsTimeOutReleasesAHeldDecision(t *testing.T) {
	opus := fake.New("opus", gatedAuthority("taken off this task"))
	codex := fake.New("codex", func(int, *domain.Task, domain.Message) fake.Turn {
		return fake.Failing(domain.FailureTimeout)
	})
	codex.TurnLimit = 100 * time.Millisecond
	h, final := runGate(t, opus, codex, 3*time.Second)

	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", final.State, final.PauseReason)
	}
	if !final.IsDropped("codex") || len(h.eventsFor(domain.EvtDecisionDeferred, final.ID)) == 0 {
		t.Errorf("the decision is held until codex is dropped: dropped=%v", final.Dropped)
	}
	if ds, _ := h.store.Decisions.ListByTask(context.Background(), final.ID); len(ds) != 1 {
		t.Errorf("one decision, taken after the drop: %d", len(ds))
	}
}

// TestAnImplementLeftUnansweredReleasesAHeldDecisionWhenItIsClosed: an
// IMPLEMENT to an agent that is idle and silent is chased -- a reminder, then
// closed as unanswered -- and the authority's decision, sent again, then
// completes the task.
func TestAnImplementLeftUnansweredReleasesAHeldDecisionWhenItIsClosed(t *testing.T) {
	orchestrator.SetHumanAnswerGrace(100 * time.Millisecond)
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() {
		orchestrator.SetHumanAnswerGrace(45 * time.Second)
		orchestrator.SetFollowUpSettleInterval(2 * time.Second)
	})
	opus := fake.New("opus", gatedAuthority("closed it as unanswered"))
	codex := fake.New("codex", func(int, *domain.Task, domain.Message) fake.Turn { return fake.Turn{} })
	h, final := runGate(t, opus, codex, 3*time.Second)

	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", final.State, final.PauseReason)
	}
	if len(h.eventsFor(domain.EvtDecisionDeferred, final.ID)) == 0 || len(h.eventsFor(domain.EvtRequestAbandoned, final.ID)) == 0 {
		t.Error("the decision is held until the IMPLEMENT is closed as unanswered")
	}
}
