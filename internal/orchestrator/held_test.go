package orchestrator_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/decisions"
	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/logging"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
)

// inbound records what a mock agent was sent.
type inbound struct {
	mu   sync.Mutex
	msgs []domain.Message
}

func (r *inbound) add(m domain.Message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

func (r *inbound) all() []domain.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]domain.Message(nil), r.msgs...)
}

func (r *inbound) find(match func(domain.Message) bool) *domain.Message {
	for _, m := range r.all() {
		if match(m) {
			return &m
		}
	}
	return nil
}

// runInBackground starts a run as the app does after a human action, and
// returns a channel that yields its result.
func runInBackground(h *harness, id domain.TaskID) chan error {
	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(context.Background(), id) }()
	return done
}

func waitRun(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never ended")
	}
}

// TestAnsweringAHeldDecisionReachesTheAskingAgent: the authority asked the
// human to run the build and decided before the answer came, so the decision
// was held for the human -- whose answer was then refused as "task is not
// running". The answer now reaches the authority with a note that its
// decision still waits, and the authority's confirmation completes the task.
func TestAnsweringAHeldDecisionReachesTheAskingAgent(t *testing.T) {
	got := &inbound{}
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		got.add(in)
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return []domain.Message{
				{ToAgent: domain.HumanAgentID, Type: domain.MsgQuestion, Content: "Please run the build and send me the output."},
				{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Content: "Approved, but not yet built."},
			}
		case in.Content == "The build passed.":
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision,
				Content: "Approved and built."}}
		}
		return nil
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 2*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	held, _ := h.orch.Decisions.Latest(ctx, task.ID)
	if held == nil || held.Status != domain.DecisionProposed {
		t.Fatalf("the decision must be held: %+v", held)
	}

	if err := h.orch.SendInstruction(ctx, task.ID, "opus", "The build passed."); err != nil {
		t.Fatalf("answering a held decision: %v", err)
	}
	waitRun(t, runInBackground(h, task.ID))

	if got.find(func(m domain.Message) bool { return m.Content == "The build passed." }) == nil {
		t.Error("the answer never reached the agent that asked")
	}
	if got.find(func(m domain.Message) bool { return strings.Contains(m.Content, "still waiting for the human") }) == nil {
		t.Error("the authority must be told its decision still waits")
	}
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", cur.State)
	}
	all, _ := h.orch.Decisions.ListByTask(ctx, task.ID)
	live := 0
	for _, d := range all {
		if d.Status == domain.DecisionProposed || d.Status == domain.DecisionAccepted {
			live++
		}
	}
	if old, _ := h.orch.Decisions.Get(ctx, held.ID); old.Status != domain.DecisionSuperseded || live != 1 {
		t.Errorf("the confirmed decision must supersede the held one: %s, %d live", old.Status, live)
	}
}

// TestARejectionReachesTheAuthorityAndTheRevisionCanBeApproved: rejecting a
// held decision used to tell no agent anything and leave the task in
// DELIBERATING with no run, refused by every action but cancel. The note now
// reaches the authority as a REJECT, the run picks the task up, and the
// revised decision can be approved. The original decision keeps its
// reasoning, and the conflict it had settled is open again.
func TestARejectionReachesTheAuthorityAndTheRevisionCanBeApproved(t *testing.T) {
	got := &inbound{}
	var h *harness
	var runningAtReject bool
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		got.add(in)
		switch in.Type {
		case domain.MsgTaskAssignment:
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision,
				Content: "Ship it.", Metadata: map[string]string{"reasoning": "Users asked for it."}}}
		case domain.MsgReject:
			runningAtReject = h.orch.Running(tk.ID)
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision,
				Content: "Ship it once the tests are in."}}
		}
		return nil
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h = newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, true)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	conflict := &domain.Disagreement{ID: "DIS-1", TaskID: task.ID, Claimant: "opus", Challenger: "codex", Claim: "x"}
	if err := h.store.Disagreements.Create(ctx, conflict); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := h.orch.Decisions.Latest(ctx, task.ID)
	if dis, _ := h.store.Disagreements.Get(ctx, "DIS-1"); !dis.Resolved {
		t.Fatal("the decision settles the open conflict")
	}

	if err := h.orch.Reject(ctx, task.ID, "NEEDS-TESTS-FIRST"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskDeliberating {
		t.Fatalf("state = %s, want DELIBERATING", cur.State)
	}
	if dis, _ := h.store.Disagreements.Get(ctx, "DIS-1"); dis.Resolved || dis.ResolvedByDecision != "" {
		t.Errorf("the conflict the rejected decision settled must be open again: %+v", dis)
	}
	rejected, _ := h.orch.Decisions.Get(ctx, first.ID)
	if rejected.Status != domain.DecisionRejected || rejected.HumanNote != "NEEDS-TESTS-FIRST" ||
		rejected.Reasoning != "Users asked for it." {
		t.Errorf("the rejection must keep the authority's reasoning beside the note: %+v", rejected)
	}
	if h.orch.Running(task.ID) {
		t.Fatal("no run should be alive yet")
	}
	waitRun(t, runInBackground(h, task.ID))

	rej := got.find(func(m domain.Message) bool { return m.Type == domain.MsgReject })
	if rej == nil || !strings.Contains(rej.Content, "NEEDS-TESTS-FIRST") || rej.Metadata["decisionId"] != string(first.ID) {
		t.Fatalf("the authority must get the note as a REJECT naming the decision: %+v", rej)
	}
	if !runningAtReject {
		t.Error("a run must be alive while the authority revises")
	}
	revised, _ := h.orch.Decisions.Latest(ctx, task.ID)
	if revised.ID == first.ID || revised.Status != domain.DecisionProposed {
		t.Fatalf("a revised decision must wait for approval: %+v", revised)
	}
	if err := h.orch.Approve(ctx, task.ID, "ok"); err != nil {
		t.Fatal(err)
	}
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", cur.State)
	}
}

