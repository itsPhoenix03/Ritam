package core

import (
	"fmt"
	"testing"
	"time"

	"ritam/internal/domain"
)

// TestALimitsResetTimeIsReadWhenTheCLIGivesIt: a limit that resets in two
// minutes can be waited out; one that resets tonight cannot. Telling them
// apart needs the reset time, read from the CLI's own wording.
func TestALimitsResetTimeIsReadWhenTheCLIGivesIt(t *testing.T) {
	now := time.Unix(1_760_500_000, 0)
	cases := map[string]time.Duration{
		"Claude AI usage limit reached|1760500120":                       2 * time.Minute,
		"You've hit your usage limit. Try again in 2 hours 5 minutes.":   2*time.Hour + 5*time.Minute,
		"rate limit exceeded, retry after 30 seconds":                    30 * time.Second,
		"429 Too Many Requests (retry-after: 12)":                        12 * time.Second,
		"Rate limit reached. Resets in 45s":                              45 * time.Second,
		"5-hour limit reached - resets 3pm":                              0, // a clock time: zone unknown
		"Claude AI usage limit reached|1760499000":                       0, // already past
		"You've hit your usage limit. Upgrade to Pro or try again later": 0,
	}
	for text, want := range cases {
		if got := ParseRetryAfter(text, now); got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}

func TestTheRunnersOwnCausesBecomeFailures(t *testing.T) {
	if f := RunnerFailure(fmt.Errorf("agent x: %w after 5m0s", ErrTimeout)); f == nil || f.Kind != domain.FailureTimeout || !f.Retryable {
		t.Errorf("a timeout is a retryable Timeout failure, got %+v", f)
	}
	if f := RunnerFailure(fmt.Errorf("agent x: %w", ErrCancelled)); f == nil || f.Kind != domain.FailureCancelled {
		t.Errorf("a cancel is a Cancelled failure, got %+v", f)
	}
	if f := RunnerFailure(fmt.Errorf("agent x: claude exited with code 1: boom")); f != nil {
		t.Errorf("an exit code means what the CLI's output says; the runner must not guess: %+v", f)
	}
}
