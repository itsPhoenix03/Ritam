// Package ports declares the interfaces that connect the domain to the outside
// world: storage, agent runtimes/adapters, the LLM proxy boundary and the
// application service consumed by the TUI.
//
// Everything here is provider-neutral on purpose. No Anthropic- or
// OpenAI-specific type may appear in this package, and the domain must never
// import a concrete implementation of any of these interfaces.
package ports

import (
	"context"
	"errors"
	"strings"
	"time"

	"ritam/internal/domain"
)

// Common errors returned by port implementations.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrNotSupported  = errors.New("not supported")
	// ErrNotImplemented marks a scaffolded seam that a later phase will fill in.
	ErrNotImplemented = errors.New("not implemented in this phase")
	// ErrConflict is an update refused because the record changed after the
	// copy being written was read. Read it again and redo the change.
	ErrConflict = errors.New("changed since it was read")
)

// ---------------------------------------------------------------------------
// Storage ports
// ---------------------------------------------------------------------------

// TaskRepository persists tasks. The in-memory implementation is authoritative
// for the scaffold; a SQLite implementation can be added behind this interface.
type TaskRepository interface {
	// Create stores a new task at Version 1 and sets t.Version to it.
	Create(ctx context.Context, t *domain.Task) error
	// Update writes t only if the stored task is still at t.Version, the
	// revision t was read at, and then sets t.Version to the new one. A
	// stored task that moved on is ErrConflict: writing the whole task
	// back would undo whatever changed it.
	Update(ctx context.Context, t *domain.Task) error
	Get(ctx context.Context, id domain.TaskID) (*domain.Task, error)
	List(ctx context.Context) ([]*domain.Task, error)
	// NextSequence returns the next task number used to mint "TASK-00N" ids.
	NextSequence(ctx context.Context) (int, error)
}

// MessageRepository persists the routed message history of every task.
type MessageRepository interface {
	Append(ctx context.Context, m *domain.Message) error
	Get(ctx context.Context, id domain.MessageID) (*domain.Message, error)
	ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Message, error)
	CountByTask(ctx context.Context, id domain.TaskID) (int, error)
}

// EventRepository is the append-only chronological event log.
type EventRepository interface {
	Append(ctx context.Context, e domain.Event) error
	ListByTask(ctx context.Context, id domain.TaskID) ([]domain.Event, error)
	ListAll(ctx context.Context) ([]domain.Event, error)
}

// DecisionRepository persists decisions. Decisions are never deleted; they are
// superseded.
type DecisionRepository interface {
	Create(ctx context.Context, d *domain.Decision) error
	Update(ctx context.Context, d *domain.Decision) error
	Get(ctx context.Context, id domain.DecisionID) (*domain.Decision, error)
	ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error)
	NextSequence(ctx context.Context) (int, error)
}

// KnowledgeRepository is the shared project knowledge store. This is the
// abstraction that a SQLite + vector-search backend would later implement.
type KnowledgeRepository interface {
	Put(ctx context.Context, e *domain.KnowledgeEntry) error
	Get(ctx context.Context, id domain.KnowledgeID) (*domain.KnowledgeEntry, error)
	Update(ctx context.Context, e *domain.KnowledgeEntry) error
	Delete(ctx context.Context, id domain.KnowledgeID) error
	Search(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error)
}

// DisagreementRepository records explicit conflicts between agents. Like
// decisions, disagreements are never deleted: they are resolved in place so
// the claim, the counterclaim and the resolution stay auditable.
type DisagreementRepository interface {
	Create(ctx context.Context, d *domain.Disagreement) error
	Update(ctx context.Context, d *domain.Disagreement) error
	Get(ctx context.Context, id domain.DisagreementID) (*domain.Disagreement, error)
	ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error)
	// ListOpen returns the unresolved disagreements of a task.
	ListOpen(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error)
}

