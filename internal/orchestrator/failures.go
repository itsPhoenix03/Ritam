package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// defaultRetryBackoff is how long to wait before each retry of a turn that
// failed on an overloaded server, a server error or a crash; there are as
// many retries as delays.
var defaultRetryBackoff = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second}

// defaultRateLimitWait is the longest rate-limit reset waited out before the
// turn is tried again; a longer one pauses the task.
const defaultRateLimitWait = 2 * time.Minute

// maxRateLimitWaits is how many short rate-limit resets in a row are waited
// out before the agent is given up on.
const maxRateLimitWaits = 3

// handleFailure acts on a failed agent turn by its kind:
//
//	Malformed            re-prompt, up to maxMalformedReplies (malformedReply)
//	Timeout              retry once at once, told it was cut off; then give up
//	Overloaded, Crash,
//	SessionMissing       retry after each retryBackoff delay; then give up
//	RateLimit            a reset under rateLimitWait: wait for it and retry;
//	                     a later reset: pause, saying when; no reset given:
//	                     pause for the authority, drop a supporting agent
//	Auth, Billing, Plan,
//	ModelUnavailable     drop a supporting agent, pause for the authority
//	BudgetStop           pause for the budget
//	Cancelled            nothing
//
// To give up on a supporting agent is to drop it from the task; on the
// authority, without which nothing can be decided, to pause the task saying
// why. A task never moves to FAILED because of one agent: a timeout from a
// supporting agent used to fail the whole task, terminally.
//
// It never reads the error's text. The orchestrator used to match adapter
// wording ("missing 'to'", "overloaded"), and when the wording changed a reply
// with no recipient sent the task to FAILED. Adapters now say what kind of
// failure it was (domain.AgentFailure); an error that carries no kind is
// taken as a crash.
func (o *Orchestrator) handleFailure(ctx context.Context, id domain.TaskID, out ports.Outbound) (bool, error) {
	f := domain.FailureOf(out.Err)
	agent := out.AgentID
	if f.Kind == domain.FailureCancelled {
		// Stopped on purpose: nobody is waiting for the answer.
		return false, nil
	}
	o.Agents.ReportOutboundFor(id, agent, out)
	o.Events.emit(ctx, domain.EvtAgentFailed, id, agent,
		fmt.Sprintf("agent %s failed (%s): %s", agent, f.Kind, f.Detail),
		map[string]string{"state": string(domain.AgentStateError), "kind": string(f.Kind)})

	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return true, err
	}
	if !t.HasParticipant(agent) || t.IsDropped(agent) || t.State.IsTerminal() || t.State == domain.TaskPaused {
		// A turn that was already running when its agent was dropped, or
		// the task stopped, changes nothing.
		return false, nil
	}

	switch f.Kind {
	case domain.FailureMalformed:
		// A malformed reply is not a dead agent. Real CLIs answer in prose,
		// wrap a header in a code fence, or ramble past the format from time
		// to time; killing the task over one unparseable turn throws away
		// every round that came before it. Tell the agent what was wrong and
		// ask again -- but only so often: none of the message limits see a
		// reply that was never routed, and the idle timer is not reset by
		// one, so malformedReply counts them and stops asking (see there).
		return o.malformedReply(ctx, id, out, f)

	case domain.FailureBudgetStop:
		// The CLI stopped the turn at what was left of the task's budget.
		return o.pauseForBudget(ctx, id, agent)

	case domain.FailureAuth, domain.FailureBilling, domain.FailurePlan, domain.FailureModelUnavailable:
		// An account that cannot run the model is not a misbehaving agent:
		// asking it to reply again only fails again.
		return o.cannotRun(ctx, id, agent, f)

	case domain.FailureRateLimit:
		return o.rateLimited(ctx, t, agent, f)

	case domain.FailureTimeout:
		// A long build or test run can outlast the turn limit once; the
		// agent's session has what it did, so it is told to continue.
		if n := o.noteFailure(id, agent, f.Kind); n == 1 {
			o.retryTurn(ctx, t, agent, f, 1, 1, 0, "your previous turn on this message was cut off at its time "+
				"limit before you replied. Continue from where you were (your session has what you did), "+
				"then reply; keep long commands short or split them.")
			return false, nil
		}
		return o.giveUp(ctx, t, agent, f, 2)
	}

	// An overloaded server, a server error, a crash, a lost session: the
	// same turn may well work a little later.
	n := o.noteFailure(id, agent, f.Kind)
	if n <= len(o.retryBackoff) {
		o.retryTurn(ctx, t, agent, f, n, len(o.retryBackoff), o.retryBackoff[n-1],
			fmt.Sprintf("your previous turn on this message failed (%s: %s), so it is sent again.",
				f.Kind, excerpt(f.Detail, 200)))
		return false, nil
	}
	return o.giveUp(ctx, t, agent, f, n)
}

