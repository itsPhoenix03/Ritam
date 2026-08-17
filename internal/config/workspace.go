package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"projectorch/internal/brand"
)

// The workspace: where agents work, and the only place they may write.
//
// orch is meant to be launched inside a project folder, with that folder as
// the agents' context. The workspace is therefore the folder holding
// orch.yaml (or workspace.root). Agents run there, and an agent allowed to
// write may write there and nowhere else: not to temp folders, not to the
// rest of the disk.
//
// Launching orch at a "global" level -- the home folder, a drive root, a
// system folder -- would hand agents everything below it. That is refused by
// default: the agents run read-only and orch says why. A user who really
// wants it sets workspace.allowGlobal, and even then agents write only into
// orch's own folder (~/.orch/workspace), never into the folder orch was
// launched from.

// WorkspaceConfig configures the workspace.
type WorkspaceConfig struct {
	// Root is the project folder. Empty means the folder holding orch.yaml.
	Root string `yaml:"root"`
	// AllowGlobal permits write-enabled agents when the root is a global
	// location. They then write only into orch's own workspace folder.
	AllowGlobal bool `yaml:"allowGlobal"`
	// AllowedCommands are shell commands every write-enabled agent may run,
	// as command prefixes ("go test", "go build", "npm test"). Empty means
	// none: such an agent edits files but runs nothing.
	AllowedCommands []string `yaml:"allowedCommands"`
	// OwnDir is the app's own workspace folder in the app home, used after
	// a global launch. Set at start, never configured.
	OwnDir string `yaml:"-"`
}

// Workspace is the resolved workspace.
type Workspace struct {
	// Root is where the agents run.
	Root string
	// Global reports that the configured root is a global location.
	Global bool
	// Writable reports whether write-enabled agents may write at all.
	Writable bool
	// Notice explains a restriction to the human, "" when there is none.
	Notice string
}

// ResolveWorkspace works out the workspace from the configuration. An empty
// root means the current directory (main sets it to orch.yaml's folder).
func ResolveWorkspace(wc WorkspaceConfig) (Workspace, error) {
	root := wc.Root
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return Workspace{}, fmt.Errorf("workspace: %w", err)
		}
		root = wd
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	ws := Workspace{Root: filepath.Clean(abs), Writable: true}
	if !IsGlobalLocation(ws.Root) {
		return ws, nil
	}
	ws.Global = true
	if !wc.AllowGlobal {
		ws.Writable = false
		ws.Notice = fmt.Sprintf("orch is running at a global location (%s): agents are read-only. "+
			"Launch orch inside a project folder, or set workspace.allowGlobal to let agents write "+
			"in orch's own workspace folder.", ws.Root)
		return ws, nil
	}
	own := wc.OwnDir
	if own == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Workspace{}, fmt.Errorf("workspace: %w", err)
		}
		own = filepath.Join(home, brand.HiddenDir(), "workspace")
	}
	if err := os.MkdirAll(own, 0o700); err != nil {
		return Workspace{}, fmt.Errorf("workspace: %w", err)
	}
	ws.Root = own
	ws.Notice = fmt.Sprintf("orch is running at a global location: agents work in %s, orch's own folder.", own)
	return ws, nil
}

// AgentDir resolves an agent's configured working directory against the
// workspace. Empty means the workspace root; anything else must lie inside
// it.
func (ws Workspace) AgentDir(dir string) (string, error) {
	if dir == "" {
		return ws.Root, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(ws.Root, dir)
	}
	dir = filepath.Clean(dir)
	if !Within(ws.Root, dir) {
		return "", fmt.Errorf("working directory %s is outside the workspace %s", dir, ws.Root)
	}
	return dir, nil
}

// Within reports whether path is root or lies below it.
func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(path)) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// IsGlobalLocation reports whether dir is too broad to hand to agents: a
// filesystem or drive root, the home folder itself, or a system folder.
func IsGlobalLocation(dir string) bool {
	dir = filepath.Clean(dir)
	if dir == filepath.VolumeName(dir)+string(filepath.Separator) || dir == string(filepath.Separator) ||
		filepath.Dir(dir) == dir {
		return true
	}
	same := func(a, b string) bool {
		a, b = filepath.Clean(a), filepath.Clean(b)
		if runtime.GOOS == "windows" {
			return strings.EqualFold(a, b)
		}
		return a == b
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if same(dir, home) || same(dir, filepath.Dir(home)) {
			return true
		}
	}
	var system []string
	if runtime.GOOS == "windows" {
		for _, env := range []string{"SystemRoot", "windir", "ProgramFiles", "ProgramFiles(x86)", "ProgramData"} {
			if v := os.Getenv(env); v != "" {
				system = append(system, v)
			}
		}
	} else {
		system = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/opt", "/proc", "/sbin", "/sys", "/usr", "/var",
			"/System", "/Library", "/Applications"}
	}
	for _, s := range system {
		if same(dir, s) || withinFold(s, dir) {
			return true
		}
	}
	return false
}

func withinFold(root, path string) bool {
	if runtime.GOOS == "windows" {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	return Within(root, path)
}
