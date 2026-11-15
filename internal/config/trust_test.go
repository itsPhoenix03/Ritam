package config

import (
	"os"
	"path/filepath"
	"slices"
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
backups: {whenProjectDeleted: delete, deleteAfterDays: 1}
access: {allowUnsandboxed: true}
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
	if cfg.Access.AllowUnsandboxed {
		t.Error("only the global settings may let agents with no sandbox run commands")
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
tui: {useMockAgents: true}
`), 0o600)
	global := Default()
	cfg, err := LoadLayers(proj)
	if err != nil {
		t.Fatal(err)
	}
	if held := cfg.RestrictProject(global, root); len(held) != 0 {
		t.Errorf("nothing to hold back: %v", held)
	}
	if !cfg.Orchestrator.RequireHumanApproval || !cfg.TUI.UseMockAgents {
		t.Errorf("tightening is the project's to say: %+v %+v", cfg.Orchestrator, cfg.TUI)
	}
}

// TestACloneCannotGiveItsOwnAgentWriteAccess: the global settings make
// Codex read-only; a cloned project's ritam.yaml defines a Codex agent of
// its own that may write. The write access is held back and named.
func TestACloneCannotGiveItsOwnAgentWriteAccess(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	_ = os.MkdirAll(root, 0o700)
	globalFile := filepath.Join(dir, "global.yaml")
	_ = os.WriteFile(globalFile, []byte("providers: {codex: {allowWrites: false}}\n"), 0o600)
	projFile := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(projFile, []byte("agents: {x: {type: codex, role: IMPLEMENTER, enabled: true, allowWrites: true}}\n"), 0o600)

	global, err := LoadLayers(globalFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(globalFile, projFile)
	if err != nil {
		t.Fatal(err)
	}
	held := cfg.RestrictProject(global, root)
	if cfg.Agents["x"].AllowWrites {
		t.Error("an agent a project defines may not write unless the global settings say so")
	}
	if !slices.Contains(held, "agents.x.allowWrites") {
		t.Errorf("the human must be told the write access was held back: %v", held)
	}
	if msg := HeldNotice(held, globalFile); !strings.Contains(msg, "agents.x.allowWrites") {
		t.Errorf("the notice names it: %q", msg)
	}
}

// TestAnAgentTheGlobalSettingsLetWriteKeepsIt: write access the user gave
// in the global file is not the project's to lose by repeating it.
func TestAnAgentTheGlobalSettingsLetWriteKeepsIt(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	_ = os.MkdirAll(root, 0o700)
	globalFile := filepath.Join(dir, "global.yaml")
	_ = os.WriteFile(globalFile, []byte("agents: {x: {type: codex, role: IMPLEMENTER, enabled: true, allowWrites: true}}\n"), 0o600)
	projFile := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(projFile, []byte("agents: {x: {type: codex, role: IMPLEMENTER, enabled: true, allowWrites: true}}\n"), 0o600)

	global, _ := LoadLayers(globalFile)
	cfg, err := LoadLayers(globalFile, projFile)
	if err != nil {
		t.Fatal(err)
	}
	if held := cfg.RestrictProject(global, root); len(held) != 0 || !cfg.Agents["x"].AllowWrites {
		t.Errorf("the global file's grant stands: held %v, agent %+v", held, cfg.Agents["x"])
	}
}

// TestAProjectCannotSwitchTheScriptedAgentsOff: a cloned folder may not move
// ritam from the scripted agents to the real CLIs by itself; it may ask for
// the scripted ones, and the user's global file may switch them off.
func TestAProjectCannotSwitchTheScriptedAgentsOff(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	_ = os.MkdirAll(root, 0o700)
	globalFile := filepath.Join(dir, "global.yaml")
	projFile := filepath.Join(root, "ritam.yaml")
	load := func(global, project string) (*Config, []string) {
		t.Helper()
		_ = os.WriteFile(globalFile, []byte(global), 0o600)
		_ = os.WriteFile(projFile, []byte(project), 0o600)
		g, err := LoadLayers(globalFile)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadLayers(globalFile, projFile)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, cfg.RestrictProject(g, root)
	}

	cfg, held := load("", "tui: {useMockAgents: false}\n")
	if !cfg.TUI.UseMockAgents || !slices.Contains(held, "tui.useMockAgents") {
		t.Errorf("a project may not switch the scripted agents off: mocks %v, held %v", cfg.TUI.UseMockAgents, held)
	}
	cfg, held = load("tui: {useMockAgents: false}\n", "")
	if cfg.TUI.UseMockAgents || len(held) != 0 {
		t.Errorf("the global file may switch them off: mocks %v, held %v", cfg.TUI.UseMockAgents, held)
	}
	cfg, held = load("tui: {useMockAgents: false}\n", "tui: {useMockAgents: true}\n")
	if !cfg.TUI.UseMockAgents || len(held) != 0 {
		t.Errorf("a project may switch them on: mocks %v, held %v", cfg.TUI.UseMockAgents, held)
	}
}