// rateLimited handles a turn stopped by a usage or rate limit. A limit that
// resets soon is waited out; one that resets later pauses the task with the
// time, for the human (automatically resuming then is not built yet). With no
// reset time given, the authority's limit pauses the task and a supporting
// agent is dropped. A limit used to drop a supporting agent whatever its
// reset, and "overloaded" counted as one.
func (o *Orchestrator) rateLimited(ctx context.Context, t *domain.Task, agent domain.AgentID, f *domain.AgentFailure) (bool, error) {
	who := agentLabel(t, agent)
	soon := f.RetryAfter > 0 && f.RetryAfter <= o.rateLimitWait
	if soon {
		// Bounded, so a limit that keeps coming back cannot be waited on
		// for ever.
		if n := o.noteFailure(t.ID, agent, f.Kind); n <= maxRateLimitWaits {
			o.retryTurn(ctx, t, agent, f, n, maxRateLimitWaits, f.RetryAfter,
				"your previous turn on this message hit a usage limit, which has now reset, so it is sent again.")
			return false, nil
		}
	}
	switch {
	case soon:
		return o.giveUp(ctx, t, agent, f, maxRateLimitWaits+1)
	case f.RetryAfter > 0:
		at := time.Now().Add(f.RetryAfter)
		reason := fmt.Sprintf("%s hit a usage limit that resets in %s (about %s); resume after then",
			who, f.RetryAfter.Round(time.Second), at.Format("15:04 Jan 2"))
		return o.pauseForFailure(ctx, t, agent, f, reason)
	case agent == t.AuthorityAgent:
		reason := fmt.Sprintf("%s hit a usage limit and the CLI gave no reset time (%s); resume once it resets",
			who, excerpt(f.Detail, 160))
		return o.pauseForFailure(ctx, t, agent, f, reason)
	}
	o.dropAgent(ctx, t, agent, f.Kind,
		fmt.Sprintf("%s hit a usage limit and the CLI gave no reset time (%s)", agent, excerpt(f.Detail, 160)), "dropped")
	return false, nil
}

// giveUp ends the retries of an agent whose turns keep failing: a
// supporting agent is dropped from the task, the authority pauses it.
func (o *Orchestrator) giveUp(ctx context.Context, t *domain.Task, agent domain.AgentID, f *domain.AgentFailure, n int) (bool, error) {
	what := fmt.Sprintf("%s's turn failed %d times in a row (%s: %s)", agent, n, f.Kind, excerpt(f.Detail, 200))
	if agent == t.AuthorityAgent {
		reason := fmt.Sprintf("%s (the authority): %s; %s", what, f.Kind.Advice(), "resume to try again")
		return o.pauseForFailure(ctx, t, agent, f, reason)
	}
	o.dropAgent(ctx, t, agent, f.Kind, what, "dropped")
	return false, nil
}

// pauseForFailure pauses t for an agent failure the human has to look at.
func (o *Orchestrator) pauseForFailure(ctx context.Context, t *domain.Task, agent domain.AgentID, f *domain.AgentFailure, reason string) (bool, error) {
	o.Events.emit(ctx, domain.EvtLimitReached, t.ID, agent, reason, map[string]string{"kind": string(f.Kind)})
	_, err := o.Tasks.Pause(ctx, t.ID, domain.HumanAgentID, reason)
	return true, err
}

