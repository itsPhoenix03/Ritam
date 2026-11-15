package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// TaskManager owns task records and the task state machine. It is the only
// component permitted to change a task's state, so every transition is
// validated in one place and produces exactly one event.
//
// Every change to an existing task goes through change: the run loop and
// the UI change the same task at the same time, and a change made from a
// copy read before the other's write used to undo it.
type TaskManager struct {
	repo   ports.TaskRepository
	events *EventBus
	log    *logging.Logger

	defaultLimits domain.TaskLimits

	locksMu sync.Mutex
	locks   map[domain.TaskID]*sync.Mutex
}

// NewTaskManager builds a TaskManager.
func NewTaskManager(repo ports.TaskRepository, events *EventBus, limits domain.TaskLimits, log *logging.Logger) *TaskManager {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &TaskManager{repo: repo, events: events, log: log, defaultLimits: limits,
		locks: map[domain.TaskID]*sync.Mutex{}}
}

// maxChangeAttempts bounds how often change re-reads a task that another
// writer changed under it. The per-task lock leaves only writers outside
// this TaskManager to race with, so running out means something else
// rewrites the task without pause.
const maxChangeAttempts = 8

// errNoChange, returned by a change's function, means there is nothing to
// write: change returns the task as it was read.
var errNoChange = errors.New("no change")

// lockTask serialises this TaskManager's changes to one task.
func (tm *TaskManager) lockTask(id domain.TaskID) (unlock func()) {
	tm.locksMu.Lock()
	l, ok := tm.locks[id]
	if !ok {
		l = &sync.Mutex{}
		tm.locks[id] = l
	}
	tm.locksMu.Unlock()
	l.Lock()
	return l.Unlock
}

