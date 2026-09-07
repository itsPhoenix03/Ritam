package domain

import "testing"

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
