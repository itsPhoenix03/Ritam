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

// These replay the shapes a follow-up can take once agents pass the human's
// question between them. The first is a real run: the human asked codex to ask
// opus; codex did; opus answered the human directly and told codex; codex
// replied only to opus. The task never completed, because the only answer the
// orchestrator recognised was codex's own.

type threadRig struct {
	h         *harness
	task      *domain.Task
	mu        sync.Mutex
	delivered []domain.Message
}

func (r *threadRig) sent(match func(domain.Message) bool) (domain.Message, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.delivered {
		if match(m) {
			return m, true
		}
	}
	return domain.Message{}, false
}

// followUpRig completes a task with authority opus and implementer codex, then
// installs follow-up behaviours on top of their normal ones.
func followUpRig(t *testing.T, opusFollow, codexFollow mockagent.Behavior, idle time.Duration) *threadRig {
	t.Helper()
	orchestrator.SetFollowUpSettleInterval(30 * time.Millisecond)
	orchestrator.SetHumanAnswerGrace(150 * time.Millisecond)
	t.Cleanup(func() {
		orchestrator.SetFollowUpSettleInterval(2 * time.Second)
		orchestrator.SetHumanAnswerGrace(45 * time.Second)
	})

	layer := func(base, extra mockagent.Behavior) mockagent.Behavior {
		return func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
			if extra != nil {
				if out := extra(self, tk, in); out != nil {
					return out
				}
			}
			return base(self, tk, in)
		}
	}
	rig := &threadRig{}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, layer(mockagent.AuthorityBehavior(), opusFollow)), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, layer(mockagent.ImplementerBehavior(), codexFollow)), domain.RoleImplementer)
	gw.onSend = func(m domain.Message) {
		rig.mu.Lock()
		rig.delivered = append(rig.delivered, m)
		rig.mu.Unlock()
	}
	rig.h = newHarness(t, defaultLimits(), idle, gw)
	rig.task = createTask(t, rig.h, "opus", "opus", "codex")
	if err := rig.h.orch.RunTask(context.Background(), rig.task.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	return rig
}

func (r *threadRig) followUp(t *testing.T, to domain.AgentID, question string) *domain.Task {
	t.Helper()
	ctx := context.Background()
	if err := r.h.orch.Reopen(ctx, r.task.ID, to, question); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_ = r.h.orch.RunTask(runCtx, r.task.ID)
	final, _ := r.h.orch.Tasks.Get(ctx, r.task.ID)
	return final
}

func fromHuman(in domain.Message) bool {
	return in.FromAgent == domain.HumanAgentID && in.Type == domain.MsgInformation && in.Metadata["orchestrator"] == ""
}

// TestTheRealRunCompletes replays the run exactly.
func TestTheRealRunCompletes(t *testing.T) {
	codex := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		switch {
		case fromHuman(in):
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgQuestion,
				Content: "The human asks how well scout and I performed. Could you evaluate each review?"}}
		case in.FromAgent == "opus" && in.Type == domain.MsgAnswer:
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgAnswer, Content: "Understood. Thanks for the evaluation."}}
		}
		return nil
	}
	opus := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == "codex" && in.Type == domain.MsgQuestion {
			return []domain.Message{
				{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "Both reviews found 7/7; codex's tests were thinner."},
				{ToAgent: "codex", Type: domain.MsgAnswer, Content: "I've answered the human directly. No further action needed."},
			}
		}
		return nil
	}
	rig := followUpRig(t, opus, codex, 5*time.Second)
	final := rig.followUp(t, "codex", "ask opus how good were you and scout in this task?")

	if final.State != domain.TaskCompleted {
		t.Errorf("opus answered the human on codex's behalf; the follow-up must complete, got %s (%s)",
			final.State, final.PauseReason)
	}
	q, ok := rig.sent(func(m domain.Message) bool {
		return m.FromAgent == "codex" && m.ToAgent == "opus" && m.Type == domain.MsgQuestion
	})
	if !ok {
		t.Fatal("codex's question to opus was never delivered")
	}
	if note := q.Metadata[ports.ThreadMetadataKey]; !strings.Contains(note, "the human asked codex") {
		t.Errorf("opus must be told the question is the human's, so it can answer the human directly; note = %q", note)
	}
	if _, reminded := rig.sent(func(m domain.Message) bool { return m.Metadata["orchestrator"] == "reminder" }); reminded {
		t.Error("the human was answered; nobody should have been reminded")
	}
}

