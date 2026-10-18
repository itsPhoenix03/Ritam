package orchestrator_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
)

type said struct {
	from, to domain.AgentID
	typ      domain.MessageType
	content  string
	verdict  string
}

// replay appends each message to the task's history and meters it exactly
// as the run loop does, returning the task afterwards.
func replay(t *testing.T, h *harness, task *domain.Task, msgs []said) *domain.Task {
	t.Helper()
	ctx := context.Background()
	at := time.Now()
	cur := task
	base, err := h.store.Messages.CountByTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range msgs {
		m := &domain.Message{
			ID: domain.MessageID(fmt.Sprintf("MSG-%03d", base+i)), TaskID: task.ID,
			FromAgent: s.from, ToAgent: s.to, Type: s.typ, Content: s.content,
			Timestamp: at.Add(time.Duration(base+i) * time.Second),
		}
		if s.verdict != "" {
			m.Metadata = map[string]string{domain.MetaVerdict: s.verdict}
		}
		if err := h.store.Messages.Append(ctx, m); err != nil {
			t.Fatal(err)
		}
		if cur, err = h.orch.Tasks.NoteMessage(ctx, task.ID, m, h.orch.IsProgress(ctx, cur, m)); err != nil {
			t.Fatal(err)
		}
	}
	return cur
}

func stallHarness(t *testing.T) (*harness, *domain.Task) {
	t.Helper()
	gw := newGateway()
	gw.add(mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior()), domain.RoleAuthority)
	gw.add(mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	gw.add(mockagent.New("scout", domain.AgentKindMock, mockagent.ImplementerBehavior()), domain.RoleImplementer)
	limits := defaultLimits()
	limits.StallMessages = 8
	h := newHarness(t, limits, time.Second, gw)
	return h, createTask(t, h, "opus", "opus", "codex", "scout")
}

// TestAgentsWorkingTogetherAreNotAStall replays the agent-to-agent messages
// of a real run (TASK-002, round 2) that was paused as STALLED while codex and
// scout were dividing up an implementation, reviewing each other's drafts and
// keeping the authority informed.
func TestAgentsWorkingTogetherAreNotAStall(t *testing.T) {
	h, task := stallHarness(t)
	got := replay(t, h, task, []said{
		{"opus", domain.BroadcastAgentID, domain.MsgTaskAssignment, "New task: TTL + LRU cache for the pricing service. Work together on one implementation.", ""},
		{"scout", "codex", domain.MsgProposal, "Here are my drafts for getOrCompute, hitRate and the tests. I also found core bugs.", ""},
		{"scout", "opus", domain.MsgStatusUpdate, "I've sent codex my drafts for getOrCompute and hitRate and a test plan.", ""},
		{"codex", "scout", domain.MsgProposal, "My proposed members: erase, setCapacity, purgeExpired.", ""},
		{"codex", "opus", domain.MsgStatusUpdate, "I drafted erase, setCapacity, and purgeExpired and sent them to scout.", ""},
		{"opus", "scout", domain.MsgInformation, "Thanks for checking the core code. Include whatever fixes you and codex agree on, with a test for each.", ""},
		{"opus", "codex", domain.MsgInformation, "Thanks. Put the agreed core fixes in the final header, with a test for each.", ""},
		{"codex", "scout", domain.MsgReview, "Your getOrCompute and hitRate drafts match the contract. I agree with all four core fixes.", "approve"},
		{"codex", "opus", domain.MsgStatusUpdate, "Scout and I agree on the member implementations and four necessary core fixes.", ""},
		{"codex", "scout", domain.MsgInformation, "Opus requests a test and design note for each of the four core fixes.", ""},
		{"codex", "opus", domain.MsgStatusUpdate, "I'll include all four core fixes and their tests in the joint result.", ""},
		{"scout", "codex", domain.MsgReview, "Your three members are correct, and I agree with your three core fixes.", "approve"},
	})
	if reason, over := orchestrator.LimitExceeded(got); over {
		t.Errorf("agents working together were paused: %s (stall count %d)", reason, got.StallCount)
	}
	if got.StallCount > 1 {
		t.Errorf("stall count = %d after a run of real work", got.StallCount)
	}
}

// TestRestatingThePointIsStillAStall: real agents rarely repeat themselves
// word for word. Changing a word or two must not hide a loop.
func TestRestatingThePointIsStillAStall(t *testing.T) {
	h, task := stallHarness(t)
	codex := "I still think the cache must refresh the TTL on overwrite because the documented contract says every put starts a fresh lifetime for the entry"
	scout := "The documented contract does not require refreshing the TTL on overwrite and doing so would hide stale entries from the purge loop entirely"
	var msgs []said
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			said{"codex", "scout", domain.MsgInformation, codex + fmt.Sprintf(" (point %d)", i%2), ""},
			said{"scout", "codex", domain.MsgInformation, scout + fmt.Sprintf(" (again %d)", i%2), ""})
	}
	got := replay(t, h, task, msgs)
	if reason, over := orchestrator.LimitExceeded(got); !over || reason != domain.PauseReasonStalled {
		t.Errorf("two agents restating the same points must stall, got %q (count %d)", reason, got.StallCount)
	}
}

func TestReportingAndDirectionAreNeutral(t *testing.T) {
	h, task := stallHarness(t)
	got := replay(t, h, task, []said{
		{"codex", "scout", domain.MsgInformation, "fyi one", ""},
		{"scout", "codex", domain.MsgInformation, "fyi two", ""},
	})
	if got.StallCount != 2 {
		t.Fatalf("remarks between implementers count toward a stall: %d", got.StallCount)
	}
	got = replay(t, h, got, []said{
		{"codex", "opus", domain.MsgStatusUpdate, "I am halfway through the tests.", ""},
		{"opus", "codex", domain.MsgInformation, "Good. Cover the zero-capacity case too.", ""},
	})
	if got.StallCount != 2 {
		t.Errorf("a status report and the authority's direction neither stall nor reset: %d", got.StallCount)
	}
}
