// Package app is the application service layer. It is the only thing the TUI
// talks to, keeping the call chain TUI -> application service -> domain
// instead of routing the user interface through HTTP or through the domain
// directly.
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/orchestrator"
	"ritam/internal/ports"
)

// Service implements ports.Service on top of the orchestrator. It holds no
// task state of its own: the orchestrator remains the source of truth. What
// it does hold is the set of runs it started in the background, so they can
// be stopped and waited for before the program exits.
type Service struct {
	orch      *orchestrator.Orchestrator
	storage   ports.Storage
	knowledge *knowledge.Store
	team      *Team
	access    *accessControl

	runs runSet
}

// runSet tracks the background runs (StartTask, Resume, FollowUp). Its zero
// value is ready to use.
//
// Every run is cancelled through one context, base, so Shutdown stops them
// all; and each is counted, so Shutdown can wait for them. Before this a run
// was detached from every context (context.WithoutCancel) and nobody waited
// for it: quitting closed the database under runs still writing to it.
type runSet struct {
	mu      sync.Mutex
	base    context.Context
	stop    context.CancelFunc
	closed  bool
	wg      sync.WaitGroup
	running map[domain.TaskID]int
}

// errShuttingDown refuses a run asked for while the service is stopping.
var errShuttingDown = errors.New("ritam is shutting down: no new run starts")

// accepting reports whether a new run may still start.
func (s *Service) accepting() error {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	if s.runs.closed {
		return errShuttingDown
	}
	return nil
}

// goRun runs task id on a tracked goroutine. The run outlives the caller's
// context (a request's context ends with the request) but not Shutdown, and
// a pause or a cancel stopping it is a normal end. Any other error is
// published as the task failing, prefixed with what the run was.
func (s *Service) goRun(ctx context.Context, id domain.TaskID, what string) error {
	r := &s.runs
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return errShuttingDown
	}
	if r.base == nil {
		r.base, r.stop = context.WithCancel(context.Background())
		r.running = map[domain.TaskID]int{}
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	unhook := context.AfterFunc(r.base, cancel)
	r.wg.Add(1)
	r.running[id]++
	r.mu.Unlock()

	go func() {
		defer r.wg.Done()
		defer func() {
			unhook()
			cancel()
			r.mu.Lock()
			if r.running[id]--; r.running[id] <= 0 {
				delete(r.running, id)
			}
			r.mu.Unlock()
		}()
		if err := s.orch.RunTask(runCtx, id); err != nil && !errors.Is(err, context.Canceled) {
			s.orch.Events.Publish(context.WithoutCancel(ctx), domain.Event{
				Type: domain.EvtTaskFailed, TaskID: id, Summary: what + " failed: " + err.Error(),
			})
		}
	}()
	return nil
}

