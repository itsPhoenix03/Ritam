// Package app is the application service layer. It is the only thing the TUI
// talks to, keeping the call chain TUI -> application service -> domain
// instead of routing the user interface through HTTP or through the domain
// directly.
package app

import (
	"context"

	"projectorch/internal/domain"
	"projectorch/internal/knowledge"
	"projectorch/internal/orchestrator"
	"projectorch/internal/ports"
)

// Service implements ports.Service on top of the orchestrator. It holds no
// state of its own: the orchestrator remains the source of truth.
type Service struct {
	orch      *orchestrator.Orchestrator
	storage   ports.Storage
	knowledge *knowledge.Store
	team      *Team
}

// NewService builds the application service.
func NewService(orch *orchestrator.Orchestrator, storage ports.Storage, k *knowledge.Store) *Service {
	return &Service{orch: orch, storage: storage, knowledge: k}
}

var _ ports.Service = (*Service)(nil)

// CreateTask records a new task without starting it.
func (s *Service) CreateTask(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	return s.orch.CreateTask(ctx, in)
}

// RunTask drives a task's deliberation. It blocks until the task reaches a
// terminal state, is paused, or the context is cancelled, so callers run it on
// their own goroutine.
func (s *Service) RunTask(ctx context.Context, id domain.TaskID) error {
	return s.orch.RunTask(ctx, id)
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
	if err := s.orch.Resume(ctx, id); err != nil {
		return err
	}
	// Resuming means the task starts deliberating again; do it off the
	// caller's goroutine so the UI stays responsive.
	go func() {
		if err := s.orch.RunTask(context.WithoutCancel(ctx), id); err != nil {
			s.orch.Events.Publish(context.WithoutCancel(ctx), domain.Event{
				Type: domain.EvtTaskFailed, TaskID: id, Summary: "resume failed: " + err.Error(),
			})
		}
	}()
	return nil
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
	return s.orch.Reject(ctx, id, note)
}

// FollowUp reopens a completed task with content addressed to agent to, and
// runs it again off the caller's goroutine, as Resume does.
func (s *Service) FollowUp(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	if err := s.orch.Reopen(ctx, id, to, content); err != nil {
		return err
	}
	go func() {
		if err := s.orch.RunTask(context.WithoutCancel(ctx), id); err != nil {
			s.orch.Events.Publish(context.WithoutCancel(ctx), domain.Event{
				Type: domain.EvtTaskFailed, TaskID: id, Summary: "follow-up failed: " + err.Error(),
			})
		}
	}()
	return nil
}

// SendInstruction injects a human message into a live task.
func (s *Service) SendInstruction(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	return s.orch.SendInstruction(ctx, id, to, content)
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