// agentLabel names agent, marking the authority.
func agentLabel(t *domain.Task, agent domain.AgentID) string {
	if agent == t.AuthorityAgent {
		return string(agent) + " (the authority)"
	}
	return string(agent)
}

// retryTurn sends agent the turn that failed again, after delay, with note
// in front of it. attempt of max (max 0: no fixed number) is recorded as an
// event, and a supporting agent's authority is told.
//
// The turn is the last delivery the agent was handed, re-sent as it was --
// it is already in the history, so it is not routed again. With no delivery
// on record (the run started after it), the agent is asked to carry on.
func (o *Orchestrator) retryTurn(ctx context.Context, t *domain.Task, agent domain.AgentID, f *domain.AgentFailure, attempt, max int, delay time.Duration, note string) {
	of := ""
	if max > 0 {
		of = fmt.Sprintf(" of %d", max)
	}
	when := "now"
	if delay > 0 {
		when = "in " + delay.String()
	}
	o.Events.emit(ctx, domain.EvtAgentRetried, t.ID, agent,
		fmt.Sprintf("retrying %s's turn %s (attempt %d%s) after %s: %s", agent, when, attempt, of, f.Kind, excerpt(f.Detail, 160)),
		map[string]string{"kind": string(f.Kind), "attempt": fmt.Sprint(attempt), "delay": delay.String()})
	if agent != t.AuthorityAgent {
		o.notice(ctx, t, t.AuthorityAgent, "retrying", fmt.Sprintf(
			"Note from the orchestrator: %s's turn failed (%s: %s). It is being tried again %s (attempt %d%s); "+
				"no action is needed, its answer will follow.", agent, f.Kind, excerpt(f.Detail, 160), when, attempt, of))
	}

	last, carried := o.lastDelivery(t.ID, agent), o.lastDeliveredIDs(t.ID, agent)
	o.setRetrying(t.ID, agent, 1)
	go func() {
		defer o.setRetrying(t.ID, agent, -1)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
		cur, err := o.Tasks.Get(ctx, t.ID)
		if err != nil || ctx.Err() != nil || cur.State.IsTerminal() || cur.State == domain.TaskPaused || cur.IsDropped(agent) {
			return
		}
		if last == nil {
			o.notice(ctx, cur, agent, "retry", "Note from the orchestrator: "+note+" Carry on with the task from where it stood.")
			return
		}
		m := last.Clone()
		m.Content = "Note from the orchestrator: " + note + "\n\n" + m.Content
		if m.Metadata == nil {
			m.Metadata = map[string]string{}
		}
		m.Metadata["retry"] = fmt.Sprint(attempt)
		// The retry is a turn of its own carrying the same messages: they
		// are sent again with it, not queued again beside it (see settle).
		if err := o.sendTurn(ctx, cur, agent, m, carried); err != nil {
			o.log.Warn("retry turn", "taskId", string(t.ID), "agentId", string(agent), "error", err)
		}
	}()
}

// notice routes an orchestrator note to agent, marked kind.
func (o *Orchestrator) notice(ctx context.Context, t *domain.Task, to domain.AgentID, kind, content string) {
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   to,
		Type:      domain.MsgInformation,
		Content:   content,
		Metadata:  map[string]string{domain.MetaOrchestrator: kind},
	}); err != nil {
		o.log.Warn("send notice", "taskId", string(t.ID), "agentId", string(to), "kind", kind, "error", err)
	}
}

