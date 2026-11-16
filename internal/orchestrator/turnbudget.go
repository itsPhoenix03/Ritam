package orchestrator

import (
	"context"
	"fmt"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Per-turn spending allowances.
//
// Claude's CLI stops a turn at --max-budget-usd. That cap used to be what was
// left of the task's budget, worked out by the adapter from the task as it
// stood when the job was queued, and every agent working at once got all of
// it: two agents on $1 left could each spend $1. The figure was stale too.
// A turn's usage arrives after its messages, so an agent handed a turn the
// moment a co-worker's reply was routed was capped as if that co-worker's
// turn had cost nothing.
//
// So the orchestrator decides each turn's allowance when it hands the turn
// over (flush), and holds it in reserve until the turn is billed
// (recordUsage), fails (handleOutbound) or is killed with the run (endRun).
// A new turn gets what is left unreserved, shared among the agents that
// could start a turn at the same time, and never less than minTurnUSD. With
// not even that left, the turn does not start: its messages wait while a
// co-worker's turn holds the money it needs, and otherwise the task pauses
// for its budget.

// minTurnUSD is the smallest allowance a turn is started with. A cap of a
// cent stops a Claude turn at its first API call, which a long session
// history alone can cost: a turn that can do nothing is a paid turn wasted.
var minTurnUSD = 0.05

// turnBudgets holds the allowances of the turns in progress. Its zero value
// is ready to use.
type turnBudgets struct {
	mu    sync.Mutex
	slots map[domain.TaskID]map[domain.AgentID]*turnSlot
}

// turnSlot is one agent's reservation on a task: the allowance of its
// latest turn.
//
// The orchestrator cannot tell which turn a usage report belongs to, only
// whose it is. An agent can be handed its next turn before the usage of its
// previous one has been read -- it went idle the moment it sent it -- so due
// counts the reports still to come, and only the last of them releases the
// latest turn's money.
//
// An idle agent's reservation still counts against its co-workers: its usage
// may be on its way, and their caps must include its turn. But an agent that
// never reports usage (the mock agent) would then hold money for ever, and
// in a test a mock authority's idle reservation paused the task for its
// budget with most of it unspent. So an agent that has been handed more than
// one turn without reporting usage once is taken not to report it: its
// reservation counts only while it is working.
type turnSlot struct {
	usd    float64 // the latest turn's allowance; 0 once it is billed or failed
	due    int     // usage reports still to come, one per turn handed over
	billed bool    // the agent has reported usage on this task
}

// counts reports whether s holds money against agent's co-workers.
func (s *turnSlot) counts(working bool) bool {
	return s.usd > 0 && (working || s.billed || s.due <= 1)
}

// turnVerdict is what allowTurn decided: wait (hand nothing over now) or
// pause the task for reason. The zero value starts the turn.
type turnVerdict struct {
	wait  bool
	pause string
}

// admitTurn decides whether agent may start a turn on task id now, and
// reserves the turn's allowance. false: the turn is not handed over, and its
// messages stay in the inbox. Either a co-worker's turn in progress holds
// the money it needs and will release it (the next dispatch tries again), or
// there is none and the task has been paused for its budget.
func (o *Orchestrator) admitTurn(ctx context.Context, id domain.TaskID, agent domain.AgentID) bool {
	v := o.allowTurn(ctx, id, agent)
	switch {
	case v.pause != "":
		o.pauseBeforeTurn(ctx, id, agent, v.pause)
		return false
	case v.wait:
		o.log.Debug("turn waits for budget held by turns in progress", "taskId", string(id), "agentId", string(agent))
		return false
	}
	return true
}

// allowTurn works out agent's allowance and reserves it. The task is read
// under the lock: a turn billed meanwhile is then counted either in the
// task's cost or in its reservation, never in neither (turnBilled releases
// the reservation only after the cost is added).
func (o *Orchestrator) allowTurn(ctx context.Context, id domain.TaskID, agent domain.AgentID) turnVerdict {
	if ad, ok := o.Agents.AdapterFor(id, agent); ok && ad.Kind() == domain.AgentKindCodex {
		// Codex reports no dollar cost and takes no cap (until A-2 prices
		// it): a reservation would only shrink its co-workers' share. Its
		// tokens still count toward the token limits.
		return turnVerdict{}
	}
	b := &o.turns
	b.mu.Lock()
	defer b.mu.Unlock()
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		o.log.Warn("work out a turn's allowance", "taskId", string(id), "error", err)
		return turnVerdict{}
	}
	if t.State.IsTerminal() || t.State == domain.TaskPaused {
		// Paused a moment ago, by this check for a co-worker, say:
		// nothing more starts.
		return turnVerdict{wait: true}
	}
	if t.Limits.MaxCostUSD <= 0 {
		return turnVerdict{}
	}
	left := t.Limits.MaxCostUSD - t.CostUSD - o.heldExcept(t, agent)
	if left < minTurnUSD {
		if o.coWorkerHolds(t, agent) {
			return turnVerdict{wait: true}
		}
		return turnVerdict{pause: fmt.Sprintf("%s ($%.2f of $%.2f; less than the $%.2f a turn needs is left for %s's)",
			domain.PauseReasonBudget, t.CostUSD, t.Limits.MaxCostUSD, minTurnUSD, agent)}
	}
	share := left / float64(1+o.couldStartBeside(t, agent))
	b.reserve(id, agent, min(left, max(share, minTurnUSD)))
	return turnVerdict{}
}

