package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"ritam/internal/decisions"
	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/logging"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

// Runs started by the service (StartTask, Resume, FollowUp) go on in the
// background. These tests drive them with agents whose turn never ends on
// its own, the way a long CLI call does, so a run is reliably in progress
// when it is paused or shut down.

// stallAgent takes every message and starts a turn that lasts until the run
// is cancelled. Stop waits for the turn to end, as a real adapter waits for
// its process.
type stallAgent struct {
	id  domain.AgentID
	out chan ports.Outbound

	mu      sync.Mutex
	turns   int
	started chan struct{} // receives once per turn started
	running sync.WaitGroup
	stops   int
	// hold, when set, keeps Stop from returning until it is closed: an
	// agent whose process will not die.
	hold chan struct{}
}

func newStallAgent(id domain.AgentID) *stallAgent {
	return &stallAgent{id: id, out: make(chan ports.Outbound), started: make(chan struct{}, 16)}
}

func (a *stallAgent) ID() domain.AgentID              { return a.id }
func (a *stallAgent) Kind() domain.AgentKind          { return domain.AgentKindMock }
func (a *stallAgent) Start(context.Context) error     { return nil }
func (a *stallAgent) Interrupt(context.Context) error { return nil }
func (a *stallAgent) Status() domain.AgentState       { return domain.AgentStateIdle }
func (a *stallAgent) Outbound() <-chan ports.Outbound { return a.out }
func (a *stallAgent) Activity() <-chan ports.AgentActivity {
	return nil
}

func (a *stallAgent) turn(ctx context.Context) error {
	a.mu.Lock()
	a.turns++
	a.mu.Unlock()
	a.running.Add(1)
	go func() {
		defer a.running.Done()
		<-ctx.Done()
	}()
	a.started <- struct{}{}
	return nil
}

func (a *stallAgent) SendTask(ctx context.Context, _ *domain.Task, _ domain.Message) error {
	return a.turn(ctx)
}

func (a *stallAgent) SendMessage(ctx context.Context, _ *domain.Task, _ domain.Message) error {
	return a.turn(ctx)
}

func (a *stallAgent) Stop(context.Context) error {
	a.running.Wait()
	if a.hold != nil {
		<-a.hold
	}
	a.mu.Lock()
	a.stops++
	a.mu.Unlock()
	return nil
}

func (a *stallAgent) stopCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stops
}

// stallGateway is an orchestrator.AgentGateway over stall agents.
type stallGateway struct {
	agents map[domain.AgentID]*stallAgent
}

func (g *stallGateway) StartFor(ctx context.Context, _ domain.TaskID, id domain.AgentID) error {
	return g.agents[id].Start(ctx)
}
func (g *stallGateway) StopFor(ctx context.Context, _ domain.TaskID, id domain.AgentID) error {
	return g.agents[id].Stop(ctx)
}
func (g *stallGateway) Send(ctx context.Context, t *domain.Task, m domain.Message) error {
	return g.agents[m.ToAgent].SendMessage(ctx, t, m)
}
func (g *stallGateway) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return g.agents[m.ToAgent].SendTask(ctx, t, m)
}
func (g *stallGateway) AdapterFor(_ domain.TaskID, id domain.AgentID) (ports.AgentAdapter, bool) {
	a, ok := g.agents[id]
	return a, ok
}
func (g *stallGateway) ReportOutboundFor(domain.TaskID, domain.AgentID, ports.Outbound) {}
func (g *stallGateway) Statuses() []domain.Agent {
	var out []domain.Agent
	for id, a := range g.agents {
		out = append(out, domain.Agent{ID: id, Kind: a.Kind(), Enabled: true, State: a.Status()})
	}
	return out
}

// eventLog collects published events.
type eventLog struct {
	mu     sync.Mutex
	events []domain.Event
}

func (l *eventLog) add(e domain.Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) failures() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.events {
		if e.Type == domain.EvtTaskFailed {
			out = append(out, e.Summary)
		}
	}
	return out
}

