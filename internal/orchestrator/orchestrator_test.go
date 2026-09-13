package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/decisions"
	"projectorch/internal/domain"
	"projectorch/internal/knowledge"
	"projectorch/internal/logging"
	"projectorch/internal/orchestrator"
	"projectorch/internal/ports"
	"projectorch/internal/storage"
)

// testGateway is a minimal AgentGateway over a set of adapters. The
// orchestrator is tested against the interface, not against the concrete agent
// manager, so a failure here is unambiguously an orchestration failure.
type testGateway struct {
	adapters map[domain.AgentID]ports.AgentAdapter
	roles    map[domain.AgentID]domain.AgentRole
	states   map[domain.AgentID]domain.AgentState
	// onSend, when set, sees every delivery exactly as the adapter gets it.
	onSend func(m domain.Message)
}

func newGateway() *testGateway {
	return &testGateway{
		adapters: map[domain.AgentID]ports.AgentAdapter{},
		roles:    map[domain.AgentID]domain.AgentRole{},
		states:   map[domain.AgentID]domain.AgentState{},
	}
}

func (g *testGateway) add(ad ports.AgentAdapter, role domain.AgentRole) {
	g.adapters[ad.ID()] = ad
	g.roles[ad.ID()] = role
}

func (g *testGateway) StartFor(ctx context.Context, _ domain.TaskID, id domain.AgentID) error {
	return g.adapters[id].Start(ctx)
}

func (g *testGateway) StopFor(ctx context.Context, _ domain.TaskID, id domain.AgentID) error {
	return g.adapters[id].Stop(ctx)
}

// Send forwards the task alongside the message. Supporting agents never
// receive an opening assignment, so the task record has to travel with every
// delivery or a delegated agent would not know what it is working on.
func (g *testGateway) Send(ctx context.Context, t *domain.Task, m domain.Message) error {
	if g.onSend != nil {
		g.onSend(m)
	}
	ad, ok := g.adapters[m.ToAgent]
	if !ok {
		return ports.ErrNotFound
	}
	return ad.SendMessage(ctx, t, m)
}

func (g *testGateway) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	if g.onSend != nil {
		g.onSend(m)
	}
	ad, ok := g.adapters[m.ToAgent]
	if !ok {
		return ports.ErrNotFound
	}
	return ad.SendTask(ctx, t, m)
}

func (g *testGateway) AdapterFor(_ domain.TaskID, id domain.AgentID) (ports.AgentAdapter, bool) {
	ad, ok := g.adapters[id]
	return ad, ok
}

func (g *testGateway) ReportOutboundFor(_ domain.TaskID, id domain.AgentID, out ports.Outbound) {
	if out.Err != nil {
		g.states[id] = domain.AgentStateError
	}
}

func (g *testGateway) Statuses() []domain.Agent {
	out := make([]domain.Agent, 0, len(g.adapters))
	for id, ad := range g.adapters {
		out = append(out, domain.Agent{ID: id, Role: g.roles[id], State: ad.Status()})
	}
	return out
}

type harness struct {
	orch   *orchestrator.Orchestrator
	store  ports.Storage
	gw     *testGateway
	events []domain.Event
}

func newHarness(t *testing.T, limits domain.TaskLimits, idle time.Duration, gw *testGateway) *harness {
	t.Helper()
	return newHarnessApproval(t, limits, idle, gw, false)
}

func newHarnessApproval(t *testing.T, limits domain.TaskLimits, idle time.Duration, gw *testGateway, requireApproval bool) *harness {
	t.Helper()
	store := storage.NewInMemory()
	h := &harness{store: *store, gw: gw}
	h.orch = orchestrator.New(orchestrator.Deps{
		Storage:            *store,
		Agents:             gw,
		Knowledge:          knowledge.New(store.Knowledge),
		Decisions:          decisions.New(store.Decisions),
		Logger:             logging.NewDiscard(),
		Limits:             limits,
		MaxConcurrentTasks: 2,
		IdleTimeout:        idle,

		RequireHumanApproval: requireApproval,
	})
	h.orch.Events.Subscribe(func(e domain.Event) { h.events = append(h.events, e) })
	return h
}

func (h *harness) hasEvent(tp domain.EventType) bool {
	for _, e := range h.events {
		if e.Type == tp {
			return true
		}
	}
	return false
}

func (h *harness) eventSummary(tp domain.EventType) string {
	for _, e := range h.events {
		if e.Type == tp {
			return e.Summary
		}
	}
	return ""
}

