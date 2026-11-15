package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"ritam/internal/decisions"
	"ritam/internal/domain"
	"ritam/internal/knowledge"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// Deps are the collaborators an Orchestrator needs. Everything is an interface
// or an owned component, so the whole coordination core can be exercised with
// mock agents and in-memory storage.
type Deps struct {
	Storage   ports.Storage
	Agents    AgentGateway
	Knowledge *knowledge.Store
	Decisions *decisions.Manager
	Logger    *logging.Logger

	Limits domain.TaskLimits
	// MaxConcurrentTasks bounds how many tasks may deliberate at once.
	MaxConcurrentTasks int
	// IdleTimeout bounds how long the orchestrator waits for any agent to
	// speak before pausing the task for a human. A hung agent must not leave a
	// task silently stuck.
	IdleTimeout time.Duration
	// RequireHumanApproval holds a task at AWAITING_DECISION until the human
	// approves or rejects the authority agent's FINAL_DECISION.
	RequireHumanApproval bool
	// Daily, when set, totals what agents spend each day across every task;
	// a task that takes the day over MaxDailyCostUSD or MaxDailyTokens is
	// paused (zero disables each).
	Daily           ports.SpendLedger
	MaxDailyCostUSD float64
	MaxDailyTokens  int64
	// RetryBackoff is how long to wait before each retry of a turn that
	// failed on an overloaded server, a server error or a crash; there are
	// as many retries as delays. Empty means 10s, 30s, 90s.
	RetryBackoff []time.Duration
	// RateLimitWait is the longest rate-limit reset waited out before the
	// turn is retried; a later reset pauses the task. Zero means 2 minutes.
	RateLimitWait time.Duration
}

// Orchestrator coordinates tasks, agents, messages, knowledge and decisions.
// It is the source of truth: agents contribute information and actions, but
// state, authority and routing live here.
type Orchestrator struct {
	storage ports.Storage

	Tasks     *TaskManager
	Bus       *MessageBus
	Authority *AuthorityManager
	Events    *EventBus
	Agents    AgentGateway
	Knowledge *knowledge.Store
	Decisions *decisions.Manager

	log                  *logging.Logger
	daily                ports.SpendLedger
	maxDailyCostUSD      float64
	maxDailyTokens       int64
	idleTimeout          time.Duration
	requireHumanApproval bool
	sem                  chan struct{}

	mu sync.Mutex
	// running holds every RunTask holding a task, from before it waits for
	// a slot until it returns. A run used to be entered here only once it
	// had a slot: a queued run could not be paused or cancelled, started
	// on a task paused while it waited, and a second resume queued a
	// second run.
	running map[domain.TaskID]*taskRun
	// queued holds the human's messages for a task with no run loop: a
	// reopened task's follow-up, an answer or instruction sent while a
	// decision waits for the human, a rejection. They are delivered by the
	// next run once the participants are up: sent straight away, they would
	// reach agents that are still stopped.
	queued map[domain.TaskID][]*domain.Message
	// resumed holds, for a task just resumed, why it had been paused, so the
	// run loop opens by telling the agents to carry on and what was lost.
	resumed map[domain.TaskID]string
	// inboxes holds each running task's undelivered messages (inbox.go).
	inboxes map[domain.TaskID]*taskInbox

	// The failure policy (failures.go): each agent's run of failed turns by
	// kind, the last delivery each agent was handed (what a retry sends
	// again), and the retries waiting to be sent.
	retryBackoff  []time.Duration
	rateLimitWait time.Duration
	failing       map[domain.TaskID]map[domain.AgentID]map[domain.FailureKind]int
	lastTurn      map[domain.TaskID]map[domain.AgentID]*domain.Message
	retrying      map[domain.TaskID]map[domain.AgentID]int
}

// New wires an Orchestrator from its dependencies.
func New(d Deps) *Orchestrator {
	log := d.Logger
	if log == nil {
		log = logging.NewDiscard()
	}
	if d.MaxConcurrentTasks <= 0 {
		d.MaxConcurrentTasks = 1
	}
	if d.IdleTimeout <= 0 {
		d.IdleTimeout = 60 * time.Second
	}

	events := NewEventBus(d.Storage.Events, log)
	authority := NewAuthorityManager()

	o := &Orchestrator{
		storage:              d.Storage,
		Tasks:                NewTaskManager(d.Storage.Tasks, events, d.Limits, log),
		Bus:                  NewMessageBus(d.Storage.Messages, events, authority, d.Agents, log),
		Authority:            authority,
		Events:               events,
		Agents:               d.Agents,
		Knowledge:            d.Knowledge,
		Decisions:            d.Decisions,
		log:                  log,
		daily:                d.Daily,
		maxDailyCostUSD:      d.MaxDailyCostUSD,
		maxDailyTokens:       d.MaxDailyTokens,
		idleTimeout:          d.IdleTimeout,
		requireHumanApproval: d.RequireHumanApproval,
		sem:                  make(chan struct{}, d.MaxConcurrentTasks),
		running:              map[domain.TaskID]*taskRun{},
		queued:               map[domain.TaskID][]*domain.Message{},
		resumed:              map[domain.TaskID]string{},
		inboxes:              map[domain.TaskID]*taskInbox{},
		retryBackoff:         d.RetryBackoff,
		rateLimitWait:        d.RateLimitWait,
		failing:              map[domain.TaskID]map[domain.AgentID]map[domain.FailureKind]int{},
		lastTurn:             map[domain.TaskID]map[domain.AgentID]*domain.Message{},
		retrying:             map[domain.TaskID]map[domain.AgentID]int{},
	}
	if len(o.retryBackoff) == 0 {
		o.retryBackoff = defaultRetryBackoff
	}
	if o.rateLimitWait <= 0 {
		o.rateLimitWait = defaultRateLimitWait
	}
	o.Bus.Enrich = func(ctx context.Context, t *domain.Task, m *domain.Message) {
		o.attachKnowledge(ctx, t, m)
		o.attachRecap(ctx, t, m)
		o.attachThread(ctx, t, m)
	}
	o.Bus.Handoff = o.handoff
	o.Bus.RecordOnly = func(t *domain.Task, m *domain.Message) bool {
		// A closing note that needs no action is history, not a turn: waking
		// an agent just to tell it a reply it no longer needs is not coming
		// would cost a CLI call for nothing.
		return o.decisionCompletesTask(t, m) || m.Metadata[domain.MetaOrchestrator] == "closed" ||
			m.Metadata[metaDecisionDeferred] != ""
	}
	return o
}

// decisionCompletesTask reports whether m is a FINAL_DECISION that will be
// auto-accepted, ending the task as soon as it is routed. Recipients then have
// nothing left to contribute, so the bus must not invoke them. With human
// approval required -- or an agent still waiting for the human's answer --
// the task stays open awaiting the human, and recipients may legitimately
// still CHALLENGE, so they are delivered to as usual.
func (o *Orchestrator) decisionCompletesTask(t *domain.Task, m *domain.Message) bool {
	if m.Type != domain.MsgFinalDecision || o.requireHumanApproval {
		return false
	}
	return t == nil || len(o.awaitingTheHuman(context.Background(), t)) == 0
}

// knowledgeContextLimit bounds how many knowledge entries are attached to a
// single outbound message.
const knowledgeContextLimit = 6

// attachKnowledge gives the recipient the project knowledge relevant to what
// it is being asked, rather than the whole task history.
//
// Real agents are invoked one-shot, so they remember nothing between messages.
// This is the mechanism that keeps them oriented: the orchestrator retrieves
// what matters and puts it in the prompt, which is why shared knowledge — not
// an agent's conversation — is the source of truth.
func (o *Orchestrator) attachKnowledge(ctx context.Context, t *domain.Task, m *domain.Message) {
	if o.Knowledge == nil || t == nil {
		return
	}
	query := strings.TrimSpace(m.Content)
	if query == "" {
		query = t.Title
	}

	hits, err := o.Knowledge.ContextFor(ctx, t.ID, query, knowledgeContextLimit)
	if err != nil {
		o.log.Warn("retrieve knowledge context", "taskId", string(t.ID), "error", err)
		return
	}
	if len(hits) == 0 {
		return
	}

	var b strings.Builder
	for _, h := range hits {
		if h.Entry == nil {
			continue
		}
		fmt.Fprintf(&b, "- [%s] %s\n  %s\n", h.Entry.Category, h.Entry.Title, h.Entry.Body)
	}
	if b.Len() == 0 {
		return
	}
	if m.Metadata == nil {
		m.Metadata = map[string]string{}
	}
	m.Metadata["knowledge"] = b.String()
}

// recapMessages and recapChars bound the transcript attached for an agent
// starting a session: enough to know who asked what and where things stand,
// not a replay of every paragraph.
const (
	recapMessages = 15
	recapChars    = 400
)

// attachRecap gives the recipient a transcript of the task so far.
//
// Agents keep their own CLI session, so most deliveries need none of this and
// the adapters drop it when they resume. It matters when an agent has no
// memory of the task: the first time a supporting agent is contacted mid-way
// through, or when its session was lost and is being rebuilt. Without it that
// agent would answer a message whose context it has never seen.
//
// It holds only what the recipient was party to: a supporting agent sees the
// messages to or from it and the broadcasts, never the human's brief to the
// authority or a private exchange between two others. The authority, who is
// responsible for the whole task, sees everything.
func (o *Orchestrator) attachRecap(ctx context.Context, t *domain.Task, m *domain.Message) {
	if t == nil {
		return
	}
	history, err := o.storage.Messages.ListByTask(ctx, t.ID)
	if err != nil {
		o.log.Warn("build recap", "taskId", string(t.ID), "error", err)
		return
	}
	var lines []string
	for _, h := range history {
		if h == nil || h.ID == m.ID {
			continue
		}
		if m.ToAgent != t.AuthorityAgent && h.FromAgent != m.ToAgent && h.ToAgent != m.ToAgent &&
			h.ToAgent != domain.BroadcastAgentID {
			continue
		}
		body := strings.Join(strings.Fields(h.Content), " ")
		if r := []rune(body); len(r) > recapChars {
			body = string(r[:recapChars]) + "..."
		}
		// A note recorded as the human's before the orchestrator had its own
		// id is shown as what it was: the human did not say it.
		from := h.FromAgent
		if domain.FromOrchestrator(h) {
			from = domain.OrchestratorID
		}
		lines = append(lines, fmt.Sprintf("- [R%d] %s -> %s %s: %s", h.Round, from, h.ToAgent, h.Type, body))
	}
	if len(lines) == 0 {
		return
	}
	if len(lines) > recapMessages {
		skipped := len(lines) - recapMessages
		lines = append([]string{fmt.Sprintf("- (%d earlier messages omitted)", skipped)}, lines[skipped:]...)
	}
	if m.Metadata == nil {
		m.Metadata = map[string]string{}
	}
	m.Metadata[ports.RecapMetadataKey] = strings.Join(lines, "\n")
}

// threadExcerptChars bounds how much of the human's question a thread note
// quotes.
const threadExcerptChars = 300

// attachThread labels a request that is part of a question the human asked,
// so the agent receiving it knows it may answer the human directly -- the path
// agents should prefer, and one the ledger now recognises as answering the
// whole chain.
func (o *Orchestrator) attachThread(ctx context.Context, t *domain.Task, m *domain.Message) {
	// The human's own message needs no label, and a notice is never a
	// request, so it belongs to no thread: labelling one "answer the human
	// directly" would send the agent to the human about the notice.
	if t == nil || m.FromAgent == domain.HumanAgentID || m.FromAgent == domain.OrchestratorID {
		return
	}
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		return
	}
	root := humanRoot(ledger, m.ID)
	if root == nil {
		return
	}
	if m.Metadata == nil {
		m.Metadata = map[string]string{}
	}
	m.Metadata[ports.ThreadMetadataKey] = fmt.Sprintf("the human asked %s: %q",
		root.ToAgent, excerpt(root.Content, threadExcerptChars))
}

// humanRoot returns the human's question at the root of the thread request id
// belongs to, or nil when id is not a request or its thread was not started by
// the human.
func humanRoot(ledger []domain.Request, id domain.MessageID) *domain.Message {
	var thread domain.MessageID
	for _, r := range ledger {
		if r.Message.ID == id {
			thread = r.Thread
		}
	}
	if thread == "" || thread == id {
		return nil
	}
	for _, r := range ledger {
		if r.Message.ID == thread && r.Message.FromAgent == domain.HumanAgentID {
			return r.Message
		}
	}
	return nil
}

func excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}

// openHumanQuestions returns the human's questions still waiting for an
// answer, oldest first.
func openHumanQuestions(ledger []domain.Request) []*domain.Message {
	var out []*domain.Message
	for _, r := range domain.OpenRequests(ledger) {
		if r.Message.FromAgent == domain.HumanAgentID {
			out = append(out, r.Message)
		}
	}
	return out
}

// waitingReason describes a pause caused by the human's question going
// unanswered. "TIMEOUT: no agent activity" said nothing about what was
// actually missing.
func waitingReason(q *domain.Message) string {
	return fmt.Sprintf("waiting for an answer to your question to %s: %q", q.ToAgent, excerpt(q.Content, 80))
}

// missingReply names the reply the task is waiting for, or "" when nothing is
// outstanding. An agent's question to the human comes first: the agents
// cannot move until the human answers it, and since the orchestrator's notes
// stopped counting as that answer it stays open until the human does. Then
// the human's own question, then anything the agents owe each other.
func missingReply(ledger []domain.Request) string {
	for _, r := range domain.OpenRequests(ledger) {
		if q := r.Message; q.ToAgent == domain.HumanAgentID {
			return fmt.Sprintf("waiting for your answer to %s's %s: %q", q.FromAgent, q.Type, excerpt(q.Content, 80))
		}
	}
	if qs := openHumanQuestions(ledger); len(qs) > 0 {
		return waitingReason(qs[0])
	}
	for _, r := range domain.OpenRequests(ledger) {
		if q := r.Message; q.ToAgent != domain.HumanAgentID {
			return fmt.Sprintf("waiting on %s to answer %s's %s: %q", q.ToAgent, q.FromAgent, q.Type, excerpt(q.Content, 80))
		}
	}
	return ""
}

