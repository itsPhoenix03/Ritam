package layout

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"ritam/internal/brand"
)

// TestAnApprovalCoversOneFolderOnly: access approved for a project is kept
// in the app home, survives a reload, and does not cover a copy of the
// project in another folder, even one carrying the same id.
func TestAnApprovalCoversOneFolderOnly(t *testing.T) {
	h := Home{Root: setHome(t, brand.Name)}
	reg, err := LoadRegistry(h.ProjectsFile())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "app")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := OpenProject(root, reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Touch(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Grant(p.ID, p.Root); ok {
		t.Fatal("a new project has no approval")
	}
	if err := reg.SetGrant(p.ID, AccessGrant{Path: p.Root, Level: 2, Commands: []string{"cmake"}}); err != nil {
		t.Fatal(err)
	}

	reg2, _ := LoadRegistry(h.ProjectsFile())
	if g, ok := reg2.Grant(p.ID, p.Root); !ok || g.Level != 2 || len(g.Commands) != 1 {
		t.Errorf("the approval is kept in the app home: %+v %v", g, ok)
	}
	if _, ok := reg2.Grant(p.ID, filepath.Join(t.TempDir(), "clone")); ok {
		t.Error("a copy elsewhere with the same id is not approved")
	}
	if err := reg2.Touch(p); err != nil {
		t.Fatal(err)
	}
	if _, ok := reg2.Grant(p.ID, p.Root); !ok {
		t.Error("opening the project again keeps its approval")
	}
}

// TestOnlyOldCanaryFoldersAreRemovedAtStart: a sandbox check cut short can
// leave its folder; a start removes it once it is old, but never the folder
// of a check that may still be running, in this ritam or another.
func TestOnlyOldCanaryFoldersAreRemovedAtStart(t *testing.T) {
	h := Home{Root: setHome(t, brand.Name)}
	if err := PruneCanary(h); err != nil {
		t.Fatal(err)
	}
	leave := func(name string, age time.Duration) string {
		t.Helper()
		dir := filepath.Join(h.CanaryDir(), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "canary.txt"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	stale := leave("check-old", CanaryMaxAge+time.Minute)
	running := leave("check-running", 2*time.Minute)
	oldFile := filepath.Join(h.CanaryDir(), "canary-old.txt") // as an older version left them
	_ = os.WriteFile(oldFile, []byte("x"), 0o600)
	when := time.Now().Add(-2 * CanaryMaxAge)
	_ = os.Chtimes(oldFile, when, when)

	if err := PruneCanary(h); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{stale, oldFile} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s is old and should be gone: %v", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(running, "canary.txt")); err != nil {
		t.Errorf("a check that may still be running keeps its file: %v", err)
	}
	if fi, err := os.Stat(h.CanaryDir()); err != nil || !fi.IsDir() {
		t.Errorf("the canary folder stays: %v", err)
	}
}
