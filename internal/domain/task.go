package domain

import (
	"fmt"
	"time"
)

// TaskState is the state of a task in the orchestration lifecycle.
type TaskState string

const (
	TaskCreated          TaskState = "CREATED"
	TaskPlanning         TaskState = "PLANNING"
	TaskInvestigating    TaskState = "INVESTIGATING"
	TaskImplementing     TaskState = "IMPLEMENTING"
	TaskReviewing        TaskState = "REVIEWING"
	TaskDeliberating     TaskState = "DELIBERATING"
	TaskAwaitingDecision TaskState = "AWAITING_DECISION"
	TaskPaused           TaskState = "PAUSED"
	TaskCompleted        TaskState = "COMPLETED"
	TaskFailed           TaskState = "FAILED"
	TaskCancelled        TaskState = "CANCELLED"
)

// IsTerminal reports whether no further transitions are possible.
func (s TaskState) IsTerminal() bool {
	switch s {
	case TaskCompleted, TaskFailed, TaskCancelled:
		return true
	}
	return false
}

// transitions is the explicit, authoritative transition table. The lifecycle is
// deliberately not a linear pipeline: review can send work back to
// implementation, and deliberation can send work back to investigation, so
// agents may run any number of rounds.
var transitions = map[TaskState][]TaskState{
	TaskCreated: {TaskPlanning, TaskPaused, TaskCancelled, TaskFailed},
	// AWAITING_DECISION is reachable from every working state. The authority
	// decides when it has enough evidence, and real deliberations frequently
	// get there without passing through implementation or review at all — a
	// question answered in one round needs no code change.
	TaskPlanning: {
		TaskInvestigating, TaskImplementing, TaskDeliberating, TaskAwaitingDecision,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	TaskInvestigating: {
		TaskPlanning, TaskImplementing, TaskDeliberating, TaskReviewing, TaskAwaitingDecision,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	TaskImplementing: {
		TaskReviewing, TaskInvestigating, TaskDeliberating, TaskAwaitingDecision,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	TaskReviewing: {
		// REQUEST_CHANGE loops back to IMPLEMENTING.
		TaskImplementing, TaskInvestigating, TaskDeliberating, TaskAwaitingDecision,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	TaskDeliberating: {
		// CHALLENGE loops back to INVESTIGATING.
		TaskInvestigating, TaskImplementing, TaskReviewing, TaskAwaitingDecision,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	TaskAwaitingDecision: {
		TaskCompleted, TaskDeliberating, TaskImplementing, TaskInvestigating,
		TaskPaused, TaskCancelled, TaskFailed,
	},
	// PAUSED can resume anywhere the task legitimately was; the task manager
	// restores the pre-pause state rather than choosing arbitrarily.
	TaskPaused: {
		TaskCreated, TaskPlanning, TaskInvestigating, TaskImplementing,
		TaskReviewing, TaskDeliberating, TaskAwaitingDecision,
		TaskCancelled, TaskFailed,
	},
	TaskCompleted: {},
	TaskFailed:    {},
	TaskCancelled: {},
}

// CanTransition reports whether from -> to is a legal task transition.
func CanTransition(from, to TaskState) bool {
	if from == to {
		return false
	}
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// AllowedTransitions returns the legal successor states of s.
func AllowedTransitions(s TaskState) []TaskState {
	out := make([]TaskState, len(transitions[s]))
	copy(out, transitions[s])
	return out
}

// ErrInvalidTransition is returned when an illegal state change is attempted.
type ErrInvalidTransition struct {
	From, To TaskState
}

func (e ErrInvalidTransition) Error() string {
	return fmt.Sprintf("invalid task transition %s -> %s", e.From, e.To)
}

// TaskLimits keeps agents from running away on their own. Reaching a hard
// limit pauses the task for a human; it never picks a winner.
//
// The limits guard *autonomous* agent work only. A message from the human --
// an instruction, a follow-up, a resume, an approval -- starts a fresh
// allowance, and messages to or from the human never count. A conversation the
// human is driving is not the runaway these exist to stop, and counting it
// meant long human-led work could never finish.
type TaskLimits struct {
	// MaxDeliberationRounds is the checkpoint interval: after this many
	// authority-led agent rounds without human input, the authority posts a
	// progress update and the human is notified. Work continues -- a long
	// task legitimately needs many rounds.
	MaxDeliberationRounds int
	// MaxMessages pauses after this many agent-to-agent messages without
	// human input.
	MaxMessages int
	// MaxDuration pauses after this long without human input.
	MaxDuration time.Duration
	// StallMessages pauses after this many consecutive agent messages that
	// make no progress: no request answered, nothing new produced, or a near
	// repeat of an earlier message. This, not a round count, is what catches
	// agents going in circles.
	StallMessages int
	// MaxTotalMessages is the absolute ceiling over the task's whole life.
	// Nothing resets it; it is the backstop against an unattended bill.
	MaxTotalMessages int
	// MaxCostUSD and MaxTokens are the spending ceilings, from the usage the
	// agent CLIs report. Zero disables each.
	MaxCostUSD float64
	MaxTokens  int64
}

// Task is the orchestrator-owned unit of work. The orchestrator, not any agent
// conversation, is the source of truth for this record.
type Task struct {
	ID          TaskID
	Title       string
	Description string
	// AuthorityAgent holds final decision-making responsibility for this task.
	AuthorityAgent AgentID
	Participants   []AgentID
	State          TaskState
	// PreviousState records where the task was before it was paused.
	PreviousState TaskState
	Limits        TaskLimits
	// Round counts authority-led exchanges between agents over the task's
	// life; MessageCount counts every routed message.
	Round        int
	MessageCount int

	// Counters since the last human input -- the allowance TaskLimits
	// meters. NoteHumanInput resets them.
	AgentRounds   int
	AgentMessages int
	SegmentStart  time.Time
	// StallCount is the run of consecutive agent messages without progress.
	StallCount int
	// LastCheckpoint is the AgentRounds value at the last checkpoint.
	LastCheckpoint int

	// FollowUp marks a completed task reopened to answer a human follow-up.
	// It completes again once the follow-up has been answered.
	FollowUp bool
	// FollowUpWork marks a follow-up in which the authority handed out new
	// work rather than only answering. Such a follow-up is not over when
	// the requests are answered; like the original task it ends with a
	// FINAL_DECISION.
	FollowUpWork bool

	// Usage reported by the agent CLIs, summed over the task's life.
	CostUSD      float64
	InputTokens  int64
	OutputTokens int64
	// AgentUsage breaks the same totals down by agent.
	AgentUsage map[AgentID]AgentUsage
	// FailureReason records why a task entered FAILED (timeout, agent error...).
	FailureReason string
	// PauseReason records why the task is awaiting a human.
	PauseReason string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	CompletedAt *time.Time
}

// AgentUsage is what one agent's CLI turns on a task consumed, summed.
type AgentUsage struct {
	Turns           int
	CostUSD         float64
	InputTokens     int64
	OutputTokens    int64
	CacheReadTokens int64
}

// HasParticipant reports whether the agent takes part in this task.
func (t *Task) HasParticipant(id AgentID) bool {
	for _, p := range t.Participants {
		if p == id {
			return true
		}
	}
	return false
}

// Clone returns a deep copy so callers cannot mutate repository-held state.
func (t *Task) Clone() *Task {
	if t == nil {
		return nil
	}
	cp := *t
	cp.Participants = append([]AgentID(nil), t.Participants...)
	if t.AgentUsage != nil {
		cp.AgentUsage = make(map[AgentID]AgentUsage, len(t.AgentUsage))
		for k, v := range t.AgentUsage {
			cp.AgentUsage[k] = v
		}
	}
	if t.CompletedAt != nil {
		v := *t.CompletedAt
		cp.CompletedAt = &v
	}
	return &cp
}

// PauseReasonLimit describes a loop-protection pause.
const (
	PauseReasonMaxMessages = "limit reached: maxMessages without human input"
	PauseReasonMaxDuration = "limit reached: maxTaskDuration without human input"
	PauseReasonStalled     = "STALLED: agents exchanged messages without making progress"
	PauseReasonMaxTotal    = "limit reached: maxTotalMessages over the task's life"
	PauseReasonBudget      = "limit reached: spending budget"
	PauseReasonHuman       = "paused by human"
	// PauseReasonInterrupted marks a task that was running when ritam last
	// exited. Its agents' in-flight turns were lost; resuming tells them so.
	PauseReasonInterrupted = "INTERRUPTED: ritam exited while this task was running; resume to continue"
)
