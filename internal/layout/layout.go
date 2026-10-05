// Package layout decides where the application keeps its files.
//
// There are two places:
//
//   - The app home, one per user, created on first run: %LOCALAPPDATA%\<name>
//     on Windows, ~/.<name> elsewhere, or $<NAME>_HOME. It holds what belongs
//     to the user rather than to a project: the global settings, the logs,
//     the record of known projects, every project's backups, archives, what
//     was learned about models, and the canary folder for sandbox checks.
//
//   - The project folder, <project>/.<name>, created the first time the app
//     runs in a project. It holds the project's own state: its database, its
//     team, its agent sessions, its settings (which override the global
//     ones), its id, and tmp/ for the agents' scratch files.
//
// The name comes from package brand, so renaming the app renames every path.
package layout

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"projectorch/internal/brand"
)

// Home is the app home.
type Home struct {
	Root string
}

// ResolveHome finds the app home and creates it if needed.
func ResolveHome() (Home, error) {
	root, err := homeFor(brand.Name)
	if err != nil {
		return Home{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Home{}, fmt.Errorf("app home: %w", err)
	}
	return Home{Root: root}, nil
}

// homeFor is where the app home is for a given app name.
func homeFor(name string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(brand.HomeEnvFor(name))); v != "" {
		return filepath.Abs(v)
	}
	if runtime.GOOS == "windows" {
		if v := os.Getenv("LOCALAPPDATA"); v != "" {
			return filepath.Join(v, name), nil
		}
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("app home: %w", err)
	}
	return filepath.Join(h, brand.HiddenDirFor(name)), nil
}

// ConfigPath is the global settings file.
func (h Home) ConfigPath() string { return filepath.Join(h.Root, brand.ConfigFile()) }

// LogsDir holds one log file per project.
func (h Home) LogsDir() string { return filepath.Join(h.Root, "logs") }

// ProjectsFile records the known projects.
func (h Home) ProjectsFile() string { return filepath.Join(h.Root, "projects.json") }

// BackupsDir holds a folder of snapshots per project.
func (h Home) BackupsDir() string { return filepath.Join(h.Root, "backups") }

// ArchivesDir holds archives made when a project's tasks are cleared.
func (h Home) ArchivesDir() string { return filepath.Join(h.Root, "archives") }

// ModelsFile records what was learned about models on this account.
func (h Home) ModelsFile() string { return filepath.Join(h.Root, "models.json") }

// CanaryDir is where sandbox checks try to write.
func (h Home) CanaryDir() string { return filepath.Join(h.Root, "canary") }

// OwnWorkspace is where agents work when the app is launched at a global
// location (the home folder, a drive root) and that is allowed.
func (h Home) OwnWorkspace() string { return filepath.Join(h.Root, "workspace") }

// Project is one project's folder.
type Project struct {
	// Root is the project folder; Dir is its hidden folder inside it.
	Root, Dir string
	// ID identifies the project across moves and renames.
	ID string
	// Name is the project folder's name.
	Name string
	// Created reports that the hidden folder was made on this run.
	Created bool
}

// projectFile is the hidden folder's record of the project's identity.
type projectFile struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
}

// OpenProject opens the project at root, creating its hidden folder on
// first use. A project the registry already knows by its path keeps its id,
// so a project whose hidden folder was deleted finds its backups again.
func OpenProject(root string, reg *Registry) (Project, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Project{}, fmt.Errorf("project: %w", err)
	}
	p := Project{Root: filepath.Clean(abs), Dir: filepath.Join(filepath.Clean(abs), brand.HiddenDir())}
	p.Name = filepath.Base(p.Root)
	if _, err := os.Stat(p.Dir); errors.Is(err, os.ErrNotExist) {
		p.Created = true
	}
	if err := os.MkdirAll(filepath.Join(p.Dir, "tmp"), 0o755); err != nil {
		return Project{}, fmt.Errorf("project: %w", err)
	}
	idPath := filepath.Join(p.Dir, "project.json")
	var pf projectFile
	if data, err := os.ReadFile(idPath); err == nil && json.Unmarshal(data, &pf) == nil && pf.ID != "" {
		p.ID = pf.ID
	} else {
		if known, ok := reg.ByPath(p.Root); ok {
			p.ID = known.ID
		} else {
			p.ID = newID()
		}
		pf = projectFile{ID: p.ID, Created: time.Now()}
		data, _ := json.MarshalIndent(pf, "", "  ")
		if err := writeAtomic(idPath, data); err != nil {
			return Project{}, fmt.Errorf("project: %w", err)
		}
	}
	return p, nil
}

// DBPath is the project's database.
func (p Project) DBPath() string { return filepath.Join(p.Dir, brand.DBFile()) }

// SessionsFile records which CLI session belongs to which task and agent.
func (p Project) SessionsFile() string { return filepath.Join(p.Dir, "sessions.json") }

// TeamFile records the project's recruited agents.
func (p Project) TeamFile() string { return filepath.Join(p.Dir, "team.json") }

// ConfigPath is the project's settings, which override the global ones.
func (p Project) ConfigPath() string { return filepath.Join(p.Dir, brand.ConfigFile()) }

// TmpDir is for the agents' scratch files.
func (p Project) TmpDir() string { return filepath.Join(p.Dir, "tmp") }

// Key names the project's folders in the app home: its name and id, so two
// projects with the same folder name stay apart.
func (p Project) Key() string { return projectKey(p.Name, p.ID) }

func projectKey(name, id string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, name)
	if clean == "" {
		clean = "project"
	}
	return clean + "-" + id
}

// BackupDir is where the project's snapshots go.
func (h Home) BackupDir(p Project) string { return filepath.Join(h.BackupsDir(), p.Key()) }

// LogPath is the project's log file.
func (h Home) LogPath(p Project) string { return filepath.Join(h.LogsDir(), p.Key()+".log") }

func newID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano()&0xffffffff)
	}
	return hex.EncodeToString(b)
}

// writeAtomic writes data to path through a temporary file and a rename, so
// a crash never leaves a half-written file.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// samePath compares paths the way the OS does.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}
