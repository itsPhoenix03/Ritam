package domain

import (
	"errors"
	"strings"
	"time"
)

// FailureKind is why an agent's turn failed.
//
// Adapters used to report failures as free text that the orchestrator
// pattern-matched, and the two drifted apart: the adapters' new wording for a
// reply with no recipient was not on the orchestrator's list, so one such
// reply sent a task to FAILED. Adapters now name the kind, from the CLI's
// structured output where it has any, and the orchestrator acts on the kind
// alone (see AgentFailure).
//
// The values of the kinds that existed before (credits, plan, model, auth,
// limit) are unchanged: they are stored in events and in models.json.
type FailureKind string

const (
	// FailureMalformed: the turn finished but its reply could not be read
	// (no header, no recipient, not the expected output).
	FailureMalformed FailureKind = "malformed"
	// FailureTimeout: the turn ran past its time limit and was ended.
	FailureTimeout FailureKind = "timeout"
	// FailureSessionMissing: the CLI no longer has the session the turn
	// tried to resume.
	FailureSessionMissing FailureKind = "session-missing"
	// FailureRateLimit: a usage or rate limit, which lifts by itself later.
	FailureRateLimit FailureKind = "limit"
	// FailureOverloaded: the provider was overloaded or had a server error;
	// the same request may well succeed a little later.
	FailureOverloaded FailureKind = "overloaded"
	// FailureAuth: the CLI is not logged in.
	FailureAuth FailureKind = "auth"
	// FailureBilling: the account has no credits or quota left.
	FailureBilling FailureKind = "credits"
	// FailurePlan: the account's plan or organisation does not include the model.
	FailurePlan FailureKind = "plan"
	// FailureModelUnavailable: the CLI does not know the model.
	FailureModelUnavailable FailureKind = "model"
	// FailureBudgetStop: the CLI stopped the turn at the task's spending limit.
	FailureBudgetStop FailureKind = "budget"
	// FailureCrash: the CLI failed in a way nothing more specific explains
	// (a non-zero exit, an error result with no recognisable cause).
	FailureCrash FailureKind = "crash"
	// FailureCancelled: the turn was stopped on purpose; nobody is waiting
	// for its answer.
	FailureCancelled FailureKind = "cancelled"
)

// The older names of three kinds, kept for the model checks that use them.
const (
	FailureCredits = FailureBilling
	FailureModel   = FailureModelUnavailable
	FailureLimit   = FailureRateLimit
)

// Error makes a kind usable with errors.Is: errors.Is(err, FailureTimeout)
// reports whether err is (or wraps) an AgentFailure of that kind.
func (k FailureKind) Error() string { return string(k) }

// Retryable reports whether a turn that failed this way may succeed if it is
// simply tried again: a malformed reply, a timeout, a lost session, a limit
// that lifts, an overloaded server or a crash. Login, billing, plan and model
// failures fail again until a human changes something; a budget stop and a
// cancel are not failures of the agent at all.
func (k FailureKind) Retryable() bool {
	switch k {
	case FailureMalformed, FailureTimeout, FailureSessionMissing, FailureRateLimit, FailureOverloaded, FailureCrash:
		return true
	}
	return false
}

// AgentFailure is a failed agent turn, as an adapter reports it on
// ports.Outbound.Err. It satisfies error, and errors.As finds it through any
// wrapping.
type AgentFailure struct {
	Kind FailureKind
	// Retryable says whether trying the same turn again may succeed.
	Retryable bool
	// RetryAfter is how long until a limit resets, when the CLI said; 0 when
	// unknown.
	RetryAfter time.Duration
	// Detail explains the failure to the human and the log.
	Detail string
	// Raw is, for a Malformed failure, what the agent actually wrote, so the
	// orchestrator can quote it back instead of the adapter's complaint.
	Raw string
	// Err is the underlying error, when there is one.
	Err error
}

// NewFailure builds a failure of kind, retryable when kinds of it are.
func NewFailure(kind FailureKind, detail string, err error) *AgentFailure {
	return &AgentFailure{Kind: kind, Retryable: kind.Retryable(), Detail: detail, Err: err}
}

func (f *AgentFailure) Error() string {
	switch {
	case f.Detail != "":
		return f.Detail
	case f.Err != nil:
		return f.Err.Error()
	}
	return "agent failure: " + string(f.Kind)
}

// Unwrap returns the underlying error.
func (f *AgentFailure) Unwrap() error { return f.Err }

// Is matches a FailureKind target against the failure's kind.
func (f *AgentFailure) Is(target error) bool {
	k, ok := target.(FailureKind)
	return ok && k == f.Kind
}