// remind asks the agent the human addressed to answer, once. It is sent as
// the orchestrator's note rather than a new human request, so it neither
// opens a second question in the ledger, nor answers one the agent put to
// the human, nor resets the allowance.
func (o *Orchestrator) remind(ctx context.Context, t *domain.Task, q *domain.Message) {
	summary := "reminder: the human's question to " + string(q.ToAgent) + " is unanswered"
	content := fmt.Sprintf("Reminder from the orchestrator: the human asked you %q and has not had an "+
		"answer. Reply to=human now with the answer, or with what you are still waiting for. If another "+
		"agent has already answered the human, say so to the human in one line.",
		excerpt(q.Content, threadExcerptChars))
	if q.FromAgent != domain.HumanAgentID {
		summary = fmt.Sprintf("reminder: %s's %s to %s is unanswered", q.FromAgent, q.Type, q.ToAgent)
		content = fmt.Sprintf("Reminder from the orchestrator: %s asked you (%s) %q and has had no reply. "+
			"Reply to %s now, or say why you cannot.", q.FromAgent, q.Type, excerpt(q.Content, threadExcerptChars), q.FromAgent)
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, t.ID, domain.HumanAgentID, summary, nil)
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   q.ToAgent,
		Type:      domain.MsgInformation,
		Content:   content,
		Metadata:  map[string]string{domain.MetaOrchestrator: "reminder"},
	}); err != nil {
		o.log.Warn("remind", "taskId", string(t.ID), "error", err)
	}
}

// CreateTask records a new task.
func (o *Orchestrator) CreateTask(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	t, err := o.Tasks.Create(ctx, in)
	if err != nil {
		return nil, err
	}
	// The task record itself is shared project knowledge, not just UI state.
	if o.Knowledge != nil {
		if _, err := o.Knowledge.Put(ctx, &domain.KnowledgeEntry{
			Category: domain.KnowledgeTask,
			TaskID:   t.ID,
			AgentID:  domain.HumanAgentID,
			Title:    string(t.ID) + " " + t.Title,
			Body:     t.Description,
			Refs:     []string{string(t.ID)},
		}); err != nil {
			o.log.Warn("record task knowledge", "taskId", string(t.ID), "error", err)
		}
	}
	return t, nil
}

// RunTask drives a task's deliberation until it reaches a terminal state, is
// paused for a human, or the context is cancelled.
//
// There is no fixed number of rounds. The authority agent decides when enough
// evidence exists; the configured limits only ever pause the task and ask for
// a human, they never declare a winner.
func (o *Orchestrator) RunTask(ctx context.Context, id domain.TaskID) (err error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The run is entered before it waits for a slot, so a pause or a cancel
	// finds it and a second start sees it.
	r := &taskRun{cancel: cancel}
	o.mu.Lock()
	if _, already := o.running[id]; already {
		// A human action starts a run when it finds none; two in quick
		// succession can both start one. A second loop on the same task
		// would deliver everything twice, so the later one stands down,
		// whether the first is running or still queued.
		o.mu.Unlock()
		return nil
	}
	o.running[id] = r
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		if o.running[id] == r {
			delete(o.running, id)
		}
		o.mu.Unlock()
	}()
	// Runs after the agents are stopped and billed (endRun), before the run
	// is gone.
	defer func() { o.pauseAfterError(context.WithoutCancel(ctx), id, err) }()

	waited, err := o.acquireSlot(runCtx, id)
	if err != nil {
		return err
	}
	defer func() { <-o.sem }()
	started := false
	if waited {
		defer func() {
			if !started {
				o.Events.emit(context.WithoutCancel(runCtx), domain.EvtRunUnqueued, id, "",
					"the queued run ended without starting", nil)
			}
		}()
	}
	if err := runCtx.Err(); err != nil {
		// Paused or cancelled at the moment the slot came free.
		return err
	}

	// The task is read again now that the run has its slot: while it was
	// queued the human may have paused it, or it may have ended. A queued run
	// used to start on a paused task anyway, and the authority took a paid
	// turn whose reply was refused because the task was PAUSED.
	t, err := o.Tasks.Get(runCtx, id)
	if err != nil {
		return err
	}
	if t.State.IsTerminal() {
		if waited {
			return nil
		}
		return fmt.Errorf("run %s: task is already %s", id, t.State)
	}
	if t.State == domain.TaskPaused {
		return nil
	}
	if o.waitsOnlyOnTheHuman(runCtx, t) {
		return nil
	}
	if t.Closing && len(o.owingTheHuman(runCtx, t)) == 0 {
		// Accepted, and the question it was kept open for has been answered
		// (or withdrawn) while no run was alive: nothing is left to do.
		return o.completeAfterDecision(runCtx, id, t.AuthorityAgent)
	}

	started = true
	o.Events.emit(runCtx, domain.EvtRunStarted, id, "", "run started", nil)
	if err := o.startParticipants(runCtx, t); err != nil {
		// One agent that cannot start (its CLI is not installed, say) is
		// something the human can fix; it used to send the task to FAILED,
		// which nothing could undo.
		reason := fmt.Sprintf("could not start the task's agents (%v); fix that, then resume", err)
		o.Events.emit(runCtx, domain.EvtLimitReached, id, "", reason, nil)
		if _, perr := o.Tasks.Pause(runCtx, id, domain.HumanAgentID, reason); perr != nil {
			o.log.Error("pause task", "taskId", string(id), "error", perr)
		}
		// Those that did start are not left running with no run behind them.
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(runCtx), stopDeadline)
		o.stopParticipants(stopCtx, t)
		stopCancel()
		return err
	}
	defer o.forgetRun(id)

	o.openInbox(id)
	defer o.closeInbox(id)
	// The channels are taken now, not when the run ends: a task's own agent
	// instance is discarded once it is stopped, and asking for its adapter
	// then returns the shared one, whose channel holds none of this task's
	// usage. enders are the agents whose outbound ended the loop.
	merge := o.mergeOutbound(t)
	var enders []domain.AgentID
	defer func() { o.endRun(context.WithoutCancel(runCtx), t, merge, enders) }()
	inbox := merge.out
	alive := make(chan struct{}, 1)
	o.streamActivity(runCtx, t, alive)

	if t.State == domain.TaskCreated {
		if t, err = o.Tasks.Transition(runCtx, id, domain.TaskPlanning, domain.HumanAgentID, "task started"); err != nil {
			return err
		}
	}
	if err := o.kickoff(runCtx, t); err != nil {
		return err
	}

	idle := time.NewTimer(o.idleTimeout)
	defer idle.Stop()
	resetIdle := func() {
		if !idle.Stop() {
			select {
			case <-idle.C:
			default:
			}
		}
		idle.Reset(o.idleTimeout)
	}
	// lastActivity is when anything last happened: a message or agent output.
	// reminded records which of the human's questions have had their one
	// reminder, and when.
	lastActivity := time.Now()
	reminded := map[domain.MessageID]time.Time{}

	// A follow-up has no FINAL_DECISION to end it, so it is checked
	// periodically: once nothing is outstanding and nobody is working, the
	// human's question has been answered and the task completes again.
	settle := time.NewTicker(followUpSettleInterval)
	defer settle.Stop()
	// Inboxes are handed over as agents become free (inbox.go).
	deliver := time.NewTicker(dispatchInterval)
	defer deliver.Stop()

	for {
		select {
		case <-runCtx.Done():
			return runCtx.Err()

		case <-alive:
			resetIdle()
			lastActivity = time.Now()

		case <-deliver.C:
			o.dispatch(runCtx, id)

		case <-settle.C:
			if done, err := o.settleClosing(runCtx, id); err != nil || done {
				return err
			}
			if done, err := o.settleFollowUp(runCtx, id); err != nil || done {
				return err
			}
			done, nudged, err := o.chaseOpenRequests(runCtx, id, lastActivity, reminded)
			if err != nil || done {
				return err
			}
			if nudged {
				lastActivity = time.Now()
				resetIdle()
			}

		case <-idle.C:
			// Idle means nobody is working and no retry is waiting. A turn
			// that runs long with nothing streamed -- a build, a test run --
			// is bounded by the agent's response timeout, whose failure has
			// its own policy; the idle timer used to pause such a task
			// mid-turn as "no agent activity".
			if cur, err := o.Tasks.Get(runCtx, id); err == nil && o.busy(id, cur) {
				resetIdle()
				continue
			}
			// An agent hung or produced nothing usable. Pause and ask for a
			// human rather than letting the task disappear -- and if what is
			// missing is an answer to the human's own question, say that.
			reason := "TIMEOUT: no agent activity"
			if ledger, err := o.ledger(runCtx, id); err == nil {
				if why := missingReply(ledger); why != "" {
					reason = why
				}
			}
			o.Events.emit(runCtx, domain.EvtLimitReached, id, "", reason, nil)
			_, err := o.Tasks.Pause(runCtx, id, domain.HumanAgentID, reason)
			return err

		case out, ok := <-inbox:
			if !ok {
				// Every adapter closed its outbound channel.
				_, err := o.Tasks.Pause(runCtx, id, domain.HumanAgentID, "all agents stopped")
				return err
			}
			// Only a message is activity. An error -- and the usage report
			// that comes with every turn, failed or not -- used to reset the
			// idle timer too, so an agent failing every turn kept the task
			// "active" for ever.
			if out.Err == nil && !out.IsUsageOnly() {
				resetIdle()
				lastActivity = time.Now()
			}

			done, err := o.handleOutbound(runCtx, id, out)
			if err != nil || done {
				enders = append(enders, out.AgentID)
				return err
			}
			o.dispatch(runCtx, id)
		}
	}
}

// handleOutbound processes one message an agent claims to have produced. It
// returns done=true when the task has reached a state the run loop must exit
// on. Agent output is untrusted, so a rejected message is recorded and the
// loop continues rather than aborting the task.
func (o *Orchestrator) handleOutbound(ctx context.Context, id domain.TaskID, out ports.Outbound) (bool, error) {
	if out.IsUsageOnly() {
		return o.recordUsage(ctx, id, out)
	}
	if out.Err != nil {
		return o.handleFailure(ctx, id, out)
	}

	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return true, err
	}

	m := out.Message.Clone()
	switch {
	case domain.IsReservedAgentID(out.AgentID):
		// An agent running under the human's or the orchestrator's id would
		// pass every check those senders pass. Task creation refuses such a
		// participant; this keeps one from speaking if it got in anyway.
		o.Events.emit(ctx, domain.EvtValidationRejected, id, out.AgentID,
			fmt.Sprintf("%s: an agent may not run as the reserved id %s", ViolationIdentity, out.AgentID),
			map[string]string{"kind": string(ViolationIdentity)})
		return o.pauseIfOverTimeOrBudget(ctx, t, out.AgentID)
	case m.FromAgent == "":
		m.FromAgent = out.AgentID
	case m.FromAgent != out.AgentID:
		// An adapter claimed a different sender. Record it and rewrite the
		// field: the transport identity always wins over the claimed one.
		o.Events.emit(ctx, domain.EvtValidationRejected, id, out.AgentID,
			fmt.Sprintf("%s: agent %s claimed to be %s", ViolationIdentity, out.AgentID, m.FromAgent),
			map[string]string{"kind": string(ViolationIdentity)})
		return o.pauseIfOverTimeOrBudget(ctx, t, out.AgentID)
	}

	// Whether a FINAL_DECISION can be taken up is settled before it is
	// routed: one that cannot is recorded but delivered to nobody, and it
	// changes neither the task's state nor its decisions. The mark is the
	// orchestrator's alone; an agent's reply cannot carry it in.
	delete(m.Metadata, metaDecisionDeferred)
	var outstanding []string
	if m.Type == domain.MsgFinalDecision && m.FromAgent == t.AuthorityAgent {
		if outstanding = o.outstandingWork(ctx, t); len(outstanding) > 0 {
			if m.Metadata == nil {
				m.Metadata = map[string]string{}
			}
			m.Metadata[metaDecisionDeferred] = "true"
		}
	}
	deferred := len(outstanding) > 0

	if err := o.Bus.Route(ctx, t, m); err != nil {
		var v *Violation
		if errors.As(err, &v) {
			// Rejected agent action: already recorded as an event. The task
			// continues; a misbehaving agent does not get to stop the work.
			o.log.Warn("rejected agent action", "taskId", string(id),
				"agentId", string(out.AgentID), "kind", string(v.Kind), "reason", v.Reason)
			o.notifyRejection(ctx, t, m, v)
			return o.pauseIfOverTimeOrBudget(ctx, t, out.AgentID)
		}
		return true, err
	}

	// A readable message ends the sender's run of failed turns -- unless it
	// is the agent reporting an ERROR itself, which is no sign it works.
	if m.Type != domain.MsgError {
		o.clearFailures(id, m.FromAgent)
	}
	if t.IsDropped(m.ToAgent) && t.HasParticipant(m.FromAgent) {
		o.refuseForDropped(ctx, t, m)
	}

	if t, err = o.Tasks.NoteMessage(ctx, id, m, o.isProgress(ctx, t, m)); err != nil {
		return true, err
	}
	if problems := m.Metadata[ports.ProtocolProblemsMetadataKey]; problems != "" {
		o.notifyPartialReply(ctx, t, m.FromAgent, problems)
	}
	if t.FollowUp && !t.FollowUpWork && handsOutWork(t, m) {
		if t, err = o.Tasks.MarkFollowUpWork(ctx, id); err != nil {
			return true, err
		}
	}

	if next := StateForMessage(m.Type); next != "" && next != t.State && !deferred {
		if domain.CanTransition(t.State, next) {
			if t, err = o.Tasks.Transition(ctx, id, next, m.FromAgent, string(m.Type)); err != nil {
				return true, err
			}
		} else {
			o.log.Debug("message implies an illegal transition; state kept",
				"taskId", string(id), "from", string(t.State), "to", string(next),
				"messageType", string(m.Type))
		}
	}

	o.recordKnowledge(ctx, m)

	if m.Type == domain.MsgChallenge {
		o.recordDisagreement(ctx, t, m)
	}

	switch {
	case deferred:
		// Not taken up; the limits below still apply to it, so an authority
		// sending the same decision again and again is caught as a stall.
		o.deferDecision(ctx, t, m, outstanding)
	case m.Type == domain.MsgFinalDecision:
		return o.handleFinalDecision(ctx, t, m)
	}

	if reason, over := LimitExceeded(t); over {
		o.Events.emit(ctx, domain.EvtLimitReached, id, m.FromAgent, reason,
			map[string]string{"round": fmt.Sprint(t.Round), "messages": fmt.Sprint(t.MessageCount)})
		// Loop protection pauses for a human. It deliberately does NOT choose
		// a winner just because a limit was hit.
		_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
		return true, err
	}
	if CheckpointDue(t) {
		o.checkpoint(ctx, t)
	}
	return false, nil
}

