package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// deliveryOf returns the stored delivery of the message whose content holds
// text, to recipient.
func deliveryOf(t *testing.T, h *harness, task domain.TaskID, to domain.AgentID, text string) *domain.Delivery {
	t.Helper()
	ctx := context.Background()
	rows, err := h.store.Deliveries.ListByTask(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range rows {
		if d.Recipient != to {
			continue
		}
		if m, err := h.store.Messages.Get(ctx, d.MessageID); err == nil && strings.Contains(m.Content, text) {
			return d
		}
	}
	return nil
}

// containing counts the deliveries whose content holds text.
func containing(msgs []domain.Message, text string) int {
	n := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, text) {
			n++
		}
	}
	return n
}

// endRun stops a run in the background and waits for it.
func endRun(t *testing.T, h *harness, id domain.TaskID, done chan error) {
	t.Helper()
	if err := h.orch.Pause(context.Background(), id, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run never ended")
	}
}

// TestAReplyWaitingForABusyAgentReachesItAfterAPauseOrARestart is the probe
// made permanent. codex's IMPLEMENTATION_RESULT reached opus mid-turn and
// waited in its inbox; the task was paused. The resume only re-requested
// open requests -- codex's reply had closed this one -- so opus was told to
// carry on and never got the result. The inbox was memory, dropped at the
// end of the run; now it is queued in the store and the next run, in this
// process or a new one, hands it over first.
func TestAReplyWaitingForABusyAgentReachesItAfterAPauseOrARestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		opus, codex := newPromptAgent("opus"), newPromptAgent("codex")
		gw := newGateway()
		gw.add(opus, domain.RoleAuthority)
		gw.add(codex, domain.RoleImplementer)
		h := newHarness(t, defaultLimits(), time.Minute, gw)
		task := createTask(t, h, "opus", "opus", "codex")
		ctx := context.Background()
		done := runInBackground(h, task.ID)
		waitFor(t, func() bool { return len(opus.deliveries()) == 1 })
		// opus hands out the work and is still working on its own turn.
		opus.out <- ports.Outbound{AgentID: "opus", Message: domain.Message{
			ToAgent: "codex", Type: domain.MsgImplement, Content: "Write cancel()."}}
		waitFor(t, func() bool { return len(codex.deliveries()) == 1 })
		codex.out <- ports.Outbound{AgentID: "codex", Message: domain.Message{
			ToAgent: "opus", Type: domain.MsgImplementationResult, Content: "CANCEL-IS-DONE: tests pass."}}
		codex.finish()
		waitFor(t, func() bool { return h.orch.Backlog(task.ID)["opus"] == 1 })
		endRun(t, h, task.ID, done)

		if d := deliveryOf(t, h, task.ID, "opus", "CANCEL-IS-DONE"); d == nil || d.Status != domain.DeliveryQueued {
			t.Fatalf("restart=%v: the reply must stay queued when the run ends: %+v", restart, d)
		}
		if restart {
			h = restarted(h)
		}
		if err := h.orch.Resume(ctx, task.ID); err != nil {
			t.Fatal(err)
		}
		done = runInBackground(h, task.ID)
		waitFor(t, func() bool { return len(opus.deliveries()) > 1 })
		next := opus.deliveries()[1]
		if !strings.Contains(next.Content, "CANCEL-IS-DONE") {
			t.Errorf("restart=%v: opus's next turn must carry codex's result:\n%s", restart, next.Content)
		}
		if i, j := strings.Index(next.Content, "CANCEL-IS-DONE"), strings.Index(next.Content, "has resumed it"); j >= 0 && i > j {
			t.Errorf("restart=%v: what was queued comes before the note to carry on:\n%s", restart, next.Content)
		}
		endRun(t, h, task.ID, done)
		if n := containing(opus.deliveries(), "CANCEL-IS-DONE"); n != 1 {
			t.Errorf("restart=%v: the result must be delivered once, was %d times", restart, n)
		}
	}
}

