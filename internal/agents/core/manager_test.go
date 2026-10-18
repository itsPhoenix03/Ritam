package core_test

import (
	"context"
	"testing"
	"time"

	"ritam/internal/agents/core"
	mockagent "ritam/internal/agents/mock"
	"ritam/internal/domain"
)

func newManager(t *testing.T, ids ...domain.AgentID) (*core.Manager, *core.Registry, map[domain.AgentID]*mockagent.Adapter) {
	t.Helper()
	reg := core.NewRegistry()
	ads := make(map[domain.AgentID]*mockagent.Adapter, len(ids))
	for _, id := range ids {
		ad := mockagent.New(id, domain.AgentKindMock, mockagent.ImplementerBehavior())
		ads[id] = ad
		a := domain.Agent{ID: id, DisplayName: string(id), Kind: domain.AgentKindMock, Role: domain.RoleImplementer, Enabled: true, State: domain.AgentStateStopped}
		if err := reg.Register(a, ad); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return core.NewManager(reg, nil), reg, ads
}

func TestManagerStartStopRestartTransitions(t *testing.T) {
	ctx := context.Background()
	mgr, _, ads := newManager(t, "opus")

	if got := mgr.Status("opus"); got != domain.AgentStateStopped {
		t.Fatalf("expected initial state Stopped, got %s", got)
	}

	if err := mgr.Start(ctx, "opus"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if got := mgr.Status("opus"); got != domain.AgentStateIdle {
		t.Fatalf("expected Idle after start, got %s", got)
	}
	if ads["opus"].Status() != domain.AgentStateIdle {
		t.Fatalf("adapter should be idle too")
	}

	if err := mgr.Stop(ctx, "opus"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := mgr.Status("opus"); got != domain.AgentStateStopped {
		t.Fatalf("expected Stopped after stop, got %s", got)
	}

	if err := mgr.Restart(ctx, "opus"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := mgr.Status("opus"); got != domain.AgentStateIdle {
		t.Fatalf("expected Idle after restart, got %s", got)
	}
}

func TestManagerStartAllStopAllNAgents(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManager(t, "opus", "codex", "reviewer")

	if err := mgr.StartAll(ctx); err != nil {
		t.Fatalf("start all: %v", err)
	}
	for _, a := range mgr.Statuses() {
		if a.State != domain.AgentStateIdle {
			t.Fatalf("agent %s not idle: %s", a.ID, a.State)
		}
	}

	if err := mgr.StopAll(ctx); err != nil {
		t.Fatalf("stop all: %v", err)
	}
	for _, a := range mgr.Statuses() {
		if a.State != domain.AgentStateStopped {
			t.Fatalf("agent %s not stopped: %s", a.ID, a.State)
		}
	}
}

func TestManagerSendRoutesToCorrectAdapter(t *testing.T) {
	ctx := context.Background()
	mgr, _, ads := newManager(t, "opus", "codex")
	_ = mgr.StartAll(ctx)

	msg := domain.Message{ID: domain.NewMessageID(), TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "go investigate"}
	if err := mgr.Send(ctx, nil, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	sentToCodex := ads["codex"].Sent()
	if len(sentToCodex) != 1 || sentToCodex[0].ToAgent != "codex" {
		t.Fatalf("expected codex to receive the message, got %+v", sentToCodex)
	}
	if len(ads["opus"].Sent()) != 0 {
		t.Fatalf("expected opus to receive nothing, got %+v", ads["opus"].Sent())
	}
}

func TestManagerSendUnknownAgentErrors(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManager(t, "opus")
	_ = mgr.StartAll(ctx)

	msg := domain.Message{ID: domain.NewMessageID(), ToAgent: "ghost", Type: domain.MsgDelegate}
	if err := mgr.Send(ctx, nil, msg); err == nil {
		t.Fatal("expected error routing to unknown agent")
	}
}

func TestManagerSendToStoppedAgentErrors(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newManager(t, "opus")
	// Never started: status is Stopped.
	msg := domain.Message{ID: domain.NewMessageID(), ToAgent: "opus", Type: domain.MsgDelegate}
	if err := mgr.Send(ctx, nil, msg); err == nil {
		t.Fatal("expected error sending to a non-running agent")
	}
}

func TestManagerCrashDetectionSetsAgentError(t *testing.T) {
	ctx := context.Background()
	reg := core.NewRegistry()
	crashing := mockagent.NewCrashing("codex")
	a := domain.Agent{ID: "codex", DisplayName: "codex", Kind: domain.AgentKindMock, Role: domain.RoleImplementer, Enabled: true}
	if err := reg.Register(a, crashing); err != nil {
		t.Fatalf("register: %v", err)
	}

	var changes []domain.Agent
	mgr := core.NewManager(reg, nil)
	mgr.OnStateChange = func(ag domain.Agent) { changes = append(changes, ag) }

	if err := mgr.Start(ctx, "codex"); err != nil {
		t.Fatalf("start: %v", err)
	}

	msg := domain.Message{ID: domain.NewMessageID(), TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "go"}
	if err := mgr.Send(ctx, nil, msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Drain the outbound channel like the orchestrator would, feeding any
	// error back into the manager for crash bookkeeping. CrashingBehavior
	// pushes both a protocol ERROR message and a raw Outbound.Err; keep
	// reading until we see the latter (or time out).
	var sawErr bool
	deadline := time.After(2 * time.Second)
	for !sawErr {
		select {
		case out := <-crashing.Outbound():
			if out.Err != nil {
				sawErr = true
				mgr.ReportOutbound("codex", out)
			}
		case <-deadline:
			t.Fatal("timed out waiting for outbound error")
		}
	}

	if got := mgr.Status("codex"); got != domain.AgentStateError && got != domain.AgentStateUnavailable {
		t.Fatalf("expected agent to be in an error state, got %s", got)
	}
	if len(changes) == 0 {
		t.Fatal("expected OnStateChange to fire")
	}
}
