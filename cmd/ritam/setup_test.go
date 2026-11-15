package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"ritam/internal/brand"
	"ritam/internal/layout"
)

// inProject runs the test with a temporary app home and a temporary project
// folder as the working directory.
func inProject(t *testing.T) (home, project string) {
	t.Helper()
	base := t.TempDir()
	home, project = filepath.Join(base, "home"), filepath.Join(base, "myproj")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(brand.HomeEnv(), home)
	// The former names' homes too, so no test ever looks at (or moves) a
	// real one on this machine.
	for _, old := range brand.FormerNames {
		t.Setenv(brand.HomeEnvFor(old), filepath.Join(base, "home-"+old))
	}
	t.Chdir(project)
	return home, project
}

// TestStartUpPutsEveryFileInItsPlace: the database, team, sessions and
// scratch folder in the project's hidden folder; the log, backups and model
// records in the app home; and the project's settings over the global ones.
func TestStartUpPutsEveryFileInItsPlace(t *testing.T) {
	home, project := inProject(t)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, brand.ConfigFile()),
		[]byte("orchestrator:\n  maxTaskCostUSD: 20\n  stallMessages: 11\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(project, brand.HiddenDir())
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, brand.ConfigFile()), []byte("orchestrator:\n  maxTaskCostUSD: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, pl, err := prepare("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator.MaxTaskCostUSD != 2 || cfg.Orchestrator.StallMessages != 11 {
		t.Errorf("project settings override global ones: %+v", cfg.Orchestrator)
	}
	inHidden := func(p string) bool { return strings.HasPrefix(p, hidden) }
	inHome := func(p string) bool { return strings.HasPrefix(p, home) }
	if !inHidden(cfg.Storage.Path) || !inHidden(cfg.Sessions.File) || !inHidden(cfg.Runtime.TeamFile) || !inHidden(cfg.Runtime.ScratchDir) {
		t.Errorf("project state belongs in %s: %+v %+v", hidden, cfg.Storage, cfg.Runtime)
	}
	if !inHome(cfg.Logging.File) || !inHome(cfg.Runtime.BackupDir) || !inHome(cfg.Runtime.ModelsFile) {
		t.Errorf("user-level files belong in %s: log %s, %+v", home, cfg.Logging.File, cfg.Runtime)
	}
	if e, ok := pl.reg.ByPath(project); !ok || e.ID != pl.proj.ID {
		t.Errorf("the project should be recorded in the app home: %+v", e)
	}
	if cfg.Workspace.Root != project {
		t.Errorf("the workspace is the project: %s", cfg.Workspace.Root)
	}
}