// ArtifactRepository records artifacts produced during a task.
type ArtifactRepository interface {
	Create(ctx context.Context, a *domain.Artifact) error
	Get(ctx context.Context, id domain.ArtifactID) (*domain.Artifact, error)
	ListByTask(ctx context.Context, id domain.TaskID) ([]*domain.Artifact, error)
}

// DeliveryRepository records each stored message's way to each recipient:
// queued in its inbox, sent in a turn, acknowledged when that turn ended
// well. It is what lets an inbox outlive the run, and the process, that
// filled it.
type DeliveryRepository interface {
	// Enqueue records d as waiting. A message already recorded for that
	// recipient is ErrAlreadyExists.
	Enqueue(ctx context.Context, d *domain.Delivery) error
	// MarkSent records that turn carried ids to recipient.
	MarkSent(ctx context.Context, task domain.TaskID, recipient domain.AgentID, ids []domain.MessageID, turn domain.TurnID, at time.Time) error
	// SettleTurn moves every delivery turn carried to status: acknowledged,
	// or back to queued. One already back in the queue is left there: a
	// later turn may carry it, and settling the old turn again must not
	// mark it delivered.
	SettleTurn(ctx context.Context, turn domain.TurnID, status domain.DeliveryStatus) error
	// ListByTask returns a task's deliveries in the order they were queued.
	ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Delivery, error)
	// Discard forgets what still waits for recipient (not what it was given).
	Discard(ctx context.Context, task domain.TaskID, recipient domain.AgentID) error
	// Clear forgets every delivery of the task that was not acknowledged.
	Clear(ctx context.Context, task domain.TaskID) error
}

// TurnRepository records every agent turn (domain.Turn).
type TurnRepository interface {
	// Create records a new turn. An id already recorded is ErrAlreadyExists.
	Create(ctx context.Context, t *domain.Turn) error
	// Update rewrites a recorded turn: its end, outcome and usage.
	Update(ctx context.Context, t *domain.Turn) error
	// ListByTask returns a task's turns in the order they started.
	ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Turn, error)
}

// Storage bundles the repositories so wiring code can pass one value around.
type Storage struct {
	Tasks         TaskRepository
	Messages      MessageRepository
	Events        EventRepository
	Decisions     DecisionRepository
	Disagreements DisagreementRepository
	Knowledge     KnowledgeRepository
	Artifacts     ArtifactRepository
	Deliveries    DeliveryRepository
	Turns         TurnRepository

	// Tx runs a group of writes as one; the backend sets it. Callers use
	// WithTx rather than this field.
	Tx Transactor
}

// Transactor runs fn against a Storage whose repositories all act inside
// one transaction: everything fn wrote is kept when it returns nil, and
// nothing of it when it returns an error or panics.
type Transactor interface {
	WithTx(ctx context.Context, fn func(tx *Storage) error) error
}

// ErrNoTransactions is returned by WithTx on a Storage built without a
// Transactor: running the writes one by one instead would quietly give up
// the all-or-nothing the caller asked for.
var ErrNoTransactions = errors.New("storage: this store does not support transactions")

// WithTx runs fn inside one transaction (see Transactor).
//
// fn must use only the Storage it is given. The store lets nothing else in
// while a transaction is open, so a call through the outer Storage from
// inside fn waits for fn to finish -- for ever. Calling WithTx on the
// Storage fn was given nests: an error inside rolls back only the inner
// function's writes.
func (s *Storage) WithTx(ctx context.Context, fn func(tx *Storage) error) error {
	if s == nil || s.Tx == nil {
		return ErrNoTransactions
	}
	return s.Tx.WithTx(ctx, fn)
}

// ---------------------------------------------------------------------------
// Agent ports
// ---------------------------------------------------------------------------