func defaultLimits() domain.TaskLimits {
	return domain.TaskLimits{MaxDeliberationRounds: 20, MaxMessages: 100, MaxDuration: time.Minute}
}

func createTask(t *testing.T, h *harness, authority domain.AgentID, participants ...domain.AgentID) *domain.Task {
	t.Helper()
	task, err := h.orch.CreateTask(context.Background(), ports.CreateTaskInput{
		Title:        "Fix the authentication cookie bug",
		Description:  "Logged-out users keep an authenticated session cookie.",
		Authority:    authority,
		Participants: participants,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return task
}

// TestEndToEndMockFlow is the first complete demonstration of the
// architecture: a human creates a task, the authority agent delegates, the
// implementer answers, the authority requests implementation, reviews it, asks
// for a change, and finally decides. No LLM is involved.
func TestEndToEndMockFlow(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskCompleted {
		t.Fatalf("task state = %s, want COMPLETED (pause reason %q, failure %q)",
			final.State, final.PauseReason, final.FailureReason)
	}

	msgs, err := h.store.Messages.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var got []domain.MessageType
	for _, m := range msgs {
		got = append(got, m.Type)
	}
	// Exactly ONE TASK_ASSIGNMENT: only the authority is briefed. Supporting
	// agents are left idle and act only once they are delegated to, so the
	// authority decides what work happens rather than every agent starting on
	// its own reading of the task.
	want := []domain.MessageType{
		domain.MsgTaskAssignment,
		domain.MsgDelegate,
		domain.MsgAnswer,
		domain.MsgImplement,
		domain.MsgImplementationResult,
		domain.MsgRequestChange,
		domain.MsgImplementationResult,
		domain.MsgFinalDecision,
	}
	if len(got) != len(want) {
		t.Fatalf("message sequence = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("message[%d] = %s, want %s (full: %v)", i, got[i], want[i], got)
		}
	}

	ds, err := h.store.Decisions.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Status != domain.DecisionAccepted {
		t.Fatalf("expected one ACCEPTED decision, got %+v", ds)
	}
	if ds[0].Author != "opus" {
		t.Errorf("decision author = %s, want opus", ds[0].Author)
	}

	for _, want := range []domain.EventType{
		domain.EvtTaskCreated, domain.EvtAgentDelegatedTask, domain.EvtAgentMessageSent,
		domain.EvtAgentMessageReceived, domain.EvtDecisionCreated, domain.EvtDecisionAccepted,
		domain.EvtTaskCompleted,
	} {
		if !h.hasEvent(want) {
			t.Errorf("missing event %s", want)
		}
	}

	// The investigation finding must land in shared knowledge, not only in the
	// conversation: the project state is the source of truth.
	hits, err := h.store.Knowledge.Search(context.Background(), domain.KnowledgeQuery{Text: "Path=/api"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("expected the investigation finding to be searchable in shared knowledge")
	}
}

// TestOnlyTheAuthorityIsBriefed is a regression test taken from a live run
// with real agents. Every participant used to receive the opening
// TASK_ASSIGNMENT, so a supporting agent started working on its own reading of
// the task, answered unprompted, and advanced task state before the authority
// had decided what investigation was even needed.
func TestOnlyTheAuthorityIsBriefed(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	gw.add(codex, domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 150*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	msgs, err := h.store.Messages.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var assignments []domain.AgentID
	for _, m := range msgs {
		if m.Type == domain.MsgTaskAssignment {
			assignments = append(assignments, m.ToAgent)
		}
	}
	if len(assignments) != 1 {
		t.Fatalf("expected exactly one TASK_ASSIGNMENT, got %v", assignments)
	}
	if assignments[0] != "opus" {
		t.Fatalf("the assignment went to %s, want the authority agent", assignments[0])
	}
	// The supporting agent must not have been contacted at all.
	if sent := codex.Sent(); len(sent) != 0 {
		t.Fatalf("the supporting agent should be idle until delegated to, got %+v", sent)
	}
}

// TestAuthorityViolationIsRejected proves authority is enforced in code: a
// supporting agent cannot end the task by asserting a FINAL_DECISION, and it
// cannot address an agent that is not a participant.
func TestAuthorityViolationIsRejected(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.RogueBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 300*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	_ = h.orch.RunTask(context.Background(), task.ID)

	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State == domain.TaskCompleted {
		t.Fatal("a supporting agent must not be able to complete the task")
	}
	if !h.hasEvent(domain.EvtAuthorityViolation) {
		t.Fatal("expected an AUTHORITY_VIOLATION event")
	}
	if s := h.eventSummary(domain.EvtAuthorityViolation); !strings.Contains(s, "FINAL_DECISION") {
		t.Errorf("violation summary = %q, expected it to name the attempted action", s)
	}

	ds, err := h.store.Decisions.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 0 {
		t.Fatalf("a rejected FINAL_DECISION must not create a decision, got %+v", ds)
	}
}

// TestChallengeLoopIsPausedAsAStall covers both the disagreement flow and
// loop protection: the implementer repeats the same challenge and the
// authority the same delegation, so the task never converges. It is paused
// for a human -- as a stall, not a round limit, since repeating yourself is
// what makes it a loop -- instead of a winner being declared.
func TestChallengeLoopIsPausedAsAStall(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ChallengingImplementerBehavior()), domain.RoleImplementer)

	limits := domain.TaskLimits{MaxDeliberationRounds: 3, MaxMessages: 50, MaxDuration: time.Minute, StallMessages: 4}
	h := newHarness(t, limits, 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskPaused {
		t.Fatalf("task state = %s, want PAUSED", final.State)
	}
	if final.PauseReason != domain.PauseReasonStalled {
		t.Errorf("pause reason = %q, want %q", final.PauseReason, domain.PauseReasonStalled)
	}
	if !h.hasEvent(domain.EvtAgentChallenged) {
		t.Error("expected the disagreement to be recorded as an event")
	}
	if !h.hasEvent(domain.EvtLimitReached) {
		t.Error("expected a LimitReached event")
	}
	if h.hasEvent(domain.EvtTaskCompleted) {
		t.Error("hitting a limit must never complete the task")
	}

	// The conflict must be recorded explicitly rather than smoothed away.
	dis, err := h.store.Disagreements.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dis) == 0 {
		t.Fatal("expected the challenge to be recorded as a disagreement")
	}
	d := dis[0]
	if d.Challenger != "codex" || d.Claimant != "opus" {
		t.Errorf("disagreement parties = challenger %s / claimant %s, want codex / opus",
			d.Challenger, d.Claimant)
	}
	if !strings.Contains(d.Counterclaim, "not the root cause") {
		t.Errorf("counterclaim = %q, expected the challenger's position", d.Counterclaim)
	}
	if d.Claim == "" {
		t.Error("expected the disputed claim to be captured alongside the counterclaim")
	}
	if d.Resolved {
		t.Error("an unresolved disagreement must not be marked resolved")
	}
}

// TestFinalDecisionResolvesOpenDisagreements checks that the authority's
// decision settles recorded conflicts in place, without deleting them.
func TestFinalDecisionResolvesOpenDisagreements(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior())
	gw.add(codex, domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()

	// Seed an open disagreement, then let the normal flow reach a decision.
	if err := h.store.Disagreements.Create(ctx, &domain.Disagreement{
		ID: domain.NewDisagreementID(), TaskID: task.ID,
		Claimant: "opus", Challenger: "codex",
		Claim: "the cookie path is wrong", Counterclaim: "the path is fine",
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	dis, err := h.store.Disagreements.ListByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(dis) != 1 {
		t.Fatalf("expected the disagreement to be kept, got %d", len(dis))
	}
	if !dis[0].Resolved {
		t.Fatal("the final decision should resolve open disagreements")
	}
	if dis[0].ResolvedBy != "opus" {
		t.Errorf("resolved by %s, want opus", dis[0].ResolvedBy)
	}
	if dis[0].Counterclaim != "the path is fine" {
		t.Error("resolving must not erase the counterclaim")
	}
}

// TestAgentsCannotActOnAStoppedTask proves an agent cannot keep working on a
// task the human has paused or cancelled.
func TestAgentsCannotActOnAStoppedTask(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	msg := &domain.Message{FromAgent: "codex", ToAgent: "opus", Type: domain.MsgAnswer, Content: "still working"}

	for _, state := range []domain.TaskState{domain.TaskPaused, domain.TaskCancelled, domain.TaskCompleted} {
		task := &domain.Task{
			ID: "TASK-001", AuthorityAgent: "opus", State: state,
			Participants: []domain.AgentID{"opus", "codex"},
		}
		err := auth.Authorize(task, msg)
		if err == nil {
			t.Fatalf("an agent must not act on a %s task", state)
		}
		var v *orchestrator.Violation
		if !errors.As(err, &v) || v.Kind != orchestrator.ViolationState {
			t.Fatalf("state = %s: error = %v, want a STATE_VIOLATION", state, err)
		}
	}

	// The human is never locked out by task state.
	task := &domain.Task{
		ID: "TASK-001", AuthorityAgent: "opus", State: domain.TaskPaused,
		Participants: []domain.AgentID{"opus", "codex"},
	}
	human := &domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "opus", Type: domain.MsgInformation}
	if err := auth.Authorize(task, human); err != nil {
		t.Fatalf("the human must always be able to intervene: %v", err)
	}
}

// TestAgentFailureFailsTaskExplicitly proves a crashed agent produces an
// explicit failure rather than a silently vanished task.
func TestAgentFailureFailsTaskExplicitly(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.NewCrashing("codex"), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	_ = h.orch.RunTask(context.Background(), task.ID)

	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskFailed {
		t.Fatalf("task state = %s, want FAILED", final.State)
	}
	if !strings.Contains(final.FailureReason, "AGENT_ERROR") {
		t.Errorf("failure reason = %q, want it to name AGENT_ERROR", final.FailureReason)
	}
	if !h.hasEvent(domain.EvtAgentFailed) {
		t.Error("expected an AgentFailed event")
	}
}

// TestIdleAgentPausesForHuman covers the hung-agent case.
func TestIdleAgentPausesForHuman(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 150*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}
	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskPaused {
		t.Fatalf("task state = %s, want PAUSED", final.State)
	}
	if !strings.Contains(final.PauseReason, "TIMEOUT") {
		t.Errorf("pause reason = %q, want a TIMEOUT", final.PauseReason)
	}
}

// TestHumanCancelStopsTheRun proves the human can always intervene, whichever
// agent holds task authority.
func TestHumanCancelStopsTheRun(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(context.Background(), task.ID) }()

	// Give the run loop a moment to reach its select before cancelling.
	time.Sleep(50 * time.Millisecond)
	if err := h.orch.Cancel(context.Background(), task.ID, "human changed their mind"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("RunTask did not return after cancellation")
	}

	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskCancelled {
		t.Fatalf("task state = %s, want CANCELLED", final.State)
	}
	if !h.hasEvent(domain.EvtHumanOverride) {
		t.Error("expected a HumanOverride event")
	}
}

// TestPauseResumeRestoresPreviousState covers the human pause/resume path.
func TestPauseResumeRestoresPreviousState(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()

	if _, err := h.orch.Tasks.Transition(ctx, task.ID, domain.TaskPlanning, "opus", "start"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.orch.Tasks.Transition(ctx, task.ID, domain.TaskInvestigating, "opus", "investigate"); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.Pause(ctx, task.ID, "human"); err != nil {
		t.Fatal(err)
	}
	paused, _ := h.store.Tasks.Get(ctx, task.ID)
	if paused.State != domain.TaskPaused {
		t.Fatalf("state = %s, want PAUSED", paused.State)
	}
	if _, err := h.orch.Tasks.Resume(ctx, task.ID, domain.HumanAgentID); err != nil {
		t.Fatal(err)
	}
	resumed, _ := h.store.Tasks.Get(ctx, task.ID)
	if resumed.State != domain.TaskInvestigating {
		t.Fatalf("resumed state = %s, want INVESTIGATING", resumed.State)
	}
}

// TestInvalidTransitionRejected proves the state machine is enforced.
func TestInvalidTransitionRejected(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	_, err := h.orch.Tasks.Transition(context.Background(), task.ID, domain.TaskCompleted, "opus", "shortcut")
	if err == nil {
		t.Fatal("CREATED -> COMPLETED must be rejected")
	}
	var invalid domain.ErrInvalidTransition
	if !asInvalid(err, &invalid) {
		t.Fatalf("error = %v, want ErrInvalidTransition", err)
	}
}

func asInvalid(err error, target *domain.ErrInvalidTransition) bool {
	if e, ok := err.(domain.ErrInvalidTransition); ok {
		*target = e
		return true
	}
	return false
}

// TestTaskCreationRequiresTitleAndAuthority covers input validation.
func TestTaskCreationRequiresTitleAndAuthority(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	h := newHarness(t, defaultLimits(), time.Second, gw)
	ctx := context.Background()

	if _, err := h.orch.CreateTask(ctx, ports.CreateTaskInput{Authority: "opus"}); err == nil {
		t.Error("a task without a title must be rejected")
	}
	if _, err := h.orch.CreateTask(ctx, ports.CreateTaskInput{Title: "x"}); err == nil {
		t.Error("a task without an authority agent must be rejected")
	}
}

// decideImmediatelyBehavior answers the opening assignment with a
// FINAL_DECISION, modelling the real case where the authority reaches a
// conclusion in a single round without any implementation or review.
func decideImmediatelyBehavior() mockagent.Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		if in.Type != domain.MsgTaskAssignment {
			return nil
		}
		return []domain.Message{{
			Type:    domain.MsgFinalDecision,
			ToAgent: domain.BroadcastAgentID,
			Content: "No code change is needed; the answer is in the transition table.",
		}}
	}
}

