package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestAccessDefaultsToFilesOnly(t *testing.T) {
	cfg, err := LoadLayers(filepath.Join(t.TempDir(), "none.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.Level != 0 || len(cfg.Access.Commands) != 0 {
		t.Errorf("the default is level 0 with no commands: %+v", cfg.Access)
	}
}

func TestTheProjectsAccessOverridesTheGlobalOne(t *testing.T) {
	dir := t.TempDir()
	global, project := filepath.Join(dir, "global.yaml"), filepath.Join(dir, "project.yaml")
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(global, "access:\n  level: 2\n  commands: [go test]\n")
	write(project, "access:\n  commands: [cmake, ctest]\n")
	cfg, err := LoadLayers(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.Level != 2 {
		t.Errorf("a level the project leaves out comes from the global settings: %d", cfg.Access.Level)
	}
	if !slices.Equal(cfg.Access.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("the project's list replaces the global one: %v", cfg.Access.Commands)
	}

	write(project, "access:\n  level: 7\n")
	if _, err := LoadLayers(global, project); err == nil {
		t.Error("a level outside 0..3 is an error")
	}
}

// TestAProjectGetsOnlyWhatThisMachineApproved: a project's settings travel
// with it, so a clone that asks for more than was approved here is held
// back.
func TestAProjectGetsOnlyWhatThisMachineApproved(t *testing.T) {
	cfg := Default()
	cfg.Access = AccessConfig{Level: 3, Commands: []string{"cmake", "python evil.py"}}

	granted, held := cfg.Trusted()
	if granted.Level != 0 || len(granted.Commands) != 0 {
		t.Errorf("nothing approved, nothing granted: %+v", granted)
	}
	if !strings.Contains(held, "level 3") || !strings.Contains(held, "python evil.py") {
		t.Errorf("the notice says what was held back: %q", held)
	}

	cfg.Runtime.Grant = &AccessConfig{Level: 2, Commands: []string{"cmake"}}
	granted, held = cfg.Trusted()
	if granted.Level != 2 || !slices.Equal(granted.Commands, []string{"cmake"}) {
		t.Errorf("what was approved here is granted, no more: %+v", granted)
	}
	if !strings.Contains(held, "level 3") || strings.Contains(held, "cmake") {
		t.Errorf("only the excess is held back: %q", held)
	}

	cfg.Access = AccessConfig{Level: 1, Commands: []string{"cmake"}}
	if granted, held = cfg.Trusted(); granted.Level != 1 || held != "" {
		t.Errorf("asking for less than was approved is granted in full: %+v %q", granted, held)
	}

	// The global settings are the user's own file, trusted as they are.
	cfg.Runtime.Grant = nil
	cfg.Runtime.GlobalAccess = AccessConfig{Level: 2, Commands: []string{"cmake"}}
	cfg.Access = AccessConfig{Level: 2, Commands: []string{"cmake"}}
	if granted, held = cfg.Trusted(); granted.Level != 2 || held != "" {
		t.Errorf("a level set in the global settings needs no approval: %+v %q", granted, held)
	}
}

func TestSaveAccessKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".ritam", "ritam.yaml")
	if err := SaveAccess(path, AccessConfig{Level: 1, Commands: []string{"cmake"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Access.Level != 1 || !slices.Equal(cfg.Access.Commands, []string{"cmake"}) {
		t.Errorf("a new file holds the access settings: %+v", cfg.Access)
	}

	orig := "# my notes\norchestrator:\n  maxTaskCostUSD: 2 # cheap\naccess:\n  level: 1\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveAccess(path, AccessConfig{Level: 3}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	for _, want := range []string{"# my notes", "maxTaskCostUSD: 2", "# cheap", "level: 3", "commands: []"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("saved file lost %q:\n%s", want, data)
		}
	}
	if strings.Count(string(data), "access:") != 1 {
		t.Errorf("the access entry is replaced, not repeated:\n%s", data)
	}
	if cfg, err = LoadLayers(path); err != nil || cfg.Orchestrator.MaxTaskCostUSD != 2 || cfg.Access.Level != 3 {
		t.Errorf("reloaded: %+v %v", cfg, err)
	}
}