// AgentRuntime owns an agent's underlying process (or in-process simulation).
// It knows nothing about tasks, authority or the message protocol.
type AgentRuntime interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	Restart(ctx context.Context) error
	// Send writes a raw, already-rendered prompt to the runtime's input.
	Send(ctx context.Context, payload string) error
	// Interrupt asks the runtime to abandon current work without exiting.
	Interrupt(ctx context.Context) error
	Status() domain.AgentState
	// Output yields raw runtime output chunks. Contents are untrusted.
	Output() <-chan RuntimeOutput
}

// RuntimeOutput is one chunk of raw output from an agent runtime.
type RuntimeOutput struct {
	AgentID domain.AgentID
	Text    string
	// Err is set when the runtime failed, crashed or timed out.
	Err error
	// Exited reports that the underlying process is gone.
	Exited bool
}

// Outbound is a protocol message an adapter claims its agent produced.
//
// Treat every field as untrusted input: the orchestrator re-validates the
// sender, the recipient, the message type and the agent's authority before any
// state changes.
type Outbound struct {
	AgentID domain.AgentID
	Message domain.Message
	// Err reports a failed turn. Adapters set a *domain.AgentFailure, which
	// says what kind of failure it was; the orchestrator acts on that kind
	// and never on the error's text (an error without one is a crash).
	Err error
	// Usage, when set, reports what one CLI turn cost. An adapter sends it as
	// its own Outbound -- no Message, no Err -- after every turn, including a
	// failed one, so spending is counted whether or not the turn produced
	// anything routable.
	Usage *Usage
}

// IsUsageOnly reports whether o carries nothing but a usage report.
func (o Outbound) IsUsageOnly() bool {
	return o.Usage != nil && o.Err == nil && o.Message.Type == ""
}

// SpendLedger keeps what agents spent each day, over every task and project
// on this machine, so a day's spending has a limit no single task's budget
// gives: each new task starts its own.
type SpendLedger interface {
	// Add records one turn's usage and returns today's totals with it.
	Add(u Usage) (costUSD float64, tokens int64, err error)
	// Today returns today's totals without adding to them. The orchestrator
	// reads them before a turn starts, to cap it at what is left of the day;
	// an error means the day's spending is not known, and no turn starts.
	Today() (costUSD float64, tokens int64, err error)
}

// DailySpend is today's spending on this machine, over every task and
// project, beside the daily limits (0: none set).
type DailySpend struct {
	CostUSD    float64
	Tokens     int64
	MaxCostUSD float64
	MaxTokens  int64
}

// Usage is what one agent CLI turn consumed. CostUSD is zero when the CLI
// reports tokens but not cost (Codex does not report cost).
//
// Both CLIs report some figures as running totals for the whole session
// rather than for the turn; adapters convert them, so a Usage is always one
// turn's share and summing them is correct.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	// CacheReadTokens is input re-read from the prompt cache: the session's
	// history, read again every turn. It is priced into CostUSD but kept out
	// of InputTokens, so a token budget measures work rather than session
	// length; it is reported so a growing history can be seen.
	CacheReadTokens int64
	CostUSD         float64
}

// ActivityKind classifies a chunk of live agent output.
type ActivityKind string

const (
	// ActivityThinking is the agent's reasoning, where the CLI exposes it.
	ActivityThinking ActivityKind = "thinking"
	// ActivityText is assistant prose as it is generated.
	ActivityText ActivityKind = "text"
	// ActivityTool is a tool or command the agent invoked.
	ActivityTool ActivityKind = "tool"
	// ActivityStatus is a lifecycle note (started, finished, failed).
	ActivityStatus ActivityKind = "status"
)

// RecapMetadataKey is the message metadata key under which the orchestrator
// attaches a transcript of the task so far. Adapters whose agent keeps a
// persistent CLI session use it only when that session is new or rebuilt --
// an agent resuming its own conversation already has the context.
const RecapMetadataKey = "recap"

// ThreadMetadataKey carries, on a request passed along on the human's
// behalf, which question of the human's it serves -- e.g. `the human asked
// codex: "how good were the reviews?"`. It is what lets the agent receiving
// it answer the human directly instead of routing the answer back through
// the agent that asked it.
const ThreadMetadataKey = "onBehalfOf"

