package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
)

// The failure policy, on the scripted fake agent (failures.go has the table).

// failingCodex fails its first turn with first, then answers the authority.
func failingCodex(first fake.Turn) *fake.Agent {
	codex := fake.New("codex", fake.Steps(first, codexAnswer))
	codex.TurnLimit = 100 * time.Millisecond
	return codex
}

func countEvents(h *harness, tp domain.EventType) int {
	n := 0
	for _, e := range h.snapshot() {
		if e.Type == tp {
			n++
		}
	}
	return n
}

// TestEachFailureKindLeadsToItsDocumentedAction runs a supporting agent whose
// first turn fails with each kind in turn, and checks what the orchestrator
// did: ask again, retry, wait, drop, pause -- and never FAILED.
func TestEachFailureKindLeadsToItsDocumentedAction(t *testing.T) {
	type outcome struct {
		state   domain.TaskState
		retried bool
		dropped bool
		reason  string // in the pause reason
	}
	cases := map[string]struct {
		turn fake.Turn
		want outcome
	}{
		"malformed: asked again": {fake.Reply(fake.Header(domain.MsgAnswer, "", "no recipient")),
			outcome{state: domain.TaskCompleted}},
		"timeout: retried once": {fake.Failing(domain.FailureTimeout),
			outcome{state: domain.TaskCompleted, retried: true}},
		"overloaded: retried with backoff": {fake.Failing(domain.FailureOverloaded),
			outcome{state: domain.TaskCompleted, retried: true}},
		"crash: retried with backoff": {fake.Failing(domain.FailureCrash),
			outcome{state: domain.TaskCompleted, retried: true}},
		"session missing: retried": {fake.Failing(domain.FailureSessionMissing),
			outcome{state: domain.TaskCompleted, retried: true}},
		"rate limit resetting soon: waited out": {fake.RateLimited(100 * time.Millisecond),
			outcome{state: domain.TaskCompleted, retried: true}},
		"rate limit resetting late: paused with the time": {fake.RateLimited(3 * time.Hour),
			outcome{state: domain.TaskPaused, reason: "resets in 3h0m0s"}},
		"rate limit with no reset: dropped": {fake.RateLimited(0),
			outcome{state: domain.TaskPaused, dropped: true}},
		"auth: dropped":    {fake.Failing(domain.FailureAuth), outcome{state: domain.TaskPaused, dropped: true}},
		"billing: dropped": {fake.Failing(domain.FailureBilling), outcome{state: domain.TaskPaused, dropped: true}},
		"plan: dropped":    {fake.Failing(domain.FailurePlan), outcome{state: domain.TaskPaused, dropped: true}},
		"model: dropped": {fake.Failing(domain.FailureModelUnavailable),
			outcome{state: domain.TaskPaused, dropped: true}},
		"budget stop: paused for the budget": {fake.Failing(domain.FailureBudgetStop),
			outcome{state: domain.TaskPaused, reason: domain.PauseReasonBudget}},
		"cancelled: nothing": {fake.Failing(domain.FailureCancelled), outcome{state: domain.TaskPaused}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h, final := runFake(t, failingCodex(c.turn), 400*time.Millisecond)
			got := outcome{state: final.State, retried: h.hasEvent(domain.EvtAgentRetried), dropped: final.IsDropped("codex")}
			if got.state != c.want.state || got.retried != c.want.retried || got.dropped != c.want.dropped {
				t.Errorf("got %+v (%s), want %+v", got, final.PauseReason, c.want)
			}
			if c.want.reason != "" && !strings.Contains(final.PauseReason, c.want.reason) {
				t.Errorf("pause reason %q should contain %q", final.PauseReason, c.want.reason)
			}
		})
	}
}

