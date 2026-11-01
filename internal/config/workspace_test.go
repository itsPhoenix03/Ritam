package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
// drive root, ritam would hand agents everything below it.
func TestALaunchAtAGlobalLocationIsReadOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	root := filepath.VolumeName(home) + string(filepath.Separator)
	own := t.TempDir()
	for _, dir := range []string{home, root} {
		ws, err := ResolveWorkspace(WorkspaceConfig{Root: dir, OwnDir: own})
		if err != nil {
			t.Fatal(err)
		}
		if !ws.Global || ws.Writable || !strings.Contains(ws.Notice, "read-only") {
			t.Errorf("%s: a global launch must be read-only by default: %+v", dir, ws)
		}
		// Read-only is not enough: an agent running at the home folder could
		// still read all of it. It runs in ritam's own folder, reads confined.
		if ws.Root != own || !ws.ConfineReads {
			t.Errorf("%s: a global launch runs the agents in ritam's own folder, reads confined: %+v", dir, ws)
		}
	}
}

func TestAllowingAGlobalLaunchUsesRitamsOwnFolder(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	home, _ := os.UserHomeDir()
	ws, err := ResolveWorkspace(WorkspaceConfig{Root: home, AllowGlobal: true})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".ritam", "workspace")
	if !ws.Writable || ws.Root != want {
		t.Errorf("allowed, the agents write only in ritam's own folder %s: %+v", want, ws)
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
	cfg, err := Load(filepath.Join("..", "..", "ritam.example.yaml"))
	if err != nil {
		t.Fatalf("ritam.example.yaml: %v", err)
	}
	if cfg.Workspace.AllowGlobal || cfg.Access.Level != 0 || len(cfg.Access.Commands) != 0 {
		t.Errorf("the example must ship the restrictive defaults: %+v %+v", cfg.Workspace, cfg.Access)
	}
}

// TestOtherGlobalLocations: other users' homes, the app-data and temp
// folders and network-share roots hand agents too much as well.
func TestOtherGlobalLocations(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home folder")
	}
	users := filepath.Dir(home)
	global := []string{
		users,
		filepath.Join(users, "someone-else"),
		filepath.Join(users, "someone-else", "project"),
		os.TempDir(),
	}
	if runtime.GOOS == "windows" {
		global = append(global, `\\server\share`, `\\server\share\`, os.Getenv("APPDATA"), os.Getenv("LOCALAPPDATA"),
			filepath.Dir(os.Getenv("LOCALAPPDATA")))
	}
	for _, dir := range global {
		if dir != "" && !IsGlobalLocation(dir) {
			t.Errorf("%s must count as a global location", dir)
		}
	}
	for _, dir := range []string{filepath.Join(home, "projects", "app"), t.TempDir()} {
		if IsGlobalLocation(dir) {
			t.Errorf("%s is a project folder, not a global location", dir)
		}
	}
}

// TestLinksAreFollowed: a folder inside the workspace that is a link to
// somewhere else is outside, and a project folder that is a link to the home
// folder is the home folder.
func TestLinksAreFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "sub")
	if err := makeLink(outside, link); err != nil {
		t.Skipf("cannot make a link here: %v", err)
	}
	ws := Workspace{Root: root, Writable: true}
	if _, err := ws.AgentDir("sub"); err == nil {
		t.Error("a working folder that links out of the workspace must be refused")
	}
	if Within(root, filepath.Join(link, "deeper", "not-yet")) {
		t.Error("a path below a link out of the workspace is outside it")
	}
	if !Within(outside, filepath.Join(link, "file.txt")) {
		t.Error("a path through the link is inside the folder it leads to")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	toHome := filepath.Join(t.TempDir(), "project")
	if err := makeLink(home, toHome); err != nil {
		t.Skipf("cannot make a link here: %v", err)
	}
	if !IsGlobalLocation(toHome) {
		t.Error("a project folder that is a link to the home folder is a global location")
	}
}

// makeLink makes link point at the folder target: a junction on Windows,
// which needs no special privilege, a symbolic link elsewhere.
func makeLink(target, link string) error {
	if runtime.GOOS == "windows" {
		out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	}
	return os.Symlink(target, link)
}
