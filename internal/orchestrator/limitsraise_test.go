package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
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

// failingMessages records messages until failAfter of them are stored, then
// refuses every write, as a full disk or a broken database does.
type failingMessages struct {
	ports.MessageRepository
	failAfter int32
	n         atomic.Int32
}

func (f *failingMessages) Append(ctx context.Context, m *domain.Message) error {
	if f.n.Add(1) > f.failAfter {
		return errors.New("disk I/O error (injected)")
	}
	return f.MessageRepository.Append(ctx, m)
}

// TestARunEndedByAStoreErrorPausesTheTask: an error returned from the run --
// here the store refusing the authority's first reply -- used to leave the
// task PLANNING with no run behind it and nothing on screen to say so, until
// a restart. Now the task is paused with the error as its reason.
func TestARunEndedByAStoreErrorPausesTheTask(t *testing.T) {
	store := storage.NewInMemory()
	broken := *store
	broken.Messages = &failingMessages{MessageRepository: store.Messages, failAfter: 1}
	gw := newGateway()
	gw.add(fake.New("opus", fakeAuthority()), domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(codexAnswer)), domain.RoleImplementer)
	h := &harness{store: broken, gw: gw}
	h.orch = orchestrator.New(orchestrator.Deps{
		Storage: broken, Agents: gw, Knowledge: knowledge.New(store.Knowledge),
		Decisions: decisions.New(store.Decisions), Logger: logging.NewDiscard(),
		Limits: defaultLimits(), MaxConcurrentTasks: 2, IdleTimeout: 5 * time.Second,
	})
	task := createTask(t, h, "opus", "opus", "codex")

	err := h.orch.RunTask(context.Background(), task.ID)
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("the run must return the store's error, got %v", err)
	}
	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused || !strings.Contains(got.PauseReason, "disk I/O error (injected)") {
		t.Errorf("the task must be paused with the error as its reason: %s (%q)", got.State, got.PauseReason)
	}
	if h.orch.Running(task.ID) {
		t.Error("no run may be left behind")
	}
}

// delegatingAuthority delegates to codex on every delivery, with new words
// each time; codex answers each.
func delegatingAuthority() fake.Script {
	return func(n int, _ *domain.Task, _ domain.Message) fake.Turn {
		return fake.Reply(fake.Header(domain.MsgDelegate, "codex", fmt.Sprintf("Look at part %d of the cookie handling.", n)))
	}
}