// writeRegistry records projects in the app home, as earlier runs would.
func writeRegistry(t *testing.T, home string, entries ...layout.Entry) {
	t.Helper()
	data, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "projects.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestAProjectWithBothOldAndNewFoldersWarnsAndRitamStarts: a leftover
// former-name folder beside the current one in some other registered
// project is named in a warning; it does not stop ritam here.
func TestAProjectWithBothOldAndNewFoldersWarnsAndRitamStarts(t *testing.T) {
	home, _ := inProject(t)
	other := filepath.Join(t.TempDir(), "other")
	old := filepath.Join(other, brand.HiddenDirFor(brand.FormerNames[0]))
	for _, d := range []string{old, filepath.Join(other, brand.HiddenDir())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(old, "project.json"), []byte(`{"id":"0a1b2c3d"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeRegistry(t, home, layout.Entry{ID: "0a1b2c3d", Name: "other", Path: other, LastSeen: time.Now()})

	_, pl, err := prepare("")
	if err != nil {
		t.Fatalf("ritam must start: %v", err)
	}
	if !strings.Contains(strings.Join(pl.notices, "\n"), old) {
		t.Errorf("the warning names the old folder: %v", pl.notices)
	}
}

// TestAnUnrelatedAppHomeUnderTheFormerNameIsNotAdopted: a folder that only
// shares the former name (another tool's "orch") is not made ritam's home,
// and its settings file does not become ritam's trusted global settings.
func TestAnUnrelatedAppHomeUnderTheFormerNameIsNotAdopted(t *testing.T) {
	home, _ := inProject(t)
	old := os.Getenv(brand.HomeEnvFor(brand.FormerNames[0]))
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(old, brand.ConfigFileFor(brand.FormerNames[0]))
	if err := os.WriteFile(settings, []byte("orchestrator:\n  stallMessages: 77\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := prepare("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Orchestrator.StallMessages == 77 {
		t.Error("another tool's settings must not become ritam's global settings")
	}
	if _, err := os.Stat(settings); err != nil {
		t.Errorf("the unrelated folder is left as it was: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, brand.ConfigFile())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no global settings are made from it: %v", err)
	}
}

// TestBackupsOfAProjectOnAnUnmountedDriveAreKept: with "delete" and the
// days of grace long past, a project whose drive is not plugged in is not
// taken for deleted, and its backups stay; a project really deleted loses
// them. (The settings refuse zero days of grace, so both projects are
// recorded as gone for two days against a grace of one.)
func TestBackupsOfAProjectOnAnUnmountedDriveAreKept(t *testing.T) {
	home, _ := inProject(t)
	away := filepath.Join(string(filepath.Separator), "no-such-volume-ritam-test", "work", "away")
	if runtime.GOOS == "windows" {
		away = ""
		for l := 'Z'; l >= 'D' && away == ""; l-- {
			if _, err := os.Stat(string(l) + `:\`); err != nil {
				away = filepath.Join(string(l)+`:\`, "work", "away")
			}
		}
		if away == "" {
			t.Skip("every drive letter is in use")
		}
	}
	gone := filepath.Join(t.TempDir(), "gone")
	longGone := time.Now().Add(-48 * time.Hour)
	entries := []layout.Entry{
		{ID: "a0a0a0a0", Name: "away", Path: away, LastSeen: time.Now(), MissingSince: longGone},
		{ID: "b0b0b0b0", Name: "gone", Path: gone, LastSeen: time.Now(), MissingSince: longGone},
	}
	writeRegistry(t, home, entries...)
	for _, e := range entries {
		if err := os.MkdirAll(filepath.Join(home, "backups", e.Key()), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, brand.ConfigFile()),
		[]byte("backups:\n  whenProjectDeleted: delete\n  deleteAfterDays: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := prepare(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "backups", entries[0].Key())); err != nil {
		t.Errorf("the unmounted project's backups must be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "backups", entries[1].Key())); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the deleted project's backups go, as set: %v", err)
	}
}

// TestAnEmptySessionsFileDoesNotStopStartUp: the empty sessions.json a
// power cut can leave is kept aside with a warning, and ritam starts.
func TestAnEmptySessionsFileDoesNotStopStartUp(t *testing.T) {
	_, project := inProject(t)
	hidden := filepath.Join(project, brand.HiddenDir())
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "sessions.json"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, pl, err := prepare("")
	if err != nil {
		t.Fatalf("ritam must start: %v", err)
	}
	if !strings.Contains(strings.Join(pl.notices, "\n"), "sessions.json.bad-") {
		t.Errorf("the human is told where the file was kept: %v", pl.notices)
	}
}

// TestTheDataCommandsWorkTogether: archive -clear empties the project, and
// restore brings the archive back.
func TestTheDataCommandsWorkTogether(t *testing.T) {
	inProject(t)
	var out bytes.Buffer
	if ok, err := runCommand([]string{"where"}, &out); !ok || err != nil {
		t.Fatalf("where: %v %v", ok, err)
	}
	if !strings.Contains(out.String(), "project folder:") {
		t.Errorf("where output:\n%s", out.String())
	}
	out.Reset()
	if _, err := runCommand([]string{"archive", "-clear"}, &out); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if !strings.Contains(out.String(), "archived 0 tasks") || !strings.Contains(out.String(), "cleared") {
		t.Errorf("archive output:\n%s", out.String())
	}
	if ok, _ := runCommand([]string{"not-a-command"}, &out); ok {
		t.Error("an unknown word is not a data command")
	}
	if _, err := runCommand([]string{"backups", "bogus"}, &out); err == nil {
		t.Error("an unknown backups command is an error")
	}
}
