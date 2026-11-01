package app

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"ritam/internal/config"
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