// TestRaisingTheMessageCeilingLetsAResumedTaskGoOn: MaxTotalMessages counts
// over the task's whole life and nothing reset it, so once a task reached it
// every resume allowed exactly one more message before pausing again. The
// human can now raise it.
func TestRaisingTheMessageCeilingLetsAResumedTaskGoOn(t *testing.T) {
	limits := defaultLimits()
	limits.MaxTotalMessages = 2
	gw := newGateway()
	gw.add(fake.New("opus", delegatingAuthority()), domain.RoleAuthority)
	gw.add(fake.New("codex", func(n int, _ *domain.Task, _ domain.Message) fake.Turn {
		return fake.Reply(fake.Header(domain.MsgAnswer, "opus", fmt.Sprintf("Part %d is fine.", n)))
	}), domain.RoleImplementer)
	h := newHarness(t, limits, 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	run := func() *domain.Task {
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = h.orch.RunTask(runCtx, task.ID)
		got, _ := h.orch.Tasks.Get(ctx, task.ID)
		return got
	}

	paused := run()
	if paused.State != domain.TaskPaused || paused.PauseReason != domain.PauseReasonMaxTotal {
		t.Fatalf("the ceiling pauses the task: %s (%s)", paused.State, paused.PauseReason)
	}
	if err := h.orch.RaiseLimits(ctx, task.ID, domain.TaskLimits{MaxTotalMessages: paused.MessageCount + 10}); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	again := run()
	if more := again.MessageCount - paused.MessageCount; more <= 1 {
		t.Errorf("after raising the ceiling the task must go on for more than one message: %d more", more)
	}
	if again.State != domain.TaskPaused || again.MessageCount != paused.MessageCount+10 {
		t.Errorf("it runs up to the new ceiling: %s at %d messages", again.State, again.MessageCount)
	}
	if !hasSummary(h, "LIMITS raised: messages 2 -> 12") {
		t.Error("raising the limit is recorded")
	}
}

func hasSummary(h *harness, s string) bool {
	for _, e := range h.snapshot() {
		if strings.Contains(e.Summary, s) {
			return true
		}
	}
	return false
}

// TestLimitsCanOnlyBeRaised: lowering a limit, imposing one the task does not
// have, or changing any other limit is refused and changes nothing.
func TestLimitsCanOnlyBeRaised(t *testing.T) {
	limits := defaultLimits()
	limits.MaxTotalMessages = 50
	limits.MaxCostUSD = 2
	h, task := tmHarness(t, limits)
	ctx := context.Background()
	for _, bad := range []domain.TaskLimits{
		{},
		{MaxTotalMessages: 40},
		{MaxCostUSD: 2},
		{MaxTokens: 1000},
		{MaxMessages: 500},
	} {
		if err := h.orch.RaiseLimits(ctx, task.ID, bad); err == nil {
			t.Errorf("%+v must be refused", bad)
		}
	}
	if err := h.orch.RaiseLimits(ctx, task.ID, domain.TaskLimits{MaxTotalMessages: 80, MaxCostUSD: 5}); err != nil {
		t.Fatal(err)
	}
	got, _ := h.orch.Tasks.Get(ctx, task.ID)
	if got.Limits.MaxTotalMessages != 80 || got.Limits.MaxCostUSD != 5 || got.Limits.MaxMessages != limits.MaxMessages {
		t.Errorf("only the raised limits change: %+v", got.Limits)
	}
}

// TestTheReasonForAPauseIsTheMostImportantLimitReached: spending first, then
// time, then the message counts. The message ceiling was checked first, so a
// task over it reported every budget breach as a message limit.
func TestTheReasonForAPauseIsTheMostImportantLimitReached(t *testing.T) {
	now := time.Now()
	over := &domain.Task{
		Limits: domain.TaskLimits{MaxTotalMessages: 10, MaxMessages: 5, MaxCostUSD: 1, MaxDuration: time.Minute},
		MessageCount: 10, AgentMessages: 5, CostUSD: 1.5, CreatedAt: now, SegmentStart: now.Add(-time.Hour),
	}
	for _, want := range []string{domain.PauseReasonBudget, domain.PauseReasonMaxDuration, domain.PauseReasonMaxTotal, domain.PauseReasonMaxMessages} {
		reason, ok := orchestrator.LimitExceeded(over)
		if !ok || !strings.HasPrefix(reason, want) {
			t.Fatalf("got %q, want %q", reason, want)
		}
		switch want { // clear this one, and the next is reported
		case domain.PauseReasonBudget:
			over.CostUSD = 0
		case domain.PauseReasonMaxDuration:
			over.SegmentStart = now
		case domain.PauseReasonMaxTotal:
			over.MessageCount = 0
		}
	}
}

// TestATaskOverItsMessageCeilingAndItsBudgetPausesForTheBudget: a run on a
// task already over both pauses on its first message, giving the budget as
// the reason -- the one the human must act on first.
func TestATaskOverItsMessageCeilingAndItsBudgetPausesForTheBudget(t *testing.T) {
	limits := defaultLimits()
	limits.MaxTotalMessages = 1
	limits.MaxCostUSD = 0.50
	gw := newGateway()
	gw.add(fake.New("opus", delegatingAuthority()), domain.RoleAuthority)
	gw.add(fake.New("codex", fake.Steps(codexAnswer)), domain.RoleImplementer)
	h := newHarness(t, limits, 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	// Spent and talked in an earlier run.
	if _, err := h.orch.Tasks.AddUsage(ctx, task.ID, "opus", ports.Usage{CostUSD: 0.60}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	got, _ := h.orch.Tasks.Get(ctx, task.ID)
	if got.State != domain.TaskPaused || !strings.HasPrefix(got.PauseReason, domain.PauseReasonBudget) {
		t.Errorf("state %s, reason %q: want a budget pause", got.State, got.PauseReason)
	}
}