// change is the one way an existing task is written: read it, apply f, and
// write it back, all under the task's lock. The lock is the cheap first
// line; the storage's version check is the guarantee -- a write from a copy
// that someone else changed in the meantime is refused, and f is applied
// again to a fresh read. f may therefore run more than once, and must
// derive everything it does from the task it is given.
//
// UpdatedAt is set before f runs, so f can use it as "now".
func (tm *TaskManager) change(ctx context.Context, id domain.TaskID, f func(t *domain.Task) error) (*domain.Task, error) {
	unlock := tm.lockTask(id)
	defer unlock()
	var conflict error
	for attempt := 0; attempt < maxChangeAttempts; attempt++ {
		t, err := tm.repo.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		read := t.UpdatedAt
		t.UpdatedAt = time.Now()
		if err := f(t); err != nil {
			if errors.Is(err, errNoChange) {
				t.UpdatedAt = read
				return t, nil
			}
			return nil, err
		}
		err = tm.repo.Update(ctx, t)
		if errors.Is(err, ports.ErrConflict) {
			conflict = err
			continue
		}
		if err != nil {
			return nil, err
		}
		return t.Clone(), nil
	}
	return nil, fmt.Errorf("task %s kept changing during an update (%d attempts): %w", id, maxChangeAttempts, conflict)
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
	for _, p := range participants {
		// An agent under one of these ids would be taken for the human or
		// the orchestrator, whose messages pass checks an agent's do not.
		if domain.IsReservedAgentID(p) {
			return nil, fmt.Errorf("create task: %q is reserved and cannot be an agent's id", p)
		}
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
		Builder:        in.Builder,
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
	var from domain.TaskState
	t, err := tm.change(ctx, id, func(t *domain.Task) error {
		if t.State != domain.TaskCompleted {
			return fmt.Errorf("reopen %s (%s): %w", id, t.State, ErrNotReopenable)
		}
		from = t.State
		t.State = domain.TaskDeliberating
		t.CompletedAt = nil
		t.FollowUp = true
		t.FollowUpWork = false
		t.Closing = false
		resetAllowance(t)
		return nil
	})
	if err != nil {
		return nil, err
	}
	tm.events.emit(ctx, domain.EvtTaskStateChanged, t.ID, by,
		fmt.Sprintf("%s -> %s", from, t.State),
		map[string]string{"from": string(from), "to": string(t.State), "reason": reason})
	tm.events.emit(ctx, domain.EvtTaskReopened, t.ID, by, reason, nil)
	return t, nil
}

func (tm *TaskManager) Transition(ctx context.Context, id domain.TaskID, to domain.TaskState, by domain.AgentID, reason string) (*domain.Task, error) {
	var from domain.TaskState
	t, err := tm.change(ctx, id, func(t *domain.Task) error {
		from = ""
		if t.State == to {
			return errNoChange
		}
		var err error
		from, err = applyTransition(t, to, reason)
		return err
	})
	if err != nil {
		return nil, err
	}
	if from == "" {
		return t, nil
	}
	tm.emitTransition(ctx, t.ID, from, to, by, reason)
	return t, nil
}

// applyTransition moves t to the state to, validating the transition table,
// and returns the state it left. It only changes t; the caller writes it.
func applyTransition(t *domain.Task, to domain.TaskState, reason string) (domain.TaskState, error) {
	if !domain.CanTransition(t.State, to) {
		return "", domain.ErrInvalidTransition{From: t.State, To: to}
	}
	from := t.State
	if to == domain.TaskPaused {
		t.PreviousState = from
	}
	t.State = to
	if to.IsTerminal() {
		// Nothing is left to wait for once the task has ended.
		t.Closing = false
	}

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
	return from, nil
}

// emitTransition reports a state change once it is written.
func (tm *TaskManager) emitTransition(ctx context.Context, id domain.TaskID, from, to domain.TaskState, by domain.AgentID, reason string) {
	tm.events.emit(ctx, domain.EvtTaskStateChanged, id, by,
		fmt.Sprintf("%s -> %s", from, to),
		map[string]string{"from": string(from), "to": string(to), "reason": reason})

	switch to {
	case domain.TaskCompleted:
		tm.events.emit(ctx, domain.EvtTaskCompleted, id, by, string(id)+" completed", nil)
	case domain.TaskFailed:
		tm.events.emit(ctx, domain.EvtTaskFailed, id, by, reason, nil)
	case domain.TaskCancelled:
		tm.events.emit(ctx, domain.EvtTaskCancelled, id, by, reason, nil)
	case domain.TaskPaused:
		tm.events.emit(ctx, domain.EvtTaskPaused, id, by, reason, nil)
	}
}

// Pause halts a task and records why a human is needed.
func (tm *TaskManager) Pause(ctx context.Context, id domain.TaskID, by domain.AgentID, reason string) (*domain.Task, error) {
	return tm.Transition(ctx, id, domain.TaskPaused, by, reason)
}

// Resume restores the state the task held before it was paused.
//
// The fresh allowance and the state change are one write: as two, a failed
// or refused state change left the allowance reset on a task still paused.
func (tm *TaskManager) Resume(ctx context.Context, id domain.TaskID, by domain.AgentID) (*domain.Task, error) {
	var prev domain.TaskState
	t, err := tm.change(ctx, id, func(t *domain.Task) error {
		if t.State != domain.TaskPaused {
			return fmt.Errorf("resume %s: task is %s, not PAUSED", id, t.State)
		}
		prev = t.PreviousState
		if prev == "" || prev.IsTerminal() || prev == domain.TaskPaused {
			prev = domain.TaskDeliberating
		}
		// Resuming is human input, so it starts a fresh allowance. Without
		// this a task paused by a limit was still over it, and re-paused on
		// the very next agent message.
		resetAllowance(t)
		_, err := applyTransition(t, prev, "resumed")
		return err
	})
	if err != nil {
		return nil, err
	}
	tm.emitTransition(ctx, id, domain.TaskPaused, prev, by, "resumed")
	tm.events.emit(ctx, domain.EvtTaskResumed, id, by, "resumed at "+string(prev), nil)
	return t, nil
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
	return tm.update(ctx, id, func(t *domain.Task) {
		t.MessageCount++
		// A readable message ends the sender's run of unreadable ones.
		delete(t.MalformedCount, m.FromAgent)
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
	})
}

// NoteMalformed records a reply from agent that could not be read. It extends
// the agent's run of unreadable replies and, since nothing moved, the stall:
// without this an agent that never produced a readable reply was re-prompted
// for ever with every counter still at zero.
func (tm *TaskManager) NoteMalformed(ctx context.Context, id domain.TaskID, agent domain.AgentID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) {
		if t.MalformedCount == nil {
			t.MalformedCount = map[domain.AgentID]int{}
		}
		t.MalformedCount[agent]++
		t.StallCount++
	})
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