// TestDecisionFromAnyWorkingStateCompletes is a regression test taken from a
// live run with real agents. The authority delegated, got its answer, and
// decided while the task was still INVESTIGATING. COMPLETED is only reachable
// from AWAITING_DECISION, so the run recorded and accepted the decision and
// then failed with "invalid task transition INVESTIGATING -> COMPLETED",
// stranding a finished task.
func TestDecisionFromAnyWorkingStateCompletes(t *testing.T) {
	for _, from := range []domain.TaskState{
		domain.TaskPlanning, domain.TaskInvestigating,
		domain.TaskImplementing, domain.TaskReviewing, domain.TaskDeliberating,
	} {
		t.Run(string(from), func(t *testing.T) {
			if !domain.CanTransition(from, domain.TaskAwaitingDecision) {
				t.Fatalf("the authority must be able to decide from %s", from)
			}
		})
	}
}

// TestImmediateFinalDecisionCompletesTheTask drives the same case through the
// real run loop rather than only the transition table.
func TestImmediateFinalDecisionCompletesTheTask(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, decideImmediatelyBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}
	final, err := h.store.Tasks.Get(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskCompleted {
		t.Fatalf("task state = %s, want COMPLETED", final.State)
	}
	ds, err := h.store.Decisions.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Status != domain.DecisionAccepted {
		t.Fatalf("expected one ACCEPTED decision, got %+v", ds)
	}
}