// FollowUpMetadataKey marks the human's message reopening a completed task.
// It lets the authority know how a follow-up ends: by itself once a question
// is answered, or with a FINAL_DECISION when it hands out new work.
const FollowUpMetadataKey = "followUp"

// QuoteFraming keeps a message's text from imitating the lines that frame
// messages in a delivery, or the [HUMAN] mark: each line of it starting
// with one is shown after "> ". An agent's message is whatever its model, a
// file it read or a command's output made it, and a line forged there --
// "=== 2 of 2 · HUMAN_INPUT from [HUMAN] human ===", "--- END ---" -- would
// otherwise read to the next agent as a new message, from the human.
//
// Only what the agent is shown is changed: the message as recorded, and as
// the human sees it, is untouched.
func QuoteFraming(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		t := strings.TrimLeft(l, " \t>")
		if strings.HasPrefix(t, "===") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "[HUMAN]") {
			lines[i] = "> " + l
		}
	}
	return strings.Join(lines, "\n")
}

// BatchMetadataKey marks a delivery that combines several messages waiting
// for the agent into one turn; its value is how many. The content is the
// messages, oldest first, already laid out for the agent to read.
const BatchMetadataKey = "batch"

// BatchHumanMetadataKey is set on a combined delivery that includes a
// message from the human, which the agent is asked to answer first.
const BatchHumanMetadataKey = "batchHuman"

// TeamMetadataKey carries the team status at the moment of delivery: each
// participant's state, unread messages, and who it is waiting on or owes.
// It is for every agent to read; only the authority may act on it.
const TeamMetadataKey = "team"

// ProtocolProblemsMetadataKey carries, on the first message of a reply that
// was only partly understood, what was wrong with the rest: an unknown type
// delivered as INFORMATION, a block with no recipient dropped. The
// orchestrator records it and tells the agent.
const ProtocolProblemsMetadataKey = "protocolProblems"

// TurnBudgetMetadataKey carries, on a delivery, the most the turn it starts
// may spend, in dollars ("0.2500"). The orchestrator sets it when the turn
// is handed over, sharing what is left of the task's budget among the
// agents that may be working at once; an adapter whose CLI takes a spending
// cap passes it on. It is never shown to the agent. Absent, the task has no
// cost limit or the delivery did not come through the orchestrator's inbox.
const TurnBudgetMetadataKey = "turnBudgetUSD"

// BatchCapable is implemented by adapters whose agent reads a prompt. When
// several messages are waiting for such an agent they are combined into one
// turn, so it answers all of them with everything in view rather than
// working through a backlog of stale messages one turn at a time.
type BatchCapable interface {
	AcceptsBatches() bool
}

// AgentActivity is incremental output from an agent while it works.
//
// It exists so a human can watch an agent think rather than staring at a
// frozen pane for the minutes a real CLI call takes. It is presentation only:
// activity is never persisted, never routed, and never influences task state —
// the protocol message an agent finally emits remains the only thing the
// orchestrator acts on.
type AgentActivity struct {
	AgentID domain.AgentID
	TaskID  domain.TaskID
	Kind    ActivityKind
	Text    string
	Time    time.Time
}

// AgentRights is what one agent may do on a task: the facts the team roster
// shows each agent, and the bus checks before handing an agent work it
// cannot do.
type AgentRights struct {
	// Writes says the agent may change files in the project.
	Writes bool
	// Builds says the agent may run builds and tests on this task.
	Builds bool
}

