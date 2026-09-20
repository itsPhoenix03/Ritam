package core_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"projectorch/internal/agents/core"
	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// TestOneAgentWorksOnTwoTasksAtOnce: each task gets its own instance of the
// agent, so stopping one task's run cannot stop the other's.
func TestOneAgentWorksOnTwoTasksAtOnce(t *testing.T) {
	reg := core.NewRegistry()
	var made []*mockagent.Adapter
	var mu sync.Mutex
	err := reg.RegisterFactory(domain.Agent{ID: "opus", Kind: domain.AgentKindMock, Enabled: true,
		State: domain.AgentStateStopped}, func() (ports.AgentAdapter, error) {
		ad := mockagent.New("opus", domain.AgentKindMock, mockagent.ImplementerBehavior())
		mu.Lock()
		made = append(made, ad)
		mu.Unlock()
		return ad, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mgr := core.NewManager(reg, nil)
	ctx := context.Background()
	if err := mgr.StartFor(ctx, "TASK-001", "opus"); err != nil {
		t.Fatal(err)
	}
	if err := mgr.StartFor(ctx, "TASK-002", "opus"); err != nil {
		t.Fatal(err)
	}
	a1, _ := mgr.AdapterFor("TASK-001", "opus")
	a2, _ := mgr.AdapterFor("TASK-002", "opus")
	if len(made) != 2 || a1 == a2 {
		t.Fatalf("each task needs its own instance: made %d, same=%v", len(made), a1 == a2)
	}

	msg := domain.Message{ID: "M1", TaskID: "TASK-002", FromAgent: "human", ToAgent: "opus", Type: domain.MsgDelegate, Content: "go"}
	if err := mgr.Send(ctx, &domain.Task{ID: "TASK-002"}, msg); err != nil {
		t.Fatal(err)
	}
	if len(made[1].Sent()) != 1 || len(made[0].Sent()) != 0 {
		t.Error("a message must reach the instance of its own task only")
	}

	if err := mgr.StopFor(ctx, "TASK-001", "opus"); err != nil {
		t.Fatal(err)
	}
	if a2.Status() == domain.AgentStateStopped {
		t.Error("stopping one task's instance must not stop another's")
	}
	if got := mgr.TaskStatuses("TASK-002")[0].State; got != domain.AgentStateIdle {
		t.Errorf("TASK-002's opus = %s, want IDLE", got)
	}
	if got := mgr.TaskStatuses("TASK-001")[0].State; got != domain.AgentStateStopped {
		t.Errorf("TASK-001's opus = %s, want STOPPED", got)
	}
}

// slowWorker is an adapter whose turn the test ends.
type slowWorker struct {
	mu    sync.Mutex
	state domain.AgentState
	out   chan ports.Outbound
}

func (s *slowWorker) ID() domain.AgentID          { return "codex" }
func (s *slowWorker) Kind() domain.AgentKind      { return domain.AgentKindMock }
func (s *slowWorker) Start(context.Context) error { s.set(domain.AgentStateIdle); return nil }
func (s *slowWorker) Stop(context.Context) error  { s.set(domain.AgentStateStopped); return nil }
func (s *slowWorker) SendTask(c context.Context, t *domain.Task, m domain.Message) error {
	return s.SendMessage(c, t, m)
}
func (s *slowWorker) SendMessage(context.Context, *domain.Task, domain.Message) error {
	s.set(domain.AgentStateWorking)
	return nil
}
func (s *slowWorker) Interrupt(context.Context) error      { return nil }
func (s *slowWorker) Outbound() <-chan ports.Outbound      { return s.out }
func (s *slowWorker) Activity() <-chan ports.AgentActivity { return nil }
func (s *slowWorker) Status() domain.AgentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
func (s *slowWorker) set(st domain.AgentState) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// TestAWorkingAgentIsShownWorking: the agent list never showed WORKING --
// the adapter knew, the registry did not. Instances are watched now.
func TestAWorkingAgentIsShownWorking(t *testing.T) {
	reg := core.NewRegistry()
	w := &slowWorker{out: make(chan ports.Outbound)}
	_ = reg.RegisterFactory(domain.Agent{ID: "codex", Enabled: true}, func() (ports.AgentAdapter, error) { return w, nil })
	mgr := core.NewManager(reg, nil)
	seen := make(chan domain.AgentState, 8)
	mgr.OnInstanceChange = func(task domain.TaskID, a domain.Agent) { seen <- a.State }
	ctx := context.Background()
	_ = mgr.StartFor(ctx, "TASK-001", "codex")
	_ = mgr.Send(ctx, &domain.Task{ID: "TASK-001"}, domain.Message{ToAgent: "codex", Type: domain.MsgDelegate})

	deadline := time.After(3 * time.Second)
	for {
		select {
		case s := <-seen:
			if s == domain.AgentStateWorking {
				if got := mgr.Statuses()[0].State; got != domain.AgentStateWorking {
					t.Errorf("the agent list shows %s while its instance works", got)
				}
				return
			}
		case <-deadline:
			t.Fatal("the instance going WORKING was never reported")
		}
	}
}
