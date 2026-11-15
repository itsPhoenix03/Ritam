package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ritam/internal/backup"
	"ritam/internal/brand"
)

// writeGlobal writes the global settings file in the app home.
func writeGlobal(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, brand.ConfigFile()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBackupsNowKeepsTheBackupItTook: with keep 1 and a snapshot dated in
// the future (a clock set back since), pruning after `backups now` used to
// delete the backup just asked for.
func TestBackupsNowKeepsTheBackupItTook(t *testing.T) {
	home, _ := inProject(t)
	writeGlobal(t, home, "backups:\n  keep: 1\n")
	_, pl, err := prepare("")
	if err != nil {
		t.Fatal(err)
	}
	dir := pl.home.BackupDir(pl.proj)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(dir, "20991231-235959.000-periodic.db.gz")
	if err := os.WriteFile(future, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := runCommand([]string{"backups", "now"}, &out); err != nil {
		t.Fatalf("backups now: %v", err)
	}
	taken := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out.String(), "\n", 2)[0], "backup taken:"))
	if _, err := os.Stat(taken); err != nil {
		t.Errorf("the backup just taken must survive pruning (%s): %v\n%s", taken, err, out.String())
	}
	if list, _ := backup.List(dir); len(list) != 2 {
		t.Errorf("the newest and the one just taken are kept: %+v", list)
	}
}

// TestASettingsMistakeStopsEveryCommand: a negative keep used to reach
// `backups now` and panic; now the settings do not load.
func TestASettingsMistakeStopsEveryCommand(t *testing.T) {
	home, _ := inProject(t)
	writeGlobal(t, home, "backups:\n  keep: -1\n")
	var out bytes.Buffer
	_, err := runCommand([]string{"backups", "now"}, &out)
	if err == nil || !strings.Contains(err.Error(), "backups.keep must be at least 1, got -1") {
		t.Errorf("want the settings error, got %v", err)
	}
}