// overDailyLimit adds a turn's usage to the day's spending and reports
// whether the day is over its limit.
//
// A record that cannot be locked, read or written -- or was found corrupt and
// set aside -- counts as over the limit. It used to be logged and the day
// treated as under it, so a broken record meant no daily limit at all, and
// spending went on unmetered with nothing on screen to say so. The task
// pauses saying why; the human resumes it once the record is fixed.
func (o *Orchestrator) overDailyLimit(u ports.Usage) (string, bool) {
	if o.daily == nil {
		return "", false
	}
	cost, tokens, err := o.daily.Add(u)
	if err != nil {
		o.log.Error("recording the day's spending", "error", err)
		return fmt.Sprintf("%s: the daily spend record could not be read or written (%v); "+
			"fix it, then resume", domain.PauseReasonDailyBudget, err), true
	}
	switch {
	case o.maxDailyCostUSD > 0 && cost >= o.maxDailyCostUSD:
		return fmt.Sprintf("%s ($%.2f of $%.2f)", domain.PauseReasonDailyBudget, cost, o.maxDailyCostUSD), true
	case o.maxDailyTokens > 0 && tokens >= o.maxDailyTokens:
		return fmt.Sprintf("%s (%d of %d tokens)", domain.PauseReasonDailyBudget, tokens, o.maxDailyTokens), true
	}
	return "", false
}

// addUsage adds one CLI turn's reported usage to the task and records it.
func (o *Orchestrator) addUsage(ctx context.Context, id domain.TaskID, out ports.Outbound) (*domain.Task, error) {
	u := *out.Usage
	t, err := o.Tasks.AddUsage(ctx, id, out.AgentID, u)
	if err != nil {
		return nil, err
	}
	o.Events.emit(ctx, domain.EvtAgentUsage, id, out.AgentID, usageSummary(out.AgentID, u, t),
		map[string]string{
			"costUSD":         fmt.Sprintf("%.6f", u.CostUSD),
			"inputTokens":     fmt.Sprint(u.InputTokens),
			"outputTokens":    fmt.Sprint(u.OutputTokens),
			"cacheReadTokens": fmt.Sprint(u.CacheReadTokens),
			"taskCostUSD":     fmt.Sprintf("%.6f", t.CostUSD),
		})
	return t, nil
}

// recordUsage adds one CLI turn's reported usage to the task, and to the
// day's spending, and pauses if a spending ceiling has been reached.
func (o *Orchestrator) recordUsage(ctx context.Context, id domain.TaskID, out ports.Outbound) (bool, error) {
	u := *out.Usage
	t, err := o.addUsage(ctx, id, out)
	if err != nil {
		return false, err
	}
	reason, over := LimitExceeded(t)
	over = over && strings.HasPrefix(reason, domain.PauseReasonBudget)
	if r, ok := o.overDailyLimit(u); ok && !over {
		reason, over = r, true
	}
	if over {
		o.Events.emit(ctx, domain.EvtLimitReached, id, out.AgentID, reason, map[string]string{
			"costUSD": fmt.Sprintf("%.4f", t.CostUSD),
			"tokens":  fmt.Sprint(t.InputTokens + t.OutputTokens),
		})
		_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
		return true, err
	}
	return false, nil
}

// usageSummary describes one turn's usage for the event log, e.g.
// "opus turn: $0.0312, 1204 in / 587 out, 45210 cache read (task $1.48)".
func usageSummary(agent domain.AgentID, u ports.Usage, t *domain.Task) string {
	cost := "cost not reported"
	if u.CostUSD > 0 {
		cost = fmt.Sprintf("$%.4f", u.CostUSD)
	}
	s := fmt.Sprintf("%s turn: %s, %d in / %d out", agent, cost, u.InputTokens, u.OutputTokens)
	if u.CacheReadTokens > 0 {
		s += fmt.Sprintf(", %d cache read", u.CacheReadTokens)
	}
	if t.CostUSD > 0 {
		s += fmt.Sprintf(" (task $%.2f)", t.CostUSD)
	}
	return s
}

// recentForDuplicates is how many of a sender's earlier messages a new one is
// compared against when deciding whether it merely repeats itself.
const recentForDuplicates = 6

// isProgress decides whether an agent's message moved the work on. A run of
// messages that do not is a stall, and that -- not a round count -- is what
// pauses agents going in circles.
//
// Progress is: answering a request someone was waiting on, producing a
// result (a proposal, an implementation or test result, a review or verdict,
// an answer, a final decision), or asking for something new. Reporting
// (STATUS_UPDATE) and the authority's own direction (INFORMATION) are
// neutral: agents keeping the authority informed while they work are not
// circling, which is what a real run was paused for. Anything that repeats
// one of the sender's recent messages, word for word or nearly, is not
// progress whatever its type; that is the signature of two agents trading
// the same point back and forth.
func (o *Orchestrator) isProgress(ctx context.Context, t *domain.Task, m *domain.Message) Progress {
	history, err := o.storage.Messages.ListByTask(ctx, t.ID)
	if err != nil {
		return MadeProgress // never pause on a read error
	}
	idx := -1
	for i := len(history) - 1; i >= 0; i-- {
		if history[i] != nil && history[i].ID == m.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return MadeProgress
	}

	// A repeat is not progress even when it technically answers something:
	// two agents trading the same DELEGATE and the same CHALLENGE close each
	// other's requests forever while saying nothing new.
	if repeatsItself(history[:idx], m) {
		return NoProgress
	}

	before := domain.BuildLedger(history[:idx])
	after := domain.BuildLedger(history[:idx+1])
	for i, r := range before {
		if r.Open() && i < len(after) && !after[i].Open() {
			return MadeProgress // it answered someone
		}
	}
	switch m.Type {
	case domain.MsgProposal, domain.MsgImplementationResult, domain.MsgTestResult, domain.MsgFinalDecision,
		domain.MsgReview, domain.MsgApprove, domain.MsgReject, domain.MsgRequestChange,
		domain.MsgImplement, domain.MsgAnswer:
		return MadeProgress
	}
	if len(after) > len(before) {
		return MadeProgress // it asked for something new
	}
	if m.Type == domain.MsgStatusUpdate || (m.Type == domain.MsgInformation && m.FromAgent == t.AuthorityAgent) {
		return Neutral
	}
	return NoProgress
}

// repeatsItself reports whether m says what its sender already said recently:
// the same words, or nearly all the same words (an agent restating its point
// with a word or two changed is still restating it).
func repeatsItself(earlier []*domain.Message, m *domain.Message) bool {
	key := normaliseForCompare(m.Content)
	if key == "" {
		return true // an empty message says nothing new
	}
	words := wordSet(key)
	seen := 0
	for i := len(earlier) - 1; i >= 0 && seen < recentForDuplicates; i-- {
		e := earlier[i]
		if e == nil || e.FromAgent != m.FromAgent {
			continue
		}
		seen++
		prev := normaliseForCompare(e.Content)
		if prev == key || nearlySame(words, wordSet(prev)) {
			return true
		}
	}
	return false
}

// normaliseForCompare folds case and whitespace. The whole message is kept:
// two reviews that quote the same code before saying different things are
// not repeats, and comparing only an opening prefix said they were.
func normaliseForCompare(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// nearDuplicateWords is the fewest distinct words two messages need before
// they are compared by vocabulary: short replies ("Agreed.") share most of
// their words by nature, so only an exact repeat counts for them.
const nearDuplicateWords = 12

// nearlySame reports whether two messages use almost exactly the same words
// (Jaccard similarity of 0.9 or more).
func nearlySame(a, b map[string]bool) bool {
	if len(a) < nearDuplicateWords || len(b) < nearDuplicateWords {
		return false
	}
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	return float64(shared) >= 0.9*float64(len(a)+len(b)-shared)
}

func wordSet(s string) map[string]bool {
	set := map[string]bool{}
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		set[w] = true
	}
	return set
}

// checkpoint marks another interval of autonomous agent work: the human is
// notified and the authority is asked for a progress update. Work continues.
//
// This replaces the old round limit, which paused the task outright. A long
// task legitimately needs many rounds; what the human needs is to see where it
// stands, regularly, without having to stop it to find out.
func (o *Orchestrator) checkpoint(ctx context.Context, t *domain.Task) {
	if _, err := o.Tasks.MarkCheckpoint(ctx, t.ID); err != nil {
		o.log.Warn("mark checkpoint", "taskId", string(t.ID), "error", err)
		return
	}
	o.Events.emit(ctx, domain.EvtCheckpoint, t.ID, t.AuthorityAgent,
		fmt.Sprintf("checkpoint: %d agent rounds since your last input; agents continue", t.AgentRounds),
		map[string]string{"agentRounds": fmt.Sprint(t.AgentRounds), "messages": fmt.Sprint(t.AgentMessages)})

	// Sent as the orchestrator's, not the human's: it is not human input, so
	// it must not reset the allowance it is reporting on, and it must not
	// count as the human answering a question the authority asked. It used to
	// be sent as "human", and a checkpoint arriving before the human's reply
	// let a decision through that should have waited for it.
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   t.AuthorityAgent,
		Type:      domain.MsgInformation,
		Content: fmt.Sprintf("Checkpoint from the orchestrator: the agents have worked %d rounds since the "+
			"operator last spoke. Send the operator (to=human) a short STATUS_UPDATE: what is done, what is "+
			"still open and who it is waiting on, and what happens next. Then carry on; nobody has asked you "+
			"to stop.", t.AgentRounds),
		Metadata: map[string]string{domain.MetaOrchestrator: "checkpoint"},
	}); err != nil {
		o.log.Warn("checkpoint request", "taskId", string(t.ID), "error", err)
	}
}

// unansweredRequests returns the messages that asked for a response and never
// got one.
//
// A message counts as answered when the agent it was addressed to later sent
// anything back to the asker. The adapters do not set InReplyTo, so this is
// positional rather than exact; it is used for reporting, never for blocking.
func (o *Orchestrator) unansweredRequests(ctx context.Context, t *domain.Task) []*domain.Message {
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		o.log.Warn("scan for unanswered requests", "taskId", string(t.ID), "error", err)
		return nil
	}
	// The caveat is about agents waiting on agents. A question to or from the
	// human is the human's business, not a weakness in the agents' decision.
	var open []*domain.Message
	for _, r := range domain.OpenRequests(ledger) {
		if r.Message.FromAgent == domain.HumanAgentID || r.Message.ToAgent == domain.HumanAgentID {
			continue
		}
		open = append(open, r.Message)
	}
	return open
}

// ledger reads the task's request ledger from its message history.
func (o *Orchestrator) ledger(ctx context.Context, id domain.TaskID) ([]domain.Request, error) {
	history, err := o.storage.Messages.ListByTask(ctx, id)
	if err != nil {
		return nil, err
	}
	return domain.BuildLedger(history), nil
}

// metaDecisionDeferred marks, on the recorded message, a FINAL_DECISION that
// was not taken up because work was still outstanding (outstandingWork).
const metaDecisionDeferred = "decisionDeferred"

// outstandingWork lists what a decision taken now would cut short: a
// participant other than the authority in the middle of a turn (or about to
// retry one), and an IMPLEMENT or REQUEST_CHANGE not yet reported on.
//
// A decision accepted while another agent was implementing completed the
// task, and ending the task stopped that agent in the middle of its turn: the
// workspace was left half-changed with the decision ACCEPTED. The authority
// sending IMPLEMENT and FINAL_DECISION in one reply did exactly that.
//
// Neither can hold a task for ever. A turn is bounded by the agent's response
// timeout, after which the failure policy retries it and then drops the
// agent; an IMPLEMENT to an idle agent is chased by a reminder and then closed
// as unanswered (chaseOpenRequests). A dropped agent never counts. An open
// DELEGATE or QUESTION does not either: deciding without its answer is the
// authority's call, recorded as a caveat (handleFinalDecision).
func (o *Orchestrator) outstandingWork(ctx context.Context, t *domain.Task) []string {
	var out []string
	for _, p := range t.Participants {
		if p == t.AuthorityAgent || t.IsDropped(p) {
			continue
		}
		if ad, ok := o.Agents.AdapterFor(t.ID, p); ok && ad.Status() == domain.AgentStateWorking {
			out = append(out, fmt.Sprintf("%s is in the middle of a turn", p))
		} else if o.isRetrying(t.ID, p) {
			out = append(out, fmt.Sprintf("%s's failed turn is about to be tried again", p))
		}
	}
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		o.log.Warn("check outstanding work", "taskId", string(t.ID), "error", err)
		return out
	}
	for _, r := range domain.OpenRequests(ledger) {
		q := r.Message
		if q.Type != domain.MsgImplement && q.Type != domain.MsgRequestChange {
			continue
		}
		if !t.HasParticipant(q.ToAgent) || q.ToAgent == t.AuthorityAgent || t.IsDropped(q.ToAgent) {
			continue
		}
		out = append(out, fmt.Sprintf("%s's %s to %s (%q) has not been reported on",
			q.FromAgent, q.Type, q.ToAgent, excerpt(q.Content, 120)))
	}
	return out
}