// TestAnAuthorityThatKeepsFailingPausesTheTaskWithAReason: the authority
// cannot be dropped -- nothing could be decided without it -- so where a
// supporting agent would be, the task pauses saying why.
func TestAnAuthorityThatKeepsFailingPausesTheTaskWithAReason(t *testing.T) {
	cases := map[string]struct {
		turn   fake.Turn
		reason string
	}{
		"timeout":           {fake.Failing(domain.FailureTimeout), "opus's turn failed 2 times in a row (timeout"},
		"crash":             {fake.Failing(domain.FailureCrash), "opus's turn failed 4 times in a row (crash"},
		"rate limit":        {fake.RateLimited(0), "opus (the authority) hit a usage limit"},
		"auth":              {fake.Failing(domain.FailureAuth), "not logged in"},
		"rate limit, later": {fake.RateLimited(2 * time.Hour), "resets in 2h0m0s"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			opus := fake.New("opus", func(int, *domain.Task, domain.Message) fake.Turn { return c.turn })
			opus.TurnLimit = 50 * time.Millisecond
			gw := newGateway()
			gw.add(opus, domain.RoleAuthority)
			gw.add(fake.New("codex", fake.Steps(codexAnswer)), domain.RoleImplementer)
			h := newHarness(t, defaultLimits(), 5*time.Second, gw)
			task := createTask(t, h, "opus", "opus", "codex")
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			_ = h.orch.RunTask(ctx, task.ID)
			final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
			if final.State != domain.TaskPaused || !strings.Contains(final.PauseReason, c.reason) {
				t.Errorf("want PAUSED with %q, got %s (%s)", c.reason, final.State, final.PauseReason)
			}
		})
	}
}

// TestASupportingAgentThatTimesOutOnceThenAnswersLetsTheTaskComplete: one
// slow turn used to end the task -- a timeout went straight to FAILED. It is
// now retried once in the same session, and the task completes.
func TestASupportingAgentThatTimesOutOnceThenAnswersLetsTheTaskComplete(t *testing.T) {
	codex := failingCodex(fake.Failing(domain.FailureTimeout))
	h, final := runFake(t, codex, 5*time.Second)
	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s %s), want COMPLETED", final.State, final.PauseReason, final.FailureReason)
	}
	if n := countEvents(h, domain.EvtAgentRetried); n != 1 {
		t.Errorf("want exactly one retry recorded, got %d", n)
	}
	got := codex.Received()
	if len(got) != 2 || got[1].Type != domain.MsgDelegate || !strings.Contains(got[1].Content, "cut off") ||
		!strings.Contains(got[1].Content, "Look at the cookie handling.") {
		t.Errorf("the retry is the same delegation, with a note that the turn was cut off: %+v", got)
	}
}

// TestASupportingAgentThatCrashesThreeTimesIsDropped: dropping an agent was
// only a message to the authority; the agent stayed a participant and went
// on receiving work. With two retries allowed, codex's third crash drops it:
// it receives nothing more (not even the authority's new request, which is
// closed back to the authority), the authority is told, and the task
// continues to its decision.
func TestASupportingAgentThatCrashesThreeTimesIsDropped(t *testing.T) {
	codex := fake.New("codex", func(int, *domain.Task, domain.Message) fake.Turn {
		return fake.Failing(domain.FailureCrash)
	})
	var droppedNote, refusal bool
	authority := func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return fake.Reply(fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."))
		case strings.Contains(in.Content, "taken off this task"):
			droppedNote = true
			// Asking the dropped agent again: it must not reach it.
			return fake.Reply(fake.Header(domain.MsgDelegate, "codex", "Try once more?"))
		case strings.Contains(in.Content, "was not delivered and is closed"):
			refusal = true
			return fake.Reply(fake.Header(domain.MsgFinalDecision, "*", "Decided without codex."))
		}
		return fake.Turn{}
	}
	gw := newGateway()
	gw.add(fake.New("opus", authority), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarnessWith(t, defaultLimits(), 5*time.Second, gw, []time.Duration{20 * time.Millisecond, 20 * time.Millisecond})
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)

	if n := codex.Turns(); n != 3 {
		t.Errorf("codex took %d turns, want 3: the first and two retries", n)
	}
	if n := len(codex.Received()); n != 3 {
		t.Errorf("codex received %d deliveries, want 3: nothing after it was dropped", n)
	}
	if !droppedNote || !refusal {
		t.Errorf("the authority must be told of the drop (%v) and of its undelivered request (%v)", droppedNote, refusal)
	}
	if !final.IsDropped("codex") || countEvents(h, domain.EvtAgentDropped) != 1 || countEvents(h, domain.EvtAgentRetried) != 2 {
		t.Errorf("two retries and one drop must be recorded on the task: dropped %v", final.Dropped)
	}
	if final.State != domain.TaskCompleted {
		t.Errorf("the task continues without codex: %s (%s)", final.State, final.PauseReason)
	}
}

