package layout

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ritam/internal/brand"
)

// writeFormerHome makes dir look like an app home this program made: it
// holds a record of known projects.
func writeFormerHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := `[{"id":"0a1b2c3d","name":"p","path":"` + filepath.ToSlash(filepath.Join(dir, "p")) + `"}]`
	if err := os.WriteFile(filepath.Join(dir, "projects.json"), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

// formerProject makes root/.<old> look like a hidden folder this program
// made, and returns it.
func formerProject(t *testing.T, root, old string) string {
	t.Helper()
	dir := filepath.Join(root, brand.HiddenDirFor(old))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), []byte(`{"id":"0a1b2c3d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAFolderUnderTheFormerNameThatIsNotOursIsLeftAlone(t *testing.T) {
	const old = "oldorch"
	oldHome := setHome(t, old)
	newHome := setHome(t, brand.Name)
	if err := os.MkdirAll(oldHome, 0o755); err != nil {
		t.Fatal(err)
	}
	// Another tool's home that happens to share the old name, with a
	// settings file that must never become ritam's trusted settings.
	if err := os.WriteFile(filepath.Join(oldHome, brand.ConfigFileFor(old)), []byte("access:\n  level: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := RenameHome(old); err != nil || msg != "" {
		t.Errorf("an unrelated app home is not adopted: %q %v", msg, err)
	}
	if _, err := os.Stat(newHome); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the new home is not made from it: %v", err)
	}

	root := t.TempDir()
	other := filepath.Join(root, brand.HiddenDirFor(old))
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(other, "notes.txt"), []byte("someone else's"), 0o600)
	_ = os.WriteFile(filepath.Join(other, brand.DBFileFor(old)), []byte("not a database"), 0o600)
	reg, _ := LoadRegistry(filepath.Join(t.TempDir(), "projects.json"))
	notices, err := MigrateFormerName(old, reg, root)
	if err != nil || len(notices) != 0 {
		t.Errorf("nothing to do: %v %v", notices, err)
	}
	if _, err := os.Stat(filepath.Join(other, "notes.txt")); err != nil {
		t.Errorf("the unrelated folder stays where it is: %v", err)
	}
	if exists(filepath.Join(root, brand.HiddenDir())) {
		t.Error("no hidden folder is made from it")
	}
}

func TestTheFormerFolderIsMigratedOnceAndRecorded(t *testing.T) {
	const old = "oldorch"
	reg, _ := LoadRegistry(filepath.Join(t.TempDir(), "projects.json"))
	root := t.TempDir()
	formerProject(t, root, old)

	notices, err := MigrateFormerName(old, reg, root)
	if err != nil || len(notices) != 1 || !strings.Contains(notices[0], "renamed") {
		t.Fatalf("the genuine former folder is renamed: %v %v", notices, err)
	}
	p, err := OpenProject(root, reg)
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != "0a1b2c3d" || p.Created {
		t.Errorf("the project keeps its identity: %+v", p)
	}
	if err := reg.Touch(p); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := LoadRegistry(reg.path)
	if e, _ := reloaded.ByPath(root); len(e.Migrated) != 1 || e.Migrated[0] != old {
		t.Fatalf("the project records that the rename is done: %+v", e)
	}

	// A folder appearing under the old name later (an old copy restored, an
	// old version run once) is not looked at again at every start.
	formerProject(t, root, old)
	if notices, err := MigrateFormerName(old, reloaded, root); err != nil || len(notices) != 0 {
		t.Errorf("the migration runs once per project: %v %v", notices, err)
	}
}

func TestAProjectWithBothFoldersIsAWarningNotAnError(t *testing.T) {
	const old = "oldorch"
	reg, _ := LoadRegistry(filepath.Join(t.TempDir(), "projects.json"))
	elsewhere := t.TempDir()
	if err := reg.Touch(Project{ID: "e1", Name: "elsewhere", Root: elsewhere}); err != nil {
		t.Fatal(err)
	}
	oldDir := formerProject(t, elsewhere, old)
	if err := os.MkdirAll(filepath.Join(elsewhere, brand.HiddenDir()), 0o755); err != nil {
		t.Fatal(err)
	}

	here := t.TempDir()
	notices, err := MigrateFormerName(old, reg, here)
	if err != nil {
		t.Fatalf("another project's leftover must not stop ritam here: %v", err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], oldDir) {
		t.Errorf("the warning names the folder: %v", notices)
	}
	if e, _ := reg.ByPath(elsewhere); len(e.Migrated) != 0 {
		t.Errorf("an unsettled project is looked at again next time: %+v", e)
	}
	if notices, _ := MigrateFormerName(old, reg, here); len(notices) != 1 {
		t.Errorf("the warning stays until the human settles it: %v", notices)
	}
}

func TestBothAppHomesIsReportedByName(t *testing.T) {
	const old = "oldorch"
	oldHome := setHome(t, old)
	newHome := setHome(t, brand.Name)
	writeFormerHome(t, oldHome)
	if err := os.MkdirAll(newHome, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := RenameHome(old)
	var both *BothExistError
	if !errors.As(err, &both) || both.Old != oldHome || !strings.Contains(err.Error(), oldHome) {
		t.Errorf("both homes are reported as such, naming the old one: %v", err)
	}
}