// deferDecision tells the authority its FINAL_DECISION was not taken up and
// what it is waiting for. There is no way to withdraw the work instead: the
// adapters cannot interrupt a turn short of killing it, which is what this
// exists to avoid.
func (o *Orchestrator) deferDecision(ctx context.Context, t *domain.Task, m *domain.Message, outstanding []string) {
	what := strings.Join(outstanding, "; ")
	o.Events.emit(ctx, domain.EvtDecisionDeferred, t.ID, m.FromAgent,
		"decision not taken up while work is outstanding: "+what,
		map[string]string{"messageId": string(m.ID), "count": fmt.Sprint(len(outstanding))})
	o.notice(ctx, t, t.AuthorityAgent, "decision-deferred", fmt.Sprintf(
		"Note from the orchestrator: your FINAL_DECISION was not taken up, because work is still outstanding: %s. "+
			"Completing the task now would stop that work part-way and could leave files half-changed. "+
			"Wait until it has been reported to you, then send FINAL_DECISION again; if it cannot wait, "+
			"ask the human (to=human).", what))
}

// handleFinalDecision records the authority agent's decision and, unless the
// human has asked to approve it first, completes the task.
func (o *Orchestrator) handleFinalDecision(ctx context.Context, t *domain.Task, m *domain.Message) (bool, error) {
	// The authority may decide whenever it judges the evidence sufficient —
	// that is the whole content of the role, so this does not block. But a
	// decision taken while a question it asked is still outstanding is a
	// materially weaker decision, and that must be visible rather than
	// silently folded into an accepted result.
	if open := o.unansweredRequests(ctx, t); len(open) > 0 {
		var parts []string
		for _, q := range open {
			parts = append(parts, fmt.Sprintf("%s to %s", q.Type, q.ToAgent))
		}
		summary := fmt.Sprintf("decision made with %d unanswered request(s): %s",
			len(open), strings.Join(parts, "; "))

		o.Events.emit(ctx, domain.EvtDecisionCaveat, t.ID, m.FromAgent, summary,
			map[string]string{"kind": "UNANSWERED_REQUEST", "count": fmt.Sprint(len(open))})
		o.log.Warn("final decision with outstanding requests",
			"taskId", string(t.ID), "agentId", string(m.FromAgent), "open", len(open))

		if m.Metadata == nil {
			m.Metadata = map[string]string{}
		}
		m.Metadata["unansweredRequests"] = summary
	}

	d, err := o.proposeDecision(ctx, t, m)
	if err != nil {
		return true, err
	}
	// The authority's decision is the resolution of any conflict still open.
	o.resolveDisagreements(ctx, t, m.FromAgent, d)

	// A decision taken while an agent waits for the human's answer -- say,
	// to "run the build and tell me the result" -- cannot close the task:
	// the answer may change it. It waits for the human like one that needs
	// approval.
	held := o.requireHumanApproval
	if askers := o.awaitingTheHuman(ctx, t); len(askers) > 0 {
		held = true
		o.Events.emit(ctx, domain.EvtDecisionCaveat, t.ID, m.FromAgent,
			fmt.Sprintf("decision held for you: %s asked you something you have not answered; "+
				"answer it, approve or reject", listAgents(askers)),
			map[string]string{"kind": "AWAITING_HUMAN_ANSWER"})
	}
	if held {
		o.Events.emit(ctx, domain.EvtDecisionCreated, t.ID, m.FromAgent,
			"final decision awaiting human approval", map[string]string{"decisionId": string(d.ID)})
		// The run ends here rather than idling until the human acts: with
		// one task running at a time (the default), a loop kept alive for a
		// task waiting on the human would hold up every other task. Each
		// human action works without a loop instead -- an answer or a
		// rejection is queued for the next run (see SendInstruction and
		// Reject), which the caller starts, and Approve needs none.
		return true, nil
	}

	if _, err := o.Decisions.Accept(ctx, d.ID); err != nil {
		return true, err
	}
	o.Events.emit(ctx, domain.EvtDecisionAccepted, t.ID, m.FromAgent,
		"decision accepted: "+d.Decision, map[string]string{"decisionId": string(d.ID)})

	// A question the human asked is not dropped because the decision came
	// first. In a real run the human asked codex for its progress; codex
	// never got to it, the decision completed the task, and the question
	// was simply lost. The task now stays up until it is answered.
	if owners := o.owingTheHuman(ctx, t); len(owners) > 0 {
		if _, err := o.Tasks.SetClosing(ctx, t.ID, true); err != nil {
			return true, err
		}
		o.Events.emit(ctx, domain.EvtDecisionCaveat, t.ID, m.FromAgent,
			fmt.Sprintf("decision accepted; the task completes once %s answer%s your open question",
				listAgents(owners), plural(len(owners) == 1, "s", "")),
			map[string]string{"kind": "HUMAN_QUESTION_OPEN"})
		return false, nil
	}
	return true, o.completeAfterDecision(ctx, t.ID, m.FromAgent)
}

// awaitingTheHuman lists the participants waiting for the human to answer
// a request they sent.
func (o *Orchestrator) awaitingTheHuman(ctx context.Context, t *domain.Task) []domain.AgentID {
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		return nil
	}
	var out []domain.AgentID
	seen := map[domain.AgentID]bool{}
	for _, r := range domain.OpenRequests(ledger) {
		q := r.Message
		if q.ToAgent == domain.HumanAgentID && q.FromAgent != domain.HumanAgentID && !seen[q.FromAgent] {
			seen[q.FromAgent] = true
			out = append(out, q.FromAgent)
		}
	}
	return out
}

// owingTheHuman lists the participants that owe the human a reply.
func (o *Orchestrator) owingTheHuman(ctx context.Context, t *domain.Task) []domain.AgentID {
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		return nil
	}
	var out []domain.AgentID
	seen := map[domain.AgentID]bool{}
	for _, r := range domain.OpenRequests(ledger) {
		q := r.Message
		if q.FromAgent == domain.HumanAgentID && t.HasParticipant(q.ToAgent) && !seen[q.ToAgent] {
			seen[q.ToAgent] = true
			out = append(out, q.ToAgent)
		}
	}
	return out
}

// settleClosing completes a task whose decision was accepted while the human
// still had a question open, once nobody owes the human a reply.
func (o *Orchestrator) settleClosing(ctx context.Context, id domain.TaskID) (bool, error) {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return false, err
	}
	if t.State.IsTerminal() {
		// Ended from outside the loop, e.g. the human approved a closing
		// task: the run has nothing left to do.
		return true, nil
	}
	if !t.Closing {
		return false, nil
	}
	if len(o.owingTheHuman(ctx, t)) > 0 {
		return false, nil
	}
	return true, o.completeAfterDecision(ctx, id, t.AuthorityAgent)
}

// completeAfterDecision walks the task to COMPLETED via AWAITING_DECISION.
//
// The two-step walk matters: the authority may decide from any working state,
// and COMPLETED is only reachable from AWAITING_DECISION. Doing this in one
// hop used to fail whenever the agents reached a conclusion without passing
// through implementation and review — which is exactly what happens when a
// question is answered in a single round.
func (o *Orchestrator) completeAfterDecision(ctx context.Context, id domain.TaskID, by domain.AgentID) error {
	cur, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if cur.State.IsTerminal() {
		return nil
	}

	if cur.State != domain.TaskAwaitingDecision {
		if _, err := o.Tasks.Transition(ctx, id, domain.TaskAwaitingDecision, by, "final decision received"); err != nil {
			// The decision is already recorded and accepted; refusing to
			// complete would strand a finished task. Surface it and stop.
			return fmt.Errorf("completing %s after its final decision: %w", id, err)
		}
	}
	_, err = o.Tasks.Transition(ctx, id, domain.TaskCompleted, by, "final decision accepted")
	return err
}

// proposeDecision turns a FINAL_DECISION message into a durable decision
// record. Decisions are never overwritten; a later one supersedes an earlier.
func (o *Orchestrator) proposeDecision(ctx context.Context, t *domain.Task, m *domain.Message) (*domain.Decision, error) {
	text := m.Metadata["decision"]
	if text == "" {
		text = m.Content
	}
	var evidence []string
	if e := m.Metadata["evidence"]; e != "" {
		evidence = append(evidence, e)
	}
	// A caveat on the evidence is part of the evidence: it travels with the
	// durable record rather than living only in the event log.
	if u := m.Metadata["unansweredRequests"]; u != "" {
		evidence = append(evidence, "CAVEAT: "+u)
	}

	d := &domain.Decision{
		TaskID:       t.ID,
		Author:       m.FromAgent,
		Decision:     text,
		Reasoning:    m.Metadata["reasoning"],
		Evidence:     evidence,
		Participants: append([]domain.AgentID(nil), t.Participants...),
	}

	// A decision still waiting for the human is superseded too, not left
	// beside the new one: two live PROPOSED decisions would leave Approve
	// guessing which one the human means. The authority re-sends its
	// decision, revised or confirmed, after the human answers it.
	prev, err := o.Decisions.Latest(ctx, t.ID)
	if err == nil && prev != nil && (prev.Status == domain.DecisionAccepted || prev.Status == domain.DecisionProposed) {
		d, err = o.Decisions.Supersede(ctx, prev.ID, d)
		if err != nil {
			return nil, err
		}
		o.Events.emit(ctx, domain.EvtDecisionSuperseded, t.ID, m.FromAgent,
			fmt.Sprintf("%s superseded by %s", prev.ID, d.ID), nil)
	} else {
		if d, err = o.Decisions.Propose(ctx, d); err != nil {
			return nil, err
		}
	}

	o.Events.emit(ctx, domain.EvtDecisionCreated, t.ID, m.FromAgent,
		fmt.Sprintf("%s %s", d.ID, d.Decision), map[string]string{"decisionId": string(d.ID)})

	if o.Knowledge != nil {
		if _, err := o.Knowledge.RecordDecision(ctx, d); err != nil {
			o.log.Warn("record decision knowledge", "taskId", string(t.ID), "error", err)
		}
	}
	return d, nil
}

// maxMalformedReplies is how many unreadable replies in a row an agent may
// send before the orchestrator stops asking it again. A probe with every limit
// set ran 1,799 re-prompted turns in three seconds against an agent that never
// produced a header; with this, a broken agent costs at most one extra turn.
const maxMalformedReplies = 2

// malformedExcerptChars bounds how much of an unreadable reply (or of the
// adapter's error about it) is quoted back to the agent and to the human.
const malformedExcerptChars = 300

// malformedText is the best available account of what an agent sent: its raw
// text when the adapter passed it on, otherwise the adapter's account of the
// failure.
func malformedText(out ports.Outbound, f *domain.AgentFailure) string {
	if s := strings.TrimSpace(f.Raw); s != "" {
		return s
	}
	if s := strings.TrimSpace(out.Message.Content); s != "" {
		return s
	}
	return f.Detail
}

// malformedReply handles a reply that could not be read. It is counted
// against the agent and the stall, and the agent is asked again until it has
// sent maxMalformedReplies in a row. Then a supporting agent is dropped from
// the work -- its open requests are closed and whoever asked is told -- and an
// authority, without which nothing can be decided, pauses the task showing
// what it last sent.
func (o *Orchestrator) malformedReply(ctx context.Context, id domain.TaskID, out ports.Outbound, f *domain.AgentFailure) (bool, error) {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return true, err
	}
	agent := out.AgentID
	if !t.HasParticipant(agent) || t.State.IsTerminal() || t.State == domain.TaskPaused {
		return false, nil
	}
	if t, err = o.Tasks.NoteMalformed(ctx, id, agent); err != nil {
		return true, err
	}
	n := t.MalformedCount[agent]
	what := excerpt(malformedText(out, f), malformedExcerptChars)

	if n >= maxMalformedReplies && agent == t.AuthorityAgent {
		reason := fmt.Sprintf("the authority's replies could not be read: %s sent %d unreadable replies in a row; the last: %q",
			agent, n, what)
		o.Events.emit(ctx, domain.EvtLimitReached, id, agent, reason,
			map[string]string{"kind": "MALFORMED_REPLIES", "count": fmt.Sprint(n)})
		_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
		return true, err
	}
	if reason, over := LimitExceeded(t); over {
		o.Events.emit(ctx, domain.EvtLimitReached, id, agent, reason,
			map[string]string{"stalled": fmt.Sprint(t.StallCount)})
		_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
		return true, err
	}
	if n >= maxMalformedReplies {
		o.giveUpOn(ctx, t, agent, n, what)
		return false, nil
	}
	o.notifyMalformed(ctx, t, agent, what)
	return false, nil
}

// giveUpOn stops waiting for a supporting agent whose replies cannot be read:
// it is dropped from the task (dropAgent), so the work is reassigned rather
// than waited on until the idle timeout, and it is sent nothing more.
func (o *Orchestrator) giveUpOn(ctx context.Context, t *domain.Task, agent domain.AgentID, n int, what string) {
	o.Events.emit(ctx, domain.EvtValidationRejected, t.ID, agent,
		fmt.Sprintf("%s sent %d unreadable replies in a row; it is no longer asked to reply again", agent, n),
		map[string]string{"kind": "MALFORMED_REPLIES", "count": fmt.Sprint(n)})
	why := fmt.Sprintf("%s could not produce a valid reply (%d unreadable replies in a row; the last: %q)", agent, n, what)
	o.dropAgent(ctx, t, agent, domain.FailureMalformed, why, "unreadable")
}

// notifyMalformed tells an agent that its last reply could not be understood,
// so it can try again in the protocol instead of the task stalling on silence.
// It shows the exact header to write, with this task's real ids: "as
// specified in your brief" alone did not get a model that had drifted out of
// the format back into it.
func (o *Orchestrator) notifyMalformed(ctx context.Context, t *domain.Task, agent domain.AgentID, what string) {
	if t == nil || !t.HasParticipant(agent) || t.State.IsTerminal() || t.State == domain.TaskPaused {
		return
	}
	next := "If your next reply cannot be read either, your open requests are closed and the authority " +
		"carries on without you."
	if agent == t.AuthorityAgent {
		next = "If your next reply cannot be read either, the task is paused for the human."
	}
	content := fmt.Sprintf(
		"Your last reply could not be read by the orchestrator and was discarded. What it got: %q\n\n"+
			"Reply again. The first line of your reply must be the header, with no prose, code fence or "+
			"blank line before it, for example:\n\n  %s\n\n"+
			"Valid recipients (to=) on this task: %s. %s",
		what, o.headerExample(t, agent), recipientList(t, agent), next)

	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   agent,
		Type:      domain.MsgError,
		Content:   content,
		Metadata:  map[string]string{domain.MetaReason: "malformed-reply"},
	}); err != nil {
		o.log.Warn("notify malformed reply", "taskId", string(t.ID),
			"agentId", string(agent), "error", err)
	}
}