// TestARejectionWithoutANoteAsksTheAuthorityToAskTheHuman: the TUI sends an
// empty note. The authority is told the human gave no reason, not handed a
// blank REJECT.
func TestARejectionWithoutANoteAsksTheAuthorityToAskTheHuman(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, decideImmediatelyBehavior()), domain.RoleAuthority)
	gw.add(newPromptAgent("codex"), domain.RoleImplementer)
	h := newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, true)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	_ = h.orch.RunTask(ctx, task.ID)
	if err := h.orch.Reject(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool {
		msgs, _ := h.store.Messages.ListByTask(ctx, task.ID)
		for _, m := range msgs {
			if m.Type == domain.MsgReject && strings.Contains(m.Content, "without giving a reason") {
				return true
			}
		}
		return false
	})
	_ = h.orch.Pause(ctx, task.ID, "")
	<-done
}

// TestRejectingWithNoDecisionPendingChangesNothing: Reject used to log a
// REJECT, refresh the allowance and send a working task to DELIBERATING even
// when there was nothing to reject.
func TestRejectingWithNoDecisionPendingChangesNothing(t *testing.T) {
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
	before, _ := h.orch.Tasks.Get(ctx, task.ID)
	events := len(h.events)

	if err := h.orch.Reject(ctx, task.ID, "no"); err == nil {
		t.Fatal("rejecting with no decision pending must be refused")
	}
	if err := h.orch.Approve(ctx, task.ID, "yes"); err == nil {
		t.Fatal("approving with no decision pending must be refused")
	}
	after, _ := h.orch.Tasks.Get(ctx, task.ID)
	if after.State != before.State || !after.SegmentStart.Equal(before.SegmentStart) {
		t.Errorf("the task changed: %s -> %s", before.State, after.State)
	}
	if len(h.events) != events {
		t.Errorf("a refused reject must emit nothing: %+v", h.events[events:])
	}
	cancel()
	<-done
}

// TestApprovingAHeldDecisionAfterAPauseCompletesTheTask: approving while the
// task was paused accepted the decision and then failed the transition,
// leaving it ACCEPTED on a PAUSED task that a second approve refused.
func TestApprovingAHeldDecisionAfterAPauseCompletesTheTask(t *testing.T) {
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, decideImmediatelyBehavior()), domain.RoleAuthority)
	gw.add(newPromptAgent("codex"), domain.RoleImplementer)
	h := newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, true)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	_ = h.orch.RunTask(ctx, task.ID)
	if err := h.orch.Pause(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.Approve(ctx, task.ID, "fine"); err != nil {
		t.Fatalf("approve while paused: %v", err)
	}
	cur, _ := h.orch.Tasks.Get(ctx, task.ID)
	d, _ := h.orch.Decisions.Latest(ctx, task.ID)
	if cur.State != domain.TaskCompleted || d.Status != domain.DecisionAccepted {
		t.Errorf("task %s, decision %s; want COMPLETED and ACCEPTED", cur.State, d.Status)
	}
	if err := h.orch.Approve(ctx, task.ID, "again"); err == nil {
		t.Error("a completed task has nothing left to approve")
	}
}

