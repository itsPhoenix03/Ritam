package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
)

// failureSample is one stored CLI run (testdata/failure_samples.json).
type failureSample struct {
	Name              string            `json:"name"`
	Stdout            []json.RawMessage `json:"stdout"`
	Stderr            string            `json:"stderr"`
	Exit              int               `json:"exit"`
	Timeout           bool              `json:"timeout"`
	Kind              string            `json:"kind"`
	RetryAfterSeconds *int              `json:"retryAfterSeconds"`
}

func loadSamples(t *testing.T) []failureSample {
	t.Helper()
	data, err := os.ReadFile("testdata/failure_samples.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Samples []failureSample `json:"samples"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Samples
}

// run rebuilds what the runner hands the adapter: the output, and the
// runner's error for a timeout or a non-zero exit.
func (s failureSample) run() (*core.OneShotResult, error) {
	var lines []string
	for _, raw := range s.Stdout {
		var str string
		if json.Unmarshal(raw, &str) == nil {
			lines = append(lines, str)
		} else {
			lines = append(lines, string(raw))
		}
	}
	res := &core.OneShotResult{Stdout: strings.Join(lines, "\n") + "\n", Stderr: s.Stderr, ExitCode: s.Exit}
	switch {
	case s.Timeout:
		return res, fmt.Errorf("agent opus: %w after %s", core.ErrTimeout, 5*time.Minute)
	case s.Exit != 0:
		return res, fmt.Errorf("agent opus: claude exited with code %d: %s", s.Exit, s.Stderr)
	}
	return res, nil
}

// TestStoredClaudeOutputIsClassifiedByKind feeds stored Claude Code runs
// through the adapter's reading of a turn and checks the failure kind it
// reports -- the kind, not the wording, is what the orchestrator acts on.
func TestStoredClaudeOutputIsClassifiedByKind(t *testing.T) {
	for _, s := range loadSamples(t) {
		t.Run(s.Name, func(t *testing.T) {
			res, runErr := s.run()
			f := turnFailure(res, runErr)
			if f == nil {
				// The same order invoke reads a turn in.
				text, err := extractResult(res.Stdout)
				if err != nil {
					f = domain.FailureOf(err)
				} else if strings.TrimSpace(text) != "" {
					_, f = readReply("opus", text)
				}
			}
			var got string
			if f != nil {
				got = string(f.Kind)
			}
			if got != s.Kind {
				t.Fatalf("kind = %q, want %q (failure: %+v)", got, s.Kind, f)
			}
			if s.RetryAfterSeconds != nil && f.RetryAfter != time.Duration(*s.RetryAfterSeconds)*time.Second {
				t.Errorf("reset = %v, want %ds", f.RetryAfter, *s.RetryAfterSeconds)
			}
			if f != nil && f.Kind == domain.FailureMalformed && strings.Contains(s.Name, "reply") && f.Raw == "" {
				t.Error("a malformed reply must carry what the agent wrote")
			}
		})
	}
}