// headerExample is the header line agent should have written for the reply
// most likely expected of it: an answer to the oldest request it owes, or
// else (for the authority) a delegation, or (for anyone else) an answer to
// the authority. The marker and key spelling follow each adapter's brief
// (renderPrompt in internal/agents/claude and internal/agents/codex); the
// orchestrator does not import the adapters, so it goes by the agent's kind,
// and an agent of any other kind is told to use the marker from its brief.
func (o *Orchestrator) headerExample(t *domain.Task, agent domain.AgentID) string {
	typ, to := domain.MsgAnswer, t.AuthorityAgent
	if agent == t.AuthorityAgent {
		typ, to = domain.MsgDelegate, domain.BroadcastAgentID
		for _, p := range t.Participants {
			if p != agent {
				to = p
				break
			}
		}
	}
	if ledger, err := o.ledger(context.Background(), t.ID); err == nil {
		for _, r := range domain.OpenRequests(ledger) {
			if r.Message.ToAgent == agent {
				typ, to = domain.MsgAnswer, r.Message.FromAgent
				break
			}
		}
	}
	var kind domain.AgentKind
	for _, a := range o.Agents.Statuses() {
		if a.ID == agent {
			kind = a.Kind
		}
	}
	switch kind {
	case domain.AgentKindClaudeCode:
		return fmt.Sprintf("@@ORCH type=%s to=%s", typ, to)
	case domain.AgentKindCodex:
		return fmt.Sprintf("##CODEX TYPE=%s TO=%s", typ, to)
	}
	return fmt.Sprintf("<the header marker from your brief> type=%s to=%s", typ, to)
}

// recipientList names everyone agent may address on t.
func recipientList(t *domain.Task, agent domain.AgentID) string {
	var ids []string
	for _, p := range t.Participants {
		switch {
		case p == agent:
		case p == t.AuthorityAgent:
			ids = append(ids, string(p)+" (the authority)")
		default:
			ids = append(ids, string(p))
		}
	}
	ids = append(ids, string(domain.HumanAgentID), string(domain.BroadcastAgentID)+" (everyone)")
	return strings.Join(ids, ", ")
}

// pauseIfOverTimeOrBudget pauses t when it has run past its duration or
// spending limit. It is called on the outbounds that are never routed -- a
// refused action, a forged sender -- which the message limits do not see.
func (o *Orchestrator) pauseIfOverTimeOrBudget(ctx context.Context, t *domain.Task, agent domain.AgentID) (bool, error) {
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		return false, nil
	}
	reason, over := timeOrBudgetExceeded(t)
	if !over {
		return false, nil
	}
	o.Events.emit(ctx, domain.EvtLimitReached, t.ID, agent, reason, nil)
	_, err := o.Tasks.Pause(ctx, t.ID, domain.HumanAgentID, reason)
	return true, err
}

// cannotRun handles an agent whose CLI could not run its model. The model's
// unavailability is recorded so the team view can show it. A supporting
// agent drops out of the task and the authority is told to carry on without
// it; an authority that cannot run pauses the task, saying why and what to do.
func (o *Orchestrator) cannotRun(ctx context.Context, id domain.TaskID, agent domain.AgentID, f *domain.AgentFailure) (bool, error) {
	kind := f.Kind
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return true, err
	}
	provider, model := "", ""
	for _, a := range o.Agents.Statuses() {
		if a.ID == agent {
			provider, model = string(a.Kind), a.Model
		}
	}
	what := model
	if what == "" {
		what = "its default model"
	}
	summary := fmt.Sprintf("%s cannot run %s: %s", agent, what, kind.Advice())
	detail := f.Detail
	if r := []rune(detail); len(r) > 300 {
		detail = string(r[:300])
	}
	o.Events.emit(ctx, domain.EvtModelUnavailable, id, agent, summary, map[string]string{
		"provider": provider, "model": model, "kind": string(kind), "error": detail,
	})

	if agent == t.AuthorityAgent {
		_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, summary)
		return true, err
	}
	o.dropAgent(ctx, t, agent, kind,
		fmt.Sprintf("%s cannot work on this task: its CLI could not run %s (%s)", agent, what, kind.Advice()), "unavailable")
	return false, nil
}

// notifyPartialReply records that only part of an agent's reply followed the
// protocol, and tells the agent what to correct. The rest of the reply was
// delivered.
func (o *Orchestrator) notifyPartialReply(ctx context.Context, t *domain.Task, agent domain.AgentID, problems string) {
	o.Events.emit(ctx, domain.EvtValidationRejected, t.ID, agent,
		fmt.Sprintf("%s's reply was partly understood: %s", agent, problems),
		map[string]string{"kind": "PARTIAL_REPLY"})
	if !t.HasParticipant(agent) || t.State.IsTerminal() || t.State == domain.TaskPaused {
		return
	}
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   agent,
		Type:      domain.MsgError,
		Content: "Part of your last reply did not follow the protocol: " + problems + ". " +
			"The rest was delivered as written. Use only the message types listed in your brief, " +
			"and give every block a recipient (to=).",
		Metadata: map[string]string{domain.MetaReason: "partial-reply"},
	}); err != nil {
		o.log.Warn("notify partial reply", "taskId", string(t.ID), "agentId", string(agent), "error", err)
	}
}

// notifyRejection tells an agent that its action was refused and why.
//
// Without this an agent that emits something invalid gets pure silence: it
// believes it acted, the orchestrator dropped the action, and the task sits
// idle until the timeout fires. Telling the agent gives it the chance to
// correct itself, which is the difference between a recoverable protocol
// mistake and a stalled task.
func (o *Orchestrator) notifyRejection(ctx context.Context, t *domain.Task, m *domain.Message, v *Violation) {
	// Never answer an ERROR with an ERROR, and never write to an agent that is
	// not a participant — both would risk a feedback loop or leak task detail
	// to an identity the orchestrator does not recognise.
	if m.Type == domain.MsgError || !t.HasParticipant(m.FromAgent) {
		return
	}
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		return
	}

	content := fmt.Sprintf(
		"Your %s was rejected by the orchestrator (%s): %s. "+
			"The action did not take effect. Correct it and try again.",
		m.Type, v.Kind, v.Reason)

	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   m.FromAgent,
		Type:      domain.MsgError,
		Content:   content,
		InReplyTo: m.ID,
		Metadata:  map[string]string{domain.MetaViolation: string(v.Kind)},
	}); err != nil {
		o.log.Warn("notify rejection", "taskId", string(t.ID),
			"agentId", string(m.FromAgent), "error", err)
	}
}

// recordDisagreement turns a CHALLENGE into an explicit, durable conflict
// record. The system does not try to force consensus: it stores the claim, the
// counterclaim and the evidence so the disagreement can be settled by evidence
// and remains visible if it never is.
func (o *Orchestrator) recordDisagreement(ctx context.Context, t *domain.Task, m *domain.Message) {
	if o.storage.Disagreements == nil {
		return
	}

	claim := o.claimUnderChallenge(ctx, t, m)
	var evidence []string
	if e := m.Metadata["evidence"]; e != "" {
		evidence = append(evidence, e)
	}

	d := &domain.Disagreement{
		ID:               domain.NewDisagreementID(),
		TaskID:           t.ID,
		Claimant:         m.ToAgent,
		Challenger:       m.FromAgent,
		ChallengeMessage: m.ID,
		Claim:            claim,
		Counterclaim:     m.Content,
		Evidence:         evidence,
		Timestamp:        m.Timestamp,
	}
	if err := o.storage.Disagreements.Create(ctx, d); err != nil {
		o.log.Warn("record disagreement", "taskId", string(t.ID), "error", err)
	}
}

// claimUnderChallenge finds the statement the challenger is disputing: the
// message it replied to, or failing that the most recent message from the
// agent being challenged.
func (o *Orchestrator) claimUnderChallenge(ctx context.Context, t *domain.Task, m *domain.Message) string {
	if m.InReplyTo != "" {
		if prior, err := o.storage.Messages.Get(ctx, m.InReplyTo); err == nil {
			return prior.Content
		}
	}
	history, err := o.storage.Messages.ListByTask(ctx, t.ID)
	if err != nil {
		return ""
	}
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].FromAgent == m.ToAgent && history[i].ID != m.ID {
			return history[i].Content
		}
	}
	return ""
}

// resolveDisagreements settles every open conflict of a task with decision d.
// Resolution is recorded against the existing record rather than deleting it,
// with the decision's id, so rejecting d can reopen them.
func (o *Orchestrator) resolveDisagreements(ctx context.Context, t *domain.Task, by domain.AgentID, d *domain.Decision) {
	if o.storage.Disagreements == nil {
		return
	}
	open, err := o.storage.Disagreements.ListOpen(ctx, t.ID)
	if err != nil {
		o.log.Warn("list disagreements", "taskId", string(t.ID), "error", err)
		return
	}
	now := time.Now()
	for _, dis := range open {
		dis.Resolved = true
		dis.Resolution = d.Decision
		dis.ResolvedBy = by
		dis.ResolvedByDecision = d.ID
		dis.ResolvedAt = &now
		if err := o.storage.Disagreements.Update(ctx, dis); err != nil {
			o.log.Warn("resolve disagreement", "taskId", string(t.ID), "error", err)
		}
	}
}

// reopenDisagreements undoes the resolution decision id gave a task's
// conflicts. They were settled only by that decision; once the human rejects
// it, they are as open as they were before it.
func (o *Orchestrator) reopenDisagreements(ctx context.Context, t *domain.Task, id domain.DecisionID) {
	if o.storage.Disagreements == nil {
		return
	}
	all, err := o.storage.Disagreements.ListByTask(ctx, t.ID)
	if err != nil {
		o.log.Warn("list disagreements", "taskId", string(t.ID), "error", err)
		return
	}
	for _, dis := range all {
		if !dis.Resolved || dis.ResolvedByDecision != id {
			continue
		}
		dis.Resolved = false
		dis.Resolution = ""
		dis.ResolvedBy = ""
		dis.ResolvedByDecision = ""
		dis.ResolvedAt = nil
		if err := o.storage.Disagreements.Update(ctx, dis); err != nil {
			o.log.Warn("reopen disagreement", "taskId", string(t.ID), "error", err)
		}
	}
}

// recordKnowledge promotes investigation findings and test results into shared
// project knowledge, so later rounds can retrieve them instead of replaying
// the entire conversation.
func (o *Orchestrator) recordKnowledge(ctx context.Context, m *domain.Message) {
	if o.Knowledge == nil {
		return
	}
	var err error
	switch m.Type {
	case domain.MsgAnswer, domain.MsgChallenge:
		_, err = o.Knowledge.RecordInvestigation(ctx, m.TaskID, m.FromAgent,
			fmt.Sprintf("%s %s", m.Type, m.ID), m.Content)
	case domain.MsgTestResult, domain.MsgImplementationResult:
		_, err = o.Knowledge.RecordTestResult(ctx, m.TaskID, m.FromAgent,
			fmt.Sprintf("%s %s", m.Type, m.ID), m.Content)
	default:
		return
	}
	if err != nil {
		o.log.Warn("record knowledge", "taskId", string(m.TaskID), "error", err)
	}
}

// assign sends the opening TASK_ASSIGNMENT to the authority agent, and only to
// the authority agent.
//
// Supporting agents are deliberately left idle until they are delegated to.
// Briefing everyone at once looks harmless but breaks the authority model in
// practice: a supporting agent that receives the raw task starts working on
// its own reading of it, answers unprompted, and advances task state before
// the authority has decided what investigation is even needed. The authority
// deciding what work happens is the point of the role, so the orchestrator
// enforces it here rather than asking agents to restrain themselves.
// humanAnswerGrace is how long the agents may go quiet, with nobody working
// and a question of the human's still open, before the agent the human asked
// is reminded -- and, after a reminder that also goes unanswered, before the
// task pauses saying exactly what it is waiting for.
var humanAnswerGrace = 45 * time.Second

// chaseOpenRequests keeps requests from quietly going unanswered while the
// agents talk among themselves or fall silent. It acts only when nobody is
// working and nothing has happened for a while -- an answer still being
// prepared is not a missing one -- and on one request per pass.
//
// It starts at the end of a chain: when the human asked A and A is waiting on
// C, it is C that is silent, so C is reminded and A is left alone. A request
// that is still unanswered after its reminder is handled by who asked it:
//
//   - the human's question pauses the task, saying exactly what is missing;
//   - an agent's request is closed as unanswered. If the human's question it
//     served is still open, the agent that asked is told to answer the human
//     with what it has; if the human already has an answer, it is recorded
//     and the follow-up can complete.
func (o *Orchestrator) chaseOpenRequests(ctx context.Context, id domain.TaskID, lastActivity time.Time,
	reminded map[domain.MessageID]time.Time) (done, acted bool, err error) {

	if time.Since(lastActivity) < humanAnswerGrace {
		return false, false, nil
	}
	t, err := o.Tasks.Get(ctx, id)
	if err != nil || t.State.IsTerminal() || t.State == domain.TaskPaused {
		return false, false, err
	}
	ledger, err := o.ledger(ctx, id)
	if err != nil {
		return false, false, err
	}
	r := oldestLeaf(ledger)
	if r == nil {
		return false, false, nil
	}
	for _, p := range t.Participants {
		if ad, ok := o.Agents.AdapterFor(t.ID, p); ok && ad.Status() == domain.AgentStateWorking {
			return false, false, nil
		}
	}

	q := r.Message
	at, already := reminded[q.ID]
	if !already {
		o.remind(ctx, t, q)
		reminded[q.ID] = time.Now()
		return false, true, nil
	}
	if time.Since(at) < humanAnswerGrace {
		return false, false, nil
	}
	if q.FromAgent == domain.HumanAgentID && t.Closing {
		// The decision is already accepted: the task is done, the question
		// was not answered, and the record says so.
		o.Events.emit(ctx, domain.EvtRequestAbandoned, id, q.ToAgent,
			fmt.Sprintf("%s never answered your %s; the task completed with its decision", q.ToAgent, q.Type),
			map[string]string{"request": string(q.ID)})
		return true, false, o.completeAfterDecision(ctx, id, t.AuthorityAgent)
	}
	if q.FromAgent == domain.HumanAgentID {
		reason := waitingReason(q)
		o.Events.emit(ctx, domain.EvtLimitReached, id, "", reason, nil)
		_, err = o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
		return true, false, err
	}
	o.abandon(ctx, t, ledger, *r)
	return false, true, nil
}

