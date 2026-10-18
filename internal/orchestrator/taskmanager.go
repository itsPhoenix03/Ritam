package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// TaskManager owns task records and the task state machine. It is the only
// component permitted to change a task's state, so every transition is
// validated in one place and produces exactly one event.
type TaskManager struct {
	repo   ports.TaskRepository
	events *EventBus
	log    *logging.Logger

	defaultLimits domain.TaskLimits
}

// NewTaskManager builds a TaskManager.
func NewTaskManager(repo ports.TaskRepository, events *EventBus, limits domain.TaskLimits, log *logging.Logger) *TaskManager {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &TaskManager{repo: repo, events: events, log: log, defaultLimits: limits}
}

// Create records a new task in state CREATED.
func (tm *TaskManager) Create(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("create task: title is required")
	}
	if in.Authority == "" {
		return nil, fmt.Errorf("create task: an authority agent is required")
	}

	participants := append([]domain.AgentID(nil), in.Participants...)
	if !contains(participants, in.Authority) {
		participants = append([]domain.AgentID{in.Authority}, participants...)
	}
	if len(participants) < 2 {
		// Refuse rather than warn. An authority-only task cannot deliberate:
		// the authority has nobody to delegate to, so the run loop sits idle
		// until the timeout fires and the task is paused for no useful reason.
		// Failing here puts the error in front of the human immediately.
		return nil, fmt.Errorf(
			"create task: %q has only the authority agent %q as a participant; "+
				"a task needs at least one supporting agent to deliberate with",
			in.Title, in.Authority)
	}
	if dup := firstDuplicate(participants); dup != "" {
		return nil, fmt.Errorf("create task: agent %q listed twice as a participant", dup)
	}

	seq, err := tm.repo.NextSequence(ctx)
	if err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	limits := tm.defaultLimits
	if in.Limits != nil {
		limits = *in.Limits
	}

	now := time.Now()
	t := &domain.Task{
		ID:             domain.FormatTaskID(seq),
		Title:          in.Title,
		Description:    in.Description,
		AuthorityAgent: in.Authority,
		Participants:   participants,
		State:          domain.TaskCreated,
		Limits:         limits,
		CreatedAt:      now,
		UpdatedAt:      now,
		SegmentStart:   now,
	}
	if err := tm.repo.Create(ctx, t); err != nil {
		return nil, fmt.Errorf("create task: %w", err)
	}

	tm.events.emit(ctx, domain.EvtTaskCreated, t.ID, domain.HumanAgentID,
		fmt.Sprintf("%s %q authority=%s", t.ID, t.Title, t.AuthorityAgent),
		map[string]string{"participants": joinAgents(t.Participants)})
	return t.Clone(), nil
}

// Get returns a copy of the task.
func (tm *TaskManager) Get(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.repo.Get(ctx, id)
}

// List returns all tasks.
func (tm *TaskManager) List(ctx context.Context) ([]*domain.Task, error) {
	return tm.repo.List(ctx)
}

// Transition moves a task to the next state, validating the transition table.
// An illegal transition is an error, never a silent no-op.
// ErrNotReopenable is returned when a task that is not COMPLETED is asked to
// reopen.
var ErrNotReopenable = errors.New("only a completed task can be reopened")

// Reopen moves a COMPLETED task back to DELIBERATING so a human can follow it
// up.
//
// It is deliberately not an entry in the transition table. That table governs
// what the run loop and the agents can cause, and nothing they do should ever
// bring a finished task back; a follow-up is a human decision, taken through
// this one method. The accepted decision is left untouched: if the follow-up
// produces a new FINAL_DECISION, it supersedes the old one, which stays on
// record.
func (tm *TaskManager) Reopen(ctx context.Context, id domain.TaskID, by domain.AgentID, reason string) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State != domain.TaskCompleted {
		return nil, fmt.Errorf("reopen %s (%s): %w", id, t.State, ErrNotReopenable)
	}
	from := t.State
	t.State = domain.TaskDeliberating
	t.CompletedAt = nil
	t.UpdatedAt = time.Now()
	t.FollowUp = true
	t.FollowUpWork = false
	resetAllowance(t)
	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}
	tm.events.emit(ctx, domain.EvtTaskStateChanged, t.ID, by,
		fmt.Sprintf("%s -> %s", from, t.State),
		map[string]string{"from": string(from), "to": string(t.State), "reason": reason})
	tm.events.emit(ctx, domain.EvtTaskReopened, t.ID, by, reason, nil)
	return t, nil
}

