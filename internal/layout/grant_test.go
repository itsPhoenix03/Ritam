package layout

import (
	"os"
	"path/filepath"
	"testing"

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
