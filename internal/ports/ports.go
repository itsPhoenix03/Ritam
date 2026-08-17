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
	"time"

	"projectorch/internal/domain"
)

// Common errors returned by port implementations.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	ErrNotSupported  = errors.New("not supported")
	// ErrNotImplemented marks a scaffolded seam that a later phase will fill in.
	ErrNotImplemented = errors.New("not implemented in this phase")
)

// ---------------------------------------------------------------------------
// Storage ports
// ---------------------------------------------------------------------------

// TaskRepository persists tasks. The in-memory implementation is authoritative
// for the scaffold; a SQLite implementation can be added behind this interface.
type TaskRepository interface {
	Create(ctx context.Context, t *domain.Task) error
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

// Storage bundles the repositories so wiring code can pass one value around.
type Storage struct {
	Tasks         TaskRepository
	Messages      MessageRepository
	Events        EventRepository
	Decisions     DecisionRepository
	Disagreements DisagreementRepository
	Knowledge     KnowledgeRepository
	Artifacts     ArtifactRepository
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
	Err     error
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

// Service is the application-layer facade. The TUI talks to this and never to
// the domain or to HTTP.
type Service interface {
	CreateTask(ctx context.Context, in CreateTaskInput) (*domain.Task, error)
	RunTask(ctx context.Context, id domain.TaskID) error

	ListTasks(ctx context.Context) ([]*domain.Task, error)
	GetTask(ctx context.Context, id domain.TaskID) (*domain.Task, error)
	TaskMessages(ctx context.Context, id domain.TaskID) ([]*domain.Message, error)
	TaskEvents(ctx context.Context, id domain.TaskID) ([]domain.Event, error)
	TaskDecisions(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error)

	Agents(ctx context.Context) ([]domain.Agent, error)
	// TaskAgents returns the agents as they stand on one task: each task
	// has its own instance of each agent, working or not.
	TaskAgents(ctx context.Context, id domain.TaskID) ([]domain.Agent, error)
	// Models lists every model each provider's CLI can run.
	Models(ctx context.Context) ([]ModelInfo, error)
	// Recruit adds an agent running a chosen model to the team. It is kept
	// across restarts, alongside the agents in orch.yaml.
	Recruit(ctx context.Context, in RecruitInput) (domain.Agent, error)
	// Dismiss removes a recruited agent. Agents from orch.yaml stay.
	Dismiss(ctx context.Context, id domain.AgentID) error
	// CheckModel makes one short call to see whether this account can run a
	// model, and remembers the answer. It costs a little, so it runs only
	// when asked.
	CheckModel(ctx context.Context, provider, model string) (ModelInfo, error)

	// Human override surface. The human is always able to intervene.
	Pause(ctx context.Context, id domain.TaskID, reason string) error
	Resume(ctx context.Context, id domain.TaskID) error
	Cancel(ctx context.Context, id domain.TaskID, reason string) error
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

	SearchKnowledge(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error)

	// Subscribe registers an event handler and returns an unsubscribe func.
	Subscribe(h domain.EventHandler) (unsubscribe func())
}