func (tm *TaskManager) Transition(ctx context.Context, id domain.TaskID, to domain.TaskState, by domain.AgentID, reason string) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State == to {
		return t, nil
	}
	if !domain.CanTransition(t.State, to) {
		return nil, domain.ErrInvalidTransition{From: t.State, To: to}
	}

	from := t.State
	if to == domain.TaskPaused {
		t.PreviousState = from
	}
	t.State = to
	t.UpdatedAt = time.Now()

	switch to {
	case domain.TaskCompleted:
		now := t.UpdatedAt
		t.CompletedAt = &now
		t.FollowUp = false
		t.FollowUpWork = false
	case domain.TaskFailed:
		t.FailureReason = reason
	case domain.TaskPaused:
		t.PauseReason = reason
	}

	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}

	tm.events.emit(ctx, domain.EvtTaskStateChanged, t.ID, by,
		fmt.Sprintf("%s -> %s", from, to),
		map[string]string{"from": string(from), "to": string(to), "reason": reason})

	switch to {
	case domain.TaskCompleted:
		tm.events.emit(ctx, domain.EvtTaskCompleted, t.ID, by, string(t.ID)+" completed", nil)
	case domain.TaskFailed:
		tm.events.emit(ctx, domain.EvtTaskFailed, t.ID, by, reason, nil)
	case domain.TaskCancelled:
		tm.events.emit(ctx, domain.EvtTaskCancelled, t.ID, by, reason, nil)
	case domain.TaskPaused:
		tm.events.emit(ctx, domain.EvtTaskPaused, t.ID, by, reason, nil)
	}
	return t.Clone(), nil
}

// Pause halts a task and records why a human is needed.
func (tm *TaskManager) Pause(ctx context.Context, id domain.TaskID, by domain.AgentID, reason string) (*domain.Task, error) {
	return tm.Transition(ctx, id, domain.TaskPaused, by, reason)
}

// Resume restores the state the task held before it was paused.
func (tm *TaskManager) Resume(ctx context.Context, id domain.TaskID, by domain.AgentID) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State != domain.TaskPaused {
		return nil, fmt.Errorf("resume %s: task is %s, not PAUSED", id, t.State)
	}
	prev := t.PreviousState
	if prev == "" || prev.IsTerminal() {
		prev = domain.TaskDeliberating
	}
	// Resuming is human input, so it starts a fresh allowance. Without this a
	// task paused by a limit was still over it, and re-paused on the very
	// next agent message.
	resetAllowance(t)
	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}
	out, err := tm.Transition(ctx, id, prev, by, "resumed")
	if err != nil {
		return nil, err
	}
	tm.events.emit(ctx, domain.EvtTaskResumed, id, by, "resumed at "+string(prev), nil)
	return out, nil
}

// Cancel terminates a task at the human's request.
func (tm *TaskManager) Cancel(ctx context.Context, id domain.TaskID, by domain.AgentID, reason string) (*domain.Task, error) {
	return tm.Transition(ctx, id, domain.TaskCancelled, by, reason)
}

// Fail terminates a task because it could not proceed.
func (tm *TaskManager) Fail(ctx context.Context, id domain.TaskID, by domain.AgentID, reason string) (*domain.Task, error) {
	return tm.Transition(ctx, id, domain.TaskFailed, by, reason)
}

// Progress is what an agent's message does to the stall count.
type Progress int

const (
	// NoProgress extends a stall: a repeat, or a remark that neither
	// produces, answers nor asks for anything.
	NoProgress Progress = iota
	// Neutral leaves the count alone: reporting (a status update) or the
	// authority directing the work. Neither is circling, and neither is a
	// result; the message allowance still bounds how many there can be.
	Neutral
	// MadeProgress ends a stall: a result, a review, an answer, a new request.
	MadeProgress
)

// NoteMessage records an agent's message against the task's counters.
//
// Only autonomous agent work is metered. A message to the human (an answer, a
// status update) neither counts toward the allowance nor toward a stall: the
// human asked, and answering is the point. p says whether the message moved
// the work on (see Orchestrator.isProgress); a run of messages that do not is
// a stall.
func (tm *TaskManager) NoteMessage(ctx context.Context, id domain.TaskID, m *domain.Message, p Progress) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	t.MessageCount++
	if m.ToAgent != domain.HumanAgentID {
		t.AgentMessages++
		if m.FromAgent == t.AuthorityAgent {
			t.Round++
			t.AgentRounds++
		}
		switch p {
		case MadeProgress:
			t.StallCount = 0
		case NoProgress:
			t.StallCount++
		}
	} else if p == MadeProgress {
		t.StallCount = 0
	}
	t.UpdatedAt = time.Now()
	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}
	return t.Clone(), nil
}

// NoteHumanInput starts a fresh allowance: the human has spoken, so whatever
// the agents do next is human-directed work, not a runaway.
func (tm *TaskManager) NoteHumanInput(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.update(ctx, id, resetAllowance)
}

// MarkCheckpoint records that a checkpoint was taken at the current round.
func (tm *TaskManager) MarkCheckpoint(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.LastCheckpoint = t.AgentRounds })
}