// askThenDecideBehavior asks a question that requires a response and then
// decides without waiting for it — observed verbatim in a live run, where the
// authority asked a QUESTION and issued its FINAL_DECISION five seconds later,
// well before any real CLI could have answered.
func askThenDecideBehavior(counterpart domain.AgentID) mockagent.Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		if in.Type != domain.MsgTaskAssignment {
			return nil
		}
		return []domain.Message{
			{
				Type:             domain.MsgQuestion,
				ToAgent:          counterpart,
				Content:          "Which file owns the transition table?",
				RequiresResponse: true,
			},
			{
				Type:    domain.MsgFinalDecision,
				ToAgent: domain.BroadcastAgentID,
				Content: "Decided without waiting.",
			},
		}
	}
}

// TestDecisionWithUnansweredQuestionIsRecorded proves an unanswered request is
// surfaced rather than silently folded into an accepted decision. The
// authority is still allowed to decide — judging the evidence sufficient is
// the content of the role — but the caveat becomes part of the record.
func TestDecisionWithUnansweredQuestionIsRecorded(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, askThenDecideBehavior("codex")), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	ds, err := h.store.Decisions.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 {
		t.Fatalf("expected one decision, got %d", len(ds))
	}

	var caveat string
	for _, e := range ds[0].Evidence {
		if strings.HasPrefix(e, "CAVEAT:") {
			caveat = e
		}
	}
	if caveat == "" {
		t.Fatalf("the unanswered QUESTION must be recorded on the decision, got evidence %v", ds[0].Evidence)
	}
	if !strings.Contains(caveat, "QUESTION") || !strings.Contains(caveat, "codex") {
		t.Errorf("caveat should name the outstanding request and its recipient, got %q", caveat)
	}

	// The caveat is logged as a caveat. It used to be logged as
	// ValidationRejected, which told the reader a decision that went through
	// had been refused.
	evs, err := h.store.Events.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var caveats int
	for _, e := range evs {
		if e.Details["kind"] != "UNANSWERED_REQUEST" {
			continue
		}
		if e.Type != domain.EvtDecisionCaveat {
			t.Errorf("unanswered-request notice logged as %s, want %s", e.Type, domain.EvtDecisionCaveat)
		}
		caveats++
	}
	if caveats != 1 {
		t.Errorf("want exactly one DecisionCaveat event, got %d", caveats)
	}
}

