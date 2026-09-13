package orchestrator_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/orchestrator"
	"projectorch/internal/ports"
)

// promptAgent stands in for a CLI agent: it reads a prompt (so it accepts
// combined deliveries), and the test decides when its turn is over.
type promptAgent struct {
	id       domain.AgentID
	mu       sync.Mutex
	state    domain.AgentState
	received []domain.Message
	out      chan ports.Outbound
}

func newPromptAgent(id domain.AgentID) *promptAgent {
	return &promptAgent{id: id, state: domain.AgentStateStopped, out: make(chan ports.Outbound, 16)}
}

func (p *promptAgent) ID() domain.AgentID     { return p.id }
func (p *promptAgent) Kind() domain.AgentKind { return domain.AgentKindMock }
func (p *promptAgent) Start(context.Context) error {
	p.setState(domain.AgentStateIdle)
	return nil
}
func (p *promptAgent) Stop(context.Context) error { p.setState(domain.AgentStateStopped); return nil }
func (p *promptAgent) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return p.SendMessage(ctx, t, m)
}

// SendMessage records the delivery and starts a turn: the agent is working
// until the test calls finish.
func (p *promptAgent) SendMessage(_ context.Context, _ *domain.Task, m domain.Message) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.received = append(p.received, m)
	p.state = domain.AgentStateWorking
	return nil
}
func (p *promptAgent) Interrupt(context.Context) error { return nil }
func (p *promptAgent) Status() domain.AgentState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}
func (p *promptAgent) Outbound() <-chan ports.Outbound      { return p.out }
func (p *promptAgent) Activity() <-chan ports.AgentActivity { return nil }
func (p *promptAgent) AcceptsBatches() bool                 { return true }

func (p *promptAgent) setState(s domain.AgentState) {
	p.mu.Lock()
	p.state = s
	p.mu.Unlock()
}
func (p *promptAgent) finish() { p.setState(domain.AgentStateIdle) }
func (p *promptAgent) deliveries() []domain.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]domain.Message(nil), p.received...)
}

type inboxRig struct {
	h                  *harness
	task               *domain.Task
	opus, codex, scout *promptAgent
}

func newInboxRig(t *testing.T) *inboxRig {
	t.Helper()
	r := &inboxRig{opus: newPromptAgent("opus"), codex: newPromptAgent("codex"), scout: newPromptAgent("scout")}
	gw := newGateway()
	gw.add(r.opus, domain.RoleAuthority)
	gw.add(r.codex, domain.RoleImplementer)
	gw.add(r.scout, domain.RoleImplementer)
	r.h = newHarness(t, defaultLimits(), time.Minute, gw)
	r.task = createTask(t, r.h, "opus", "opus", "codex", "scout")
	ctx := context.Background()
	for _, a := range []*promptAgent{r.opus, r.codex, r.scout} {
		_ = a.Start(ctx)
	}
	r.h.orch.OpenInbox(r.task.ID)
	return r
}

func (r *inboxRig) send(t *testing.T, from, to domain.AgentID, typ domain.MessageType, content string) {
	t.Helper()
	m := &domain.Message{FromAgent: from, ToAgent: to, Type: typ, Content: content}
	if from == domain.HumanAgentID {
		m.RequiresResponse = true
	}
	tk, _ := r.h.orch.Tasks.Get(context.Background(), r.task.ID)
	if err := r.h.orch.Bus.Route(context.Background(), tk, m); err != nil {
		t.Fatalf("route %s -> %s: %v", from, to, err)
	}
}

func (r *inboxRig) dispatch() { r.h.orch.Dispatch(context.Background(), r.task.ID) }

// TestABusyAgentGetsEverythingWaitingInOneTurn: the backlog that left codex
// 48 messages behind in a real run. Messages that arrive while an agent is
// working are handed over together when it is free, not one stale turn each.
func TestABusyAgentGetsEverythingWaitingInOneTurn(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "opus", "codex", domain.MsgImplement, "Write rev 1.")
	if got := len(r.codex.deliveries()); got != 1 {
		t.Fatalf("a free agent gets its message at once, got %d deliveries", got)
	}
	// codex is now working; three more arrive.
	r.send(t, "scout", "codex", domain.MsgProposal, "Here is my draft of cancel().")
	r.send(t, domain.HumanAgentID, "codex", domain.MsgInformation, "How is it going?")
	r.send(t, "opus", "codex", domain.MsgInformation, "Ruling: expiry consumes an attempt.")
	if got := len(r.codex.deliveries()); got != 1 {
		t.Fatalf("nothing may reach a working agent, got %d deliveries", got)
	}
	if b := r.h.orch.Backlog(r.task.ID); b["codex"] != 3 {
		t.Errorf("backlog = %v, want codex: 3", b)
	}

	r.codex.finish()
	r.dispatch()
	got := r.codex.deliveries()
	if len(got) != 2 {
		t.Fatalf("the three waiting messages should arrive as one turn, got %d deliveries", len(got))
	}
	batch := got[1]
	if batch.Metadata[ports.BatchMetadataKey] != "3" {
		t.Errorf("batch size = %q", batch.Metadata[ports.BatchMetadataKey])
	}
	if batch.Metadata[ports.BatchHumanMetadataKey] == "" || !strings.Contains(batch.Content, "[HUMAN] human") {
		t.Error("the human's message must be marked so it is answered first")
	}
	for _, want := range []string{"draft of cancel()", "How is it going?", "expiry consumes an attempt"} {
		if !strings.Contains(batch.Content, want) {
			t.Errorf("the combined turn is missing %q:\n%s", want, batch.Content)
		}
	}
	if strings.Index(batch.Content, "draft of cancel()") > strings.Index(batch.Content, "expiry consumes") {
		t.Error("messages must be laid out oldest first")
	}
	if board := batch.Metadata[ports.TeamMetadataKey]; !strings.Contains(board, "codex (you)") ||
		!strings.Contains(board, "opus (authority)") {
		t.Errorf("every delivery carries the team board:\n%s", board)
	}
}

