package codex

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
	Reply             string            `json:"reply"`
	Exit              int               `json:"exit"`
	Timeout           bool              `json:"timeout"`
	Kind              string            `json:"kind"`
	RetryAfterSeconds *int              `json:"retryAfterSeconds"`
}

// TestStoredCodexOutputIsClassifiedByKind feeds stored Codex runs through the
// adapter's reading of a turn -- the stream's events, the exit, stderr and
// the reply file -- and checks the failure kind it reports.
func TestStoredCodexOutputIsClassifiedByKind(t *testing.T) {
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
	for _, s := range file.Samples {
		t.Run(s.Name, func(t *testing.T) {
			var rep turnReport
			var lines []string
			for _, raw := range s.Stdout {
				line := string(raw)
				var str string
				if json.Unmarshal(raw, &str) == nil {
					line = str
				}
				rep.observe(line)
				lines = append(lines, line)
			}
			res := &core.OneShotResult{Stdout: strings.Join(lines, "\n"), Stderr: s.Stderr, ExitCode: s.Exit}
			var runErr error
			switch {
			case s.Timeout:
				runErr = fmt.Errorf("agent codex: %w after %s", core.ErrTimeout, 5*time.Minute)
			case s.Exit != 0:
				runErr = fmt.Errorf("agent codex: codex exited with code %d: %s", s.Exit, s.Stderr)
			}

			// The same order invoke reads a turn in.
			f := turnFailure(res, runErr, rep)
			if f == nil && strings.TrimSpace(s.Reply) != "" {
				_, f = readReply("codex", s.Reply)
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
			if f != nil && f.Kind == domain.FailureMalformed && f.Raw != s.Reply {
				t.Errorf("a malformed reply must carry what the agent wrote, got %q", f.Raw)
			}
		})
	}
}
