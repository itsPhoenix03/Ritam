package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// noRecipientErr is what both adapters return when every block of a reply
// lacked a recipient.
const noRecipientErr = "claude-code adapter: a ANSWER block had no recipient (to=) and was dropped"

// delegateToCodex is an authority that hands codex the work and then waits.
func delegateToCodex() mockagent.Behavior {
	return func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgTaskAssignment {
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Look at the cookie handling."}}
		}
		return nil
	}
}

// TestAReplyWithNoRecipientIsReprompted: codex answered with a header that
// named no recipient. The adapter dropped the block and reported it in its
// current wording, which the orchestrator did not recognise, so the task went
// to FAILED -- terminal -- over one unusable turn. It is now told what was
// wrong and asked again.
func TestAReplyWithNoRecipientIsReprompted(t *testing.T) {
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, delegateToCodex()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(ctx, task.ID) }()

	waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
	codex.out <- ports.Outbound{AgentID: "codex", Err: errors.New(noRecipientErr)}
	codex.finish()
	waitFor(t, func() bool { return len(codex.deliveries()) == 2 })

	if got := codex.deliveries()[1]; got.Type != domain.MsgError {
		t.Errorf("codex must be told its reply was not understood, got %s %q", got.Type, got.Content)
	}
	if got, _ := h.orch.Tasks.Get(context.Background(), task.ID); got.State == domain.TaskFailed {
		t.Fatalf("one reply with no recipient failed the task: %s", got.FailureReason)
	}
	cancel()
	<-done
}

// errAgent ends every turn with the same adapter error, as a CLI that has
// drifted out of the reply format for good does.
type errAgent struct {
	*promptAgent
	err   error
	turns atomic.Int64
}

func (e *errAgent) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return e.SendMessage(ctx, t, m)
}

func (e *errAgent) SendMessage(_ context.Context, _ *domain.Task, m domain.Message) error {
	e.turns.Add(1)
	e.mu.Lock()
	e.received = append(e.received, m)
	e.mu.Unlock()
	go func() {
		select {
		case e.out <- ports.Outbound{AgentID: e.id, Err: e.err}:
		case <-time.After(time.Second):
		}
	}()
	return nil
}

const noHeaderErr = "claude-code adapter: agent opus replied without an @@ORCH header"