// TestTheAddressedAgentRelayingTheAnswerAlsoCompletes: the other path, where
// the helper answers the agent and that agent reports to the human.
func TestTheAddressedAgentRelayingTheAnswerAlsoCompletes(t *testing.T) {
	codex := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		switch {
		case fromHuman(in):
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgQuestion, Content: "how did we do?"}}
		case in.FromAgent == "opus" && in.Type == domain.MsgAnswer:
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "opus says: 7/7 each."}}
		}
		return nil
	}
	opus := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == "codex" && in.Type == domain.MsgQuestion {
			return []domain.Message{{ToAgent: "codex", Type: domain.MsgAnswer, Content: "7/7 each."}}
		}
		return nil
	}
	rig := followUpRig(t, opus, codex, 5*time.Second)
	if final := rig.followUp(t, "codex", "ask opus how we did"); final.State != domain.TaskCompleted {
		t.Errorf("relayed answer must complete the follow-up: %s (%s)", final.State, final.PauseReason)
	}
}

// TestAnUnansweredQuestionGetsOneReminderThenAClearPause: the agents talk to
// each other but never to the human.
func TestAnUnansweredQuestionGetsOneReminderThenAClearPause(t *testing.T) {
	codex := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == domain.HumanAgentID && in.Type == domain.MsgInformation {
			// Neither the question nor the reminder gets an answer to the human.
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgInformation, Content: "noted " + string(in.ID)}}
		}
		return nil
	}
	rig := followUpRig(t, nil, codex, 5*time.Second)
	final := rig.followUp(t, "codex", "is the fix safe?")

	reminders := 0
	rig.mu.Lock()
	for _, m := range rig.delivered {
		if m.Metadata["orchestrator"] == "reminder" && m.ToAgent == "codex" {
			reminders++
		}
	}
	rig.mu.Unlock()
	if reminders != 1 {
		t.Errorf("want exactly one reminder to codex, got %d", reminders)
	}
	if final.State != domain.TaskPaused || !strings.Contains(final.PauseReason, "waiting for an answer to your question to codex") {
		t.Errorf("the pause must say what is missing: %s (%q)", final.State, final.PauseReason)
	}
}

func TestAReminderThatGetsAnAnswerCompletes(t *testing.T) {
	codex := func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == domain.HumanAgentID && in.Metadata["orchestrator"] == "reminder" {
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "Yes, it is safe."}}
		}
		if fromHuman(in) {
			return []domain.Message{{ToAgent: "opus", Type: domain.MsgInformation, Content: "thinking about it"}}
		}
		return nil
	}
	rig := followUpRig(t, nil, codex, 5*time.Second)
	if final := rig.followUp(t, "codex", "is the fix safe?"); final.State != domain.TaskCompleted {
		t.Errorf("an answer prompted by the reminder must complete the follow-up: %s (%s)", final.State, final.PauseReason)
	}
}

// TestTheIdleTimeoutNamesTheMissingAnswer: even without a reminder cycle, a
// timeout with the human's question open must say so.
func TestTheIdleTimeoutNamesTheMissingAnswer(t *testing.T) {
	orchestrator.SetHumanAnswerGrace(time.Hour) // keep the reminder out of the way
	defer orchestrator.SetHumanAnswerGrace(45 * time.Second)
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	h := newHarness(t, defaultLimits(), 300*time.Millisecond, gw)
	task := createTask(t, h, "opus", "opus", "codex")
	ctx := context.Background()
	_ = h.orch.RunTask(ctx, task.ID)
	_ = h.orch.Reopen(ctx, task.ID, "codex", "anything to add?") // the implementer mock ignores it
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = h.orch.RunTask(runCtx, task.ID)

	final, _ := h.orch.Tasks.Get(ctx, task.ID)
	if !strings.Contains(final.PauseReason, "waiting for an answer to your question to codex") {
		t.Errorf("pause reason = %q; it should name the unanswered question, not just 'no agent activity'", final.PauseReason)
	}
}
