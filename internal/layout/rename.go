package layout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"projectorch/internal/brand"
)

// Renaming after the app changes its name.
//
// The app home and every project's hidden folder are named after the app.
// When the app is renamed, folders created under the old name would be left
// behind and the projects would look new. RenameHome and RenameProject move
// them, and the files inside named after the app, to the current name. They
// run on every start for each of brand.FormerNames, and on request through
// the CLI's rename command for any other old name.

// RenameHome moves the app home made under oldName to the current name. It
// reports what it did ("" when there was nothing to do). It must run before
// ResolveHome creates the new home.
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
	if !exists(oldRoot) || samePath(oldRoot, newRoot) {
		return "", nil
	}
	if exists(newRoot) {
		return "", fmt.Errorf("both %s and %s exist: merge them by hand, then remove %s", oldRoot, newRoot, oldRoot)
	}
	if err := os.Rename(oldRoot, newRoot); err != nil {
		return "", fmt.Errorf("rename %s to %s: %w (is the old version still running?)", oldRoot, newRoot, err)
	}
	renameInside(newRoot, oldName)
	return fmt.Sprintf("moved the app home %s to %s", oldRoot, newRoot), nil
}

// RenameProject moves a project's hidden folder made under oldName to the
// current name.
func RenameProject(root, oldName string) (string, error) {
	if oldName == "" || oldName == brand.Name {
		return "", nil
	}
	oldDir := filepath.Join(root, brand.HiddenDirFor(oldName))
	newDir := filepath.Join(root, brand.HiddenDir())
	if !exists(oldDir) {
		return "", nil
	}
	if exists(newDir) {
		return "", fmt.Errorf("both %s and %s exist: keep one", oldDir, newDir)
	}
	if err := os.Rename(oldDir, newDir); err != nil {
		return "", fmt.Errorf("rename %s to %s: %w (is the old version still running?)", oldDir, newDir, err)
	}
	renameInside(newDir, oldName)
	return fmt.Sprintf("renamed %s to %s", oldDir, newDir), nil
}

// RenameAll renames the app home and the hidden folder of every project in
// the registry (and of any extra roots) from oldName to the current name.
// It goes on past a failure and reports each one.
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
