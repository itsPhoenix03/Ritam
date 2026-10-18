package app_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestAnAccessChangeIsSavedAppliedAndRecorded: raising the level saves it to
// the project's settings, the next turn's grant follows it, and every open
// task's events record the change.
func TestAnAccessChangeIsSavedAppliedAndRecorded(t *testing.T) {
	cfg := mockConfig(t)
	cfg.Runtime.ProjectConfig = filepath.Join(t.TempDir(), ".ritam", "ritam.yaml")
	var approved *config.AccessConfig
	cfg.Runtime.SaveGrant = func(a config.AccessConfig) error { approved = &a; return nil }
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, err := sys.Service.Access(ctx)
	if err != nil || info.Level != domain.AccessFilesOnly {
		t.Fatalf("a workspace starts at level 0: %+v %v", info, err)
	}
	task := createDemoTask(t, sys.Service)

	info, err = sys.Service.SetAccess(ctx, ports.AccessChange{Level: domain.AccessBuildAndTest, Commands: []string{"cmake", "ctest"}})
	if err != nil {
		t.Fatal(err)
	}
	if info.Level != domain.AccessBuildAndTest || !slices.Equal(info.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("SetAccess reports the new settings: %+v", info)
	}
	if g := sys.Access.GrantFor(task, "opus", nil); !slices.Equal(g.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("the next turn's grant follows the change: %+v", g)
	}

	saved, err := config.LoadLayers(cfg.Runtime.ProjectConfig)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Access.Level != 2 || !slices.Equal(saved.Access.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("the change is saved to the project's settings: %+v", saved.Access)
	}
	if approved == nil || approved.Level != 2 || !slices.Equal(approved.Commands, []string{"cmake", "ctest"}) {
		t.Errorf("and recorded as approved on this machine: %+v", approved)
	}

	evs, _ := sys.Service.TaskEvents(ctx, task.ID)
	var access []string
	for _, e := range evs {
		if e.Type == domain.EvtAccessLevel {
			access = append(access, e.Summary)
		}
	}
	if len(access) != 2 || !strings.Contains(access[0], "Files only") || !strings.Contains(access[1], "Build and test") {
		t.Errorf("the task records its starting level and the change: %q", access)
	}
}

// TestAClonedProjectStartsAtLevelZero: settings that came with the project
// but were never approved on this machine are held back, and say so.
func TestAClonedProjectStartsAtLevelZero(t *testing.T) {
	cfg := mockConfig(t)
	cfg.Access = config.AccessConfig{Level: 3, Commands: []string{"cmake"}}
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := sys.Service.Access(context.Background())
	if info.Level != domain.AccessFilesOnly || len(info.Commands) != 0 || !strings.Contains(info.HeldBack, "level 3") {
		t.Errorf("an unapproved level is held back: %+v", info)
	}
	if _, err := sys.Service.SetAccess(context.Background(), ports.AccessChange{Level: domain.AccessBuildAndTest}); err != nil {
		t.Fatal(err)
	}
	if info, _ = sys.Service.Access(context.Background()); info.HeldBack != "" {
		t.Errorf("approving in the settings tab clears the notice: %q", info.HeldBack)
	}
}

func TestBlockedCommandsCannotBeListed(t *testing.T) {
	sys, err := build(t, mockConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = sys.Service.SetAccess(context.Background(), ports.AccessChange{Level: domain.AccessBuildAndTest,
		Commands: []string{"cmake", "rm -rf build"}})
	if err == nil || !strings.Contains(err.Error(), "always blocked") {
		t.Errorf("a destructive command is refused: %v", err)
	}
}

func TestAGlobalLaunchStaysAtLevelZero(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	cfg := mockConfig(t)
	cfg.Workspace.Root = home
	cfg.Access.Level = 3
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	info, _ := sys.Service.Access(ctx)
	if info.Level != domain.AccessFilesOnly || info.Locked == "" {
		t.Errorf("a global launch is pinned to level 0 whatever the settings say: %+v", info)
	}
	if _, err := sys.Service.SetAccess(ctx, ports.AccessChange{Level: domain.AccessAnyCommand}); err == nil {
		t.Error("and cannot be raised")
	}
}
