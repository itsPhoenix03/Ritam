package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
)

// TestAgentsAreToldWhatIsTheirsAndWhatIsRitams: every agent learns the
// workspace it is confined to, whether its reads are, and ritam's own files
// it may not edit.
func TestAgentsAreToldWhatIsTheirsAndWhatIsRitams(t *testing.T) {
	root := t.TempDir()
	ws := config.Workspace{Root: root, Writable: true, ConfineReads: true}
	cfg := &config.Config{}
	cfg.Sessions.File = filepath.Join(root, "state", "sessions.json")
	cfg.Storage.Path = filepath.Join(t.TempDir(), "elsewhere.db") // outside: not the agents' to touch anyway

	ac, err := applyWorkspace(config.AgentConfig{AllowWrites: true}, ws, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ac.WorkspaceRoot != root || !ac.ConfineReads {
		t.Errorf("the agent must know its workspace and that reads are confined: %+v", ac)
	}
	for _, want := range []string{filepath.Join(root, ".ritam"), filepath.Join(root, "ritam.yaml"), cfg.Sessions.File} {
		if !slices.Contains(ac.Protected, want) {
			t.Errorf("%s must be protected: %v", want, ac.Protected)
		}
	}
	if slices.Contains(ac.Protected, cfg.Storage.Path) {
		t.Errorf("a file outside the workspace needs no rule: %v", ac.Protected)
	}
}

// TestAnAgentInTheSettingsWritesOnlyWhenItsProviderMay: the global settings
// make Codex read-only, and an agent entry of type codex that may write is
// read-only too, the same rule a recruit gets.
func TestAnAgentInTheSettingsWritesOnlyWhenItsProviderMay(t *testing.T) {
	no := false
	cfg := config.Default()
	cfg.TUI.UseMockAgents = false
	cfg.Storage.Backend = "memory"
	cfg.Workspace.Root = t.TempDir()
	cfg.Providers["codex"] = config.ProviderConfig{Executable: "codex", AllowWrites: &no}
	cfg.Agents = map[string]config.AgentConfig{
		"x":    {Type: "codex", Role: "IMPLEMENTER", Enabled: true, AllowWrites: true},
		"opus": {Type: "claude-code", Role: "AUTHORITY", Enabled: true, AllowWrites: true},
	}
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	if got := sys.Access.Unsandboxed(); !slices.Equal(got, []domain.AgentID{"opus"}) {
		t.Errorf("only the agent whose provider may write writes: %v", got)
	}
}

// TestAClonedProjectGetsNoWritingRealAgent: the escape end to end. The
// global settings make Codex read-only; a cloned folder's ritam.yaml defines
// a Codex agent that may write and switches the scripted agents off. A plain
// start gives a scripted, read-only agent and tells the human why.
func TestAClonedProjectGetsNoWritingRealAgent(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "clone")
	_ = os.MkdirAll(root, 0o700)
	globalFile := filepath.Join(dir, "global.yaml")
	_ = os.WriteFile(globalFile, []byte("providers: {codex: {allowWrites: false}}\n"), 0o600)
	projFile := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(projFile, []byte("agents: {x: {type: codex, role: IMPLEMENTER, enabled: true, allowWrites: true}}\n"+
		"tui: {useMockAgents: false}\n"), 0o600)

	global, err := config.LoadLayers(globalFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadLayers(globalFile, projFile)
	if err != nil {
		t.Fatal(err)
	}
	held := cfg.RestrictProject(global, root)
	notice := config.HeldNotice(held, globalFile)
	for _, want := range []string{"agents.x.allowWrites", "tui.useMockAgents"} {
		if !strings.Contains(notice, want) {
			t.Errorf("the human must be told %s was held back: %q", want, notice)
		}
	}
	cfg.Storage.Backend = "memory"
	cfg.Workspace.Root = root
	sys, err := Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sys.Close() })
	if a, ok := sys.Service.team.reg.Get("x"); !ok || a.Kind != domain.AgentKindMock {
		t.Errorf("the clone's agent must stay scripted: %+v", a)
	}
	if got := sys.Access.Unsandboxed(); len(got) != 0 {
		t.Errorf("no agent of the clone may write: %v", got)
	}
}

