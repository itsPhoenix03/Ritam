package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"ritam/internal/brand"
)

// The workspace: where agents work, and the only place they may write.
//
// ritam is meant to be launched inside a project folder, with that folder as
// the agents' context. The workspace is therefore the folder holding
// ritam.yaml (or workspace.root). Agents run there, and an agent allowed to
// write may write there and nowhere else: not to temp folders, not to the
// rest of the disk.
//
// Launching ritam at a "global" level -- the home folder, a drive root, a
// system folder -- would hand agents everything below it. The agents then
// never run there: they work in ritam's own folder (~/.ritam/workspace),
// read-only unless workspace.allowGlobal lets them write, and their file
// tools read only inside it.
//
// Paths are compared as the filesystem sees them: symbolic links and
// junctions are followed first, so a folder that is really a link to the
// home folder is the home folder.

// WorkspaceConfig configures the workspace.
type WorkspaceConfig struct {
	// Root is the project folder. Empty means the folder holding ritam.yaml.
	Root string `yaml:"root"`
	// AllowGlobal permits write-enabled agents when the root is a global
	// location. They then write only into ritam's own workspace folder.
	AllowGlobal bool `yaml:"allowGlobal"`
	// ConfineReads keeps the agents' file tools reading inside their
	// working folder. Off, they may read anywhere the user can. Always on
	// after a global launch.
	ConfineReads bool `yaml:"confineReads"`
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
	// ConfineReads reports that the agents' file tools read only inside
	// their working folder.
	ConfineReads bool
	// Notice explains a restriction to the human, "" when there is none.
	Notice string
}

// ResolveWorkspace works out the workspace from the configuration. An empty
// root means the current directory (main sets it to ritam.yaml's folder).
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
	ws := Workspace{Root: filepath.Clean(abs), Writable: true, ConfineReads: wc.ConfineReads}
	if !IsGlobalLocation(ws.Root) {
		return ws, nil
	}
	// Agents never run at the global location itself, read-only or not: a
	// read-only agent there could still read the whole home folder.
	launched := ws.Root
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
	ws.Root, ws.Global, ws.ConfineReads = own, true, true
	if !wc.AllowGlobal {
		ws.Writable = false
		ws.Notice = fmt.Sprintf("ritam is running at a global location (%s): agents are read-only and work in "+
			"ritam's own folder %s. Launch ritam inside a project folder, or set workspace.allowGlobal to let "+
			"agents write there.", launched, own)
		return ws, nil
	}
	ws.Notice = fmt.Sprintf("ritam is running at a global location (%s): agents work in %s, ritam's own folder.",
		launched, own)
	return ws, nil
}

// AgentDir resolves an agent's configured working directory against the
// workspace. Empty means the workspace root; anything else must lie inside
// it, links followed.
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

// Within reports whether path is root or lies below it, once both are
// resolved to the real folders they name (see RealPath): a link inside the
// workspace that leads out of it is outside.
func Within(root, path string) bool { return within(RealPath(root), RealPath(path)) }

// within compares paths as written.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" && !strings.EqualFold(filepath.VolumeName(root), filepath.VolumeName(path)) {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// RealPath is path made absolute with every symbolic link and junction along
// it followed. A part that does not exist yet is kept as written, below the
// real path of the deepest folder that does.
func RealPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	var rest []string
	for p := abs; ; {
		if r, err := resolveLinks(p); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return abs
		}
		rest = append([]string{filepath.Base(p)}, rest...)
		p = parent
	}
}

// IsGlobalLocation reports whether dir is too broad to hand to agents: a
// filesystem, drive or network-share root, the home folder or the folder
// holding it, another user's home, the app-data and temp folders, or a
// system folder. dir is checked both as written and as the real folder it
// names, so a link to the home folder counts as the home folder.
func IsGlobalLocation(dir string) bool {
	dir = filepath.Clean(dir)
	return isGlobal(dir) || isGlobal(RealPath(dir))
}

func isGlobal(dir string) bool {
	if isRoot(dir) {
		return true
	}
	var exact, below []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		home = filepath.Clean(home)
		users := filepath.Dir(home)
		exact = append(exact, home, users)
		// Everything else in the folder holding the home folder (C:\Users,
		// /home) is another user's: C:\Users\Public, /home/other/... Unless
		// that folder is a root, where it would be the whole disk.
		if !isRoot(users) && withinFold(users, dir) && !withinFold(home, dir) {
			return true
		}
	}
	exact = append(exact, os.TempDir())
	if runtime.GOOS == "windows" {
		for _, env := range []string{"APPDATA", "LOCALAPPDATA", "TEMP", "TMP"} {
			if v := os.Getenv(env); v != "" {
				exact = append(exact, v)
			}
		}
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			exact = append(exact, filepath.Dir(v)) // AppData
		}
		for _, env := range []string{"SystemRoot", "windir", "ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "ProgramData"} {
			if v := os.Getenv(env); v != "" {
				below = append(below, v)
			}
		}
	} else {
		below = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/opt", "/proc", "/sbin", "/sys", "/usr", "/var",
			"/System", "/Library", "/Applications"}
	}
	for _, e := range exact {
		if samePath(dir, e) {
			return true
		}
	}
	for _, b := range below {
		if withinFold(b, dir) {
			return true
		}
	}
	return false
}

// isRoot reports whether dir is a filesystem, drive or network-share root.
func isRoot(dir string) bool {
	dir = filepath.Clean(dir)
	if v := filepath.VolumeName(dir); v != "" && (dir == v || dir == v+string(filepath.Separator)) {
		return true
	}
	return filepath.Dir(dir) == dir
}

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func withinFold(root, path string) bool {
	if runtime.GOOS == "windows" {
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	return within(filepath.Clean(root), filepath.Clean(path))
}
