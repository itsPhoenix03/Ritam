package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAProjectFolderIsAWritableWorkspace(t *testing.T) {
	dir := t.TempDir()
	ws, err := ResolveWorkspace(WorkspaceConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if ws.Global || !ws.Writable || ws.Notice != "" || ws.Root != filepath.Clean(dir) {
		t.Errorf("a project folder is a plain workspace: %+v", ws)
	}
}

// TestALaunchAtAGlobalLocationIsReadOnly: launched from the home folder or a
// drive root, orch would hand agents everything below it.
func TestALaunchAtAGlobalLocationIsReadOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	root := filepath.VolumeName(home) + string(filepath.Separator)
	for _, dir := range []string{home, root} {
		ws, err := ResolveWorkspace(WorkspaceConfig{Root: dir})
		if err != nil {
			t.Fatal(err)
		}
		if !ws.Global || ws.Writable || !strings.Contains(ws.Notice, "read-only") {
			t.Errorf("%s: a global launch must be read-only by default: %+v", dir, ws)
		}
	}
}

func TestAllowingAGlobalLaunchUsesOrchsOwnFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	home, _ := os.UserHomeDir()
	ws, err := ResolveWorkspace(WorkspaceConfig{Root: home, AllowGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".orch", "workspace")
	if !ws.Writable || ws.Root != want {
		t.Errorf("allowed, the agents write only in orch's own folder %s: %+v", want, ws)
	}
}

func TestAnAgentMayNotWorkOutsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	ws := Workspace{Root: root, Writable: true}
	if dir, err := ws.AgentDir(""); err != nil || dir != root {
		t.Errorf("empty means the root: %q %v", dir, err)
	}
	if dir, err := ws.AgentDir("sub/dir"); err != nil || dir != filepath.Join(root, "sub", "dir") {
		t.Errorf("a relative folder is inside the workspace: %q %v", dir, err)
	}
	for _, bad := range []string{"..", "../elsewhere", filepath.Dir(root), os.TempDir()} {
		if _, err := ws.AgentDir(bad); err == nil {
			t.Errorf("%q is outside the workspace and must be refused", bad)
		}
	}
}

// TestTheExampleConfigLoads: the shipped example, workspace section included.
func TestTheExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "orch.example.yaml"))
	if err != nil {
		t.Fatalf("orch.example.yaml: %v", err)
	}
	if cfg.Workspace.AllowGlobal || len(cfg.Workspace.AllowedCommands) != 0 {
		t.Errorf("the example must ship the restrictive workspace defaults: %+v", cfg.Workspace)
	}
}
