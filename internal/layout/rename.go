package layout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"ritam/internal/brand"
)

// Renaming after the app changes its name.
//
// The app home and every project's hidden folder are named after the app.
// When the app is renamed, folders created under the old name would be left
// behind and the projects would look new. RenameHome and RenameProject move
// them, and the files inside named after the app, to the current name: at
// start for each of brand.FormerNames (MigrateFormerName, once per project),
// and on request through the CLI's rename command for any other old name.
//
// A folder is only moved when it is recognisably this program's: a former
// name is an ordinary word, and another tool's ".orch" folder or "orch" app
// home must not be renamed, nor its settings file taken for ritam's own
// trusted global settings.

// BothExistError reports that a folder exists under both the old and the
// current name, so neither can be moved without losing something.
type BothExistError struct{ Old, New string }

func (e *BothExistError) Error() string {
	return fmt.Sprintf("both %s and %s exist: %s uses %s; merge what you need from %s by hand, then remove it",
		e.Old, e.New, brand.Name, e.New, e.Old)
}

// RenameHome moves the app home made under oldName to the current name. It
// reports what it did ("" when there was nothing to do, including when the
// old folder is not this program's). It must run before ResolveHome creates
// the new home.
func RenameHome(oldName string) (string, error) {
	if oldName == "" || oldName == brand.Name {
		return "", nil
	}
	oldRoot, err := homeFor(oldName)
	if err != nil {
		return "", err
	}
	newRoot, err := homeFor(brand.Name)
	if err != nil {
		return "", err
	}
	if !exists(oldRoot) || samePath(oldRoot, newRoot) || !isFormerHome(oldRoot) {
		return "", nil
	}
	if exists(newRoot) {
		return "", &BothExistError{Old: oldRoot, New: newRoot}
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		return "", fmt.Errorf("rename %s to %s: %w (is the old version still running?)", oldRoot, newRoot, err)
	}
	renameInside(newRoot, oldName)
	return fmt.Sprintf("moved the app home %s to %s", oldRoot, newRoot), nil
}

// RenameProject moves a project's hidden folder made under oldName to the
// current name. A folder that is not this program's is left alone.
func RenameProject(root, oldName string) (string, error) {
	if oldName == "" || oldName == brand.Name {
		return "", nil
	}
	oldDir := filepath.Join(root, brand.HiddenDirFor(oldName))
	newDir := filepath.Join(root, brand.HiddenDir())
	if !exists(oldDir) || !isFormerProject(oldDir, oldName) {
		return "", nil
	}
	if exists(newDir) {
		return "", &BothExistError{Old: oldDir, New: newDir}
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return "", fmt.Errorf("rename %s to %s: %w (is the old version still running?)", oldDir, newDir, err)
	}
	renameInside(newDir, oldName)
	return fmt.Sprintf("renamed %s to %s", oldDir, newDir), nil
}

// RenameAll renames the hidden folder of every project in the registry
// (and of any extra roots) from oldName to the current name. It goes on
// past a failure and reports each one.
func RenameAll(oldName string, reg *Registry, extraRoots ...string) ([]string, error) {
	var done []string
	var errs []error
	roots := append([]string(nil), extraRoots...)
	for _, e := range reg.Entries() {
		roots = append(roots, e.Path)
	}
	seen := map[string]bool{}
	for _, r := range roots {
		key := filepath.Clean(r)
		if seen[key] {
			continue
		}
		seen[key] = true
		msg, err := RenameProject(r, oldName)
		if err != nil {
			errs = append(errs, err)
		}
		if msg != "" {
			done = append(done, msg)
		}
	}
	return done, errors.Join(errs...)
}

// MigrateFormerName is the start-up rename from oldName: the project at root
// and every registered project that has not been through it yet. A project
// is marked done in the registry once its folder was renamed or found to
// need nothing, so the work happens once per project rather than at every
// start. A project whose folder cannot be reached (a drive not mounted) is
// tried again next time.
//
// What it did, and what it could not do, come back as notices for the
// human. A folder kept under both names is a notice naming it, not an
// error: one project's leftover must not stop ritam in every other. Only a
// failure to rename root's own folder is an error, since carrying on would
// create a new, empty hidden folder beside it.
func MigrateFormerName(oldName string, reg *Registry, root string) ([]string, error) {
	if oldName == "" || oldName == brand.Name {
		return nil, nil
	}
	roots := []string{root}
	for _, e := range reg.Entries() {
		roots = append(roots, e.Path)
	}
	var notices []string
	seen := map[string]bool{}
	marked := false
	for i, r := range roots {
		key := filepath.Clean(r)
		if seen[key] || reg.migrated(r, oldName) {
			continue
		}
		seen[key] = true
		if !exists(r) {
			continue
		}
		msg, err := RenameProject(r, oldName)
		var both *BothExistError
		switch {
		case errors.As(err, &both):
			notices = append(notices, "warning: "+err.Error())
			continue
		case err != nil && i == 0:
			return notices, err
		case err != nil:
			notices = append(notices, "warning: "+err.Error())
			continue
		}
		if msg != "" {
			notices = append(notices, msg)
		}
		reg.markMigrated(r, oldName)
		marked = true
	}
	if marked {
		if err := reg.Save(); err != nil {
			notices = append(notices, fmt.Sprintf("warning: could not record the rename from %q: %v", oldName, err))
		}
	}
	return notices, nil
}

// isFormerHome reports whether dir is this program's app home from before
// a rename: it holds a record of known projects, which every start writes.
// A settings file alone proves nothing; anyone may have an "orch.yaml".
func isFormerHome(dir string) bool {
	data, err := os.ReadFile(filepath.Join(dir, "projects.json"))
	if err != nil {
		return false
	}
	var entries []Entry
	if json.Unmarshal(data, &entries) != nil || len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if e.ID == "" || e.Path == "" {
			return false
		}
	}
	return true
}

// isFormerProject reports whether dir is this program's hidden folder from
// before a rename: it holds the project's identity file, which every start
// writes, or the project's database under the old name.
func isFormerProject(dir, oldName string) bool {
	var pf projectFile
	if data, err := os.ReadFile(filepath.Join(dir, "project.json")); err == nil &&
		json.Unmarshal(data, &pf) == nil && pf.ID != "" {
		return true
	}
	f, err := os.Open(filepath.Join(dir, brand.DBFileFor(oldName)))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 16)
	n, _ := f.Read(head)
	return bytes.Equal(head[:n], []byte("SQLite format 3\x00"))
}

// renameInside renames the files in dir that carry the old name: the
// database (and its journal and lock files) and the settings file.
func renameInside(dir, oldName string) {
	pairs := [][2]string{
		{brand.ConfigFileFor(oldName), brand.ConfigFile()},
		{brand.DBFileFor(oldName), brand.DBFile()},
		{brand.DBFileFor(oldName) + "-wal", brand.DBFile() + "-wal"},
		{brand.DBFileFor(oldName) + "-shm", brand.DBFile() + "-shm"},
		{brand.DBFileFor(oldName) + ".lock", brand.DBFile() + ".lock"},
	}
	for _, p := range pairs {
		from, to := filepath.Join(dir, p[0]), filepath.Join(dir, p[1])
		if exists(from) && !exists(to) {
			_ = os.Rename(from, to)
		}
	}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