// TestAnAgentWaitingOnACoWorkerIsNotWokenByChatter: scout asked codex for
// something and codex is still working on it. Remarks from others wait, so
// scout does not act on half the picture; they arrive with codex's answer.
func TestAnAgentWaitingOnACoWorkerIsNotWokenByChatter(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "scout", "codex", domain.MsgQuestion, "Which revision has the two-heap design?")
	// codex is working on it. opus sends scout a remark.
	r.send(t, "opus", "scout", domain.MsgInformation, "FYI codex is integrating rev 4.")
	if got := len(r.scout.deliveries()); got != 0 {
		t.Fatalf("scout is waiting on codex, who is still working: the remark must wait, got %d deliveries", got)
	}
	if !r.h.hasEvent(domain.EvtDeliveryHeld) {
		t.Error("a hold should be recorded")
	}

	r.codex.finish()
	r.send(t, "codex", "scout", domain.MsgAnswer, "Rev 4.")
	r.dispatch()
	got := r.scout.deliveries()
	if len(got) != 1 || got[0].Metadata[ports.BatchMetadataKey] != "2" {
		t.Fatalf("the answer and the held remark should arrive together, got %d deliveries %+v", len(got), got)
	}
}

func TestTheHumanIsNeverHeld(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "scout", "codex", domain.MsgQuestion, "Which revision?")
	r.send(t, domain.HumanAgentID, "scout", domain.MsgInformation, "What are you doing?")
	if got := len(r.scout.deliveries()); got != 1 {
		t.Fatalf("the human's message must go through a hold, got %d deliveries", got)
	}
}

func TestARequestToTheWaitingAgentGoesThrough(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "scout", "codex", domain.MsgQuestion, "Which revision?")
	r.send(t, "opus", "scout", domain.MsgDelegate, "Review rev 3's heap.")
	if got := len(r.scout.deliveries()); got != 1 {
		t.Fatalf("someone needs scout: holding it could stall them, got %d deliveries", got)
	}
}

func TestAHoldDoesNotLastForever(t *testing.T) {
	orchestrator.SetMaxHold(50 * time.Millisecond)
	t.Cleanup(func() { orchestrator.SetMaxHold(4 * time.Minute) })
	r := newInboxRig(t)
	r.send(t, "scout", "codex", domain.MsgQuestion, "Which revision?")
	r.send(t, "opus", "scout", domain.MsgInformation, "FYI.")
	if len(r.scout.deliveries()) != 0 {
		t.Fatal("expected a hold")
	}
	time.Sleep(80 * time.Millisecond)
	r.dispatch()
	if got := len(r.scout.deliveries()); got != 1 {
		t.Errorf("past maxHold the messages go through, got %d deliveries", got)
	}
}

func TestWaitingOnAnIdleCoWorkerIsNotAHold(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "scout", "codex", domain.MsgQuestion, "Which revision?")
	r.codex.finish() // codex finished without answering: no answer is coming
	r.send(t, "opus", "scout", domain.MsgInformation, "FYI.")
	if got := len(r.scout.deliveries()); got != 1 {
		t.Errorf("an idle co-worker will produce nothing to wait for, got %d deliveries", got)
	}
}

// TestTheTeamBoardShowsWhoWaitsOnWhom: every agent sees its co-workers'
// state, backlog and dependencies.
func TestTheTeamBoardShowsWhoWaitsOnWhom(t *testing.T) {
	r := newInboxRig(t)
	r.send(t, "opus", "codex", domain.MsgDelegate, "Integrate rev 4.")
	r.send(t, "scout", "codex", domain.MsgProposal, "My cancel() draft.") // waits: codex is busy
	r.send(t, domain.HumanAgentID, "scout", domain.MsgInformation, "Status?")
	got := r.scout.deliveries()
	if len(got) != 1 {
		t.Fatalf("scout should get the human's message, got %d", len(got))
	}
	board := got[0].Metadata[ports.TeamMetadataKey]
	for _, want := range []string{
		"codex: working · 1 unread · owes opus a reply (DELEGATE",
		"opus (authority): idle · waiting on codex (DELEGATE",
		"scout (you)",
		"human: waiting on scout",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("team board is missing %q:\n%s", want, board)
		}
	}
}