// TestAnAuthorityRateLimitWithATwoSecondResetWaitsAndContinues: a limit that
// lifts in two seconds is waited out -- with the idle timeout shorter than
// the wait -- and the authority's turn is sent again.
func TestAnAuthorityRateLimitWithATwoSecondResetWaitsAndContinues(t *testing.T) {
	gw := newGateway()
	gw.add(fake.New("opus", fake.Then(fakeAuthority(), fake.RateLimited(2*time.Second))), domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(codexAnswer)), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 500*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)

	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", final.State, final.PauseReason)
	}
	if took := time.Since(start); took < 2*time.Second {
		t.Errorf("the reset must be waited out: the task took %v", took)
	}
	if !strings.Contains(h.eventSummary(domain.EvtAgentRetried), "in 2s") {
		t.Errorf("the wait must be recorded: %q", h.eventSummary(domain.EvtAgentRetried))
	}
}

// TestATurnWorkingLongerThanTheIdleTimeoutDoesNotPauseTheTask: idle means
// nobody is working. A turn that runs past the idle timeout with nothing
// streamed is bounded by its response timeout, not paused as idle.
func TestATurnWorkingLongerThanTheIdleTimeoutDoesNotPauseTheTask(t *testing.T) {
	slow := codexAnswer
	slow.Latency = 800 * time.Millisecond
	_, final := runFake(t, fake.New("codex", fake.Steps(slow)), 200*time.Millisecond)
	if final.State != domain.TaskCompleted {
		t.Errorf("a working agent is not idle: %s (%s)", final.State, final.PauseReason)
	}
}

// TestRetryReturnsAFailedTaskToARunningState: FAILED was terminal, so a task
// failed by an older version of ritam could only be abandoned. Retry brings
// it back with the same participants, and the authority is told why.
func TestRetryReturnsAFailedTaskToARunningState(t *testing.T) {
	var resumeNote string
	authority := func(n int, tk *domain.Task, in domain.Message) fake.Turn {
		if strings.Contains(in.Content, "human has retried it") {
			resumeNote = in.Content
			return fake.Reply(fake.Header(domain.MsgFinalDecision, "*", "Done after the retry."))
		}
		return fakeAuthority()(n, tk, in)
	}
	gw := newGateway()
	gw.add(fake.New("opus", authority), domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(fake.Turn{Latency: time.Hour})), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()

	// Fail it as an older version did, mid-run.
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { _ = h.orch.RunTask(runCtx, task.ID); close(done) }()
	waitFor(t, func() bool { return h.hasEvent(domain.EvtAgentMessageReceived) })
	if _, err := h.orch.Tasks.Fail(ctx, task.ID, "codex", "AGENT_ERROR: codex: agent CLI timed out after 5m0s"); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-done

	if err := h.orch.Retry(ctx, task.ID); err != nil {
		t.Fatalf("retry: %v", err)
	}
	got, _ := h.orch.Tasks.Get(ctx, task.ID)
	if got.State.IsTerminal() || got.State == domain.TaskPaused || len(got.Participants) != 2 {
		t.Fatalf("a retried task is running again with its participants: %s %v", got.State, got.Participants)
	}
	runCtx, cancel = context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)
	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	if final.State != domain.TaskCompleted || !strings.Contains(resumeNote, "timed out after 5m0s") {
		t.Errorf("the authority is told why it failed and carries on: %s, note %q", final.State, resumeNote)
	}
	if err := h.orch.Retry(ctx, task.ID); err == nil {
		t.Error("only a failed task can be retried")
	}
}

// TestADropSurvivesAPauseAndAHumanResumeGivesAFreshChance: the drop is kept
// on the task, so a pause does not quietly bring the agent back; the human
// resuming the task does.
func TestADropSurvivesAPauseAndAHumanResumeGivesAFreshChance(t *testing.T) {
	h, final := runFake(t, failingCodex(fake.Failing(domain.FailureAuth)), 300*time.Millisecond)
	if final.State != domain.TaskPaused || !final.IsDropped("codex") {
		t.Fatalf("codex is dropped and the task paused: %s %v", final.State, final.Dropped)
	}
	stored, _ := h.store.Tasks.Get(context.Background(), final.ID)
	if !stored.IsDropped("codex") {
		t.Fatal("the drop must be stored with the task")
	}
	if err := h.orch.Resume(context.Background(), final.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.orch.Tasks.Get(context.Background(), final.ID); got.IsDropped("codex") {
		t.Error("a human resume gives a dropped agent a fresh chance")
	}
}
