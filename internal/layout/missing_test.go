package layout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// unreachableProjectPath is a project folder whose volume is not there: a
// drive letter with no drive on Windows, a root folder that does not exist
// elsewhere (as an unmounted drive's mount point usually is not).
func unreachableProjectPath(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		for l := 'Z'; l >= 'D'; l-- {
			root := string(l) + `:\`
			if _, err := os.Stat(root); err != nil {
				return filepath.Join(root, "work", "proj")
			}
		}
		t.Skip("every drive letter is in use")
	}
	return filepath.Join(string(filepath.Separator), "no-such-volume-"+newID(), "work", "proj")
}

func TestAProjectOnADriveThatIsNotMountedIsNotMissing(t *testing.T) {
	reg, _ := LoadRegistry(filepath.Join(t.TempDir(), "projects.json"))
	away := unreachableProjectPath(t)
	if err := reg.Touch(Project{ID: "away1", Name: "proj", Root: away}); err != nil {
		t.Fatal(err)
	}
	if missing := reg.Missing(); len(missing) != 0 {
		t.Fatalf("a project whose drive is not there is not missing: %+v", missing)
	}
	if e, _ := reg.Find("away1"); !e.MissingSince.IsZero() {
		t.Errorf("nor is it noted as gone: %+v", e)
	}

	// A project deleted from a drive that is there is missing.
	gone := filepath.Join(t.TempDir(), "gone")
	if err := reg.Touch(Project{ID: "gone1", Name: "gone", Root: gone}); err != nil {
		t.Fatal(err)
	}
	if missing := reg.Missing(); len(missing) != 1 || missing[0].ID != "gone1" {
		t.Errorf("a deleted project is missing: %+v", missing)
	}
}