// couldStartBeside counts the other agents on t that could start a turn
// while agent's runs: those taking part, spending dollars, and not working
// already (a working agent holds its own reservation).
func (o *Orchestrator) couldStartBeside(t *domain.Task, agent domain.AgentID) int {
	n := 0
	for _, p := range t.Participants {
		if p == agent || t.IsDropped(p) {
			continue
		}
		ad, ok := o.Agents.AdapterFor(t.ID, p)
		if !ok || ad.Kind() == domain.AgentKindCodex || ad.Status() == domain.AgentStateWorking {
			continue
		}
		n++
	}
	return n
}

// coWorkerHolds reports whether another agent of t is working on a turn
// with money reserved: once billed, it frees what it did not spend. The
// caller holds the lock.
func (o *Orchestrator) coWorkerHolds(t *domain.Task, agent domain.AgentID) bool {
	for p, s := range o.turns.slots[t.ID] {
		if p != agent && s.usd > 0 && o.working(t.ID, p) {
			return true
		}
	}
	return false
}

// pauseBeforeTurn pauses task id for reason, found before agent's turn
// started, and ends its run: the run loop is not the caller here, so it
// would not see the pause.
func (o *Orchestrator) pauseBeforeTurn(ctx context.Context, id domain.TaskID, agent domain.AgentID, reason string) {
	o.Events.emit(ctx, domain.EvtLimitReached, id, agent, reason, nil)
	if _, err := o.Tasks.Pause(ctx, id, domain.HumanAgentID, reason); err != nil {
		o.log.Error("pause a task before a turn", "taskId", string(id), "agentId", string(agent), "error", err)
	}
	o.cancelRun(id)
}

// stampTurnBudget puts agent's reserved allowance on delivery d, for an
// adapter whose CLI takes a spending cap (ports.TurnBudgetMetadataKey).
func (o *Orchestrator) stampTurnBudget(id domain.TaskID, agent domain.AgentID, d *domain.Message) {
	b := &o.turns
	b.mu.Lock()
	s := b.slots[id][agent]
	b.mu.Unlock()
	if s == nil || s.usd <= 0 {
		return
	}
	if d.Metadata == nil {
		d.Metadata = map[string]string{}
	}
	d.Metadata[ports.TurnBudgetMetadataKey] = fmt.Sprintf("%.4f", s.usd)
}

// turnBilled releases agent's reservation once its turn's usage has been
// added to the task -- unless a later turn has been handed over meanwhile,
// whose own report is still to come.
func (o *Orchestrator) turnBilled(id domain.TaskID, agent domain.AgentID) {
	b := &o.turns
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.slots[id][agent]
	if s == nil {
		return
	}
	s.billed = true
	if s.due > 0 {
		s.due--
	}
	if s.due == 0 {
		s.usd = 0
	}
}

// turnFailed releases the money of agent's failed turn. Its usage report,
// which adapters send after the failure, is still counted as due, so that it
// does not release a turn handed over in between (a re-prompt, say).
func (o *Orchestrator) turnFailed(id domain.TaskID, agent domain.AgentID) {
	b := &o.turns
	b.mu.Lock()
	defer b.mu.Unlock()
	if s := b.slots[id][agent]; s != nil && s.due <= 1 {
		s.usd = 0
	}
}

// forgetTurns releases every reservation on task id: its run has ended, and
// its agents are stopped and billed.
func (o *Orchestrator) forgetTurns(id domain.TaskID) {
	b := &o.turns
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.slots, id)
}

// reserve sets agent's reservation to usd. The caller holds the lock.
func (b *turnBudgets) reserve(id domain.TaskID, agent domain.AgentID, usd float64) {
	if b.slots == nil {
		b.slots = map[domain.TaskID]map[domain.AgentID]*turnSlot{}
	}
	if b.slots[id] == nil {
		b.slots[id] = map[domain.AgentID]*turnSlot{}
	}
	s := b.slots[id][agent]
	if s == nil {
		s = &turnSlot{}
		b.slots[id][agent] = s
	}
	s.usd = usd
	s.due++
}

// heldExcept is what the turns of t other than agent's hold. The caller
// holds the lock.
func (o *Orchestrator) heldExcept(t *domain.Task, agent domain.AgentID) float64 {
	sum := 0.0
	for p, s := range o.turns.slots[t.ID] {
		if p != agent && s.counts(o.working(t.ID, p)) {
			sum += s.usd
		}
	}
	return sum
}

// working reports whether agent is in the middle of a turn on task id.
func (o *Orchestrator) working(id domain.TaskID, agent domain.AgentID) bool {
	ad, ok := o.Agents.AdapterFor(id, agent)
	return ok && ad.Status() == domain.AgentStateWorking
}