// TestAnsweredQuestionsProduceNoCaveat guards against the warning firing on
// every healthy task.
func TestAnsweredQuestionsProduceNoCaveat(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}
	ds, err := h.store.Decisions.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ds[0].Evidence {
		if strings.HasPrefix(e, "CAVEAT:") {
			t.Errorf("a fully answered deliberation must not carry a caveat, got %q", e)
		}
	}
}

// TestAuthorityOnlyTaskIsRejected is a regression test. An authority-only task
// was previously accepted with a warning, and then stalled: the authority had
// nobody to delegate to, emitted a recipient-less DELEGATE that the bus
// rejected, and the run idled until the timeout paused it. Refusing at
// creation puts the error in front of the human instead.
func TestAuthorityOnlyTaskIsRejected(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	h := newHarness(t, defaultLimits(), time.Second, gw)

	_, err := h.orch.CreateTask(context.Background(), ports.CreateTaskInput{
		Title: "test", Authority: "opus", Participants: []domain.AgentID{"opus"},
	})
	if err == nil {
		t.Fatal("a task with only the authority agent must be rejected")
	}
	if !strings.Contains(err.Error(), "supporting agent") {
		t.Errorf("error = %v, expected it to explain that a supporting agent is needed", err)
	}
}

// TestDuplicateParticipantIsRejected guards against assigning one agent the
// same task twice.
func TestDuplicateParticipantIsRejected(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Second, gw)

	_, err := h.orch.CreateTask(context.Background(), ports.CreateTaskInput{
		Title: "test", Authority: "opus",
		Participants: []domain.AgentID{"opus", "codex", "opus"},
	})
	if err == nil {
		t.Fatal("a duplicated participant must be rejected")
	}
}