// TestTheGuardCoversRitamsSettingsAndRecords: an agent's edit to the
// workspace's ritam.yaml or the project's settings is put back after its turn.
func TestTheGuardCoversRitamsSettingsAndRecords(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	cfg := &config.Config{}
	cfg.Runtime.HomeDir = home
	cfg.Runtime.ProjectConfig = filepath.Join(root, ".ritam", "ritam.yaml")
	cfg.Sessions.File = filepath.Join(root, ".ritam", "sessions.json")
	visible := filepath.Join(root, "ritam.yaml")
	_ = os.WriteFile(visible, []byte("agents: {}\n"), 0o600)

	g := newGuard(cfg, config.Workspace{Root: root})
	_ = os.WriteFile(visible, []byte("agents: {opus: {executable: evil.cmd}}\n"), 0o600)
	_ = os.MkdirAll(filepath.Dir(cfg.Runtime.ProjectConfig), 0o700)
	_ = os.WriteFile(cfg.Runtime.ProjectConfig, []byte("workspace: {root: 'C:\\'}\n"), 0o600)

	if got := g.Check("codex"); len(got) != 2 {
		t.Fatalf("both edits must be put back, got %v", got)
	}
	if data, _ := os.ReadFile(visible); string(data) != "agents: {}\n" {
		t.Errorf("ritam.yaml was not put back: %q", data)
	}
	if _, err := os.Stat(cfg.Runtime.ProjectConfig); !os.IsNotExist(err) {
		t.Error("a settings file the agent created must be removed")
	}
	if kept, _ := os.ReadDir(filepath.Join(home, "rejected")); len(kept) != 2 {
		t.Errorf("the agent's changed version is kept for the human, one per file; got %d", len(kept))
	}
}

// TestFilesThatRunCodeLaterAreGuardedAndDenied: git hooks and settings,
// editor tasks and other AI tools' instructions are denied to Claude's file
// tools and put back after a turn that changes them.
func TestFilesThatRunCodeLaterAreGuardedAndDenied(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	ws := config.Workspace{Root: root, Writable: true}
	cfg := &config.Config{}
	cfg.Runtime.HomeDir = home
	gitConfig := filepath.Join(root, ".git", "config")
	_ = os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o700)
	_ = os.WriteFile(gitConfig, []byte("[core]\n"), 0o600)

	ac, err := applyWorkspace(config.AgentConfig{AllowWrites: true}, ws, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{filepath.Join(root, ".git"), filepath.Join(root, ".vscode", "tasks.json"),
		filepath.Join(root, "AGENTS.md"), filepath.Join(root, ".github", "workflows"), home} {
		if !slices.Contains(ac.Protected, want) {
			t.Errorf("%s must be denied to the file tools: %v", want, ac.Protected)
		}
	}

	g := newGuard(cfg, ws)
	hook := filepath.Join(root, ".git", "hooks", "post-checkout")
	tasks := filepath.Join(root, ".vscode", "tasks.json")
	_ = os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o700)
	_ = os.WriteFile(gitConfig, []byte("[core]\n\thooksPath = elsewhere\n"), 0o600)
	_ = os.MkdirAll(filepath.Dir(tasks), 0o700)
	_ = os.WriteFile(tasks, []byte(`{"tasks":[{"runOptions":{"runOn":"folderOpen"}}]}`), 0o600)
	if got := g.Check("codex"); len(got) != 3 {
		t.Fatalf("the hook, git's settings and the editor task must all be undone, got %v", got)
	}
	if data, _ := os.ReadFile(gitConfig); string(data) != "[core]\n" {
		t.Errorf("git's settings must be put back: %q", data)
	}
	for _, p := range []string{hook, tasks} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s must be taken away", p)
		}
	}
}