// AgentAdapter normalises one concrete agent CLI into the internal protocol.
// Claude Code and Codex are separate applications with different interaction
// models; all of that difference stays behind this interface.
type AgentAdapter interface {
	ID() domain.AgentID
	Kind() domain.AgentKind
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// SendTask hands the agent its initial assignment for a task, including the
	// role and authority context the orchestrator wants it to operate under.
	// Only the authority agent receives one: supporting agents are idle until
	// the authority delegates to them.
	SendTask(ctx context.Context, t *domain.Task, m domain.Message) error
	// SendMessage delivers a routed protocol message to the agent.
	//
	// The task record travels with every message because agents are invoked
	// statelessly — an agent that was never given an opening assignment still
	// has to know the task, its own role and which agent ids it may address.
	SendMessage(ctx context.Context, t *domain.Task, m domain.Message) error
	Interrupt(ctx context.Context) error
	Status() domain.AgentState
	// Outbound yields protocol messages the agent produced.
	Outbound() <-chan Outbound
	// Activity yields live output while the agent works. Adapters that cannot
	// stream may return nil; callers must handle a nil channel.
	Activity() <-chan AgentActivity
}

// ---------------------------------------------------------------------------
// LLM proxy boundary
// ---------------------------------------------------------------------------

// ProxyRoute identifies which upstream provider a proxied request targets.
type ProxyRoute string

const (
	RouteAnthropic ProxyRoute = "anthropic"
	RouteOpenAI    ProxyRoute = "openai"
)

// LLMProxy is the boundary for the future local proxy. The orchestrator must
// never depend on a provider SDK; it only ever sees this interface, and in the
// scaffold it does not depend on the proxy at all.
type LLMProxy interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// Endpoint returns the base URL agents should be pointed at, e.g.
	// "http://127.0.0.1:8787". Empty when the proxy is disabled.
	Endpoint() string
	// EnvironmentFor returns the environment variables an agent of the given
	// kind needs in order to route its traffic through the proxy.
	EnvironmentFor(kind domain.AgentKind) map[string]string
	Enabled() bool
}

// ---------------------------------------------------------------------------
// Application service (consumed by the TUI)
// ---------------------------------------------------------------------------

// CreateTaskInput is the human's request to start a task.
type CreateTaskInput struct {
	Title        string
	Description  string
	Authority    domain.AgentID
	Participants []domain.AgentID
	Limits       *domain.TaskLimits
	// Builder is who runs builds: domain.BuilderAuto (the default),
	// domain.BuilderManual, or a participant's id.
	Builder string
}

// ModelInfo is one model a provider's CLI can run, as its catalog lists it.
type ModelInfo struct {
	// Provider is the agent type that runs it: claude-code or codex.
	Provider string
	// ID is what the CLI's --model flag takes.
	ID          string
	Label       string
	Description string
	// Hidden marks a model the CLI itself does not list by default (legacy
	// or internal ones). The team view leaves these out.
	Hidden bool
	// Unavailable marks a model this account was found unable to run (no
	// credits, not in the plan, unknown to the CLI), with Reason saying why.
	// It is learned from a failed turn or a check, and cleared by a check
	// that succeeds.
	Unavailable bool
	Reason      string
	// Checked marks a model a check call ran successfully for this account.
	Checked bool
	// Builds marks a model whose CLI passed the sandbox check: agents on it
	// can build and test at the sandboxed builder level. SandboxNote says
	// what the check found when it failed or could not decide.
	Builds      bool
	SandboxNote string
}

// ModelCatalog lists the models one provider's CLI can run. Each provider
// has its own; a new provider adds one.
type ModelCatalog interface {
	Provider() string
	Models(ctx context.Context) ([]ModelInfo, error)
}

// RecruitInput is the human's request to add an agent to the team.
type RecruitInput struct {
	// Name is the new agent's id, e.g. "opus-2".
	Name domain.AgentID
	// Provider is the agent type: claude-code or codex.
	Provider string
	Model    string
	// Role defaults to IMPLEMENTER. Any participant can be made a task's
	// authority, whatever its role.
	Role        domain.AgentRole
	AllowWrites bool
}

