package core

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ritam/internal/domain"
)

// RunnerFailure is the failure a runner error stands for when the runner
// itself knows the cause: the turn ran out of time, or was cancelled. It
// returns nil for anything else, which only the adapter can read (an exit
// code means what the CLI's own output says it means).
func RunnerFailure(err error) *domain.AgentFailure {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrCancelled):
		return domain.NewFailure(domain.FailureCancelled, err.Error(), err)
	case errors.Is(err, ErrTimeout):
		return domain.NewFailure(domain.FailureTimeout, err.Error(), err)
	}
	return nil
}

// FailureFromText builds a failure from text the CLI itself gave as the
// reason for an error -- an error result, an error event's message, its
// stderr -- never from the agent's output, which can say anything (a project's
// test log saying "rate limit" once read as the agent being limited).
//
// The kind is the substring classifier's (domain.ClassifyAgentFailure), or
// fallback when it recognises nothing. A limit also gets the reset time the
// text gives, when it gives one.
func FailureFromText(text string, fallback domain.FailureKind, detail string, err error) *domain.AgentFailure {
	kind := domain.ClassifyAgentFailure(text)
	if kind == "" {
		kind = fallback
	}
	f := domain.NewFailure(kind, detail, err)
	if kind == domain.FailureRateLimit || kind == domain.FailureOverloaded {
		f.RetryAfter = ParseRetryAfter(text, time.Now())
	}
	return f
}

var (
	// "Claude AI usage limit reached|1760500000": the reset as a Unix time.
	epochReset = regexp.MustCompile(`\|(\d{10})\b`)
	// "try again in 2 hours 5 minutes", "resets in 45s", "retry after 30 seconds".
	inDuration = regexp.MustCompile(`(?:try again|retry|resets?)\s+(?:in|after)\s+((?:\d+\s*(?:hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b[\s,]*(?:and\s+)?)+)`)
	durPart    = regexp.MustCompile(`(\d+)\s*(hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)\b`)
	// "retry-after: 30" (seconds), as an HTTP header is quoted in an error.
	retryAfterHeader = regexp.MustCompile(`retry-after:?\s*(\d+)`)
)

// ParseRetryAfter reads how long until a limit resets from a CLI's error
// text, or 0 when the text does not say (a reset given only as a clock time
// is not guessed at: the time zone is unknown).
func ParseRetryAfter(text string, now time.Time) time.Duration {
	t := strings.ToLower(text)
	if m := epochReset.FindStringSubmatch(t); m != nil {
		if secs, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if d := time.Unix(secs, 0).Sub(now); d > 0 {
				return d.Round(time.Second)
			}
		}
		return 0
	}
	if m := inDuration.FindStringSubmatch(t); m != nil {
		var d time.Duration
		for _, p := range durPart.FindAllStringSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(p[1])
			switch p[2][0] {
			case 'h':
				d += time.Duration(n) * time.Hour
			case 'm':
				d += time.Duration(n) * time.Minute
			default:
				d += time.Duration(n) * time.Second
			}
		}
		return d
	}
	if m := retryAfterHeader.FindStringSubmatch(t); m != nil {
		n, _ := strconv.Atoi(m[1])
		return time.Duration(n) * time.Second
	}
	return 0
}

// ErrorText is the whole of a CLI's stderr, trimmed, for classifying a
// failure. The first line alone used to be read, so a harmless warning
// printed before the real cause hid it.
func ErrorText(res *OneShotResult) string {
	if res == nil {
		return ""
	}
	return strings.TrimSpace(res.Stderr)
}

// Excerpt shortens text for a failure's Detail, keeping it on one line.
func Excerpt(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "..."
	}
	return s
}
