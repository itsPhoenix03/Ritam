package orchestrator

import (
	"context"
	"fmt"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// handleFailure acts on a failed agent turn by its kind.
//
// It never reads the error's text. The orchestrator used to match adapter
// wording ("missing 'to'", "overloaded"), and when the adapters' wording
// changed a reply with no recipient sent the task to FAILED. Adapters now say
// what kind of failure it was (domain.AgentFailure); an error that carries no
// kind is taken as a crash.
func (o *Orchestrator) handleFailure(ctx context.Context, id domain.TaskID, out ports.Outbound) (bool, error) {
	f := domain.FailureOf(out.Err)
	if f.Kind == domain.FailureCancelled {
		// Stopped on purpose: nobody is waiting for the answer.
		return false, nil
	}
	o.Agents.ReportOutboundFor(id, out.AgentID, out)
	o.Events.emit(ctx, domain.EvtAgentFailed, id, out.AgentID,
		fmt.Sprintf("agent %s failed (%s): %s", out.AgentID, f.Kind, f.Detail),
		map[string]string{"state": string(domain.AgentStateError), "kind": string(f.Kind)})

	switch {
	case f.Kind == domain.FailureMalformed:
		// A malformed reply is not a dead agent. Real CLIs answer in prose,
		// wrap a header in a code fence, or ramble past the format from time
		// to time; killing the task over one unparseable turn throws away
		// every round that came before it. Tell the agent what was wrong and
		// ask again -- but only so often: none of the message limits see a
		// reply that was never routed, and the idle timer is not reset by
		// one, so malformedReply counts them and stops asking (see there).
		return o.malformedReply(ctx, id, out, f)

	case f.Kind.IsAccountFailure(), f.Kind == domain.FailureRateLimit, f.Kind == domain.FailureOverloaded:
		// An account that cannot run the model (credits, plan, login, a
		// limit) is not a misbehaving agent: asking it to reply again only
		// fails again.
		return o.cannotRun(ctx, id, out.AgentID, f)

	case f.Kind == domain.FailureBudgetStop:
		// The CLI stopped the turn at what was left of the task's budget.
		return o.pauseForBudget(ctx, id, out.AgentID)
	}

	_, err := o.Tasks.Fail(ctx, id, out.AgentID,
		fmt.Sprintf("AGENT_ERROR: %s: %s", out.AgentID, f.Detail))
	return true, err
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
