package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
)

// queueHarness has one slot, so a second task's run waits for the first.
// Task one is opus and codex, codex taking a while over its answer; task two
// is scout and bard, whose turns are counted to show they never ran.
func queueHarness(t *testing.T) (h *harness, scout, bard *fake.Agent, one, two *domain.Task) {
	t.Helper()
	gw := newGateway()
	gw.add(fake.New("opus", fakeAuthority()), domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(fake.Turn{Latency: 400 * time.Millisecond, Reply: codexAnswer.Reply})), domain.RoleImplementer)
	scout = fake.New("scout", fakeAuthority())
	bard = fake.New("bard", fake.Steps(codexAnswer))
	gw.add(scout, domain.RoleAuthority)
	gw.add(bard, domain.RoleImplementer)
	h = newHarnessSlots(t, defaultLimits(), 5*time.Second, gw, false,
		[]time.Duration{20 * time.Millisecond}, 1)
	one = createTask(t, h, "opus", "opus", "codex")
	two = createTask(t, h, "scout", "scout", "bard")
	return h, scout, bard, one, two
}

// startQueued runs task one, waits until it holds the slot, then starts task
// two and waits until its run reports that it is queued.
func startQueued(t *testing.T, h *harness, one, two *domain.Task) (doneOne, doneTwo chan error) {
	t.Helper()
	ctx := context.Background()
	doneOne, doneTwo = make(chan error, 1), make(chan error, 1)
	go func() { doneOne <- h.orch.RunTask(ctx, one.ID) }()
	waitFor(t, func() bool { return len(h.eventsFor(domain.EvtRunStarted, one.ID)) == 1 })
	go func() { doneTwo <- h.orch.RunTask(ctx, two.ID) }()
	waitFor(t, func() bool { return len(h.eventsFor(domain.EvtRunQueued, two.ID)) == 1 })
	if !h.orch.Queued(two.ID) || !h.orch.Running(two.ID) {
		t.Fatal("a run waiting for a slot must be seen, as queued")
	}
	return doneOne, doneTwo
}

func runResult(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the run never returned")
		return nil
	}
}

// TestARunQueuedBehindAnotherTaskCanBePausedAndNeverStarts: with one slot,
// a resumed task's run used to wait for it before it was registered. Pausing
// it found nothing to cancel, and once the first task ended the run started
// on the PAUSED task: the authority took a paid turn whose reply was refused
// as a state violation. Now the queued run is cancelled by the pause, and
// none of the second task's agents is ever started.
func TestARunQueuedBehindAnotherTaskCanBePausedAndNeverStarts(t *testing.T) {
	h, scout, bard, one, two := queueHarness(t)
	ctx := context.Background()
	doneOne, doneTwo := startQueued(t, h, one, two)

	if err := h.orch.Pause(ctx, two.ID, ""); err != nil {
		t.Fatal(err)
	}
	_ = runResult(t, doneTwo)
	if h.orch.Running(two.ID) {
		t.Error("the paused queued run must be gone")
	}
	_ = runResult(t, doneOne)

	if got, _ := h.orch.Tasks.Get(ctx, one.ID); got.State != domain.TaskCompleted {
		t.Errorf("the first task runs to the end: %s (%s)", got.State, got.PauseReason)
	}
	if got, _ := h.orch.Tasks.Get(ctx, two.ID); got.State != domain.TaskPaused {
		t.Errorf("the second task stays paused: %s", got.State)
	}
	if n := h.gw.started("scout") + h.gw.started("bard"); n != 0 || scout.Turns()+bard.Turns() != 0 {
		t.Errorf("no agent of the paused task may start: %d starts, %d turns", n, scout.Turns()+bard.Turns())
	}
	if len(h.eventsFor(domain.EvtRunStarted, two.ID)) != 0 || len(h.eventsFor(domain.EvtRunUnqueued, two.ID)) != 1 {
		t.Error("the queued run is recorded as ending without starting")
	}
}

// TestAQueuedRunWhoseTaskWasPausedMeanwhileDoesNotStart: the task is read
// again once the run has its slot. Paused while the run waited, by a path
// that did not cancel the run, it is left paused and no agent starts.
func TestAQueuedRunWhoseTaskWasPausedMeanwhileDoesNotStart(t *testing.T) {
	h, scout, bard, one, two := queueHarness(t)
	ctx := context.Background()
	doneOne, doneTwo := startQueued(t, h, one, two)

	if _, err := h.orch.Tasks.Pause(ctx, two.ID, domain.HumanAgentID, domain.PauseReasonHuman); err != nil {
		t.Fatal(err)
	}
	_ = runResult(t, doneOne)
	if err := runResult(t, doneTwo); err != nil {
		t.Errorf("finding its task paused is not an error: %v", err)
	}
	if got, _ := h.orch.Tasks.Get(ctx, two.ID); got.State != domain.TaskPaused {
		t.Errorf("the second task stays paused: %s", got.State)
	}
	if n := h.gw.started("scout") + h.gw.started("bard"); n != 0 || scout.Turns()+bard.Turns() != 0 {
		t.Errorf("no agent of the paused task may start: %d starts, %d turns", n, scout.Turns()+bard.Turns())
	}
}

// TestTwoRapidStartsMakeOneRun: a human action starts a run when it finds
// none, and two in quick succession could each start one; a second loop on
// the task would deliver everything twice.
func TestTwoRapidStartsMakeOneRun(t *testing.T) {
	gw := newGateway()
	opus := fake.New("opus", fakeAuthority())
	gw.add(opus, domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(codexAnswer)), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	ctx := context.Background()
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- h.orch.RunTask(ctx, task.ID) }()
	}
	_, _ = runResult(t, done), runResult(t, done)

	if got, _ := h.orch.Tasks.Get(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Errorf("the one run completes the task: %s (%s)", got.State, got.PauseReason)
	}
	if n := len(h.eventsFor(domain.EvtRunStarted, task.ID)); n != 1 {
		t.Errorf("%d runs started, want 1", n)
	}
	assignments := 0
	for _, m := range opus.Received() {
		if m.Type == domain.MsgTaskAssignment {
			assignments++
		}
	}
	if assignments != 1 || gw.started("opus") != 1 {
		t.Errorf("the authority is started and briefed once: %d starts, %d assignments", gw.started("opus"), assignments)
	}
}
