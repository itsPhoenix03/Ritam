package orchestrator_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
)

// The human asks opus; opus asks scout and codex in parallel; scout replies,
// codex does not. These pin what happens next in each variant.

type parallelRig struct {
	h         *harness
	task      *domain.Task
	mu        sync.Mutex
	delivered []domain.Message
}

func (r *parallelRig) count(match func(domain.Message) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.delivered {
		if match(m) {
			n++
		}
	}
	return n
}

// quiet suppresses the scripted behaviour a mock would otherwise fall back to.
var quiet = []domain.Message{}

func isFollowUp(in domain.Message) bool {
	return in.FromAgent == domain.HumanAgentID && in.Type == domain.MsgInformation &&
		in.Metadata["orchestrator"] == "" && strings.Contains(in.Content, "is the change safe?")
}

// parallel builds the rig. opus, on the follow-up, asks scout and codex; what
// it does next is opusNext. codex is the given adapter.
func parallel(t *testing.T, opusNext mockagent.Behavior, codex ports.AgentAdapter) *parallelRig {
	t.Helper()
	orchestrator.SetFollowUpSettleInterval(30 * time.Millisecond)
	orchestrator.SetHumanAnswerGrace(150 * time.Millisecond)
	t.Cleanup(func() {
		orchestrator.SetFollowUpSettleInterval(2 * time.Second)
		orchestrator.SetHumanAnswerGrace(45 * time.Second)
	})

	reopened := false
	authority := mockagent.AuthorityBehavior()
	opus := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if isFollowUp(in) {
			reopened = true
			return []domain.Message{
				{ToAgent: "scout", Type: domain.MsgQuestion, Content: "check part 1"},
				{ToAgent: "codex", Type: domain.MsgDelegate, Content: "check part 2"},
			}
		}
		if reopened {
			if out := opusNext(self, tk, in); out != nil {
				return out
			}
			return quiet
		}
		return authority(self, tk, in)
	}
	implementer := mockagent.ImplementerBehavior()
	scout := func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgQuestion {
			return []domain.Message{{ToAgent: in.FromAgent, Type: domain.MsgAnswer, Content: "part 1 is fine"}}
		}
		return implementer(self, tk, in)
	}

	rig := &parallelRig{}
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, opus), domain.RoleAuthority)
	gw.add(codex, domain.RoleImplementer)
	gw.add(mockagent.New("scout", domain.AgentKindMock, scout), domain.RoleReviewer)
	gw.onSend = func(m domain.Message) {
		rig.mu.Lock()
		rig.delivered = append(rig.delivered, m)
		rig.mu.Unlock()
	}
	rig.h = newHarness(t, defaultLimits(), 5*time.Second, gw)
	rig.task = createTask(t, rig.h, "opus", "opus", "codex", "scout")
	if err := rig.h.orch.RunTask(context.Background(), rig.task.ID); err != nil {
		t.Fatalf("first run: %v", err)
	}
	return rig
}

func (r *parallelRig) followUp(t *testing.T) *domain.Task {
	t.Helper()
	ctx := context.Background()
	if err := r.h.orch.Reopen(ctx, r.task.ID, "opus", "is the change safe?"); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	_ = r.h.orch.RunTask(runCtx, r.task.ID)
	final, _ := r.h.orch.Tasks.Get(ctx, r.task.ID)
	return final
}

// silentCodex answers the first task normally and nothing afterwards.
func silentCodex() ports.AgentAdapter {
	implementer := mockagent.ImplementerBehavior()
	done := false
	return mockagent.New("codex", domain.AgentKindMock, func(self domain.AgentID, tk *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgFinalDecision {
			done = true
		}
		if done || in.Type == domain.MsgDelegate && in.Content == "check part 2" {
			done = true
			return nil
		}
		return implementer(self, tk, in)
	})
}

func toAgentWith(agent domain.AgentID, key string) func(domain.Message) bool {
	return func(m domain.Message) bool {
		return m.ToAgent == agent && m.Metadata["orchestrator"] == key
	}
}

