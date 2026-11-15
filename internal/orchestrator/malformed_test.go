package orchestrator_test

import (
	"context"
	"errors"
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
