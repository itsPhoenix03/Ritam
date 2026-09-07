package domain

import "strings"

// Why an agent's CLI could not run its model.
//
// A turn that fails because the account cannot use the model -- no credits
// left, a model the subscription does not include, a model the CLI does not
// know, a CLI that is not logged in, a usage limit -- is not the agent
// misbehaving, and asking it to "reply again" only fails again. These are
// recognised from the CLI's own error text and reported as what they are.
type FailureKind string

const (
	// FailureCredits: the account has no credits or quota left.
	FailureCredits FailureKind = "credits"
	// FailurePlan: the account's plan or organisation does not include the model.
	FailurePlan FailureKind = "plan"
	// FailureModel: the CLI does not know the model.
	FailureModel FailureKind = "model"
	// FailureAuth: the CLI is not logged in.
	FailureAuth FailureKind = "auth"
	// FailureLimit: a usage or rate limit, which lifts by itself later.
	FailureLimit FailureKind = "limit"
)

// failurePatterns are matched, lower-cased, against the CLI's error text --
// only ever error text: bare status codes are left out because numbers in
// ordinary output (token counts) would match them.
// Order matters: the first match wins, most specific first.
var failurePatterns = []struct {
	kind     FailureKind
	patterns []string
}{
	{FailureCredits, []string{"credit balance is too low", "insufficient credit", "out of credits", "insufficient_quota",
		"exceeded your current quota", "no credits", "purchase credits", "billing", "payment required"}},
	{FailurePlan, []string{"not available on your plan", "not included in your plan", "upgrade your plan",
		"does not have access to", "do not have access to", "don't have access to", "not available for your account",
		"not supported when using codex with a chatgpt account", "requires a pro", "requires a max", "permission to use this model"}},
	{FailureModel, []string{"model not found", "unknown model", "invalid model", "model does not exist",
		"does not exist or you do not have access", "not_found_error", "no such model", "unsupported model"}},
	{FailureAuth, []string{"not logged in", "please run /login", "claude /login", "codex login", "invalid api key",
		"authentication_error", "unauthorized", "status 401"}},
	{FailureLimit, []string{"usage limit", "rate limit", "rate_limit", "too many requests", "status 429", "overloaded",
		"you've hit your", "limit reached", "try again later"}},
}

// ClassifyAgentFailure reports why a CLI could not run, from its error text,
// or "" when the failure is something else (a crash, a malformed reply).
func ClassifyAgentFailure(text string) FailureKind {
	t := strings.ToLower(text)
	for _, fp := range failurePatterns {
		for _, p := range fp.patterns {
			if strings.Contains(t, p) {
				// "do not have access" also appears in the model-not-found
				// wording; a model that does not exist is the model's fault.
				if fp.kind == FailurePlan && strings.Contains(t, "does not exist") {
					return FailureModel
				}
				return fp.kind
			}
		}
	}
	return ""
}

// MarksModelUnavailable reports whether a failure means the model cannot be
// used by this account until something changes (credits, plan, model name),
// as opposed to a login or a limit that lifts by itself.
func (k FailureKind) MarksModelUnavailable() bool {
	return k == FailureCredits || k == FailurePlan || k == FailureModel
}

// Advice is what the human can do about a failure of this kind.
func (k FailureKind) Advice() string {
	switch k {
	case FailureCredits:
		return "the account is out of credits for this model: add credits or recruit an agent on another model"
	case FailurePlan:
		return "this model is not included in your plan: recruit an agent on a model your plan includes"
	case FailureModel:
		return "the CLI does not know this model: recruit an agent on a listed model"
	case FailureAuth:
		return "the CLI is not logged in: run `claude /login` or `codex login`, then resume"
	case FailureLimit:
		return "a usage limit was reached: resume once it resets"
	}
	return ""
}