// Variant 1: opus answers the human with scout's part; codex never replies.
// codex is reminded, then its part is closed and recorded, and -- since the
// human has an answer -- the follow-up completes without waking anyone.
func TestASilentBranchIsClosedOnceTheHumanHasAnAnswer(t *testing.T) {
	rig := parallel(t, func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == "scout" && in.Type == domain.MsgAnswer {
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer,
				Content: "Part 1 is fine; still waiting on codex for part 2."}}
		}
		return nil
	}, silentCodex())
	final := rig.followUp(t)

	if final.State != domain.TaskCompleted {
		t.Errorf("the human has an answer; codex's silent branch must not hold the task open: %s (%s)",
			final.State, final.PauseReason)
	}
	if n := rig.count(toAgentWith("codex", "reminder")); n != 1 {
		t.Errorf("codex owes the reply and must be reminded exactly once, got %d", n)
	}
	if n := rig.count(toAgentWith("opus", "reminder")); n != 0 {
		t.Errorf("opus already answered the human and must not be reminded, got %d", n)
	}
	if n := rig.count(toAgentWith("opus", "closed-notify")); n != 0 {
		t.Error("with the human answered, the closure is recorded, not delivered: no CLI turn for nothing")
	}
	if !rig.h.hasEvent(domain.EvtRequestAbandoned) {
		t.Error("codex's missing part must be on the record")
	}
}

// Variant 2: opus waits for codex and says nothing to the human. When codex
// is closed as unanswered, opus is told to answer the human with what it has.
func TestASilentBranchPromptsTheAskerToAnswerWithWhatItHas(t *testing.T) {
	rig := parallel(t, func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.Metadata["orchestrator"] == "closed-notify" {
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer,
				Content: "Part 1 is fine; codex never answered on part 2."}}
		}
		return nil // waits for codex
	}, silentCodex())
	final := rig.followUp(t)

	if final.State != domain.TaskCompleted {
		t.Errorf("opus answered the human once told codex would not reply: %s (%s)", final.State, final.PauseReason)
	}
	if rig.count(toAgentWith("opus", "closed-notify")) != 1 {
		t.Error("with the human still waiting, opus must be told codex's part is closed")
	}
	if n := rig.count(toAgentWith("opus", "reminder")); n != 0 {
		t.Errorf("opus was waiting on codex, legitimately; it must not be nagged, got %d reminder(s)", n)
	}
}

// Variant 3: everyone goes quiet. codex is reminded and closed, opus is told,
// opus says nothing either, opus is reminded, and the task pauses naming the
// human's question.
func TestWhenEveryoneIsSilentThePauseNamesTheHumansQuestion(t *testing.T) {
	rig := parallel(t, func(domain.AgentID, *domain.Task, domain.Message) []domain.Message { return nil }, silentCodex())
	final := rig.followUp(t)

	if final.State != domain.TaskPaused || !strings.Contains(final.PauseReason, "waiting for an answer to your question to opus") {
		t.Errorf("the pause must name the human's unanswered question: %s (%q)", final.State, final.PauseReason)
	}
	if rig.count(toAgentWith("codex", "reminder")) != 1 || rig.count(toAgentWith("opus", "reminder")) != 1 {
		t.Errorf("codex then opus should each get one reminder: codex=%d opus=%d",
			rig.count(toAgentWith("codex", "reminder")), rig.count(toAgentWith("opus", "reminder")))
	}
}

// Variant 4: codex is slow, not silent. Nothing is chased while it works, and
// its answer completes the picture.
func TestASlowButWorkingAgentIsNeverChased(t *testing.T) {
	codex := newSlowAgent("codex", 700*time.Millisecond, true) // > the 150ms grace
	rig := parallel(t, func(_ domain.AgentID, _ *domain.Task, in domain.Message) []domain.Message {
		if in.FromAgent == "codex" && in.Type == domain.MsgAnswer {
			return []domain.Message{{ToAgent: domain.HumanAgentID, Type: domain.MsgAnswer, Content: "Both parts are fine."}}
		}
		return nil
	}, codex)
	final := rig.followUp(t)

	if final.State != domain.TaskCompleted {
		t.Errorf("codex answered in the end: %s (%s)", final.State, final.PauseReason)
	}
	if n := rig.count(func(m domain.Message) bool { return m.Metadata["orchestrator"] == "reminder" }); n != 0 {
		t.Errorf("a working agent is not a silent one; no reminders expected, got %d", n)
	}
	if rig.h.hasEvent(domain.EvtRequestAbandoned) {
		t.Error("nothing should have been closed as unanswered")
	}
}