// AddUsage adds what one of agent's CLI turns cost to the task's totals,
// overall and for that agent.
func (tm *TaskManager) AddUsage(ctx context.Context, id domain.TaskID, agent domain.AgentID, u ports.Usage) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) {
		t.CostUSD += u.CostUSD
		t.InputTokens += u.InputTokens
		t.OutputTokens += u.OutputTokens
		if t.AgentUsage == nil {
			t.AgentUsage = map[domain.AgentID]domain.AgentUsage{}
		}
		a := t.AgentUsage[agent]
		a.Turns++
		a.CostUSD += u.CostUSD
		a.InputTokens += u.InputTokens
		a.OutputTokens += u.OutputTokens
		a.CacheReadTokens += u.CacheReadTokens
		t.AgentUsage[agent] = a
	})
}

// SetAuthority hands task authority to another participant.
func (tm *TaskManager) SetAuthority(ctx context.Context, id domain.TaskID, to domain.AgentID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.AuthorityAgent = to })
}

// MarkFollowUpWork records that the authority handed out new work during a
// follow-up, so the follow-up ends with a FINAL_DECISION rather than as soon
// as the requests are answered.
func (tm *TaskManager) MarkFollowUpWork(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.FollowUpWork = true })
}

func (tm *TaskManager) update(ctx context.Context, id domain.TaskID, f func(*domain.Task)) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	f(t)
	t.UpdatedAt = time.Now()
	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}
	return t.Clone(), nil
}

func resetAllowance(t *domain.Task) {
	t.AgentRounds = 0
	t.AgentMessages = 0
	t.StallCount = 0
	t.LastCheckpoint = 0
	t.SegmentStart = time.Now()
}

// LimitExceeded reports which hard limit, if any, the task has reached.
// Reaching one never picks a winner; the caller pauses for a human.
//
// There is deliberately no round limit here: rounds drive checkpoints
// (CheckpointDue), not pauses. A long task needs many rounds, and what is
// worth stopping is agents making no progress, running unattended too long,
// or spending too much.
func LimitExceeded(t *domain.Task) (string, bool) {
	l := t.Limits
	if l.MaxTotalMessages > 0 && t.MessageCount >= l.MaxTotalMessages {
		return domain.PauseReasonMaxTotal, true
	}
	if l.MaxCostUSD > 0 && t.CostUSD >= l.MaxCostUSD {
		return fmt.Sprintf("%s ($%.2f of $%.2f)", domain.PauseReasonBudget, t.CostUSD, l.MaxCostUSD), true
	}
	if l.MaxTokens > 0 && t.InputTokens+t.OutputTokens >= l.MaxTokens {
		return fmt.Sprintf("%s (%d of %d tokens)", domain.PauseReasonBudget, t.InputTokens+t.OutputTokens, l.MaxTokens), true
	}
	if l.StallMessages > 0 && t.StallCount >= l.StallMessages {
		return domain.PauseReasonStalled, true
	}
	if l.MaxMessages > 0 && t.AgentMessages >= l.MaxMessages {
		return domain.PauseReasonMaxMessages, true
	}
	start := t.SegmentStart
	if start.IsZero() {
		start = t.CreatedAt
	}
	if l.MaxDuration > 0 && time.Since(start) >= l.MaxDuration {
		return domain.PauseReasonMaxDuration, true
	}
	return "", false
}

// CheckpointDue reports whether the agents have worked another checkpoint
// interval of rounds without human input.
func CheckpointDue(t *domain.Task) bool {
	n := t.Limits.MaxDeliberationRounds
	return n > 0 && t.AgentRounds > 0 && t.AgentRounds >= t.LastCheckpoint+n
}

// StateForMessage maps a protocol message onto the task state the exchange
// implies. The mapping is intentionally loopy: REQUEST_CHANGE sends a task
// back to IMPLEMENTING and CHALLENGE sends it back to DELIBERATING, so agents
// can run any number of rounds instead of marching down a pipeline.
func StateForMessage(t MessageKind) domain.TaskState {
	switch domain.MessageType(t) {
	case domain.MsgTaskAssignment:
		return domain.TaskPlanning
	case domain.MsgDelegate, domain.MsgQuestion:
		return domain.TaskInvestigating
	case domain.MsgAnswer, domain.MsgInformation, domain.MsgProposal, domain.MsgChallenge:
		return domain.TaskDeliberating
	case domain.MsgImplement, domain.MsgRequestChange:
		return domain.TaskImplementing
	case domain.MsgImplementationResult, domain.MsgTestResult, domain.MsgReview:
		return domain.TaskReviewing
	case domain.MsgFinalDecision:
		return domain.TaskAwaitingDecision
	}
	return ""
}

// MessageKind is an alias used by StateForMessage so the mapping reads well at
// call sites without importing the domain package twice.
type MessageKind = domain.MessageType

// firstDuplicate returns the first agent listed more than once, or "" if the
// list is unique. A duplicated participant would be assigned the task twice.
func firstDuplicate(list []domain.AgentID) domain.AgentID {
	seen := make(map[domain.AgentID]bool, len(list))
	for _, id := range list {
		if seen[id] {
			return id
		}
		seen[id] = true
	}
	return ""
}

func contains(list []domain.AgentID, id domain.AgentID) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

func joinAgents(ids []domain.AgentID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = string(id)
	}
	return strings.Join(parts, ",")
}
