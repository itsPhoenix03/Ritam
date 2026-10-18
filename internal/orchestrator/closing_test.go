package orchestrator_test

import (
	"context"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
)

// TestTheHumanCanHandAuthorityToAnotherAgent: mid-task, codex takes over from
// opus. From then on only codex may decide, and everyone is told.
func TestTheHumanCanHandAuthorityToAnotherAgent(t *testing.T) {
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgTaskAssignment {
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Integrate rev 4."}}
		}
		return nil
	}
	codex := newPromptAgent("codex")
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.orch.RunTask(ctx, task.ID) }()
	waitFor(t, func() bool { return len(codex.deliveries()) == 1 })

	if err := h.orch.SetAuthority(ctx, task.ID, "scout"); err == nil {
		t.Error("only a participant can be made the authority")
	}
	if err := h.orch.SetAuthority(ctx, task.ID, "codex"); err != nil {
		t.Fatal(err)
	}
	got, _ := h.orch.Tasks.Get(ctx, task.ID)
	if got.AuthorityAgent != "codex" {
		t.Fatalf("authority = %s, want codex", got.AuthorityAgent)
	}
	err := h.orch.Bus.Route(ctx, got, &domain.Message{FromAgent: "opus", ToAgent: domain.BroadcastAgentID,
		Type: domain.MsgFinalDecision, Content: "Rev 8."})
	if err == nil {
		t.Error("the former authority must no longer be able to decide")
	}
	msgs, _ := h.store.Messages.ListByTask(context.Background(), task.ID)
	told := map[domain.AgentID]bool{}
	for _, m := range msgs {
		if m.Metadata["orchestrator"] == "authority" {
			told[m.ToAgent] = true
		}
	}
	if !told["opus"] || !told["codex"] {
		t.Errorf("both the old and the new authority must be told: %v", told)
	}
	if !h.hasEvent(domain.EvtHumanOverride) {
		t.Error("the change must be recorded")
	}
}

// TestAPartlyUnderstoodReplyIsDeliveredAndTheAgentTold: the adapter delivered
// the good blocks and noted the bad ones; the orchestrator records it and
// tells the agent what to fix.
func TestAPartlyUnderstoodReplyIsDeliveredAndTheAgentTold(t *testing.T) {
	sent := false
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if sent {
			return nil
		}
		sent = true
		return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgInformation, Content: "Rev 8 is canonical.",
			Metadata: map[string]string{ports.ProtocolProblemsMetadataKey: `unknown message type "DECISION" was delivered as INFORMATION`}}}
	}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, authority), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 300*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	_ = h.orch.RunTask(context.Background(), task.ID)

	if s := h.eventSummary(domain.EvtValidationRejected); s == "" {
		t.Error("a partly understood reply must be recorded")
	}
	msgs, _ := h.store.Messages.ListByTask(context.Background(), task.ID)
	var delivered, told bool
	for _, m := range msgs {
		if m.FromAgent == "opus" && m.Content == "Rev 8 is canonical." {
			delivered = true
		}
		if m.ToAgent == "opus" && m.Metadata["reason"] == "partial-reply" {
			told = true
		}
	}
	if !delivered || !told {
		t.Errorf("the good part must be delivered (%v) and the agent told what to fix (%v)", delivered, told)
	}
}

// TestADecisionDoesNotDropTheHumansOpenQuestion: in a real run the human
// asked codex for its progress while codex was busy. The authority's final
// decision then completed the task and the question was never answered. Now
// the decision is accepted, and the task completes once codex has answered.
func TestADecisionDoesNotDropTheHumansOpenQuestion(t *testing.T) {
	orchestrator.SetFollowUpSettleInterval(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetFollowUpSettleInterval(2 * time.Second) })

	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Integrate rev 4."}}
		case in.FromAgent == domain.HumanAgentID && in.Content == "Decide now.":
			return []domain.Message{{ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision,
				Content: "Rev 8 is final."}}
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

	done := make(chan error, 1)
	go func() { done <- h.orch.RunTask(ctx, task.ID) }()
	waitFor(t, func() bool { return len(codex.deliveries()) == 1 }) // codex is busy with the DELEGATE

	if err := h.orch.SendInstruction(ctx, task.ID, "codex", "How is your progress?"); err != nil {
		t.Fatal(err)
	}
	if err := h.orch.SendInstruction(ctx, task.ID, "opus", "Decide now."); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return h.hasEvent(domain.EvtDecisionAccepted) })
	time.Sleep(200 * time.Millisecond)
	if got, _ := h.orch.Tasks.Get(ctx, task.ID); got.State == domain.TaskCompleted {
		t.Fatal("the task completed with the human's question to codex unanswered")
	}

	// codex finishes its turn, gets the human's question, and answers it.
	codex.finish()
	waitFor(t, func() bool { return len(codex.deliveries()) == 2 })
	codex.out <- ports.Outbound{AgentID: "codex", Message: domain.Message{
		ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "Rev 7 is posted; tests are next."}}
	codex.finish()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the task never completed after the question was answered")
	}
	if got, _ := h.orch.Tasks.Get(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", got.State)
	}
}