// ReviewInfo is how the agents' tool calls are approved, as the settings
// tab shows it.
type ReviewInfo struct {
	// Mode is auto or manual.
	Mode string
	// AskTimeout is how long a call waits for the human.
	AskTimeout time.Duration
	// Active: the review runs (real agents); with the scripted agents no
	// tool is ever called.
	Active bool
	// Warning is what auto mode does not promise.
	Warning string
	// HeldBack says what the project's settings asked for that only the
	// global settings may decide, "" when nothing was held back.
	HeldBack string
	// File is where a change is saved, "" when it is kept in memory.
	File string
	// Deny, Ask and Protect are the human's own rules (review.rules).
	Deny, Ask, Protect []string
	// Hooks are the human's hooks that run; HeldHooks the project's that
	// wait for the human's approval on this machine.
	Hooks, HeldHooks []string
}

// Approval is a tool call waiting for the human.
type Approval struct {
	ID    string
	Task  domain.TaskID
	Agent domain.AgentID
	Tool  string
	// Summary says what the call does, secrets redacted.
	Summary string
	// Reasons are why it is asked: the rules that fired, or manual mode.
	Reasons  []string
	Severity string
	Manual   bool
	// Deadline is when it is refused if unanswered.
	Deadline time.Time
}

// AccessInfo is the workspace's access level as the settings tab shows it.
type AccessInfo struct {
	Level domain.AccessLevel
	// Commands are the listed command prefixes, as saved.
	Commands []string
	// Blocked are listed commands that are refused whatever the level.
	Blocked []string
	// Locked says why the level cannot be changed, "" when it can: a global
	// launch (the home folder, a drive root) stays at level 0.
	Locked string
	// HeldBack says what the project's settings asked for but was never
	// approved for this folder on this machine (a cloned or copied project),
	// "" when nothing was held back.
	HeldBack string
	// File is the settings file a change is saved to.
	File string
	// Builders are the team's agents whose CLI runs commands in a sandbox:
	// the ones that build and test at the sandboxed builder level.
	Builders []domain.AgentID
	// Missing are listed commands whose program is not found on PATH.
	Missing []string
	// Unsandboxed are the write-enabled agents whose sandbox was not
	// verified: at levels 2 and 3 they run commands directly on this
	// machine (Claude Code on Windows, which has no sandbox there).
	Unsandboxed []domain.AgentID
	// AllowUnsandboxed says whether the Unsandboxed agents may run commands
	// at levels 2 and 3 (set in the global settings only). Off, they run
	// none, and ask a sandboxed agent or the human.
	AllowUnsandboxed bool
}

// AccessChange is the human's choice of level and command list.
type AccessChange struct {
	Level    domain.AccessLevel
	Commands []string
}