// FailureOf returns the AgentFailure err is or wraps, or nil for nil. An
// error that carries no kind -- from an adapter that predates the contract --
// is a crash: the orchestrator never reads a kind out of error text.
func FailureOf(err error) *AgentFailure {
	if err == nil {
		return nil
	}
	var f *AgentFailure
	if errors.As(err, &f) {
		return f
	}
	return NewFailure(FailureCrash, err.Error(), err)
}

// failurePatterns are matched, lower-cased, against a CLI's own error text --
// only ever error text: bare status codes are left out because numbers in
// ordinary output (token counts) would match them. They are the last resort,
// for a CLI that gave no structured reason.
// Order matters: the first match wins, most specific first. A limit comes
// before overloaded, so "you've hit your usage limit ... try again later" is
// the limit it says it is.
var failurePatterns = []struct {
	kind     FailureKind
	patterns []string
}{
	{FailureBilling, []string{"credit balance is too low", "insufficient credit", "out of credits", "insufficient_quota",
		"exceeded your current quota", "no credits", "purchase credits", "billing", "payment required"}},
	{FailurePlan, []string{"not available on your plan", "not included in your plan", "upgrade your plan",
		"does not have access to", "do not have access to", "don't have access to", "not available for your account",
		"not supported when using codex with a chatgpt account", "requires a pro", "requires a max", "permission to use this model"}},
	{FailureModelUnavailable, []string{"model not found", "unknown model", "invalid model", "model does not exist",
		"does not exist or you do not have access", "not_found_error", "no such model", "unsupported model"}},
	{FailureAuth, []string{"not logged in", "please run /login", "claude /login", "codex login", "invalid api key",
		"authentication_error", "unauthorized", "status 401"}},
	{FailureRateLimit, []string{"usage limit", "rate limit", "rate_limit", "too many requests", "status 429",
		"you've hit your", "limit reached"}},
	// A transient server problem, retried rather than taken as a limit: one
	// "overloaded" used to announce an agent as unusable for the whole task.
	{FailureOverloaded, []string{"overloaded", "try again later", "internal server error", "server_error",
		"server error", "service unavailable", "bad gateway", "gateway timeout", "status 500", "status 502",
		"status 503", "status 529", "api error: 500", "api error: 502", "api error: 503", "api error: 529",
		"connection reset", "econnreset", "stream disconnected"}},
}

// ClassifyAgentFailure reports why a CLI could not run, from its error text,
// or "" when the text names no known cause (a crash, a malformed reply).
//
// It reads text, so it is the last resort: adapters use it only on what the
// CLI itself reported as an error (an error result's text, an error event's
// message, stderr), never on the agent's output; and the orchestrator does not
// use it at all -- it acts on AgentFailure.Kind.
func ClassifyAgentFailure(text string) FailureKind {
	t := strings.ToLower(text)
	for _, fp := range failurePatterns {
		for _, p := range fp.patterns {
			if strings.Contains(t, p) {
				// "do not have access" also appears in the model-not-found
				// wording; a model that does not exist is the model's fault.
				if fp.kind == FailurePlan && strings.Contains(t, "does not exist") {
					return FailureModelUnavailable
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
	return k == FailureBilling || k == FailurePlan || k == FailureModelUnavailable
}

// IsAccountFailure reports whether the account cannot run the model at all
// until a human acts: a login, billing, the plan or the model name.
func (k FailureKind) IsAccountFailure() bool {
	return k == FailureAuth || k.MarksModelUnavailable()
}

// Advice is what the human can do about a failure of this kind.
func (k FailureKind) Advice() string {
	switch k {
	case FailureBilling:
		return "the account is out of credits for this model: add credits or recruit an agent on another model"
	case FailurePlan:
		return "this model is not included in your plan: recruit an agent on a model your plan includes"
	case FailureModelUnavailable:
		return "the CLI does not know this model: recruit an agent on a listed model"
	case FailureAuth:
		return "the CLI is not logged in: run `claude /login` or `codex login`, then resume"
	case FailureRateLimit:
		return "a usage limit was reached: resume once it resets"
	case FailureOverloaded:
		return "the provider is overloaded or had a server error: resume to try again"
	case FailureTimeout:
		return "the turn ran past its time limit: resume to try again, or raise the provider's responseTimeout"
	case FailureCrash:
		return "the CLI failed: resume to try again; the log has its output"
	case FailureMalformed:
		return "the agent's replies could not be read"
	case FailureBudgetStop:
		return "the task's spending budget was reached"
	}
	return ""
}
