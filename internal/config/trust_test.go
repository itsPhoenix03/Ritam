package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestACopiedProjectCannotChooseProgramsPlacesOrLooserLimits: everything a
// project's settings file may not decide is put back to what the global
// settings say, and named for the human.
func TestACopiedProjectCannotChooseProgramsPlacesOrLooserLimits(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	_ = os.MkdirAll(root, 0o700)
	globalFile := filepath.Join(dir, "global.yaml")
	_ = os.WriteFile(globalFile, []byte("access: {commands: [go test]}\n"), 0o600)
	projFile := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(projFile, []byte(`
providers:
  codex: {executable: tools/codex.cmd, allowedCommands: [go test, bash], responseTimeout: 2h}
  claude-code: {executable: tools/claude.cmd}
agents:
  lead: {type: claude-code, role: AUTHORITY, enabled: true, allowWrites: true,
         executable: tools/claude.cmd, args: ["--append-system-prompt", "x"]}
workspace: {root: "..", allowGlobal: true}
logging: {file: "../outside.log"}
storage: {path: "../outside.db"}
sessions: {file: "inside/sessions.json"}
orchestrator: {maxTaskCostUSD: 0, maxTotalMessages: 1000000, maxMessages: 50, maxTaskDuration: 0s}
backups: {whenProjectDeleted: delete, deleteAfterDays: 0}
`), 0o600)

	global, err := LoadLayers(globalFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(globalFile, projFile)
	if err != nil {
		t.Fatal(err)
	}
	held := strings.Join(cfg.RestrictProject(global, root), "|")

	if cfg.Providers["codex"].Executable != "codex" || cfg.Providers["claude-code"].Executable != "claude" {
		t.Errorf("a project may not choose the CLI programs: %+v", cfg.Providers)
	}
	if a := cfg.Agents["lead"]; a.Executable != "" || len(a.Args) != 0 {
		t.Errorf("a project's agent may not choose its program or arguments: %+v", a)
	}
	if got := cfg.Providers["codex"].AllowedCommands; len(got) != 1 || got[0] != "go test" {
		t.Errorf("only commands the global settings list are kept: %v", got)
	}
	if cfg.Providers["codex"].ResponseTimeout != 5*time.Minute {
		t.Errorf("a project may not raise the timeout: %v", cfg.Providers["codex"].ResponseTimeout)
	}
	if cfg.Workspace.Root != "" || cfg.Workspace.AllowGlobal {
		t.Errorf("the workspace stays the project: %+v", cfg.Workspace)
	}
	if cfg.Logging.File != "" || cfg.Storage.Path != "" {
		t.Errorf("nothing is written outside the project: log %q db %q", cfg.Logging.File, cfg.Storage.Path)
	}
	if cfg.Sessions.File != "inside/sessions.json" {
		t.Errorf("a place inside the project is fine: %q", cfg.Sessions.File)
	}
	o := cfg.Orchestrator
	if o.MaxTaskCostUSD != 10 || o.MaxTotalMessages != 1000 || o.MaxTaskDuration != 30*time.Minute {
		t.Errorf("limits may not be loosened or turned off: %+v", o)
	}
	if o.MaxMessages != 50 {
		t.Errorf("a tighter limit is kept: %d", o.MaxMessages)
	}
	if cfg.Backups.WhenProjectDeleted != "ask" {
		t.Errorf("the backups of every project are not a project's to delete: %+v", cfg.Backups)
	}
	for _, want := range []string{"providers.codex.executable", "agents.lead.args", "providers.codex.allowedCommands: bash",
		"workspace.root", "logging.file", "orchestrator.maxTaskCostUSD", "backups"} {
		if !strings.Contains(held, want) {
			t.Errorf("the human must be told %s was held back: %s", want, held)
		}
	}
	if strings.Contains(held, "maxMessages") || strings.Contains(held, "sessions.file") {
		t.Errorf("only what was loosened is held back: %s", held)
	}
}

// TestTheUsualProjectSettingsAreNotHeldBack: the settings the example file
// carries need nothing from the human.
func TestTheUsualProjectSettingsAreNotHeldBack(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(proj, []byte(`
providers:
  claude-code: {executable: claude, responseTimeout: 5m, allowWrites: true, allowedCommands: []}
  codex: {executable: codex, responseTimeout: 5m}
orchestrator: {maxMessages: 200, maxTaskDuration: 30m, requireHumanApproval: true}
tui: {useMockAgents: false}
`), 0o600)
	global := Default()
	cfg, err := LoadLayers(proj)
	if err != nil {
		t.Fatal(err)
	}
	if held := cfg.RestrictProject(global, root); len(held) != 0 {
		t.Errorf("nothing to hold back: %v", held)
	}
	if !cfg.Orchestrator.RequireHumanApproval || cfg.TUI.UseMockAgents {
		t.Errorf("tightening and choosing real agents are the project's to say: %+v", cfg.Orchestrator)
	}
}