// TestATurnKilledMidFlightIsDeliveredAgainOnce: codex was working on the
// delegation when the task was paused, which killed its turn. The delegation
// goes back to the queue, and the next run hands it over again -- once, saying
// codex may have seen it -- and the task completes on codex's answer.
func TestATurnKilledMidFlightIsDeliveredAgainOnce(t *testing.T) {
	codex := fake.New("codex", fake.Steps(fake.Turn{Latency: 10 * time.Second}, codexAnswer))
	gw := newGateway()
	gw.add(fake.New("opus", fakeAuthority()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool { return codex.Turns() == 1 })
	endRun(t, h, task.ID, done)

	if codex.Killed() != 1 {
		t.Fatalf("codex's turn must have been killed, killed = %d", codex.Killed())
	}
	d := deliveryOf(t, h, task.ID, "codex", "cookie handling")
	if d == nil || d.Status != domain.DeliveryQueued || d.TurnID == "" {
		t.Fatalf("the killed turn's delegation goes back to the queue, remembering it was sent: %+v", d)
	}

	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitRun(t, runInBackground(h, task.ID))
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s), want COMPLETED", cur.State, cur.PauseReason)
	}
	got := codex.Received()
	if n := containing(got, "cookie handling"); n != 2 {
		t.Errorf("the delegation must be delivered again exactly once: %d deliveries carry it", n)
	}
	if !strings.Contains(got[1].Content, "you may have seen these") {
		t.Errorf("the re-delivery must say codex may have seen it:\n%s", got[1].Content)
	}
}

// TestAnAcknowledgedBatchIsNeverDeliveredAgain: codex answered its
// delegation, then the task was paused while opus was still working on the
// answer. The delegation was acknowledged and stays so; only opus's cut-off
// turn is handed over again.
func TestAnAcknowledgedBatchIsNeverDeliveredAgain(t *testing.T) {
	authority := func(n int, _ *domain.Task, in domain.Message) fake.Turn {
		if in.Type == domain.MsgTaskAssignment {
			return fake.Reply(fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."))
		}
		return fake.Turn{Latency: 10 * time.Second} // thinking about the answer
	}
	opus, codex := fake.New("opus", authority), fake.New("codex", fake.Steps(codexAnswer))
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool { return opus.Turns() == 2 })
	endRun(t, h, task.ID, done)

	if d := deliveryOf(t, h, task.ID, "codex", "cookie handling"); d == nil || d.Status != domain.DeliveryAcknowledged {
		t.Fatalf("the answered delegation must be acknowledged: %+v", d)
	}
	if d := deliveryOf(t, h, task.ID, "opus", "Logout clears"); d == nil || d.Status != domain.DeliveryQueued {
		t.Fatalf("the answer opus was cut off reading must be queued again: %+v", d)
	}
	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	done = runInBackground(h, task.ID)
	waitFor(t, func() bool { return len(opus.Received()) == 3 })
	endRun(t, h, task.ID, done)
	if n := containing(codex.Received(), "cookie handling"); n != 1 {
		t.Errorf("an acknowledged delegation must never be handed over again: %d deliveries carry it", n)
	}
	if !strings.Contains(opus.Received()[2].Content, "Logout clears") {
		t.Errorf("opus gets codex's answer again:\n%s", opus.Received()[2].Content)
	}
}

// TestAFailedSendLeavesTheMessagesQueued: a send the adapter refused used to
// take the messages out of the inbox all the same, and they were lost.
func TestAFailedSendLeavesTheMessagesQueued(t *testing.T) {
	opus := newPromptAgent("opus")
	codex := fake.New("codex", fake.Steps(codexAnswer)) // never started: it refuses every send
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Minute, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	_ = opus.Start(ctx)
	h.orch.OpenInbox(task.ID)
	if err := h.orch.Bus.Route(ctx, task, &domain.Message{FromAgent: "opus", ToAgent: "codex",
		Type: domain.MsgDelegate, Content: "Look at the cookie handling."}); err != nil {
		t.Fatal(err)
	}
	if !h.hasEvent(domain.EvtAgentFailed) {
		t.Fatal("the failed send must be reported")
	}
	d := deliveryOf(t, h, task.ID, "codex", "cookie handling")
	if d == nil || d.Status != domain.DeliveryQueued || d.TurnID != "" {
		t.Fatalf("the message must stay queued, never marked sent: %+v", d)
	}
}