// oldestLeaf returns the oldest open request whose owner is not itself
// waiting on anyone -- the one actually holding things up. Requests the human
// owes are skipped: the orchestrator cannot remind the human.
//
// An owner waiting only on requests that the one in hand was asked in service
// of is not held up by them. When opus delegates to codex and codex asks opus
// to clarify, each waits on the other, but it is opus who must speak: codex's
// question is what the delegation is waiting for. Without this, now that the
// question no longer closes the delegation, neither would ever be reminded.
func oldestLeaf(ledger []domain.Request) *domain.Request {
	byID := map[domain.MessageID]*domain.Request{}
	for i := range ledger {
		byID[ledger[i].Message.ID] = &ledger[i]
	}
	open := domain.OpenRequests(ledger)
	for i := range open {
		r := &open[i]
		owner := r.Message.ToAgent
		if owner == domain.HumanAgentID {
			continue
		}
		above := map[domain.MessageID]bool{}
		for p := r.Parent; p != "" && !above[p]; {
			above[p] = true
			parent, ok := byID[p]
			if !ok {
				break
			}
			p = parent.Parent
		}
		blocked := false
		for _, q := range open {
			if q.Message.FromAgent == owner && !above[q.Message.ID] {
				blocked = true
				break
			}
		}
		if !blocked {
			return r
		}
	}
	return nil
}

// abandon closes an agent's request that stayed unanswered after a reminder.
func (o *Orchestrator) abandon(ctx context.Context, t *domain.Task, ledger []domain.Request, r domain.Request) {
	q := r.Message
	var root *domain.Request
	for i := range ledger {
		if ledger[i].Message.ID == r.Thread {
			root = &ledger[i]
		}
	}
	humanWaiting := root != nil && root.Message.FromAgent == domain.HumanAgentID && root.Open()

	content := fmt.Sprintf("%s did not answer your %s (%q) after a reminder; the orchestrator has closed it as unanswered.",
		q.ToAgent, q.Type, excerpt(q.Content, 120))
	// Whether the agent that asked needs to act decides whether it is woken.
	// With the human waiting it must answer them with what it has; in an
	// ordinary deliberation it must carry on without the reply. When the
	// human already has an answer, the closure is only recorded, and the
	// follow-up can complete.
	mode := "closed"
	switch {
	case humanWaiting:
		content += " Give the human what you have now (to=human), and say what is missing."
		mode = "closed-notify"
	case !t.FollowUp:
		content += " Carry on without it."
		mode = "closed-notify"
	}
	o.Events.emit(ctx, domain.EvtRequestAbandoned, t.ID, q.ToAgent,
		fmt.Sprintf("%s never answered %s's %s: closed as unanswered", q.ToAgent, q.FromAgent, q.Type),
		map[string]string{"request": string(q.ID)})
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   q.FromAgent,
		Type:      domain.MsgInformation,
		Content:   content,
		Metadata:  map[string]string{domain.MetaOrchestrator: mode, domain.MetaCloses: string(q.ID)},
	}); err != nil {
		o.log.Warn("close unanswered request", "taskId", string(t.ID), "error", err)
	}
}

// handsOutWork reports whether m is the authority giving the other agents new
// work -- an assignment or an implementation -- rather than consulting them.
// A follow-up that does this is a new piece of work, not a question.
func handsOutWork(t *domain.Task, m *domain.Message) bool {
	if m.FromAgent != t.AuthorityAgent || m.ToAgent == domain.HumanAgentID {
		return false
	}
	return m.Type == domain.MsgTaskAssignment || m.Type == domain.MsgImplement
}

// followUpSettleInterval is how often a reopened task checks whether the
// follow-up it was reopened for has been answered.
var followUpSettleInterval = 2 * time.Second

// settleFollowUp completes a reopened task once its follow-up is done: every
// request in the ledger answered (the human's question, and anything the
// agents asked each other to answer it) and no agent still working. The
// accepted decision is untouched; a follow-up that changes it does so with a
// new FINAL_DECISION, which completes the task through the usual path.
//
// Without this a follow-up never ended: once the human's question was
// answered, the agents kept exchanging remarks until a limit paused them.
//
// A follow-up in which the authority handed out new work is not settled
// here. Agents working together need not have a request open between them,
// so "nothing outstanding" would complete the task in the middle of the work
// -- which a real run did, the moment it was resumed. Such a follow-up ends
// the way the task did: with a FINAL_DECISION.
func (o *Orchestrator) settleFollowUp(ctx context.Context, id domain.TaskID) (bool, error) {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil || !t.FollowUp || t.FollowUpWork || t.State.IsTerminal() {
		return false, err
	}
	ledger, err := o.ledger(ctx, id)
	if err != nil {
		return false, err
	}
	if len(domain.OpenRequests(ledger)) > 0 {
		return false, nil
	}
	for _, p := range t.Participants {
		if ad, ok := o.Agents.AdapterFor(t.ID, p); ok && ad.Status() == domain.AgentStateWorking {
			return false, nil
		}
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID,
		"follow-up answered; task completed again (decision unchanged)", nil)
	if cur := t.State; cur != domain.TaskAwaitingDecision {
		if _, err := o.Tasks.Transition(ctx, id, domain.TaskAwaitingDecision, domain.HumanAgentID, "follow-up answered"); err != nil {
			return false, err
		}
	}
	_, err = o.Tasks.Transition(ctx, id, domain.TaskCompleted, domain.HumanAgentID, "follow-up answered")
	return err == nil, err
}

// kickoff starts (or restarts) the conversation when the run loop comes up.
//
// Only a task with no history gets the opening assignment. Resuming a paused
// task used to re-send it, re-briefing the authority as though nothing had
// happened yet; with agents now keeping their own sessions that would also
// contradict what they remember. A task the human sent something to while no
// run was alive -- a follow-up, an answer, a rejection -- resumes with that
// instead, and a resumed one is told to carry on (resumeAfterPause).
func (o *Orchestrator) kickoff(ctx context.Context, t *domain.Task) error {
	// Starting (or restarting) a run is the human's doing.
	if _, err := o.Tasks.NoteHumanInput(ctx, t.ID); err != nil {
		return err
	}
	o.mu.Lock()
	if r := o.running[t.ID]; r != nil {
		r.live = true
	}
	queued := o.queued[t.ID]
	delete(o.queued, t.ID)
	pausedFor, resumed := o.resumed[t.ID]
	delete(o.resumed, t.ID)
	o.mu.Unlock()
	if len(queued) > 0 {
		for _, m := range queued {
			if err := o.Bus.Route(ctx, t, m); err != nil {
				return fmt.Errorf("deliver the human's %s for %s: %w", m.Type, t.ID, err)
			}
		}
		return nil
	}

	history, err := o.storage.Messages.ListByTask(ctx, t.ID)
	if err != nil {
		return err
	}
	if len(history) == 0 {
		return o.assign(ctx, t) // paused before it began: nothing to resume
	}
	if resumed {
		return o.resumeAfterPause(ctx, t, pausedFor)
	}
	return nil
}

// Reopen brings a completed task back so a human can follow it up with
// agent to, and queues content as the message the run loop opens with. The
// agents resume their own CLI sessions, so they answer with the whole earlier
// deliberation in mind. The caller starts RunTask.
func (o *Orchestrator) Reopen(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if !t.HasParticipant(to) {
		return fmt.Errorf("reopen %s: %s is not a participant", id, to)
	}
	if err := o.checkTeam(t); err != nil {
		return err
	}
	if _, err := o.Tasks.Reopen(ctx, id, domain.HumanAgentID, "follow-up to "+string(to)); err != nil {
		return err
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "REOPEN -> "+string(to), nil)
	o.mu.Lock()
	o.queued[id] = []*domain.Message{{
		FromAgent:        domain.HumanAgentID,
		ToAgent:          to,
		Type:             domain.MsgInformation,
		Content:          content,
		RequiresResponse: true,
		Metadata:         map[string]string{ports.FollowUpMetadataKey: "true"},
	}}
	o.mu.Unlock()
	return nil
}

// queue holds msgs for delivery when the task's next run starts.
func (o *Orchestrator) queue(id domain.TaskID, msgs ...*domain.Message) {
	o.mu.Lock()
	o.queued[id] = append(o.queued[id], msgs...)
	o.mu.Unlock()
}

// pauseAfterError pauses a task whose run ended on an error, with the error
// as the reason. A storage or routing error returned from the run used to
// leave the task PLANNING or DELIBERATING with nothing driving it, until a
// restart found it. A run ended by its context -- a pause, a cancel, ritam
// exiting -- is not an error here: whoever ended it set the task's state.
func (o *Orchestrator) pauseAfterError(ctx context.Context, id domain.TaskID, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	t, gerr := o.Tasks.Get(ctx, id)
	if gerr != nil {
		o.log.Error("pause a task whose run failed", "taskId", string(id), "error", err, "readError", gerr)
		return
	}
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		return
	}
	reason := fmt.Sprintf("the run stopped on an error (%v); resume to try again", err)
	o.Events.emit(ctx, domain.EvtLimitReached, id, "", reason, map[string]string{"kind": "RUN_ERROR"})
	if _, perr := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason); perr != nil {
		o.log.Error("pause a task whose run failed", "taskId", string(id), "error", err, "pauseError", perr)
	}
}

// taskRun is one RunTask holding a task: queued for a slot, then live.
type taskRun struct {
	cancel context.CancelFunc
	// live is set once the run's agents are up and it delivers messages
	// itself. Until then what the human sends is queued for its kickoff
	// (deliverOrQueue): routed straight away, it would reach agents that
	// are not started.
	live bool
}

// acquireSlot waits for one of the slots that bound how many tasks run at
// once, and reports whether the run had to wait. A run that waits is
// announced, so the human can see it is queued rather than stuck.
func (o *Orchestrator) acquireSlot(ctx context.Context, id domain.TaskID) (waited bool, err error) {
	select {
	case o.sem <- struct{}{}:
		return false, nil
	default:
	}
	o.Events.emit(ctx, domain.EvtRunQueued, id, "",
		fmt.Sprintf("queued: %d task(s) already running, the most allowed at once; it starts when one ends", cap(o.sem)),
		nil)
	select {
	case o.sem <- struct{}{}:
		return true, nil
	case <-ctx.Done():
		o.Events.emit(context.WithoutCancel(ctx), domain.EvtRunUnqueued, id, "",
			"the queued run was withdrawn before it started", nil)
		return true, ctx.Err()
	}
}

// Running reports whether a run is alive or queued for the task, so that a
// caller that has just queued something for it knows whether to start one.
func (o *Orchestrator) Running(id domain.TaskID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.running[id]
	return ok
}

// Queued reports whether the task's run is waiting for a slot.
func (o *Orchestrator) Queued(id domain.TaskID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.running[id]
	return ok && !r.live
}

// live reports whether the task's run has its agents up and is delivering.
func (o *Orchestrator) live(id domain.TaskID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, ok := o.running[id]
	return ok && r.live
}

// deliverOrQueue routes msgs on a task whose run is live, and otherwise
// queues them for the next run's kickoff. Deciding and queueing are one step
// under o.mu, as is the kickoff marking its run live and taking the queue:
// a message queued a moment before the run went live is still delivered by
// it, never left for a run that may not come.
func (o *Orchestrator) deliverOrQueue(ctx context.Context, t *domain.Task, msgs ...*domain.Message) error {
	o.mu.Lock()
	if r, ok := o.running[t.ID]; !ok || !r.live {
		o.queued[t.ID] = append(o.queued[t.ID], msgs...)
		o.mu.Unlock()
		return nil
	}
	o.mu.Unlock()
	for _, m := range msgs {
		if err := o.Bus.Route(ctx, t, m); err != nil {
			return err
		}
	}
	return nil
}

// heldDecision returns the task's decision still waiting for the human, or
// nil when there is none.
func (o *Orchestrator) heldDecision(ctx context.Context, t *domain.Task) *domain.Decision {
	d, err := o.Decisions.Latest(ctx, t.ID)
	if err != nil || d == nil || d.Status != domain.DecisionProposed {
		return nil
	}
	return d
}

// waitsOnlyOnTheHuman reports whether a run starting on t would have nothing
// to do: its decision waits for the human and the human has sent nothing. A
// pause and resume of such a task used to start a run that told the authority
// to "carry on" -- a paid turn, and an invitation to work past a decision
// the human had not yet looked at. The task is left as it is instead.
func (o *Orchestrator) waitsOnlyOnTheHuman(ctx context.Context, t *domain.Task) bool {
	if t.State != domain.TaskAwaitingDecision || t.Closing {
		return false
	}
	o.mu.Lock()
	pending := len(o.queued[t.ID])
	o.mu.Unlock()
	if pending > 0 {
		return false
	}
	d := o.heldDecision(ctx, t)
	if d == nil {
		return false
	}
	o.mu.Lock()
	delete(o.resumed, t.ID)
	o.mu.Unlock()
	o.Events.emit(ctx, domain.EvtHumanOverride, t.ID, domain.HumanAgentID,
		fmt.Sprintf("decision %s still awaits you: answer, approve or reject it", d.ID),
		map[string]string{"decisionId": string(d.ID)})
	return true
}