// stallService is a Service over an in-memory orchestrator whose agents
// stall, with every event recorded.
func stallService(t *testing.T) (*Service, *stallAgent, *eventLog) {
	t.Helper()
	store := storage.NewInMemory()
	lead := newStallAgent("lead")
	gw := &stallGateway{agents: map[domain.AgentID]*stallAgent{"lead": lead, "helper": newStallAgent("helper")}}
	orch := orchestrator.New(orchestrator.Deps{
		Storage:            *store,
		Agents:             gw,
		Knowledge:          knowledge.New(store.Knowledge),
		Decisions:          decisions.New(store.Decisions),
		Logger:             logging.NewDiscard(),
		Limits:             domain.TaskLimits{MaxDeliberationRounds: 20, MaxMessages: 100, MaxDuration: time.Hour},
		MaxConcurrentTasks: 2,
		IdleTimeout:        time.Hour,
	})
	log := &eventLog{}
	orch.Events.Subscribe(log.add)
	return &Service{orch: orch, storage: *store}, lead, log
}

func stallTask() ports.CreateTaskInput {
	return ports.CreateTaskInput{Title: "a long task", Authority: "lead", Participants: []domain.AgentID{"lead", "helper"}}
}

// waitTurn waits for the authority's next turn to start.
func waitTurn(t *testing.T, a *stallAgent) {
	t.Helper()
	select {
	case <-a.started:
	case <-time.After(10 * time.Second):
		t.Fatal("no turn started")
	}
}

// waitStops waits for the agent to have been stopped n times, which happens
// as a run ends.
func waitStops(t *testing.T, a *stallAgent, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for a.stopCount() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the run did not end: %d stops, want %d", a.stopCount(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPausingAResumedRunIsNotAFailedResume(t *testing.T) {
	svc, lead, log := stallService(t)
	ctx := context.Background()

	task, err := svc.StartTask(ctx, stallTask())
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, lead)
	if err := svc.Pause(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitStops(t, lead, 1)

	if err := svc.Resume(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	waitTurn(t, lead)
	if err := svc.Pause(ctx, task.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Shutdown returns once every run, failure report included, is done.
	if err := shutdown(svc, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	for _, f := range log.failures() {
		if strings.Contains(f, "context canceled") {
			t.Errorf("a pause is not a failure: %q", f)
		}
	}
	got, _ := svc.GetTask(ctx, task.ID)
	if got.State != domain.TaskPaused {
		t.Errorf("state = %s, want PAUSED", got.State)
	}
}

// shutdown calls Service.Shutdown with a bounded wait.
func shutdown(svc *Service, wait time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	return svc.Shutdown(ctx)
}

// TestQuitDuringATurnWaitsForTheRun is quitting while an agent is mid-turn:
// Shutdown must not return until the run has stopped its agents and
// returned, since the database is closed right after it.
func TestQuitDuringATurnWaitsForTheRun(t *testing.T) {
	svc, lead, log := stallService(t)
	ctx := context.Background()
	task, err := svc.StartTask(ctx, stallTask())
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, lead)
	if got := svc.ActiveRuns(); len(got) != 1 || got[0] != task.ID {
		t.Fatalf("active runs = %v", got)
	}

	if err := shutdown(svc, 10*time.Second); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// No waiting here: by the time Shutdown returns the run is over.
	if lead.stopCount() != 1 {
		t.Error("Shutdown returned before the run stopped its agents")
	}
	if n := len(svc.ActiveRuns()); n != 0 {
		t.Errorf("%d runs still active after Shutdown", n)
	}
	if f := log.failures(); len(f) != 0 {
		t.Errorf("stopping for a quit is not a failure: %v", f)
	}

	if _, err := svc.StartTask(ctx, stallTask()); err == nil {
		t.Error("no new run may start once shutting down")
	}
	if err := svc.Resume(ctx, task.ID); err == nil {
		t.Error("no resume may start once shutting down")
	}
}

// TestShutdownSaysWhatIsStillRunningWhenTheWaitRunsOut: a run whose agent
// will not stop is not waited for forever.
func TestShutdownSaysWhatIsStillRunningWhenTheWaitRunsOut(t *testing.T) {
	svc, lead, _ := stallService(t)
	lead.hold = make(chan struct{})
	defer close(lead.hold)
	task, err := svc.StartTask(context.Background(), stallTask())
	if err != nil {
		t.Fatal(err)
	}
	waitTurn(t, lead)

	err = shutdown(svc, 100*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), string(task.ID)) {
		t.Fatalf("the error names the task still running, got %v", err)
	}
}