// ActiveRuns lists the tasks with a background run going.
func (s *Service) ActiveRuns() []domain.TaskID {
	s.runs.mu.Lock()
	defer s.runs.mu.Unlock()
	out := make([]domain.TaskID, 0, len(s.runs.running))
	for id := range s.runs.running {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Shutdown stops every background run and waits for each to return, so
// nothing is still writing when the storage closes. No new run starts once
// it is called. If ctx ends first it says which tasks were still running;
// their state is then whatever the run last wrote, and the next start pauses
// them as interrupted.
func (s *Service) Shutdown(ctx context.Context) error {
	r := &s.runs
	r.mu.Lock()
	r.closed = true
	if r.stop != nil {
		r.stop()
	}
	r.mu.Unlock()

	done := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		ids := s.ActiveRuns()
		names := make([]string, len(ids))
		for i, id := range ids {
			names[i] = string(id)
		}
		return fmt.Errorf("stopping agents: %s still running after the wait (%v)", strings.Join(names, ", "), ctx.Err())
	}
}

// NewService builds the application service.
func NewService(orch *orchestrator.Orchestrator, storage ports.Storage, k *knowledge.Store) *Service {
	return &Service{orch: orch, storage: storage, knowledge: k}
}

var _ ports.Service = (*Service)(nil)

// CreateTask records a new task without starting it.
func (s *Service) CreateTask(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	if err := s.prepareBuilder(ctx, in); err != nil {
		return nil, err
	}
	t, err := s.orch.CreateTask(ctx, in)
	if err == nil && s.access != nil {
		s.access.recordStart(t)
	}
	return t, err
}

// RunTask drives a task's deliberation. It blocks until the task reaches a
// terminal state, is paused, or the context is cancelled, so callers run it on
// their own goroutine.
func (s *Service) RunTask(ctx context.Context, id domain.TaskID) error {
	return s.orch.RunTask(ctx, id)
}

// StartTask creates a task and runs it on a tracked goroutine, returning as
// soon as the task is recorded. The run's outcome reaches the caller as
// events, the same way a resumed run's does.
func (s *Service) StartTask(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	if err := s.accepting(); err != nil {
		return nil, err
	}
	t, err := s.CreateTask(ctx, in)
	if err != nil {
		return nil, err
	}
	if err := s.goRun(ctx, t.ID, "run"); err != nil {
		return t, err
	}
	return t, nil
}

// ListTasks returns every task.
func (s *Service) ListTasks(ctx context.Context) ([]*domain.Task, error) {
	return s.storage.Tasks.List(ctx)
}

// GetTask returns one task.
func (s *Service) GetTask(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return s.storage.Tasks.Get(ctx, id)
}

// TaskMessages returns the routed message history of a task.
func (s *Service) TaskMessages(ctx context.Context, id domain.TaskID) ([]*domain.Message, error) {
	return s.storage.Messages.ListByTask(ctx, id)
}

// TaskEvents returns the chronological event log of a task.
func (s *Service) TaskEvents(ctx context.Context, id domain.TaskID) ([]domain.Event, error) {
	return s.storage.Events.ListByTask(ctx, id)
}

// TaskDecisions returns every decision recorded for a task, including
// superseded ones: decision history is never discarded.
func (s *Service) TaskDecisions(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error) {
	return s.storage.Decisions.ListByTask(ctx, id)
}

// TaskTurns lists every agent turn of a task: the messages each was handed,
// how it ended and what it cost.
func (s *Service) TaskTurns(ctx context.Context, id domain.TaskID) ([]*domain.Turn, error) {
	if s.storage.Turns == nil {
		return nil, nil
	}
	return s.storage.Turns.ListByTask(ctx, id)
}

// TaskDisagreements returns the explicit conflicts raised during a task,
// resolved and unresolved. Disagreement is a first-class outcome: it is
// recorded rather than smoothed away.
//
// This is deliberately not part of ports.Service yet — the TUI has no
// disagreement pane. TODO(phase-next): surface it in the UI and promote it to
// the interface.
func (s *Service) TaskDisagreements(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	return s.storage.Disagreements.ListByTask(ctx, id)
}

// Agents returns the registered agents and their runtime states.
func (s *Service) Agents(_ context.Context) ([]domain.Agent, error) {
	return s.orch.Agents.Statuses(), nil
}

// TaskAgents returns the agents as they stand on one task.
func (s *Service) TaskAgents(ctx context.Context, id domain.TaskID) ([]domain.Agent, error) {
	if ts, ok := s.orch.Agents.(interface {
		TaskStatuses(domain.TaskID) []domain.Agent
	}); ok {
		return ts.TaskStatuses(id), nil
	}
	return s.Agents(ctx)
}

// Models lists every model each provider's CLI can run.
func (s *Service) Models(ctx context.Context) ([]ports.ModelInfo, error) {
	if s.team == nil {
		return nil, ports.ErrNotImplemented
	}
	return s.team.Models(ctx)
}

// Recruit adds an agent running a chosen model to the team.
func (s *Service) Recruit(ctx context.Context, in ports.RecruitInput) (domain.Agent, error) {
	if s.team == nil {
		return domain.Agent{}, ports.ErrNotImplemented
	}
	return s.team.Recruit(ctx, in)
}

// Dismiss removes a recruited agent.
func (s *Service) Dismiss(ctx context.Context, id domain.AgentID) error {
	if s.team == nil {
		return ports.ErrNotImplemented
	}
	return s.team.Dismiss(ctx, id)
}

// CheckModel makes one short call to see whether this account can run a model.
func (s *Service) CheckModel(ctx context.Context, provider, model string) (ports.ModelInfo, error) {
	if s.team == nil {
		return ports.ModelInfo{}, ports.ErrNotImplemented
	}
	return s.team.CheckModel(ctx, provider, model)
}

// Pause halts a task.
func (s *Service) Pause(ctx context.Context, id domain.TaskID, reason string) error {
	return s.orch.Pause(ctx, id, reason)
}

// Resume restores a paused task and continues its deliberation.
func (s *Service) Resume(ctx context.Context, id domain.TaskID) error {
	if err := s.accepting(); err != nil {
		return err
	}
	if err := s.orch.Resume(ctx, id); err != nil {
		return err
	}
	// Resuming means the task starts deliberating again; do it off the
	// caller's goroutine so the UI stays responsive. A pause or a cancel
	// ends the run by cancelling it: that is the run stopping as asked, not
	// a resume that failed (goRun publishes only other errors).
	return s.goRun(ctx, id, "resume")
}

// Retry brings a failed task back (same participants, their sessions kept)
// and continues its deliberation, the authority told why it had failed.
func (s *Service) Retry(ctx context.Context, id domain.TaskID) error {
	if err := s.accepting(); err != nil {
		return err
	}
	if err := s.orch.Retry(ctx, id); err != nil {
		return err
	}
	return s.goRun(ctx, id, "retry")
}

// RaiseLimits raises a task's message ceiling or spending limits.
func (s *Service) RaiseLimits(ctx context.Context, id domain.TaskID, more domain.TaskLimits) error {
	return s.orch.RaiseLimits(ctx, id, more)
}

// Cancel terminates a task.
func (s *Service) Cancel(ctx context.Context, id domain.TaskID, reason string) error {
	return s.orch.Cancel(ctx, id, reason)
}

// Approve accepts the pending final decision.
func (s *Service) Approve(ctx context.Context, id domain.TaskID, note string) error {
	return s.orch.Approve(ctx, id, note)
}

// Reject refuses the pending final decision and returns the task to
// deliberation.
func (s *Service) Reject(ctx context.Context, id domain.TaskID, note string) error {
	if err := s.orch.Reject(ctx, id, note); err != nil {
		return err
	}
	// With no run alive the REJECT waits for one; start it off the
	// caller's goroutine, as Resume does. Without this a rejected task sat
	// in DELIBERATING with nothing driving it. It is a tracked run like any
	// other, so quitting stops it and waits for it.
	if s.orch.Running(id) {
		return nil
	}
	return s.goRun(ctx, id, "reject")
}

// FollowUp reopens a completed task with content addressed to agent to, and
// runs it again off the caller's goroutine, as Resume does.
func (s *Service) FollowUp(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	if err := s.accepting(); err != nil {
		return err
	}
	if err := s.orch.Reopen(ctx, id, to, content); err != nil {
		return err
	}
	return s.goRun(ctx, id, "follow-up")
}

// SendInstruction injects a human message into a live task.
func (s *Service) SendInstruction(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	if err := s.orch.SendInstruction(ctx, id, to, content); err != nil {
		return err
	}
	// A task with no run alive (a decision held for the human) has the
	// message queued; start the run that delivers it, as Resume does, and
	// tracked like every other run so quitting stops it and waits for it.
	if s.orch.Running(id) {
		return nil
	}
	return s.goRun(ctx, id, "instruction")
}

// SearchKnowledge queries shared project knowledge.
// SetAuthority hands a task's authority to another participant.
func (s *Service) SetAuthority(ctx context.Context, id domain.TaskID, to domain.AgentID) error {
	return s.orch.SetAuthority(ctx, id, to)
}

// Backlog reports how many messages wait undelivered for each agent.
func (s *Service) Backlog(id domain.TaskID) map[domain.AgentID]int {
	return s.orch.Backlog(id)
}

func (s *Service) SearchKnowledge(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	return s.knowledge.Search(ctx, q)
}

// Subscribe registers an event handler and returns an unsubscribe function.
func (s *Service) Subscribe(h domain.EventHandler) func() {
	return s.orch.Events.Subscribe(h)
}