// decisionPendingNote tells the authority that its decision d still waits
// for the human, who has just sent answer. The authority would otherwise not
// know whether the decision stands after the human's input; this asks it to
// say so, by sending FINAL_DECISION again, revised or not, which supersedes d.
//
// It points (InReplyTo) at the human's message it is about. Being the
// orchestrator's note, it is never taken for the human's reply to a question
// the authority asked: only the human's own message is that.
func decisionPendingNote(t *domain.Task, d *domain.Decision, answer *domain.Message) *domain.Message {
	to := "you"
	if answer.ToAgent != t.AuthorityAgent {
		to = string(answer.ToAgent)
	}
	return &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   t.AuthorityAgent,
		Type:      domain.MsgInformation,
		InReplyTo: answer.ID,
		Content: fmt.Sprintf("Note from the orchestrator: the human has sent %s a message, and your decision %s "+
			"(%q) is still waiting for the human. Once you have what you need, confirm it by sending "+
			"FINAL_DECISION again, or send a revised FINAL_DECISION.", to, d.ID, excerpt(d.Decision, 160)),
		Metadata: map[string]string{domain.MetaOrchestrator: "decision-pending", "decisionId": string(d.ID)},
	}
}

// ErrTaskNotRunning is returned when an instruction is sent to a task whose
// agents are not running, so there is nobody to deliver it to.
var ErrTaskNotRunning = errors.New("task is not running")

func (o *Orchestrator) assign(ctx context.Context, t *domain.Task) error {
	supporting := make([]string, 0, len(t.Participants))
	for _, p := range t.Participants {
		if p != t.AuthorityAgent {
			supporting = append(supporting, string(p))
		}
	}

	content := fmt.Sprintf(
		"Task %s: %s\n\n%s\n\nYou are the authoritative agent for this task. "+
			"Decide what investigation is required, delegate it, evaluate the responses, "+
			"challenge conclusions you do not believe, and make the final decision. "+
			"Authority is responsibility, not correctness: revise your position when the evidence warrants.\n\n"+
			"The other participants (%s) have NOT been briefed and are idle. "+
			"They will not act until you delegate to them, so state what you want investigated or implemented.",
		t.ID, t.Title, t.Description, strings.Join(supporting, ", "))

	m := &domain.Message{
		TaskID:           t.ID,
		FromAgent:        domain.HumanAgentID,
		ToAgent:          t.AuthorityAgent,
		Type:             domain.MsgTaskAssignment,
		Content:          content,
		RequiresResponse: true,
	}
	if err := o.Bus.Route(ctx, t, m); err != nil {
		return fmt.Errorf("assign %s to %s: %w", t.ID, t.AuthorityAgent, err)
	}
	return nil
}

// checkTeam refuses to restart a task whose agents are no longer on the
// team (dismissed, or defined in an ritam.yaml that no longer lists them).
// Starting it would fail at once and leave the task FAILED; refused, it
// stays as it was and says why.
func (o *Orchestrator) checkTeam(t *domain.Task) error {
	known := map[domain.AgentID]bool{}
	for _, a := range o.Agents.Statuses() {
		known[a.ID] = true
	}
	var gone []string
	for _, p := range t.Participants {
		if !known[p] {
			gone = append(gone, string(p))
		}
	}
	if len(gone) == 0 {
		return nil
	}
	verb := "are"
	if len(gone) == 1 {
		verb = "is"
	}
	return fmt.Errorf("%s cannot continue: %s %s no longer on the team; start a new task with the agents you have",
		t.ID, strings.Join(gone, ", "), verb)
}

func (o *Orchestrator) startParticipants(ctx context.Context, t *domain.Task) error {
	for _, p := range t.Participants {
		if err := o.Agents.StartFor(ctx, t.ID, p); err != nil {
			return fmt.Errorf("start agent %s: %w", p, err)
		}
	}
	return nil
}

// stopParticipants stops the task's agents all at once, waiting for them no
// longer than ctx allows. They used to be stopped one after another, each
// taking as long as its CLI took to die.
func (o *Orchestrator) stopParticipants(ctx context.Context, t *domain.Task) {
	var wg sync.WaitGroup
	for _, p := range t.Participants {
		wg.Add(1)
		go func(p domain.AgentID) {
			defer wg.Done()
			if err := o.Agents.StopFor(ctx, t.ID, p); err != nil {
				o.log.Warn("stop agent", "agentId", string(p), "error", err)
			}
		}(p)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		o.log.Warn("agents still stopping after the deadline", "taskId", string(t.ID))
	}
}

// outboundMerge fans every participant's outbound channel into out, which
// closes once every goroutine feeding it has finished: when its adapter
// closed its channel, when flush is closed and what was left in the channel
// has been handed on, or when abort is called.
type outboundMerge struct {
	out   chan ports.Outbound
	flush chan struct{}
	abort context.CancelFunc
}

// mergeOutbound takes every participant's outbound channel and starts
// feeding them into one. It outlives the run loop: what the agents report
// after the loop has ended is still read (endRun), which is why it is not
// bound to the run's context.
func (o *Orchestrator) mergeOutbound(t *domain.Task) *outboundMerge {
	ctx, abort := context.WithCancel(context.Background())
	mg := &outboundMerge{out: make(chan ports.Outbound, 64), flush: make(chan struct{}), abort: abort}
	send := func(out ports.Outbound) bool {
		select {
		case mg.out <- out:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var wg sync.WaitGroup
	for _, p := range t.Participants {
		ad, ok := o.Agents.AdapterFor(t.ID, p)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(ch <-chan ports.Outbound) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-mg.flush:
					// The adapter has stopped: whatever it reported is in its
					// channel by now.
					for {
						select {
						case out, ok := <-ch:
							if !ok || !send(out) {
								return
							}
						default:
							return
						}
					}
				case out, ok := <-ch:
					if !ok || !send(out) {
						return
					}
				}
			}
		}(ad.Outbound())
	}
	go func() {
		wg.Wait()
		close(mg.out)
	}()
	return mg
}

// usageGrace bounds how long the agents whose outbound ended the run are
// given to finish the turn it came from before they are stopped: their usage
// is sent after their messages.
var usageGrace = time.Second

// stopDeadline bounds stopping a run's agents and collecting what they still
// report.
var stopDeadline = 15 * time.Second

// endRun stops the task's agents and bills every turn they took.
//
// Usage is sent after a turn's messages, so the message that ended the run
// -- the deciding FINAL_DECISION, a decision held for the human, the one that
// crossed a limit, a failure -- always arrived before its turn's usage, and
// nobody read the channels once the loop had returned: the deciding turn of
// every task was never billed. Nor was any turn killed by the stop, whether
// by a pause, a cancel, a completion or a co-worker crossing the budget.
//
// So, in order: the agents that ended the run get a short grace to finish
// their turn; every agent is stopped at once; and what they report until
// they have stopped is read, its usage added to the task and the day and
// everything else ignored -- the run is over, nothing more is routed.
func (o *Orchestrator) endRun(ctx context.Context, t *domain.Task, mg *outboundMerge, enders []domain.AgentID) {
	defer mg.abort()
	in := mg.out
	take := func(out ports.Outbound, ok bool) {
		if !ok {
			in = nil
			return
		}
		if out.IsUsageOnly() {
			o.recordLateUsage(ctx, t.ID, out)
		}
	}

	if len(enders) > 0 {
		grace := time.NewTimer(usageGrace)
		tick := time.NewTicker(10 * time.Millisecond)
	wait:
		for in != nil && o.anyWorking(t.ID, enders) {
			select {
			case out, ok := <-in:
				take(out, ok)
			case <-tick.C:
			case <-grace.C:
				break wait
			}
		}
		grace.Stop()
		tick.Stop()
	}

	stopCtx, cancel := context.WithTimeout(ctx, stopDeadline)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		o.stopParticipants(stopCtx, t)
		close(stopped)
	}()
	for in != nil || stopped != nil {
		select {
		case out, ok := <-in:
			take(out, ok)
		case <-stopped:
			stopped = nil
			close(mg.flush)
		case <-stopCtx.Done():
			o.log.Warn("stopped collecting the agents' last usage at the deadline", "taskId", string(t.ID))
			return
		}
	}
}

// anyWorking reports whether any of agents is in the middle of a turn.
func (o *Orchestrator) anyWorking(id domain.TaskID, agents []domain.AgentID) bool {
	for _, a := range agents {
		if ad, ok := o.Agents.AdapterFor(id, a); ok && ad.Status() == domain.AgentStateWorking {
			return true
		}
	}
	return false
}

// recordLateUsage adds the usage of a turn reported after the run ended to
// the task and to the day's spending. Nothing else is done with it: the task
// has already stopped.
func (o *Orchestrator) recordLateUsage(ctx context.Context, id domain.TaskID, out ports.Outbound) {
	if _, err := o.addUsage(ctx, id, out); err != nil {
		o.log.Error("record the usage of a turn that ended with the run", "taskId", string(id), "error", err)
		return
	}
	o.overDailyLimit(*out.Usage)
}

// streamActivity forwards each participant's live output to subscribers for
// display. It is deliberately separate from the outbound message path: nothing
// here can affect routing, authority or task state, so a noisy or malformed
// stream cannot influence the deliberation.
func (o *Orchestrator) streamActivity(ctx context.Context, t *domain.Task, alive chan<- struct{}) {
	for _, p := range t.Participants {
		ad, ok := o.Agents.AdapterFor(t.ID, p)
		if !ok {
			continue
		}
		ch := ad.Activity()
		if ch == nil {
			continue
		}
		go func(ch <-chan ports.AgentActivity) {
			for {
				select {
				case <-ctx.Done():
					return
				case a, ok := <-ch:
					if !ok {
						return
					}
					// An agent producing output is alive, however long since
					// it last sent a message. Non-blocking: the loop only needs
					// to know something happened, not how often.
					select {
					case alive <- struct{}{}:
					default:
					}
					o.Events.PublishEphemeral(domain.Event{
						Type:    domain.EvtAgentActivity,
						TaskID:  a.TaskID,
						AgentID: a.AgentID,
						Summary: a.Text,
						Details: map[string]string{"kind": string(a.Kind)},
					})
				}
			}
		}(ch)
	}
}

// ---------------------------------------------------------------------------
// Human override surface
// ---------------------------------------------------------------------------

// Pause halts a task at the human's request.
func (o *Orchestrator) Pause(ctx context.Context, id domain.TaskID, reason string) error {
	if reason == "" {
		reason = domain.PauseReasonHuman
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "PAUSE: "+reason, nil)
	o.cancelRun(id)
	_, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
	return err
}

// RaiseLimits raises a task's message ceiling or spending limits at the
// human's request (see TaskManager.RaiseLimits). It does not resume the
// task: a task paused at a limit is resumed as usual, and then has room.
func (o *Orchestrator) RaiseLimits(ctx context.Context, id domain.TaskID, more domain.TaskLimits) error {
	before, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	after, err := o.Tasks.RaiseLimits(ctx, id, more)
	if err != nil {
		return err
	}
	var parts []string
	if b, a := before.Limits.MaxTotalMessages, after.Limits.MaxTotalMessages; a != b {
		parts = append(parts, fmt.Sprintf("messages %d -> %d (%d used)", b, a, after.MessageCount))
	}
	if b, a := before.Limits.MaxCostUSD, after.Limits.MaxCostUSD; a != b {
		parts = append(parts, fmt.Sprintf("budget $%.2f -> $%.2f ($%.2f spent)", b, a, after.CostUSD))
	}
	if b, a := before.Limits.MaxTokens, after.Limits.MaxTokens; a != b {
		parts = append(parts, fmt.Sprintf("tokens %d -> %d (%d used)", b, a, after.InputTokens+after.OutputTokens))
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "LIMITS raised: "+strings.Join(parts, "; "), nil)
	return nil
}

// Resume restores a paused task to the state it held before the pause. The
// caller is responsible for calling RunTask again to resume deliberation.
func (o *Orchestrator) Resume(ctx context.Context, id domain.TaskID) error {
	before, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := o.checkTeam(before); err != nil {
		return err
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "RESUME", nil)
	if _, err := o.Tasks.Resume(ctx, id, domain.HumanAgentID); err != nil {
		return err
	}
	// The human resuming the task gives every agent a fresh chance: those
	// dropped for failing are back, with no failures counted against them.
	if len(before.Dropped) > 0 {
		if _, err := o.Tasks.ClearDropped(ctx, id); err != nil {
			return err
		}
	}
	o.clearFailures(id, "")
	o.mu.Lock()
	o.resumed[id] = before.PauseReason
	o.mu.Unlock()
	return nil
}

// resumedAfterFailure marks, in resumed, a task the human retried after it
// failed rather than resumed after a pause.
const resumedAfterFailure = "\x00failed: "

// Retry brings a FAILED task back, at the human's request: the same
// participants, their sessions kept, and the authority told it was retried
// and why it had failed. The caller starts RunTask.
//
// FAILED was terminal -- no resume, reopen or retry -- so a task failed by
// one agent's timeout, by an older version of ritam, could only be abandoned
// with all its work.
func (o *Orchestrator) Retry(ctx context.Context, id domain.TaskID) error {
	before, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if before.State != domain.TaskFailed {
		return fmt.Errorf("retry %s: the task is %s; only a failed task can be retried", id, before.State)
	}
	if err := o.checkTeam(before); err != nil {
		return err
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "RETRY", nil)
	if _, err := o.Tasks.Retry(ctx, id, domain.HumanAgentID); err != nil {
		return err
	}
	o.clearFailures(id, "")
	o.mu.Lock()
	o.resumed[id] = resumedAfterFailure + before.FailureReason
	o.mu.Unlock()
	return nil
}

