package orchestrator_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/domain"
	"projectorch/internal/orchestrator"
	"projectorch/internal/ports"
)

// The limits meter autonomous agent work. These tests pin the rules the
// redesign introduced after a real run hit the round limit while the human
// was asking follow-up questions: human-facing messages are free, human input
// resets the allowance, rounds only trigger checkpoints, and a working agent
// is not an idle one.

func tmHarness(t *testing.T, limits domain.TaskLimits) (*harness, *domain.Task) {
	t.Helper()
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, limits, time.Second, gw)
	return h, createTask(t, h, "opus", "opus", "codex")
}

func TestMessagesToTheHumanDoNotSpendTheAllowance(t *testing.T) {
	h, task := tmHarness(t, defaultLimits())
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		_, _ = h.orch.Tasks.NoteMessage(ctx, task.ID,
			&domain.Message{FromAgent: "opus", ToAgent: domain.HumanAgentID, Type: domain.MsgStatusUpdate, Content: "status"}, orchestrator.NoProgress)
	}
	got, _ := h.orch.Tasks.Get(ctx, task.ID)
	if got.AgentRounds != 0 || got.AgentMessages != 0 || got.StallCount != 0 {
		t.Errorf("answering the human must cost nothing: rounds=%d messages=%d stall=%d",
			got.AgentRounds, got.AgentMessages, got.StallCount)
	}
	if got.MessageCount != 5 {
		t.Errorf("the life-of-task total still counts them: %d", got.MessageCount)
	}

	_, _ = h.orch.Tasks.NoteMessage(ctx, task.ID,
		&domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "look"}, orchestrator.MadeProgress)
	got, _ = h.orch.Tasks.Get(ctx, task.ID)
	if got.AgentRounds != 1 || got.AgentMessages != 1 {
		t.Errorf("an authority message to an agent is a round: rounds=%d messages=%d", got.AgentRounds, got.AgentMessages)
	}
}

func TestHumanInputStartsAFreshAllowance(t *testing.T) {
	h, task := tmHarness(t, defaultLimits())
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		_, _ = h.orch.Tasks.NoteMessage(ctx, task.ID,
			&domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgInformation, Content: "x"}, orchestrator.NoProgress)
	}
	got, _ := h.orch.Tasks.NoteHumanInput(ctx, task.ID)
	if got.AgentRounds != 0 || got.AgentMessages != 0 || got.StallCount != 0 {
		t.Errorf("after human input: rounds=%d messages=%d stall=%d", got.AgentRounds, got.AgentMessages, got.StallCount)
	}
	if got.Round != 4 {
		t.Errorf("the life-of-task round count is history and must stay at 4, got %d", got.Round)
	}
}

func TestRoundsAreNeverAHardLimit(t *testing.T) {
	tk := &domain.Task{Limits: domain.TaskLimits{MaxDeliberationRounds: 3}, AgentRounds: 50, CreatedAt: time.Now(), SegmentStart: time.Now()}
	if reason, over := orchestrator.LimitExceeded(tk); over {
		t.Errorf("50 rounds against an interval of 3 must not pause a task, got %q", reason)
	}
	if !orchestrator.CheckpointDue(tk) {
		t.Error("it should, however, be due a checkpoint")
	}
	tk.LastCheckpoint = 49
	if orchestrator.CheckpointDue(tk) {
		t.Error("one round after the last checkpoint is not another checkpoint")
	}
}

func TestTimeIsMeasuredFromTheLastHumanInput(t *testing.T) {
	tk := &domain.Task{
		Limits:       domain.TaskLimits{MaxDuration: 30 * time.Minute},
		CreatedAt:    time.Now().Add(-3 * time.Hour), // a follow-up hours later
		SegmentStart: time.Now(),
	}
	if reason, over := orchestrator.LimitExceeded(tk); over {
		t.Errorf("a task created hours ago but resumed just now must not pause: %q", reason)
	}
	tk.SegmentStart = time.Now().Add(-31 * time.Minute)
	if _, over := orchestrator.LimitExceeded(tk); !over {
		t.Error("31 minutes of unattended agent work against a 30-minute limit must pause")
	}
}

func TestTheAbsoluteCeilingIgnoresHumanInput(t *testing.T) {
	tk := &domain.Task{Limits: domain.TaskLimits{MaxTotalMessages: 100}, MessageCount: 100,
		CreatedAt: time.Now(), SegmentStart: time.Now()}
	if reason, over := orchestrator.LimitExceeded(tk); !over || reason != domain.PauseReasonMaxTotal {
		t.Errorf("the life-of-task ceiling must hold regardless of resets, got %q %v", reason, over)
	}
}

