package domain

import (
	"errors"
	"fmt"
	"testing"
)

func TestCLIFailuresAreRecognised(t *testing.T) {
	cases := map[string]FailureKind{
		"API Error: 400 Credit balance is too low to access the Anthropic API":                 FailureCredits,
		"error: insufficient_quota: You exceeded your current quota":                           FailureCredits,
		"Claude AI usage limit reached|1760500000":                                             FailureLimit,
		"You've hit your usage limit. Upgrade to Pro or try again later":                       FailureLimit,
		"Your organization does not have access to claude-fable-5-1":                           FailurePlan,
		"The 'gpt-6-astra' model is not supported when using Codex with a ChatGPT account.":    FailurePlan,
		"The model `gpt-9` does not exist or you do not have access to it.":                    FailureModel,
		`{"type":"error","error":{"type":"not_found_error","message":"model: claude-opus-9"}}`: FailureModel,
		"Invalid API key · Please run /login":                                                  FailureAuth,
		"codex adapter: exit status 1: panic: runtime error":                                   "",
		"claude-code adapter: agent opus replied without an @@ORCH header":                     "",
	}
	for text, want := range cases {
		if got := ClassifyAgentFailure(text); got != want {
			t.Errorf("%q: got %q, want %q", text, got, want)
		}
	}
	if !FailureCredits.MarksModelUnavailable() || FailureLimit.MarksModelUnavailable() || FailureAuth.MarksModelUnavailable() {
		t.Error("only credits, plan and model failures make a model unavailable; limits and logins pass")
	}
}

// TestATransientServerErrorIsNotALimit: "overloaded" and "try again later"
// were read as a usage limit, so one busy moment at the provider announced a
// supporting agent as unusable for the rest of the task.
func TestATransientServerErrorIsNotALimit(t *testing.T) {
	for _, text := range []string{
		`API Error: 529 {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		"stream disconnected before completion: 503 Service Unavailable",
		"Something went wrong, please try again later",
	} {
		if got := ClassifyAgentFailure(text); got != FailureOverloaded {
			t.Errorf("%q: got %q, want overloaded", text, got)
		}
	}
	if !FailureOverloaded.Retryable() || FailureBilling.Retryable() || FailureBudgetStop.Retryable() {
		t.Error("an overloaded server is worth retrying; billing and a budget stop are not")
	}
}

// TestAFailureIsFoundThroughWrapping: adapters may wrap the failure they
// report; the orchestrator still finds its kind, with errors.As or errors.Is.
func TestAFailureIsFoundThroughWrapping(t *testing.T) {
	cause := errors.New("exit status 1")
	f := NewFailure(FailureTimeout, "agent CLI timed out after 5m0s", cause)
	wrapped := fmt.Errorf("claude-code adapter: %w", f)

	var got *AgentFailure
	if !errors.As(wrapped, &got) || got != f {
		t.Fatal("errors.As must find the failure")
	}
	if !errors.Is(wrapped, FailureTimeout) || errors.Is(wrapped, FailureCrash) {
		t.Error("errors.Is must match the failure's own kind, and only it")
	}
	if !errors.Is(wrapped, cause) {
		t.Error("the underlying error must stay reachable")
	}
	if FailureOf(nil) != nil {
		t.Error("no error is no failure")
	}
	if k := FailureOf(errors.New("a missing 'to' that says nothing")).Kind; k != FailureCrash {
		t.Errorf("an error with no kind is a crash, whatever it says; got %q", k)
	}
}
