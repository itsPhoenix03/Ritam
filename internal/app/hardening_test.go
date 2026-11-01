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