// RecoverInterrupted pauses every task that was mid-run when ritam last
// exited. Without it such a task would sit in a working state with no run
// loop behind it: nothing would happen, and nothing would say why. Paused,
// it shows as INTERRUPTED and the human resumes it when ready. Tasks waiting
// on the human (CREATED, PAUSED, AWAITING_DECISION) are left as they are.
func (o *Orchestrator) RecoverInterrupted(ctx context.Context) ([]domain.TaskID, error) {
	tasks, err := o.Tasks.List(ctx)
	if err != nil {
		return nil, err
	}
	var recovered []domain.TaskID
	for _, t := range tasks {
		switch {
		case t.State.IsTerminal(), t.State == domain.TaskCreated, t.State == domain.TaskPaused,
			t.State == domain.TaskAwaitingDecision:
			continue
		}
		if _, err := o.Tasks.Pause(ctx, t.ID, domain.HumanAgentID, domain.PauseReasonInterrupted); err != nil {
			return recovered, fmt.Errorf("recover %s: %w", t.ID, err)
		}
		recovered = append(recovered, t.ID)
	}
	return recovered, nil
}

// resumeAfterPause opens the run of a resumed task. A pause stops the agent
// processes, so any turn in progress was lost, and nothing restarts the
// conversation by itself: before this, a task resumed after a stall or a
// budget pause sat with every agent idle until the idle timeout -- or, in a
// follow-up, was completed on the spot because nobody was working. The
// agents' sessions kept the conversation; each agent that owes a reply is
// told to send it again, or, when nobody owes one, the authority is told to
// carry on.
func (o *Orchestrator) resumeAfterPause(ctx context.Context, t *domain.Task, pausedFor string) error {
	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		return err
	}
	owe := map[domain.AgentID][]string{}
	var order []domain.AgentID
	for _, r := range domain.OpenRequests(ledger) {
		q := r.Message
		if q.ToAgent == domain.HumanAgentID || !t.HasParticipant(q.ToAgent) {
			continue
		}
		if _, seen := owe[q.ToAgent]; !seen {
			order = append(order, q.ToAgent)
		}
		owe[q.ToAgent] = append(owe[q.ToAgent],
			fmt.Sprintf("%s's %s: %q", q.FromAgent, q.Type, excerpt(q.Content, threadExcerptChars)))
	}
	failedFor, retried := strings.CutPrefix(pausedFor, resumedAfterFailure)
	if len(order) == 0 || (retried && !slices.Contains(order, t.AuthorityAgent)) {
		// The authority is always told of a retry: it decides what next.
		order = append(order, t.AuthorityAgent)
	}

	why := "The task was paused and the human has resumed it"
	if pausedFor != "" {
		why = fmt.Sprintf("The task was paused (%s) and the human has resumed it", pausedFor)
	}
	summary := "resumed: agents asked to carry on"
	switch {
	case pausedFor == domain.PauseReasonInterrupted:
		why = "The orchestrator was restarted while this task was running"
		summary = "resumed after an interruption: lost turns are being re-requested"
	case retried:
		why = fmt.Sprintf("The task had failed (%s) and the human has retried it", excerpt(failedFor, 300))
		summary = "retried after failing: agents asked to carry on"
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, t.ID, domain.HumanAgentID, summary, nil)
	for _, agent := range order {
		content := why + ", so any reply you were preparing was lost. Your session still has the conversation. "
		if items := owe[agent]; len(items) > 0 {
			content += "You still owe a reply to: " + strings.Join(items, "; ") + ". Send it now."
		} else {
			content += "Carry on with the task from where it stood."
		}
		if err := o.Bus.Route(ctx, t, &domain.Message{
			FromAgent: domain.OrchestratorID,
			ToAgent:   agent,
			Type:      domain.MsgInformation,
			Content:   content,
			Metadata:  map[string]string{domain.MetaOrchestrator: "resumed"},
		}); err != nil {
			return fmt.Errorf("resume %s: %w", t.ID, err)
		}
	}
	return nil
}

// SetAuthority hands a task's authority to another participant, at the
// human's request: the new authority issues the FINAL_DECISION from now on
// and the old one becomes a supporting agent. A running task's agents are
// told at once; every later turn's brief states each agent's role, so an
// agent that was not running learns it on its next turn.
func (o *Orchestrator) SetAuthority(ctx context.Context, id domain.TaskID, to domain.AgentID) error {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.State == domain.TaskFailed || t.State == domain.TaskCancelled {
		return fmt.Errorf("change authority of %s: the task is %s", id, t.State)
	}
	if !t.HasParticipant(to) {
		return fmt.Errorf("change authority of %s: %s is not a participant", id, to)
	}
	from := t.AuthorityAgent
	if from == to {
		return nil
	}
	if t, err = o.Tasks.SetAuthority(ctx, id, to); err != nil {
		return err
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID,
		fmt.Sprintf("AUTHORITY %s -> %s", from, to), map[string]string{"from": string(from), "to": string(to)})

	if !o.live(id) {
		// A queued run's agents learn it from their next turn's brief.
		return nil
	}
	for _, p := range t.Participants {
		var content string
		switch p {
		case to:
			content = fmt.Sprintf("The human has made you the AUTHORITY for this task, taking over from %s. "+
				"You now decide what is investigated and by whom, weigh the evidence, and issue the FINAL_DECISION. "+
				"Review where the work stands before acting.", from)
		case from:
			content = fmt.Sprintf("The human has made %s the authority for this task. You are now a SUPPORTING agent: "+
				"you may investigate, propose, review and challenge, but you may not issue FINAL_DECISION. "+
				"Hand %s anything it needs to take over.", to, to)
		default:
			content = fmt.Sprintf("The human has made %s the authority for this task, taking over from %s. "+
				"Report to %s from now on.", to, from, to)
		}
		if err := o.Bus.Route(ctx, t, &domain.Message{
			FromAgent: domain.OrchestratorID,
			ToAgent:   p,
			Type:      domain.MsgInformation,
			Content:   content,
			Metadata:  map[string]string{domain.MetaOrchestrator: "authority"},
		}); err != nil {
			o.log.Warn("announce authority change", "taskId", string(id), "agentId", string(p), "error", err)
		}
	}
	return nil
}

// AgentInUse reports whether id takes part in a task that is running now.
func (o *Orchestrator) AgentInUse(id domain.AgentID) bool {
	o.mu.Lock()
	running := make([]domain.TaskID, 0, len(o.running))
	for t := range o.running {
		running = append(running, t)
	}
	o.mu.Unlock()
	for _, tid := range running {
		if t, err := o.Tasks.Get(context.Background(), tid); err == nil && t.HasParticipant(id) {
			return true
		}
	}
	return false
}

// TasksOf lists the running tasks agent id takes part in.
func (o *Orchestrator) TasksOf(id domain.AgentID) []domain.TaskID {
	o.mu.Lock()
	running := make([]domain.TaskID, 0, len(o.running))
	for t := range o.running {
		running = append(running, t)
	}
	o.mu.Unlock()
	var out []domain.TaskID
	for _, tid := range running {
		if t, err := o.Tasks.Get(context.Background(), tid); err == nil && t.HasParticipant(id) {
			out = append(out, tid)
		}
	}
	slices.Sort(out)
	return out
}

// Cancel terminates a task. The human can always intervene, whichever agent
// holds task authority.
func (o *Orchestrator) Cancel(ctx context.Context, id domain.TaskID, reason string) error {
	if reason == "" {
		reason = "cancelled by human"
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "CANCEL: "+reason, nil)
	o.cancelRun(id)
	_, err := o.Tasks.Cancel(ctx, id, domain.HumanAgentID, reason)
	return err
}

// Approve accepts the pending final decision and completes the task.
//
// Everything is checked before anything changes. Accepting first and then
// finding the transition illegal (approving a held decision while the task
// was PAUSED) left the decision ACCEPTED and the task PAUSED, and a second
// Approve then failed with "cannot accept from status ACCEPTED".
func (o *Orchestrator) Approve(ctx context.Context, id domain.TaskID, note string) error {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	d, err := o.Decisions.Latest(ctx, id)
	if err != nil || d == nil {
		return fmt.Errorf("approve %s: no decision to approve", id)
	}
	accept := d.Status == domain.DecisionProposed
	switch {
	case t.State == domain.TaskAwaitingDecision && d.Status == domain.DecisionProposed:
	case t.State == domain.TaskPaused && t.PreviousState == domain.TaskAwaitingDecision &&
		d.Status == domain.DecisionProposed:
	case t.State == domain.TaskAwaitingDecision && d.Status == domain.DecisionAccepted:
		// Accepted but never completed: the closing state of an older
		// version, lost with the run. Finishing it is the only way out.
	default:
		return fmt.Errorf("approve %s: the task is %s and its latest decision %s is %s; "+
			"only a decision awaiting your approval can be approved", id, t.State, d.ID, d.Status)
	}

	if accept {
		if _, err := o.Decisions.Accept(ctx, d.ID); err != nil {
			return err
		}
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "APPROVE: "+note, nil)
	o.Events.emit(ctx, domain.EvtDecisionAccepted, id, domain.HumanAgentID, d.Decision,
		map[string]string{"decisionId": string(d.ID)})
	if t.State == domain.TaskPaused {
		if _, err := o.Tasks.Transition(ctx, id, domain.TaskAwaitingDecision, domain.HumanAgentID, "approved by human"); err != nil {
			return err
		}
	}
	_, err = o.Tasks.Transition(ctx, id, domain.TaskCompleted, domain.HumanAgentID, "approved by human")
	return err
}

// Reject refuses the pending final decision and sends the task back into
// deliberation: the authority is sent the human's note as a REJECT, the
// conflicts the decision settled are reopened, and the task returns to
// DELIBERATING. With no run alive the REJECT waits for the next one, which
// the caller starts (see Running).
//
// Before, the note reached nobody and the task was left in DELIBERATING with
// no run loop, refused by resume, instruction, reopen and approve alike.
func (o *Orchestrator) Reject(ctx context.Context, id domain.TaskID, note string) error {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	d := o.heldDecision(ctx, t)
	if d == nil {
		// Rejecting nothing must change nothing: it used to log a REJECT,
		// refresh the allowance and send a working task to DELIBERATING.
		return fmt.Errorf("reject %s: no decision is awaiting your approval", id)
	}
	if t.State != domain.TaskDeliberating && !domain.CanTransition(t.State, domain.TaskDeliberating) {
		return fmt.Errorf("reject %s: a %s task cannot go back to deliberation", id, t.State)
	}
	running := o.Running(id)
	if !running {
		if err := o.checkTeam(t); err != nil {
			return err
		}
	}

	if _, err := o.Decisions.Reject(ctx, d.ID, note); err != nil {
		return err
	}
	o.Events.emit(ctx, domain.EvtDecisionRejected, id, domain.HumanAgentID, note,
		map[string]string{"decisionId": string(d.ID)})
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "REJECT: "+note, nil)
	o.reopenDisagreements(ctx, t, d.ID)
	// Sending the agents back to deliberate is human direction: a fresh
	// allowance for the work it asks for.
	if _, err := o.Tasks.NoteHumanInput(ctx, id); err != nil {
		return err
	}
	if t, err = o.Tasks.Transition(ctx, id, domain.TaskDeliberating, domain.HumanAgentID, "rejected by human: "+note); err != nil {
		return err
	}

	content := fmt.Sprintf("The human rejected your decision %s (%q):\n\n%s\n\n"+
		"Address this, then send a revised FINAL_DECISION.", d.ID, excerpt(d.Decision, 160), strings.TrimSpace(note))
	if strings.TrimSpace(note) == "" {
		content = fmt.Sprintf("The human rejected your decision %s (%q) without giving a reason. "+
			"Ask the human (to=human) what they want changed before you decide again.", d.ID, excerpt(d.Decision, 160))
	}
	m := &domain.Message{
		FromAgent:        domain.HumanAgentID,
		ToAgent:          t.AuthorityAgent,
		Type:             domain.MsgReject,
		Content:          content,
		RequiresResponse: true,
		Metadata:         map[string]string{"decisionId": string(d.ID)},
	}
	return o.deliverOrQueue(ctx, t, m)
}

// SendInstruction lets the human inject a message into a task.
//
// A task that has no run loop but is still in the agents' hands -- above all
// one whose decision is held for the human, where the UI says "answer it" --
// has the message queued for its next run, which the caller starts. It used
// to be refused as "not running", leaving the human no way to answer.
func (o *Orchestrator) SendInstruction(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return err
	}
	if t.IsDropped(to) {
		// It would be recorded and never delivered.
		return fmt.Errorf("%s was dropped from %s (%s): pause and resume the task to give it a fresh chance",
			to, id, excerpt(t.Dropped[to], 160))
	}
	m := &domain.Message{
		ID:               domain.NewMessageID(),
		FromAgent:        domain.HumanAgentID,
		ToAgent:          to,
		Type:             domain.MsgInformation,
		Content:          content,
		RequiresResponse: true,
	}
	running := o.Running(id)
	if !running {
		// Paused and finished tasks are the human's to move first; refuse
		// with a reason a human can act on.
		switch t.State {
		case domain.TaskCompleted:
			return fmt.Errorf("%s is COMPLETED: send it as a follow-up to reopen the task: %w", id, ErrTaskNotRunning)
		case domain.TaskPaused:
			return fmt.Errorf("%s is PAUSED: resume it first: %w", id, ErrTaskNotRunning)
		case domain.TaskCreated, domain.TaskFailed, domain.TaskCancelled:
			return fmt.Errorf("%s is %s: %w", id, t.State, ErrTaskNotRunning)
		}
		// Refuse now what would only fail when the run starts.
		if err := o.Authority.Authorize(t, m); err != nil {
			return err
		}
		if err := o.checkTeam(t); err != nil {
			return err
		}
	}
	o.Events.emit(ctx, domain.EvtHumanOverride, id, domain.HumanAgentID, "INSTRUCTION -> "+string(to), nil)
	if t, err = o.Tasks.NoteHumanInput(ctx, id); err != nil {
		return err
	}
	msgs := []*domain.Message{m}
	if t.State == domain.TaskAwaitingDecision {
		if d := o.heldDecision(ctx, t); d != nil {
			msgs = append(msgs, decisionPendingNote(t, d, m))
		}
	}
	return o.deliverOrQueue(ctx, t, msgs...)
}

func (o *Orchestrator) cancelRun(id domain.TaskID) {
	o.mu.Lock()
	// A queued run is cancelled the same way: it never starts.
	if r, ok := o.running[id]; ok {
		r.cancel()
	}
	o.mu.Unlock()
}