func TestResumeAfterALimitDoesNotImmediatelyRePause(t *testing.T) {
	h, task := tmHarness(t, defaultLimits())
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		_, _ = h.orch.Tasks.NoteMessage(ctx, task.ID,
			&domain.Message{FromAgent: "codex", ToAgent: "opus", Type: domain.MsgInformation, Content: "same"}, orchestrator.NoProgress)
	}
	if _, err := h.orch.Tasks.Pause(ctx, task.ID, domain.HumanAgentID, domain.PauseReasonStalled); err != nil {
		t.Fatal(err)
	}
	got, err := h.orch.Tasks.Resume(ctx, task.ID, domain.HumanAgentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.StallCount != 0 || got.AgentMessages != 0 {
		t.Errorf("resuming must grant a fresh allowance, still stall=%d messages=%d", got.StallCount, got.AgentMessages)
	}
}

func TestACheckpointNotifiesAndWorkContinues(t *testing.T) {
	limits := defaultLimits()
	limits.MaxDeliberationRounds = 1 // a checkpoint every round
	h, task := tmHarness(t, limits)

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run: %v", err)
	}
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if final.State != domain.TaskCompleted {
		t.Fatalf("checkpoints must not stop the work: task ended %s (%s)", final.State, final.PauseReason)
	}
	if !h.hasEvent(domain.EvtCheckpoint) {
		t.Error("each interval must be announced with a Checkpoint event")
	}
	msgs, _ := h.store.Messages.ListByTask(context.Background(), task.ID)
	asked := false
	for _, m := range msgs {
		asked = asked || (m.Metadata["orchestrator"] == "checkpoint" && strings.Contains(m.Content, "STATUS_UPDATE"))
	}
	if !asked {
		t.Error("the authority must be asked for a progress update at a checkpoint")
	}
}

// slowAgent answers after a delay, streaming activity meanwhile when chatty.
// It stands in for a real CLI working on a long turn.
type slowAgent struct {
	id     domain.AgentID
	delay  time.Duration
	chatty bool

	mu       sync.Mutex
	state    domain.AgentState
	out      chan ports.Outbound
	activity chan ports.AgentActivity
}

func newSlowAgent(id domain.AgentID, delay time.Duration, chatty bool) *slowAgent {
	return &slowAgent{id: id, delay: delay, chatty: chatty, state: domain.AgentStateStopped,
		activity: make(chan ports.AgentActivity, 256)}
}

