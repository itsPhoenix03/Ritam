package orchestrator_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// A supporting agent whose account cannot run its model (no credits here)
// drops out: the authority is told to carry on without it, and the model's
// unavailability is recorded. Before, the failure looked like a malformed
// reply and the agent was asked to "reply again", which fails again.
func TestASupportingAgentThatCannotRunDropsOut(t *testing.T) {
	var told []domain.Message
	authority := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgDelegate, Content: "Review the ledger."}}
		case in.Metadata["orchestrator"] == "unavailable":
			told = append(told, in)
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

	codex.finish()
	codex.out <- ports.Outbound{AgentID: "codex", Err: errors.New("codex adapter: exit status 1: insufficient_quota: You exceeded your current quota")}
	waitFor(t, func() bool { return h.hasEvent(domain.EvtModelUnavailable) })
	waitFor(t, func() bool { return len(told) == 1 })

	if !strings.Contains(told[0].Content, "codex cannot work on this task") || !strings.Contains(told[0].Content, "out of credits") {
		t.Errorf("the authority should be told plainly: %q", told[0].Content)
	}
	if got, _ := h.orch.Tasks.Get(context.Background(), task.ID); got.State == domain.TaskPaused || got.State == domain.TaskFailed {
		t.Errorf("one supporting agent dropping out must not stop the task: %s (%s)", got.State, got.PauseReason)
	}
	msgs, _ := h.store.Messages.ListByTask(context.Background(), task.ID)
	for _, m := range msgs {
		if m.ToAgent == "codex" && m.Metadata["reason"] == "malformed-reply" {
			t.Error("an account failure must not be answered with a malformed-reply notice")
		}
	}
}

func TestAnAuthorityThatCannotRunPausesTheTask(t *testing.T) {
	opus := newPromptAgent("opus")
	gw := newGateway()
	gw.add(opus, domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 5*time.Second, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	done := make(chan struct{})
	go func() { _ = h.orch.RunTask(context.Background(), task.ID); close(done) }()
	waitFor(t, func() bool { return len(opus.deliveries()) == 1 })

	opus.finish()
	opus.out <- ports.Outbound{AgentID: "opus", Err: errors.New("claude-code adapter: CLI reported an error: Your organization does not have access to claude-fable-5-1")}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run should stop")
	}
	got, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	if got.State != domain.TaskPaused || !strings.Contains(got.PauseReason, "not included in your plan") {
		t.Errorf("the task should pause saying why: %s (%s)", got.State, got.PauseReason)
	}
}
