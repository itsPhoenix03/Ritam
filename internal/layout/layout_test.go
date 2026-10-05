package layout

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"projectorch/internal/brand"
)

func setHome(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home-"+name)
	t.Setenv(brand.HomeEnvFor(name), dir)
	return dir
}

func TestTheAppHomeIsCreatedOnFirstRun(t *testing.T) {
	want := setHome(t, brand.Name)
	h, err := ResolveHome()
	if err != nil {
		t.Fatal(err)
	}
	if h.Root != want {
		t.Errorf("home = %s, want %s (from %s)", h.Root, want, brand.HomeEnv())
	}
	if _, err := os.Stat(h.Root); err != nil {
		t.Errorf("the app home should exist: %v", err)
	}
	for _, p := range []string{h.ConfigPath(), h.ProjectsFile(), h.ModelsFile(), h.BackupsDir(), h.CanaryDir()} {
		if !strings.HasPrefix(p, h.Root) {
			t.Errorf("%s should be inside the app home", p)
		}
	}
	if filepath.Base(h.ConfigPath()) != brand.Name+".yaml" {
		t.Errorf("the settings file is named after the app: %s", h.ConfigPath())
	}
}

func TestAProjectKeepsItsIDAndFindsItAgainAfterItsFolderIsDeleted(t *testing.T) {
	h := Home{Root: setHome(t, brand.Name)}
	reg, err := LoadRegistry(h.ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "my app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	p, err := OpenProject(root, reg)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Created || p.ID == "" || p.Name != "my app" {
		t.Fatalf("first open: %+v", p)
	}
	if p.Dir != filepath.Join(root, brand.HiddenDir()) || !strings.HasPrefix(p.DBPath(), p.Dir) ||
		!strings.HasPrefix(p.TeamFile(), p.Dir) || !strings.HasPrefix(p.TmpDir(), p.Dir) {
		t.Errorf("the project's files belong in its hidden folder: %+v", p)
	}
	if p.Key() != "my_app-"+p.ID {
		t.Errorf("key = %s", p.Key())
	}
	if err := reg.Touch(p); err != nil {
		t.Fatal(err)
	}

	again, _ := OpenProject(root, reg)
	if again.ID != p.ID || again.Created {
		t.Errorf("reopening keeps the id: %+v", again)
	}

	// The hidden folder is deleted: the registry still knows the path, so
	// the project comes back under its old id and finds its backups.
	if err := os.RemoveAll(p.Dir); err != nil {
		t.Fatal(err)
	}
	reg2, _ := LoadRegistry(h.ProjectsFile())
	back, err := OpenProject(root, reg2)
	if err != nil {
		t.Fatal(err)
	}
	if back.ID != p.ID || !back.Created {
		t.Errorf("a recreated hidden folder should reuse the known id: %+v", back)
	}
}

func TestTheRegistryNoticesDeletedProjects(t *testing.T) {
	h := Home{Root: setHome(t, brand.Name)}
	reg, _ := LoadRegistry(h.ProjectsFile())
	keep := filepath.Join(t.TempDir(), "alive")
	gone := filepath.Join(t.TempDir(), "gone")
	for _, d := range []string{keep, gone} {
		_ = os.MkdirAll(d, 0o755)
		p, err := OpenProject(d, reg)
		if err != nil {
			t.Fatal(err)
		}
		if err := reg.Touch(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	missing := reg.Missing()
	if len(missing) != 1 || missing[0].Name != "gone" || missing[0].MissingSince.IsZero() {
		t.Fatalf("missing = %+v", missing)
	}

	// A second process sees the same record, including when it went missing.
	reg2, _ := LoadRegistry(h.ProjectsFile())
	if e, ok := reg2.Find("gone"); !ok || e.MissingSince.IsZero() {
		t.Errorf("the registry on disk should record the missing project: %+v", e)
	}
	if err := reg2.Remove(missing[0].ID); err != nil {
		t.Fatal(err)
	}
	reg3, _ := LoadRegistry(h.ProjectsFile())
	if _, ok := reg3.Find("gone"); ok || len(reg3.Entries()) != 1 {
		t.Errorf("a removed project stays removed: %+v", reg3.Entries())
	}
}

func TestFoldersMadeUnderAnOldNameAreRenamed(t *testing.T) {
	const old = "oldorch"
	oldHome := setHome(t, old)
	setHome(t, brand.Name)
	if err := os.MkdirAll(oldHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldHome, brand.ConfigFileFor(old)), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	msg, err := RenameHome(old)
	if err != nil || msg == "" {
		t.Fatalf("rename home: %q %v", msg, err)
	}
	h, _ := ResolveHome()
	if _, err := os.Stat(h.ConfigPath()); err != nil {
		t.Errorf("the settings file should carry the new name: %v", err)
	}

	root := t.TempDir()
	oldDir := filepath.Join(root, brand.HiddenDirFor(old))
	_ = os.MkdirAll(oldDir, 0o755)
	_ = os.WriteFile(filepath.Join(oldDir, brand.DBFileFor(old)), []byte("db"), 0o600)
	reg, _ := LoadRegistry(h.ProjectsFile())
	done, err := RenameAll(old, reg, root)
	if err != nil || len(done) != 1 {
		t.Fatalf("rename projects: %v %v", done, err)
	}
	if data, err := os.ReadFile(filepath.Join(root, brand.HiddenDir(), brand.DBFile())); err != nil || string(data) != "db" {
		t.Errorf("the database should move with its folder and take the new name: %q %v", data, err)
	}
	if again, err := RenameAll(old, reg, root); err != nil || len(again) != 0 {
		t.Errorf("a second run has nothing to do: %v %v", again, err)
	}
}