// dropAgent takes a supporting agent off the task: it is recorded on the task
// (so a pause and resume keeps it), its waiting messages are discarded and it
// receives nothing more, every request it owes is closed, and the authority is
// told once, in a notice marked meta, what was closed and why.
//
// Dropping used to be a message to the authority and nothing else: the agent
// stayed a participant and went on receiving deliveries.
func (o *Orchestrator) dropAgent(ctx context.Context, t *domain.Task, agent domain.AgentID, kind domain.FailureKind, why, meta string) {
	if nt, err := o.Tasks.DropAgent(ctx, t.ID, agent, why); err == nil {
		t = nt
	} else {
		o.log.Warn("drop agent", "taskId", string(t.ID), "agentId", string(agent), "error", err)
	}
	o.Events.emit(ctx, domain.EvtAgentDropped, t.ID, agent,
		fmt.Sprintf("%s was dropped from the task: %s", agent, why), map[string]string{"kind": string(kind)})
	o.discardInbox(t.ID, agent)

	ledger, err := o.ledger(ctx, t.ID)
	if err != nil {
		o.log.Warn("drop agent", "taskId", string(t.ID), "agentId", string(agent), "error", err)
	}
	var closed []string
	for _, r := range domain.OpenRequests(ledger) {
		q := r.Message
		if q.ToAgent != agent {
			continue
		}
		o.Events.emit(ctx, domain.EvtRequestAbandoned, t.ID, agent,
			fmt.Sprintf("%s was dropped: %s's %s closed as unanswered", agent, q.FromAgent, q.Type),
			map[string]string{"request": string(q.ID)})
		to, mode := t.AuthorityAgent, "closed" // recorded; the authority hears it in one notice below
		content := fmt.Sprintf("%s was dropped from the task; this %s to it is closed as unanswered.", agent, q.Type)
		switch {
		case q.FromAgent == t.AuthorityAgent:
			closed = append(closed, fmt.Sprintf("your %s to it (%q) is closed as unanswered", q.Type, excerpt(q.Content, 120)))
		case t.HasParticipant(q.FromAgent) && q.FromAgent != agent:
			to, mode = q.FromAgent, "closed-notify"
			content = why + fmt.Sprintf("; the orchestrator has closed your %s to it (%q) as unanswered. "+
				"Carry on without it.", q.Type, excerpt(q.Content, 120))
		default:
			closed = append(closed, fmt.Sprintf("the human's %s to it (%q) is closed as unanswered: answer the human "+
				"yourself (to=human) or reassign it", q.Type, excerpt(q.Content, 120)))
		}
		if err := o.Bus.Route(ctx, t, &domain.Message{
			FromAgent: domain.OrchestratorID,
			ToAgent:   to,
			Type:      domain.MsgInformation,
			Content:   content,
			Metadata:  map[string]string{domain.MetaOrchestrator: mode, domain.MetaCloses: string(q.ID)},
		}); err != nil {
			o.log.Warn("close request to dropped agent", "taskId", string(t.ID), "error", err)
		}
	}
	if agent == t.AuthorityAgent {
		return
	}
	content := why + ". It has been taken off this task and receives nothing more on it until the human resumes the task."
	if len(closed) > 0 {
		content += " " + strings.ToUpper(closed[0][:1]) + closed[0][1:]
		for _, c := range closed[1:] {
			content += "; " + c
		}
		content += "."
	}
	content += " Carry on without it; reassign anything it owed."
	o.notice(ctx, t, t.AuthorityAgent, meta, content)
}

// refuseForDropped closes a message just sent to a dropped agent, which will
// not receive it, and tells its sender.
func (o *Orchestrator) refuseForDropped(ctx context.Context, t *domain.Task, m *domain.Message) {
	why := t.Dropped[m.ToAgent]
	o.Events.emit(ctx, domain.EvtRequestAbandoned, t.ID, m.ToAgent,
		fmt.Sprintf("%s's %s to %s not delivered: %s was dropped from the task", m.FromAgent, m.Type, m.ToAgent, m.ToAgent),
		map[string]string{"request": string(m.ID)})
	if err := o.Bus.Route(ctx, t, &domain.Message{
		FromAgent: domain.OrchestratorID,
		ToAgent:   m.FromAgent,
		Type:      domain.MsgInformation,
		Content: fmt.Sprintf("%s was dropped from this task (%s), so your %s to it was not delivered and is closed. "+
			"Carry on without it.", m.ToAgent, excerpt(why, 200), m.Type),
		Metadata: map[string]string{domain.MetaOrchestrator: "closed-notify", domain.MetaCloses: string(m.ID)},
	}); err != nil {
		o.log.Warn("refuse message to dropped agent", "taskId", string(t.ID), "error", err)
	}
}

