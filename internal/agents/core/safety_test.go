package core

import (
	"strings"
	"testing"

	"ritam/internal/domain"
)

func TestDestructiveCommandsAreRefusedWhateverIsConfigured(t *testing.T) {
	kept, dropped := SafeCommands([]string{"go test", "go build ./...", "rm -rf build", "git push origin main",
		"Remove-Item x", "curl http://x", "npm test", "RM file"})
	if strings.Join(kept, "|") != "go test|go build ./...|npm test" {
		t.Errorf("kept %v", kept)
	}
	if len(dropped) != 5 {
		t.Errorf("dropped %v, want the five destructive ones", dropped)
	}
	// A prefix match only: "rmate" is not "rm".
	if IsDangerousCommand("rmate open") {
		t.Error("a command merely starting with the same letters is not refused")
	}
}

func TestTheWriteRulesNameTheFolderAndTheCommands(t *testing.T) {
	r := WriteRules(`C:\z\project-orch`, Grant{Level: domain.AccessBuildAndTest, Commands: []string{"go test"}})
	for _, want := range []string{`C:\z\project-orch`, "not temp folders", "go test", "blocked", "Build and test"} {
		if !strings.Contains(r, want) {
			t.Errorf("rules missing %q:\n%s", want, r)
		}
	}
	if r := WriteRules("/p", Grant{}); !strings.Contains(r, "may not run commands") || !strings.Contains(r, "always read and search files") {
		t.Error("with no allowed commands the agent runs nothing but may still read")
	}
}