// SetClosing records whether the task's decision is accepted and it waits
// only for the human's open questions to be answered (see domain.Task).
func (tm *TaskManager) SetClosing(ctx context.Context, id domain.TaskID, closing bool) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.Closing = closing })
}

// MarkFollowUpWork records that the authority handed out new work during a
// follow-up, so the follow-up ends with a FINAL_DECISION rather than as soon
// as the requests are answered.
func (tm *TaskManager) MarkFollowUpWork(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.FollowUpWork = true })
}

// update is change for a function that cannot fail.
func (tm *TaskManager) update(ctx context.Context, id domain.TaskID, f func(*domain.Task)) (*domain.Task, error) {
	return tm.change(ctx, id, func(t *domain.Task) error {
		f(t)
		return nil
	})
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

// timeOrBudgetExceeded reports whether the task has run past its duration or
// spending limit. Unlike the message limits, these do not depend on anything
// being routed, so they are checked on every outbound: a task whose agents
// only ever sent unreadable or refused replies used to run past MaxDuration
// unchecked.
func timeOrBudgetExceeded(t *domain.Task) (string, bool) {
	l := t.Limits
	if l.MaxCostUSD > 0 && t.CostUSD >= l.MaxCostUSD {
		return fmt.Sprintf("%s ($%.2f of $%.2f)", domain.PauseReasonBudget, t.CostUSD, l.MaxCostUSD), true
	}
	if l.MaxTokens > 0 && t.InputTokens+t.OutputTokens >= l.MaxTokens {
		return fmt.Sprintf("%s (%d of %d tokens)", domain.PauseReasonBudget, t.InputTokens+t.OutputTokens, l.MaxTokens), true
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

// DropAgent records that agent was taken off the task, and why (see
// domain.Task.Dropped).
func (tm *TaskManager) DropAgent(ctx context.Context, id domain.TaskID, agent domain.AgentID, why string) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) {
		if t.Dropped == nil {
			t.Dropped = map[domain.AgentID]string{}
		}
		t.Dropped[agent] = why
	})
}

// ClearDropped puts every dropped agent back on the task: the human resuming
// it gives each a fresh chance.
func (tm *TaskManager) ClearDropped(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	return tm.update(ctx, id, func(t *domain.Task) { t.Dropped = nil })
}

// ErrNotRetryable is returned when a task that is not FAILED is asked to
// retry.
var ErrNotRetryable = errors.New("only a failed task can be retried")

// Retry brings a FAILED task back to DELIBERATING, at the human's request,
// with the same participants. FAILED was terminal: a task failed by one
// agent's timeout (or by an older version of ritam) could only be abandoned.
//
// Like Reopen it is not an entry in the transition table, which governs what
// the run loop and the agents can cause; only the human brings a failed task
// back. The failure reason is kept on record in the events.
func (tm *TaskManager) Retry(ctx context.Context, id domain.TaskID, by domain.AgentID) (*domain.Task, error) {
	t, err := tm.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if t.State != domain.TaskFailed {
		return nil, fmt.Errorf("retry %s (%s): %w", id, t.State, ErrNotRetryable)
	}
	from, reason := t.State, t.FailureReason
	t.State = domain.TaskDeliberating
	t.FailureReason = ""
	t.Dropped = nil
	t.MalformedCount = nil
	t.UpdatedAt = time.Now()
	resetAllowance(t)
	if err := tm.repo.Update(ctx, t); err != nil {
		return nil, err
	}
	tm.events.emit(ctx, domain.EvtTaskStateChanged, t.ID, by,
		fmt.Sprintf("%s -> %s", from, t.State),
		map[string]string{"from": string(from), "to": string(t.State), "reason": "retried by human"})
	tm.events.emit(ctx, domain.EvtTaskResumed, t.ID, by, "retried after failing: "+reason, nil)
	return t.Clone(), nil
}
