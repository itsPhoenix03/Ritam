package orchestrator_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/fake"
	"ritam/internal/domain"
)

// fakeAuthority delegates to codex on its assignment and decides once codex
// answers; anything else it is told, it says nothing to.
func fakeAuthority() fake.Script {
	return func(_ int, _ *domain.Task, in domain.Message) fake.Turn {
		switch {
		case in.Type == domain.MsgTaskAssignment:
			return fake.Reply(fake.Header(domain.MsgDelegate, "codex", "Look at the cookie handling."))
		case strings.Contains(in.Content, "ANSWER from codex") || (in.FromAgent == "codex" && in.Type == domain.MsgAnswer):
			return fake.Reply(fake.Header(domain.MsgFinalDecision, "*", "Clear the cookie with Path=/."))
		}
		return fake.Turn{}
	}
}

// codexAnswer is codex's answer to the authority.
var codexAnswer = fake.Reply(fake.Header(domain.MsgAnswer, "opus", "Logout clears Path=/api; creation uses Path=/."))

// runFake runs a task of a fake authority (opus) and a fake codex until it
// stops, and returns it with the harness.
func runFake(t *testing.T, codex *fake.Agent, idle time.Duration) (*harness, *domain.Task) {
	t.Helper()
	gw := newGateway()
	gw.add(fake.New("opus", fakeAuthority()), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), idle, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = h.orch.RunTask(ctx, task.ID)
	final, _ := h.orch.Tasks.Get(context.Background(), task.ID)
	return h, final
}

// TestAReplyWithOneBlockLackingARecipientIsMalformedAndTheTaskGoesOn: the
// whole path on an agent that parses raw text the way the CLI adapters do.
// codex's first reply has one block and no to=; it is a Malformed failure, so
// codex is asked again with what it wrote quoted, answers, and the task
// completes instead of failing.
func TestAReplyWithOneBlockLackingARecipientIsMalformedAndTheTaskGoesOn(t *testing.T) {
	codex := fake.New("codex", fake.Steps(
		fake.Reply(fake.Header(domain.MsgAnswer, "", "Logout clears the wrong path.")),
		codexAnswer,
	))
	h, final := runFake(t, codex, 5*time.Second)
	if final.State != domain.TaskCompleted {
		t.Fatalf("state = %s (%s %s), want COMPLETED", final.State, final.PauseReason, final.FailureReason)
	}
	got := codex.Received()
	if len(got) < 2 || got[1].Type != domain.MsgError || !strings.Contains(got[1].Content, "Logout clears the wrong path.") {
		t.Errorf("codex must be asked again, shown what it wrote: %+v", got)
	}
	if !h.hasEvent(domain.EvtAgentFailed) {
		t.Error("the unreadable reply is recorded as a failed turn")
	}
}