// TestAuthorityWithNoCounterpartStaysSilent checks the mock does not emit a
// message with no recipient even if it is somehow left without a counterpart.
func TestAuthorityWithNoCounterpartStaysSilent(t *testing.T) {
	b := mockagent.AuthorityBehavior()
	solo := &domain.Task{ID: "TASK-001", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus"}}
	out := b("opus", solo, domain.Message{Type: domain.MsgTaskAssignment, FromAgent: domain.HumanAgentID})
	for _, m := range out {
		if m.ToAgent == "" {
			t.Fatalf("behavior emitted %s with no recipient", m.Type)
		}
	}
}

// TestRejectedActionIsReportedBackToTheAgent proves a refused action does not
// leave the agent in silence: it is told what was rejected and why, so it can
// correct itself instead of the task idling until the timeout.
func TestRejectedActionIsReportedBackToTheAgent(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	rogue := mockagent.New("codex", domain.AgentKindMock, mockagent.RogueBehavior())
	gw.add(rogue, domain.RoleImplementer)

	h := newHarness(t, defaultLimits(), 300*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	_ = h.orch.RunTask(context.Background(), task.ID)

	var errToRogue *domain.Message
	msgs, err := h.store.Messages.ListByTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Type == domain.MsgError && m.ToAgent == "codex" {
			errToRogue = m
			break
		}
	}
	if errToRogue == nil {
		t.Fatal("expected the rejected agent to be told its action was refused")
	}
	if !strings.Contains(errToRogue.Content, "FINAL_DECISION") {
		t.Errorf("rejection notice = %q, expected it to name the rejected action", errToRogue.Content)
	}
	if errToRogue.Metadata["violation"] != string(orchestrator.ViolationAuthority) {
		t.Errorf("violation metadata = %q, want AUTHORITY_VIOLATION", errToRogue.Metadata["violation"])
	}
}

// TestAuthorityIsAlwaysAParticipant guards the invariant that the authority
// agent takes part in its own task.
func TestAuthorityIsAlwaysAParticipant(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Second, gw)

	task, err := h.orch.CreateTask(context.Background(), ports.CreateTaskInput{
		Title: "x", Authority: "opus", Participants: []domain.AgentID{"codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !task.HasParticipant("opus") {
		t.Fatal("the authority agent must be a participant of its own task")
	}
}

// TestAutoAcceptedFinalDecisionDoesNotInvokeRecipients: a broadcast decision
// that completes the task used to start a paid CLI call in every recipient,
// killed seconds later when the task ended.
func TestAutoAcceptedFinalDecisionDoesNotInvokeRecipients(t *testing.T) {
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, decideImmediatelyBehavior()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)

	h := newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, false)
	task := createTask(t, h, "opus", "opus", "codex")
	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	for _, m := range codex.Sent() {
		if m.Type == domain.MsgFinalDecision {
			t.Fatal("recipient was invoked for an auto-accepted FINAL_DECISION")
		}
	}
	// It is still part of the record and still announced.
	history, err := h.orch.Bus.History(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, m := range history {
		recorded = recorded || m.Type == domain.MsgFinalDecision
	}
	if !recorded {
		t.Error("the FINAL_DECISION must remain in message history")
	}
	announced := false
	for _, e := range h.events {
		announced = announced || (e.Type == domain.EvtAgentMessageReceived &&
			strings.Contains(e.Summary, "codex received FINAL_DECISION"))
	}
	if !announced {
		t.Error("expected an AgentMessageReceived event for codex's FINAL_DECISION")
	}
}

// TestFinalDecisionStillReachesRecipientsWhenApprovalIsRequired: the task
// waits for the human, so a recipient may still legitimately CHALLENGE.
func TestFinalDecisionStillReachesRecipientsWhenApprovalIsRequired(t *testing.T) {
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, decideImmediatelyBehavior()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)

	h := newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, true)
	task := createTask(t, h, "opus", "opus", "codex")
	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	delivered := false
	for _, m := range codex.Sent() {
		delivered = delivered || m.Type == domain.MsgFinalDecision
	}
	if !delivered {
		t.Fatal("with human approval required, recipients must still receive the FINAL_DECISION")
	}
}