// TestAQueuedTurnInFlightWhenTheProcessDiedIsDeliveredAfterTheRestart: the
// process stopped while codex worked on a delegation, leaving it sent in a
// turn that never ended. The restarted ritam pauses the task as interrupted;
// resumed, codex is handed the delegation again, told it may have seen it.
func TestAQueuedTurnInFlightWhenTheProcessDiedIsDeliveredAfterTheRestart(t *testing.T) {
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(newPromptAgent("opus"), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), time.Minute, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	// What the dead process left: the delegation recorded, queued and sent
	// in a turn, and the task mid-deliberation.
	m := &domain.Message{ID: "MSG-1", TaskID: task.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate,
		Content: "Look at the cookie handling.", Timestamp: time.Now()}
	_ = h.store.Messages.Append(ctx, m)
	_ = h.store.Deliveries.Enqueue(ctx, &domain.Delivery{MessageID: m.ID, Recipient: "codex", TaskID: task.ID, QueuedAt: m.Timestamp})
	_ = h.store.Deliveries.MarkSent(ctx, task.ID, "codex", []domain.MessageID{m.ID}, "TURN-DEAD", time.Now())
	for _, s := range []domain.TaskState{domain.TaskPlanning, domain.TaskDeliberating} {
		if _, err := h.orch.Tasks.Transition(ctx, task.ID, s, "opus", "work"); err != nil {
			t.Fatal(err)
		}
	}

	h = restarted(h)
	if _, err := h.orch.RecoverInterrupted(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	done := runInBackground(h, task.ID)
	waitFor(t, func() bool { return len(codex.deliveries()) > 0 })
	endRun(t, h, task.ID, done)
	got := codex.deliveries()[0]
	if !strings.Contains(got.Content, "cookie handling") || !strings.Contains(got.Content, "you may have seen these") {
		t.Errorf("codex must get the delegation again, told it may have seen it:\n%s", got.Content)
	}
}

// TestAnAnswerQueuedForAHeldDecisionSurvivesARestart: the human's answer to
// a held decision waited for the next run in memory only, so ritam closing
// before that run started lost it. It is now a queued delivery, and the run
// of a restarted ritam hands it over.
func TestAnAnswerQueuedForAHeldDecisionSurvivesARestart(t *testing.T) {
	got := &inbound{}
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		got.add(in)
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return []domain.Message{
				{ToAgent: domain.HumanAgentID, Type: domain.MsgQuestion, Content: "Please run the build."},
				{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Content: "Approved, but not yet built."},
			}
		case in.Content == "The build passed.":
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Content: "Approved and built."}}
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
	if err := h.orch.SendInstruction(ctx, task.ID, "opus", "The build passed."); err != nil {
		t.Fatal(err)
	}

	h = restarted(h)
	waitRun(t, runInBackground(h, task.ID))
	if got.find(func(m domain.Message) bool { return m.Content == "The build passed." }) == nil {
		t.Fatal("the answer queued before the restart never reached the authority")
	}
	if cur, _ := h.orch.Tasks.Get(ctx, task.ID); cur.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", cur.State)
	}
	if rows, _ := h.store.Deliveries.ListByTask(ctx, task.ID); len(rows) == 0 {
		t.Error("the acknowledged deliveries are kept as the record of what was given")
	} else {
		for _, d := range rows {
			if d.Status != domain.DeliveryAcknowledged {
				t.Errorf("a finished task keeps nothing waiting: %+v", d)
			}
		}
	}
}