// TestResumingAHeldDecisionLeavesItWaitingForTheHuman: pausing and resuming a
// task whose decision waits for the human started a run that told the
// authority to "carry on". There is nothing for it to do until the human
// acts, so the run ends at once and the task stays as it was.
func TestResumingAHeldDecisionLeavesItWaitingForTheHuman(t *testing.T) {
	got := &inbound{}
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		got.add(in)
		return decideImmediatelyBehavior()(self, tk, in)
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(newPromptAgent("codex"), domain.RoleImplementer)
	h := newHarnessApproval(t, defaultLimits(), 2*time.Second, gw, true)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	_ = h.orch.RunTask(ctx, task.ID)
	if err := h.orch.Pause(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitRun(t, runInBackground(h, task.ID))

	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskAwaitingDecision {
		t.Errorf("state = %s, want AWAITING_DECISION", cur.State)
	}
	if n := len(got.all()); n != 1 {
		t.Errorf("the authority must not be woken: it was sent %d messages", n)
	}
	if err := h.orch.Approve(ctx, task.ID, ""); err != nil {
		t.Errorf("the decision must still be approvable: %v", err)
	}
}

// restarted builds a new orchestrator over the same store and agents, as
// ritam does when it starts again.
func restarted(h *harness) *harness {
	o := orchestrator.New(orchestrator.Deps{
		Storage:            h.store,
		Agents:             h.gw,
		Knowledge:          knowledge.New(h.store.Knowledge),
		Decisions:          decisions.New(h.store.Decisions),
		Logger:             logging.NewDiscard(),
		Limits:             defaultLimits(),
		MaxConcurrentTasks: 2,
		IdleTimeout:        5 * time.Second,
	})
	return &harness{orch: o, store: h.store, gw: h.gw}
}

// TestTheClosingStateSurvivesAPauseAndARestart: a decision accepted while
// the human's question to codex was open keeps the task up until codex
// answers. That was remembered only in memory, so a pause and resume -- or
// a restart -- lost it: the task never completed and could not be approved.
func TestTheClosingStateSurvivesAPauseAndARestart(t *testing.T) {
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetFollowUpSettleInterval(2 * time.Second) })

	for _, restart := range []bool{false, true} {
		authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
			switch {
			case in.Type == domain.MsgTaskAssignment:
				return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Integrate rev 4."}}
			case in.FromAgent == domain.HumanAgentID && in.Content == "Decide now.":
				return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Content: "Rev 8 is final."}}
			}
			return nil
		}
		codex := newPromptAgent("codex")
		gw := newGateway()
		gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
		gw.add(codex, domain.RoleImplementer)
		h := newHarness(t, defaultLimits(), 5*time.Second, gw)
		task := createTask(t, h, "opus", "opus", "codex")
		ctx := context.Background()
		done := runInBackground(h, task.ID)
		waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
		if err := h.orch.SendInstruction(ctx, task.ID, "codex", "How is your progress?"); err != nil {
			t.Fatal(err)
		}
		if err := h.orch.SendInstruction(ctx, task.ID, "opus", "Decide now."); err != nil {
			t.Fatal(err)
		}
		waitFor(t, func() bool { return h.hasEvent(domain.EvtDecisionAccepted) })
		if err := h.orch.Pause(ctx, task.ID, ""); err != nil {
			t.Fatal(err)
		}
		<-done

		if restart {
			h = restarted(h)
		}
		if err := h.orch.Resume(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		done = runInBackground(h, task.ID)
		waitFor(t, func() bool { return len(codex.deliveries()) >= 2 })
		codex.out <- ports.Outbound{AgentID: "codex", Message: domain.Message{
			ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "Rev 7 is posted."}}
		codex.finish()
		waitRun(t, done)
		if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted || cur.Closing {
			t.Errorf("restart=%v: state = %s (closing %v), want COMPLETED", restart, cur.State, cur.Closing)
		}
	}
}

// TestApprovingAClosingTaskCompletesIt: a task whose decision was accepted
// but which still waits on an answer for the human can be completed by the
// human approving it, rather than failing with "cannot accept from status
// ACCEPTED" -- the way out for tasks stuck in that state by older versions.
func TestApprovingAClosingTaskCompletesIt(t *testing.T) {
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetFollowUpSettleInterval(2 * time.Second) })
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Integrate rev 4."}}
		case in.FromAgent == domain.HumanAgentID && in.Content == "Decide now.":
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Content: "Rev 8 is final."}}
		}
		return nil
	}
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
	_ = h.orch.SendInstruction(ctx, task.ID, "codex", "How is your progress?")
	_ = h.orch.SendInstruction(ctx, task.ID, "opus", "Decide now.")
	waitFor(t, func() bool { return h.hasEvent(domain.EvtDecisionAccepted) })

	if err := h.orch.Approve(ctx, task.ID, "good enough"); err != nil {
		t.Fatal(err)
	}
	waitRun(t, done)
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", cur.State)
	}
}