// TestDeliveriesCarryARecapOfTheTaskSoFar covers the transcript an agent gets
// when it has no memory of the task -- first contacted mid-way through, or
// with its CLI session lost. Each delivery must summarise what came before it
// and must not repeat the message being delivered.
func TestDeliveriesCarryARecapOfTheTaskSoFar(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)

	var mu sync.Mutex
	var delivered []domain.Message
	gw.onSend = func(m domain.Message) {
		mu.Lock()
		delivered = append(delivered, m)
		mu.Unlock()
	}

	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) < 2 {
		t.Fatalf("expected several deliveries, got %d", len(delivered))
	}
	later := delivered[len(delivered)-1]
	recap := later.Metadata[ports.RecapMetadataKey]
	if recap == "" {
		t.Fatalf("a later delivery must carry a recap of earlier messages; metadata = %v", later.Metadata)
	}
	if !strings.Contains(recap, string(delivered[0].Type)) {
		t.Errorf("the recap should mention the first exchange (%s):\n%s", delivered[0].Type, recap)
	}
	if strings.Contains(recap, string(later.ID)) || (len(later.Content) > 20 && strings.Contains(recap, later.Content)) {
		t.Errorf("the recap must not repeat the message being delivered:\n%s", recap)
	}
}

// completedTask runs a mock deliberation to completion and records every
// delivery made from then on.
func completedTask(t *testing.T) (*harness, *domain.Task, *[]domain.Message, *sync.Mutex) {
	t.Helper()
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	var mu sync.Mutex
	var delivered []domain.Message
	gw.onSend = func(m domain.Message) {
		mu.Lock()
		delivered = append(delivered, m)
		mu.Unlock()
	}
	h := newHarness(t, defaultLimits(), 500*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	if err := h.orch.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}
	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskCompleted {
		t.Fatalf("fixture task ended %s, want COMPLETED", got.State)
	}
	return h, task, &delivered, &mu
}

// TestInstructingAFinishedTaskIsRefusedWithAReason is a regression test. An
// instruction to a completed task used to be routed to stopped agents and
// logged as AgentFailed -- as though the agent had broken.
func TestInstructingAFinishedTaskIsRefusedWithAReason(t *testing.T) {
	h, task, _, _ := completedTask(t)

	err := h.orch.SendInstruction(context.Background(), task.ID, "opus", "one more question")
	if !errors.Is(err, orchestrator.ErrTaskNotRunning) || !strings.Contains(err.Error(), "follow-up") {
		t.Fatalf("want a refusal that says how to follow up, got %v", err)
	}
	evs, _ := h.store.Events.ListByTask(context.Background(), task.ID)
	for _, e := range evs {
		if e.Type == domain.EvtAgentFailed {
			t.Errorf("a refused instruction must not be logged as an agent failure: %s", e.Summary)
		}
	}
}

func TestAFollowUpReopensTheTaskAndReachesTheAgent(t *testing.T) {
	h, task, delivered, mu := completedTask(t)
	ctx := context.Background()

	if err := h.orch.Reopen(ctx, task.ID, "opus", "Why not a dedupe table?"); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	reopened, _ := h.orch.Tasks.Get(ctx, task.ID)
	if reopened.State != domain.TaskDeliberating || reopened.CompletedAt != nil {
		t.Fatalf("after reopen: state=%s completedAt=%v", reopened.State, reopened.CompletedAt)
	}

	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	mu.Lock()
	defer mu.Unlock()
	var followUp bool
	assignments := 0
	for _, m := range *delivered {
		if m.Type == domain.MsgTaskAssignment {
			assignments++
		}
		if m.FromAgent == domain.HumanAgentID && m.ToAgent == "opus" && m.Content == "Why not a dedupe table?" {
			followUp = true
		}
	}
	if !followUp {
		t.Error("the follow-up never reached opus")
	}
	if assignments != 1 {
		t.Errorf("the opening assignment was sent %d times; a reopened task must not be re-briefed from scratch", assignments)
	}

	evs, _ := h.store.Events.ListByTask(ctx, task.ID)
	var sawReopen bool
	for _, e := range evs {
		sawReopen = sawReopen || e.Type == domain.EvtTaskReopened
	}
	if !sawReopen {
		t.Error("reopening must be on the record as a TaskReopened event")
	}
}