// TestAnAuthorityThatNeverRepliesReadablyPausesTheTask: an authority whose
// every reply lacked a header was re-prompted 1,799 times in three seconds,
// with MessageCount still 0 and no limit noticing. It now pauses within three
// turns, saying why and what the agent sent.
func TestAnAuthorityThatNeverRepliesReadablyPausesTheTask(t *testing.T) {
	opus := &errAgent{promptAgent: newPromptAgent("opus"), err: errors.New(noHeaderErr)}
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(newPromptAgent("codex"), domain.RoleImplementer)
	limits := domain.TaskLimits{MaxDeliberationRounds: 20, MaxMessages: 5, StallMessages: 3,
		MaxTotalMessages: 10, MaxDuration: time.Minute}
	h := newHarness(t, limits, 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused {
		t.Fatalf("state = %s, want PAUSED", got.State)
	}
	if n := opus.turns.Load(); n > 3 {
		t.Errorf("the authority took %d turns, want at most 3", n)
	}
	if got.MalformedCount["opus"] != 2 {
		t.Errorf("MalformedCount = %v, want opus: 2", got.MalformedCount)
	}
	if !strings.Contains(got.PauseReason, "could not be read") || !strings.Contains(got.PauseReason, "@@ORCH header") {
		t.Errorf("the pause must say why and what the agent sent: %q", got.PauseReason)
	}
	re := opus.deliveries()
	if len(re) < 2 || !strings.Contains(re[1].Content, "type=DELEGATE to=codex") {
		t.Errorf("the re-prompt must show the header to write with the task's ids: %+v", re)
	}
}

// TestASupportingAgentThatNeverRepliesReadablyIsDropped: codex keeps failing
// to produce a header. After its second unreadable reply it is not asked
// again; the request it owed is closed and the authority is told.
func TestASupportingAgentThatNeverRepliesReadablyIsDropped(t *testing.T) {
	var mu sync.Mutex
	var told []domain.Message
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgTaskAssignment {
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Look at the cookie handling."}}
		}
		mu.Lock()
		told = append(told, in)
		mu.Unlock()
		return nil
	}
	codex := &errAgent{promptAgent: newPromptAgent("codex"),
		err: errors.New("codex adapter: agent codex replied without a ##CODEX header")}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 500*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	_ = h.orch.RunTask(context.Background(), task.ID)

	if n := codex.turns.Load(); n != 2 {
		t.Errorf("codex took %d turns, want 2", n)
	}
	re := codex.deliveries()
	if len(re) < 2 || !strings.Contains(re[1].Content, "type=ANSWER to=opus") {
		t.Errorf("the re-prompt must show the answer header codex owes opus: %+v", re)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(told) == 0 || !strings.Contains(told[0].Content, "could not produce a valid reply") {
		t.Fatalf("the authority must be told codex is out: %+v", told)
	}
	history, _ := h.store.Messages.ListByTask(context.Background(), task.ID)
	if open := domain.OpenRequests(domain.BuildLedger(history)); len(open) != 0 {
		t.Errorf("codex's request must be closed, still open: %+v", open[0].Message)
	}
}

// TestAReadableReplyClearsTheMalformedCount: one bad turn is ordinary. codex
// sends one unreadable reply, then a good one; its count is cleared, so a
// later slip earns a re-prompt again rather than its removal.
func TestAReadableReplyClearsTheMalformedCount(t *testing.T) {
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, delegateToCodex()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(ctx, task.ID) }()
	count := func() int {
		got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
		return got.MalformedCount["codex"]
	}

	waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
	codex.out <- ports.Outbound{AgentID: "codex", Err: errors.New(noRecipientErr)}
	waitFor(t, func() bool { return count() == 1 })
	codex.out <- ports.Outbound{AgentID: "codex", Message: domain.Message{
		ToAgent: "opus", Type: domain.MsgAnswer, Content: "The cookie is never cleared on logout."}}
	waitFor(t, func() bool { return count() == 0 })
	codex.out <- ports.Outbound{AgentID: "codex", Err: errors.New(noRecipientErr)}
	waitFor(t, func() bool { return count() == 1 })
	codex.finish()
	waitFor(t, func() bool {
		for _, m := range codex.deliveries()[1:] {
			if strings.Contains(m.Content, "could not be read") {
				return true
			}
		}
		return false
	})

	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State == domain.TaskPaused || got.State.IsTerminal() {
		t.Fatalf("the task must carry on: %s %s", got.State, got.PauseReason)
	}
	if h.hasEvent(domain.EvtRequestAbandoned) {
		t.Error("codex must not be dropped after one slip following a good reply")
	}
	cancel()
	<-done
}

// TestMaxDurationTripsWhenAgentsOnlySendUnreadableReplies: the duration limit
// was checked only after a routed message, so a task whose agents never sent
// a readable one ran on past it.
func TestMaxDurationTripsWhenAgentsOnlySendUnreadableReplies(t *testing.T) {
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, delegateToCodex()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	limits := defaultLimits()
	limits.MaxDuration = 200 * time.Millisecond
	h := newHarness(t, limits, 10*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(ctx, task.ID) }()

	waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
	time.Sleep(300 * time.Millisecond)
	codex.out <- ports.Outbound{AgentID: "codex", Err: errors.New(noRecipientErr)}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the run never stopped")
	}
	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused || got.PauseReason != domain.PauseReasonMaxDuration {
		t.Errorf("state = %s (%s), want PAUSED for MaxDuration", got.State, got.PauseReason)
	}
}