// Service is the application-layer facade. The TUI talks to this and never to
// the domain or to HTTP.
type Service interface {
	CreateTask(ctx context.Context, in CreateTaskInput) (*domain.Task, error)
	RunTask(ctx context.Context, id domain.TaskID) error
	// StartTask creates a task and starts running it in the background. It
	// returns as soon as the task exists: a run takes minutes, and a caller
	// that waited for it (the TUI used to) is blocked for all of them. How
	// the run goes is told by the task's events.
	StartTask(ctx context.Context, in CreateTaskInput) (*domain.Task, error)

	ListTasks(ctx context.Context) ([]*domain.Task, error)
	GetTask(ctx context.Context, id domain.TaskID) (*domain.Task, error)
	TaskMessages(ctx context.Context, id domain.TaskID) ([]*domain.Message, error)
	TaskEvents(ctx context.Context, id domain.TaskID) ([]domain.Event, error)
	TaskDecisions(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error)
	// TaskTurns lists every agent turn of a task, in the order they
	// started: what each was handed, how it ended and what it cost.
	TaskTurns(ctx context.Context, id domain.TaskID) ([]*domain.Turn, error)

	Agents(ctx context.Context) ([]domain.Agent, error)
	// TaskAgents returns the agents as they stand on one task: each task
	// has its own instance of each agent, working or not.
	TaskAgents(ctx context.Context, id domain.TaskID) ([]domain.Agent, error)
	// Models lists every model each provider's CLI can run.
	Models(ctx context.Context) ([]ModelInfo, error)
	// Recruit adds an agent running a chosen model to the team. It is kept
	// across restarts, alongside the agents in ritam.yaml.
	Recruit(ctx context.Context, in RecruitInput) (domain.Agent, error)
	// Dismiss removes a recruited agent. Agents from ritam.yaml stay.
	Dismiss(ctx context.Context, id domain.AgentID) error
	// CheckModel makes one short call to see whether this account can run a
	// model, and remembers the answer. It costs a little, so it runs only
	// when asked.
	CheckModel(ctx context.Context, provider, model string) (ModelInfo, error)

	// Access reports the workspace's access level and command list.
	Access(ctx context.Context) (AccessInfo, error)
	// SetAccess changes them, saves them to the project's settings, and
	// records the change in the events of every open task. It applies from
	// each agent's next turn.
	SetAccess(ctx context.Context, in AccessChange) (AccessInfo, error)

	// Review reports how the agents' tool calls are approved.
	Review(ctx context.Context) (ReviewInfo, error)
	// SetReviewMode switches between auto and manual, saves the choice to
	// the project's settings, and records it in every open task's events.
	// It applies from the next tool call.
	SetReviewMode(ctx context.Context, mode string) (ReviewInfo, error)
	// ApproveHook approves one of the project's review hooks on this
	// machine, as it stands now; it runs from the next tool call.
	ApproveHook(ctx context.Context, name string) (ReviewInfo, error)
	// PendingApprovals lists the tool calls waiting for the human, oldest
	// first.
	PendingApprovals(ctx context.Context) []Approval
	// AnswerApproval approves or refuses a waiting tool call; a refusal's
	// note reaches the agent.
	AnswerApproval(ctx context.Context, id string, allow bool, note string) error

	// Human override surface. The human is always able to intervene.
	Pause(ctx context.Context, id domain.TaskID, reason string) error
	Resume(ctx context.Context, id domain.TaskID) error
	// Retry brings a FAILED task back with the same participants, their
	// sessions kept, and runs it; the authority is told why it had failed.
	Retry(ctx context.Context, id domain.TaskID) error
	Cancel(ctx context.Context, id domain.TaskID, reason string) error
	// RaiseLimits raises a task's message ceiling (MaxTotalMessages) or its
	// spending limits (MaxCostUSD, MaxTokens) to the non-zero values in
	// more. Nothing else resets the ceiling, so without this a task that
	// reached it got one more message per resume. It raises only, and does
	// not resume the task.
	RaiseLimits(ctx context.Context, id domain.TaskID, more domain.TaskLimits) error
	Approve(ctx context.Context, id domain.TaskID, note string) error
	Reject(ctx context.Context, id domain.TaskID, note string) error
	SendInstruction(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error
	// FollowUp reopens a completed task with a human message to one agent.
	// The agents resume their own CLI sessions, so they answer with the
	// earlier deliberation in mind; a new FINAL_DECISION supersedes the old.
	FollowUp(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error
	// SetAuthority hands a task's authority to another participant.
	SetAuthority(ctx context.Context, id domain.TaskID, to domain.AgentID) error
	// Backlog reports how many messages wait undelivered for each agent of
	// a running task.
	Backlog(id domain.TaskID) map[domain.AgentID]int
	// SpendToday reports today's spending over every task on this machine
	// against the daily limits; with no daily limit set, the limits are 0.
	SpendToday(ctx context.Context) (DailySpend, error)

	SearchKnowledge(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error)

	// Subscribe registers an event handler and returns an unsubscribe func.
	Subscribe(h domain.EventHandler) (unsubscribe func())
}
