package claude

import (
	"strings"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// missingSessionText is what the CLI says when --resume names a conversation
// it does not have.
const missingSessionText = "No conversation found"

// turnFailure reads why a turn failed from what the CLI reported, or returns
// nil when it did not fail.
//
// The structured result comes first: the final result event says whether the
// turn was an error (is_error, subtype) and why (its result text). Only when
// there is none is the CLI's own text read, and then the whole of stderr: the
// runner's error used to be all that was looked at, and that was stderr's
// first line, so a harmless warning printed before it hid a "529 overloaded"
// that the result event spelled out -- the turn was then unclassified and the
// task went to FAILED. The stream's other lines are the agent's work (tool
// output from the project included) and are never classified.
func turnFailure(res *core.OneShotResult, runErr error) *domain.AgentFailure {
	if f := core.RunnerFailure(runErr); f != nil {
		return f
	}
	var stdout, stderr string
	if res != nil {
		stdout, stderr = res.Stdout, res.Stderr
	}
	if env, ok := finalResult(stdout); ok {
		if f := resultFailure(env); f != nil {
			return f
		}
	}
	if runErr == nil {
		return nil
	}
	text := strings.TrimSpace(stderr + "\n" + cliText(stdout))
	if strings.Contains(text, missingSessionText) {
		return domain.NewFailure(domain.FailureSessionMissing, "claude-code adapter: "+core.Excerpt(text, 300), runErr)
	}
	return core.FailureFromText(text, domain.FailureCrash, "claude-code adapter: "+runErr.Error(), runErr)
}

// resultFailure reads a final result event: nil for a successful turn, a
// budget stop for a turn the CLI stopped at --max-budget-usd, otherwise a
// failure classified from the result's own text (its subtype when the text
// is empty).
func resultFailure(env claudeEnvelope) *domain.AgentFailure {
	if env.Subtype == budgetStopped {
		return domain.NewFailure(domain.FailureBudgetStop, errBudgetStopped.Error(), errBudgetStopped)
	}
	if !env.IsError && !strings.HasPrefix(env.Subtype, "error") {
		return nil
	}
	text := env.Result
	if strings.TrimSpace(text) == "" {
		text = env.Subtype
	}
	detail := "claude-code adapter: CLI reported an error: " + truncate(text, 300)
	if strings.Contains(text, missingSessionText) {
		return domain.NewFailure(domain.FailureSessionMissing, detail, nil)
	}
	return core.FailureFromText(text, domain.FailureCrash, detail, nil)
}

// cliText is what the CLI itself printed on stdout outside its JSON events
// (an auth prompt, a crash banner). Lines of the event stream are left out:
// they carry the agent's work, including whatever the project's commands
// printed.
func cliText(stdout string) string {
	var out []string
	for _, l := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "{") {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n")
}