func (s *slowAgent) ID() domain.AgentID     { return s.id }
func (s *slowAgent) Kind() domain.AgentKind { return domain.AgentKindMock }
func (s *slowAgent) Start(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.out = make(chan ports.Outbound, 16)
	s.state = domain.AgentStateIdle
	return nil
}
func (s *slowAgent) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != domain.AgentStateStopped {
		s.state = domain.AgentStateStopped
		close(s.out)
	}
	return nil
}
func (s *slowAgent) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return s.SendMessage(ctx, t, m)
}
func (s *slowAgent) SendMessage(_ context.Context, t *domain.Task, m domain.Message) error {
	var reply domain.MessageType
	switch m.Type {
	case domain.MsgDelegate:
		reply = domain.MsgAnswer
	case domain.MsgImplement, domain.MsgRequestChange:
		reply = domain.MsgImplementationResult
	default:
		return nil
	}
	s.mu.Lock()
	s.state = domain.AgentStateWorking
	out := s.out
	s.mu.Unlock()
	go func() {
		deadline := time.Now().Add(s.delay)
		for time.Now().Before(deadline) {
			if s.chatty {
				select {
				case s.activity <- ports.AgentActivity{AgentID: s.id, TaskID: t.ID, Kind: ports.ActivityText, Text: "working"}:
				default:
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.state == domain.AgentStateStopped {
			return
		}
		s.state = domain.AgentStateIdle
		out <- ports.Outbound{AgentID: s.id, Message: domain.Message{
			ToAgent: m.FromAgent, Type: reply, InReplyTo: m.ID,
			Content: "result after a long turn " + string(m.ID),
		}}
	}()
	return nil
}
func (s *slowAgent) Interrupt(context.Context) error      { return nil }
func (s *slowAgent) Status() domain.AgentState            { s.mu.Lock(); defer s.mu.Unlock(); return s.state }
func (s *slowAgent) Outbound() <-chan ports.Outbound      { return s.out }
func (s *slowAgent) Activity() <-chan ports.AgentActivity { return s.activity }

func runWithSlowImplementer(t *testing.T, chatty bool) *domain.Task {
	t.Helper()
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(newSlowAgent("codex", 700*time.Millisecond, chatty), domain.RoleImplementer)
	// Each turn takes 700ms; the idle timeout is 300ms.
	h := newHarness(t, defaultLimits(), 300*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	return final
}

// TestAWorkingAgentIsNotIdle: the idle timer used to reset only when a message
// arrived, so a long CLI turn that was visibly working paused the task as
// "no agent activity".
func TestAWorkingAgentIsNotIdle(t *testing.T) {
	if final := runWithSlowImplementer(t, true); final.State != domain.TaskCompleted {
		t.Errorf("an agent streaming activity through a long turn must not time out: %s (%s)", final.State, final.PauseReason)
	}
}

func TestASilentAgentStillTimesOut(t *testing.T) {
	// The control for the test above: the same long turn with no activity is
	// a genuinely silent agent, and must still be caught.
	final := runWithSlowImplementer(t, false)
	if final.State != domain.TaskPaused {
		t.Fatalf("a silent agent must still be paused by the idle timeout: %s (%s)", final.State, final.PauseReason)
	}
	// And the pause names the reply it is waiting for, not just "TIMEOUT".
	if !strings.Contains(final.PauseReason, "waiting on codex to answer opus") {
		t.Errorf("the pause should name the missing reply, got %q", final.PauseReason)
	}
}

// followUpRun completes a task, reopens it with a follow-up to opus, and runs
// it again. answers says whether opus replies to the human.
func followUpRun(t *testing.T, answers bool) *domain.Task {
	t.Helper()
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetFollowUpSettleInterval(2 * time.Second) })

	base := mockagent.AuthorityBehavior()
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if answers && in.FromAgent == domain.HumanAgentID && in.Type == domain.MsgInformation {
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer,
				Content: "The dedupe table is redundant once claims are made in the transaction."}}
		}
		return base(self, tk, in)
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 500*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	decisions, _ := h.store.Decisions.ListByTask(ctx, task.ID)

	if err := h.orch.Reopen(ctx, task.ID, "opus", "Why not a dedupe table?"); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	after, _ := h.store.Decisions.ListByTask(ctx, task.ID)
	if len(after) != len(decisions) {
		t.Errorf("answering a follow-up must not create or remove decisions: %d -> %d", len(decisions), len(after))
	}
	return final
}

// TestAnAnsweredFollowUpCompletesOnItsOwn: a follow-up has no FINAL_DECISION
// to end it. Without the settle check the agents kept talking after the
// human's question was answered, until a limit paused them.
func TestAnAnsweredFollowUpCompletesOnItsOwn(t *testing.T) {
	final := followUpRun(t, true)
	if final.State != domain.TaskCompleted || final.FollowUp {
		t.Errorf("once the follow-up is answered the task completes again: state=%s followUp=%v (%s)",
			final.State, final.FollowUp, final.PauseReason)
	}
}

func TestAnUnansweredFollowUpDoesNotComplete(t *testing.T) {
	if final := followUpRun(t, false); final.State == domain.TaskCompleted {
		t.Error("a follow-up nobody has answered must not complete")
	}
}

// TestSpendingIsRecordedAndTheBudgetEnforced: usage arrives as its own
// outbound after every CLI turn; past the budget the task pauses.
func TestSpendingIsRecordedAndTheBudgetEnforced(t *testing.T) {
	limits := defaultLimits()
	limits.MaxCostUSD = 1.00
	gw := newGateway()
	spender := newUsageAgent("codex", ports.Usage{InputTokens: 1000, OutputTokens: 200, CostUSD: 0.60})
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(spender, domain.RoleImplementer)
	h := newHarness(t, limits, time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)

	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if final.CostUSD < 1.00 || final.InputTokens < 2000 {
		t.Errorf("spending must be summed over turns: $%.2f, %d tokens in", final.CostUSD, final.InputTokens)
	}
	if final.State != domain.TaskPaused || !strings.HasPrefix(final.PauseReason, domain.PauseReasonBudget) {
		t.Errorf("past the budget the task must pause: %s (%s)", final.State, final.PauseReason)
	}
	codex := final.AgentUsage["codex"]
	if codex.Turns < 2 || codex.CostUSD != final.CostUSD || codex.InputTokens != final.InputTokens {
		t.Errorf("the spend must be broken down by agent: %+v (task $%.2f)", codex, final.CostUSD)
	}
	if s := h.eventSummary(domain.EvtAgentUsage); !strings.Contains(s, "codex turn: $0.6000, 1000 in / 200 out") {
		t.Errorf("each turn's usage must be logged, got %q", s)
	}
}

