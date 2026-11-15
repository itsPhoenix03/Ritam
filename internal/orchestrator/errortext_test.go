package orchestrator_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoOrchestratorCodeInspectsAnErrorString: the orchestrator used to
// decide what to do with a failed turn by matching the adapter's wording
// ("missing 'to'", "overloaded"), and when the wording changed a reply with
// no recipient sent the task to FAILED. It acts on domain.AgentFailure's
// kind now; this keeps text matching on errors from coming back.
func TestNoOrchestratorCodeInspectsAnErrorString(t *testing.T) {
	inspects := regexp.MustCompile(
		`strings\.(Contains|HasPrefix|HasSuffix|Index|EqualFold|Fields|Split)\w*\([^\n]*\.Error\(\)` +
			`|\.Error\(\)\s*(==|!=)` +
			`|regexp\.[^\n]*\.Error\(\)` +
			// Kept aside to be matched on a later line.
			`|\w+\s*:?=\s*[\w.]+\.Error\(\)\s*$` +
			`|ClassifyAgentFailure|recoverableErrors|isRecoverable`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for i, line := range strings.Split(string(data), "\n") {
			if inspects.MatchString(line) {
				t.Errorf("%s:%d reads an error's text: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	if checked == 0 {
		t.Fatal("no orchestrator source files found")
	}
}