// pauseForBudget pauses a task whose agent's turn the CLI stopped at the
// task's spending limit.
func (o *Orchestrator) pauseForBudget(ctx context.Context, id domain.TaskID, agent domain.AgentID) (bool, error) {
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return true, err
	}
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		return false, nil
	}
	// The turn's cost arrives after this (usage is reported last), so the
	// figure is not quoted here; the usage event that follows has it.
	reason := fmt.Sprintf("%s: the CLI stopped %s's turn at what was left of the task's budget",
		domain.PauseReasonBudget, agent)
	o.Events.emit(ctx, domain.EvtLimitReached, id, agent, reason, nil)
	_, err = o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason)
	return true, err
}

// noteFailure counts one more failed turn of kind for agent and returns the
// run so far. A readable message from the agent ends the run (clearFailures).
func (o *Orchestrator) noteFailure(id domain.TaskID, agent domain.AgentID, kind domain.FailureKind) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failing[id] == nil {
		o.failing[id] = map[domain.AgentID]map[domain.FailureKind]int{}
	}
	if o.failing[id][agent] == nil {
		o.failing[id][agent] = map[domain.FailureKind]int{}
	}
	o.failing[id][agent][kind]++
	return o.failing[id][agent][kind]
}

// clearFailures ends agent's run of failed turns; with no agent, every
// agent's on the task.
func (o *Orchestrator) clearFailures(id domain.TaskID, agent domain.AgentID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if agent == "" {
		delete(o.failing, id)
		return
	}
	delete(o.failing[id], agent)
}

// noteDelivery remembers the last delivery handed to agent, the turn a retry
// sends again.
func (o *Orchestrator) noteDelivery(id domain.TaskID, agent domain.AgentID, m *domain.Message) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lastTurn[id] == nil {
		o.lastTurn[id] = map[domain.AgentID]*domain.Message{}
	}
	o.lastTurn[id][agent] = m.Clone()
}

func (o *Orchestrator) lastDelivery(id domain.TaskID, agent domain.AgentID) *domain.Message {
	o.mu.Lock()
	defer o.mu.Unlock()
	if m := o.lastTurn[id][agent]; m != nil {
		return m.Clone()
	}
	return nil
}

// setRetrying counts agent's retries waiting to be sent up or down.
func (o *Orchestrator) setRetrying(id domain.TaskID, agent domain.AgentID, delta int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.retrying[id] == nil {
		o.retrying[id] = map[domain.AgentID]int{}
	}
	o.retrying[id][agent] += delta
	if o.retrying[id][agent] <= 0 {
		delete(o.retrying[id], agent)
	}
}

// isRetrying reports whether a retry of agent's turn is waiting to be sent.
func (o *Orchestrator) isRetrying(id domain.TaskID, agent domain.AgentID) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.retrying[id][agent] > 0
}

// busy reports whether anyone on the task is at work: an agent WORKING, or a
// retry waiting to be sent. Idle means nobody is.
func (o *Orchestrator) busy(id domain.TaskID, t *domain.Task) bool {
	o.mu.Lock()
	retries := len(o.retrying[id])
	o.mu.Unlock()
	if retries > 0 {
		return true
	}
	for _, p := range t.Participants {
		if ad, ok := o.Agents.AdapterFor(id, p); ok && ad.Status() == domain.AgentStateWorking {
			return true
		}
	}
	return false
}

// forgetRun drops what the run loop kept about a task's turns.
func (o *Orchestrator) forgetRun(id domain.TaskID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.lastTurn, id)
	delete(o.retrying, id)
}