// usageAgent answers like the slow agent, instantly, and reports a fixed
// usage after every turn.
type usageAgent struct {
	*slowAgent
	usage ports.Usage
}

func newUsageAgent(id domain.AgentID, u ports.Usage) *usageAgent {
	return &usageAgent{slowAgent: newSlowAgent(id, 0, false), usage: u}
}

func (u *usageAgent) SendMessage(ctx context.Context, t *domain.Task, m domain.Message) error {
	u.mu.Lock()
	out := u.out
	u.mu.Unlock()
	usage := u.usage
	out <- ports.Outbound{AgentID: u.id, Usage: &usage}
	return u.slowAgent.SendMessage(ctx, t, m)
}

func (u *usageAgent) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return u.SendMessage(ctx, t, m)
}

// TestAFollowUpThatHandsOutWorkWaitsForAFinalDecision: in a real run the
// human's follow-up asked the agents to build something together. The
// authority broadcast the assignment, which opens no request, and the agents
// reported progress without asking each other anything -- so the moment
// nobody was mid-turn, the follow-up was taken as answered and the task
// completed in the middle of the work.
func TestAFollowUpThatHandsOutWorkWaitsForAFinalDecision(t *testing.T) {
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetFollowUpSettleInterval(2 * time.Second) })

	base := mockagent.AuthorityBehavior()
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		switch {
		case in.Metadata[ports.FollowUpMetadataKey] != "":
			return []domain.Message{
				{ToAgent: domain.BroadcastAgentID, Type: domain.MsgTaskAssignment,
					Content: "New task: implement the TTL cache together."},
				{ToAgent: domain.HumanAgentID, Type: domain.MsgStatusUpdate,
					Content: "I've sent the cache task to codex."},
			}
		case tk.FollowUp:
			return nil // progress reports need no reply
		}
		return base(self, tk, in)
	}
	implementer := mockagent.ImplementerBehavior()
	codex := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if tk.FollowUp && in.Type == domain.MsgTaskAssignment {
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgStatusUpdate,
				Content: "Drafting erase and setCapacity now."}}
		}
		return implementer(self, tk, in)
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, codex), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if err := h.orch.Reopen(ctx, task.ID, "opus", "Now have them build the cache together."); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 700*time.Millisecond)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	if final.State == domain.TaskCompleted {
		t.Fatal("a follow-up that handed out work completed before any FINAL_DECISION")
	}
	if !final.FollowUpWork {
		t.Error("the authority's assignment should mark the follow-up as work")
	}
}

// TestResumingAPausedTaskRestartsTheConversation: a pause stops the agent
// processes, so the turn codex was taking is lost. Resuming used to restart
// nothing -- every agent sat idle -- and a follow-up was completed on the spot
// because nobody was working. Now the agent that owes a reply is asked again.
func TestResumingAPausedTaskRestartsTheConversation(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(newSlowAgent("codex", 10*time.Minute, false), domain.RoleImplementer) // never finishes its turn
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()

	done := make(chan struct{})
	go func() { _ = h.orch.RunTask(ctx, task.ID); close(done) }()
	waitFor(t, func() bool {
		msgs, _ := h.store.Messages.ListByTask(ctx, task.ID)
		for _, m := range msgs {
			if m.ToAgent == "codex" {
				return true
			}
		}
		return false
	})
	if err := h.orch.Pause(ctx, task.ID, domain.PauseReasonStalled); err != nil {
		t.Fatal(err)
	}
	<-done

	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	msgs, _ := h.store.Messages.ListByTask(ctx, task.ID)
	var nudge *domain.Message
	for _, m := range msgs {
		if m.ToAgent == "codex" && m.Metadata["orchestrator"] == "resumed" {
			nudge = m
		}
	}
	if nudge == nil {
		t.Fatal("resuming must ask codex again for the reply it was working on")
	}
	if !strings.Contains(nudge.Content, "You still owe a reply") || !strings.Contains(nudge.Content, "STALLED") {
		t.Errorf("the nudge should say why the task paused and what is owed: %q", nudge.Content)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