func TestOnlyACompletedTaskCanBeReopened(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")

	err := h.orch.Reopen(context.Background(), task.ID, "opus", "?")
	if !errors.Is(err, orchestrator.ErrNotReopenable) {
		t.Errorf("reopening a %s task: got %v, want ErrNotReopenable", task.State, err)
	}
}

func TestTheRunLoopCannotLeaveACompletedState(t *testing.T) {
	// Reopen is the only exit, and it is not in the transition table, so
	// nothing an agent does can bring a finished task back.
	if domain.CanTransition(domain.TaskCompleted, domain.TaskDeliberating) {
		t.Error("COMPLETED -> DELIBERATING must not be an ordinary transition")
	}
}

// TestAFollowUpToATaskWhoseAgentsAreGoneIsRefused is a regression test: a
// follow-up to a task whose agent had been removed from the team reopened
// it, failed to start the agent, and left the task FAILED.
func TestAFollowUpToATaskWhoseAgentsAreGoneIsRefused(t *testing.T) {
	h, task, _, _ := completedTask(t)
	ctx := context.Background()
	delete(h.gw.adapters, "opus") // opus is no longer on the team

	err := h.orch.Reopen(ctx, task.ID, "opus", "one more question")
	if err == nil || !strings.Contains(err.Error(), "opus is no longer on the team") {
		t.Fatalf("want a refusal naming the missing agent, got %v", err)
	}
	if got, _ := h.orch.Tasks.Get(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Errorf("a refused follow-up must leave the task as it was, got %s", got.State)
	}
}

// TestARecapHoldsOnlyWhatTheRecipientWasPartyTo is a regression test: the
// recap given to an agent starting its session listed every message of the
// task, so a supporting agent read the human's brief to the authority and a
// private instruction the authority gave another agent.
func TestARecapHoldsOnlyWhatTheRecipientWasPartyTo(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	gw.add(mockagent.New("scout", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex", "scout")
	ctx := context.Background()
	got, _ := h.orch.Tasks.Get(ctx, task.ID)

	for _, m := range []*domain.Message{
		{ID: "R1", TaskID: task.ID, FromAgent: domain.HumanAgentID, ToAgent: "opus", Type: domain.MsgTaskAssignment, Content: "the secret brief"},
		{ID: "R2", TaskID: task.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "private to codex"},
		{ID: "R3", TaskID: task.ID, FromAgent: "opus", ToAgent: domain.BroadcastAgentID, Type: domain.MsgInformation, Content: "for everyone"},
		{ID: "R4", TaskID: task.ID, FromAgent: "opus", ToAgent: "scout", Type: domain.MsgDelegate, Content: "scout's own work"},
	} {
		if err := h.store.Messages.Append(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	toScout := &domain.Message{ID: "R5", TaskID: task.ID, FromAgent: "opus", ToAgent: "scout", Type: domain.MsgQuestion}
	orchestrator.AttachRecapForTest(h.orch, ctx, got, toScout)
	recap := toScout.Metadata[ports.RecapMetadataKey]
	for _, want := range []string{"for everyone", "scout's own work"} {
		if !strings.Contains(recap, want) {
			t.Errorf("scout's recap should include %q:\n%s", want, recap)
		}
	}
	for _, leak := range []string{"the secret brief", "private to codex"} {
		if strings.Contains(recap, leak) {
			t.Errorf("scout's recap must not include %q:\n%s", leak, recap)
		}
	}

	toOpus := &domain.Message{ID: "R6", TaskID: task.ID, FromAgent: "scout", ToAgent: "opus", Type: domain.MsgAnswer}
	orchestrator.AttachRecapForTest(h.orch, ctx, got, toOpus)
	if r := toOpus.Metadata[ports.RecapMetadataKey]; !strings.Contains(r, "private to codex") || !strings.Contains(r, "the secret brief") {
		t.Errorf("the authority's recap covers the whole task:\n%s", r)
	}
}
